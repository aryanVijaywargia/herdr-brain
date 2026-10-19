package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// Skill is the reviewed content of a skill item: the header of its SKILL.md and the steps.
// The text of a skill revision is the whole file, so the file that the compiler writes is the
// text that you approved.
//
// The text form (E16-S1 tested it):
//
//	---
//	name: <name>
//	description: <one line>
//	required-tools: <tools, or none>
//	not-available: <optional: a required tool that a CLI does not have, for example gh in codex>
//	example: <one example of use>
//	---
//
//	<the steps>
type Skill struct {
	Name          string
	Description   string
	RequiredTools string
	// NotAvailable says which required tool is not available in which CLI, or "".
	NotAvailable string
	Example      string
	Steps        string
}

// skillName is the form of a skill name: lower-case letters and digits, joined by hyphens.
// The form keeps the "<name>: <description>" entry of the brief clear.
var skillName = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// The header keys of a skill text, and whether each must be there.
var skillKeys = map[string]bool{
	"name":           true,
	"description":    true,
	"required-tools": true,
	"not-available":  false,
	"example":        true,
}

// ParseSkill reads the text of a skill revision. It refuses a text with no header, an unknown
// or repeated header key, a missing required key, a bad name, or no steps.
func ParseSkill(text string) (Skill, error) {
	rest, ok := strings.CutPrefix(text, "---\n")
	if !ok {
		return Skill{}, errors.New("skill text: want a header that starts with a --- line")
	}
	header, steps, ok := strings.Cut(rest, "\n---\n")
	if !ok {
		return Skill{}, errors.New("skill text: want a --- line that ends the header")
	}
	fields := make(map[string]string, len(skillKeys))
	for _, line := range strings.Split(header, "\n") {
		k, v, ok := strings.Cut(line, ":")
		if _, known := skillKeys[k]; !ok || !known {
			return Skill{}, fmt.Errorf("skill text: header line %q: want one of name, description, required-tools, not-available, example", line)
		}
		if _, dup := fields[k]; dup {
			return Skill{}, fmt.Errorf("skill text: header key %s is there twice", k)
		}
		fields[k] = strings.TrimSpace(v)
	}
	for k, required := range skillKeys {
		if required && fields[k] == "" {
			return Skill{}, fmt.Errorf("skill text: header key %s is missing or empty", k)
		}
	}
	sk := Skill{Name: fields["name"], Description: fields["description"], RequiredTools: fields["required-tools"],
		NotAvailable: fields["not-available"], Example: fields["example"], Steps: strings.TrimSpace(steps)}
	if !skillName.MatchString(sk.Name) {
		return Skill{}, fmt.Errorf("skill text: name %q: use lower-case letters, digits and hyphens", sk.Name)
	}
	if sk.Steps == "" {
		return Skill{}, errors.New("skill text: no steps after the header")
	}
	return sk, nil
}

// ErrSkillName means that another active skill in the same scope has the name already.
var ErrSkillName = errors.New("another active skill in this scope has this name")

// ActiveSkills reads the current revision of each delivered skill item, in item id order: the
// active skills and the disputed ones.
func ActiveSkills(ctx context.Context, tx *sql.Tx) ([]ItemRevision, error) {
	return activeOfKind(ctx, tx, KindSkill)
}

// ScopeSkills returns the name of each active skill in the scope with key workspaceKey, by its
// claim id: the global skills and the skills of that workspace ("" for global only). A failure
// that one of them covers comes back to it (E20-S5).
func ScopeSkills(ctx context.Context, tx *sql.Tx, workspaceKey string) (map[string]string, error) {
	rs, err := query(ctx, tx, `SELECT i.claim_id, r.text FROM items i
		JOIN item_revisions r ON r.item_id = i.id AND r.revision = i.current_revision
		WHERE i.kind = ? AND i.state = ? AND COALESCE(i.workspace_key, '') IN (?, '') ORDER BY i.id`,
		func(rows *sql.Rows) ([2]string, error) {
			var r [2]string
			err := rows.Scan(&r[0], &r[1])
			return r, err
		}, KindSkill, StateActive, workspaceKey)
	if err != nil {
		return nil, err
	}
	skills := make(map[string]string, len(rs))
	for _, r := range rs {
		sk, err := ParseSkill(r[1])
		if err != nil {
			return nil, err
		}
		skills[r[0]] = sk.Name
	}
	return skills, nil
}

// ActiveClaimSkills returns the ids of the active skill items of claim claimID in the scope
// with key workspaceKey, as ActiveClaimItems does: the skills that a failure of the claim comes
// back to (E20-S5).
func ActiveClaimSkills(ctx context.Context, tx *sql.Tx, claimID, workspaceKey string) ([]int64, error) {
	return query(ctx, tx, `SELECT id FROM items WHERE claim_id = ? AND state = ? AND kind = ?
		AND (workspace_key IS NULL OR workspace_key = ?) ORDER BY id`,
		func(rows *sql.Rows) (int64, error) {
			var id int64
			err := rows.Scan(&id)
			return id, err
		}, claimID, StateActive, KindSkill, workspaceKey)
}

// activeOfKind reads the current revision of each active or disputed item of kind, in item id
// order.
func activeOfKind(ctx context.Context, tx *sql.Tx, kind Kind) ([]ItemRevision, error) {
	rows, err := tx.QueryContext(ctx, `SELECT `+revisionColumns+` FROM item_revisions r
		JOIN items i ON r.item_id = i.id AND r.revision = i.current_revision
		WHERE i.state IN (?, ?) AND i.kind = ? ORDER BY i.id`, StateActive, StateDisputed, kind)
	if err != nil {
		return nil, fmt.Errorf("read active %ss: %w", kind, err)
	}
	defer rows.Close() //nolint:errcheck // read-only: rows.Err reports the read errors
	var rs []ItemRevision
	for rows.Next() {
		r, err := scanRevision(rows)
		if err != nil {
			return nil, fmt.Errorf("read active %ss: %w", kind, err)
		}
		rs = append(rs, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read active %ss: %w", kind, err)
	}
	return rs, nil
}

// CheckSkill checks the current revision of item, when it is an active skill: its text must
// be a skill text, and no other active skill in the same scope (one workspace key, or global)
// may have its name. WriteRevision calls it after each revision write, in the same
// transaction, so approval, restore, undo, promotion (E16-S8) and link (E16-S9) all check the
// state that the transaction commits. A clash wraps ErrSkillName. Any other item passes.
func CheckSkill(ctx context.Context, tx *sql.Tx, item int64) error {
	it, err := ItemByID(ctx, tx, item)
	if err != nil {
		return err
	}
	if it.Kind != KindSkill || it.State != StateActive {
		return nil
	}
	cur, err := ItemRevisionByKey(ctx, tx, item, it.CurrentRevision)
	if err != nil {
		return err
	}
	sk, err := ParseSkill(cur.Text)
	if err != nil {
		return err
	}
	other, err := nameTaken(ctx, tx, KindSkill, cur, sk.Name, func(text string) (string, error) {
		o, err := ParseSkill(text)
		return o.Name, err
	})
	if err != nil || other == 0 {
		return err
	}
	return fmt.Errorf("skill %q: item %d: %w", sk.Name, other, ErrSkillName)
}

// nameTaken returns the id of another active or disputed item of kind that has name in the
// scope of cur (one workspace key, or global), or 0 when there is none. nameOf reads the name
// from an item text.
func nameTaken(ctx context.Context, tx *sql.Tx, kind Kind, cur ItemRevision, name string,
	nameOf func(text string) (string, error)) (int64, error) {
	all, err := activeOfKind(ctx, tx, kind)
	if err != nil {
		return 0, err
	}
	for _, o := range all {
		if o.ItemID == cur.ItemID || o.Scope != cur.Scope || ptrString(o.WorkspaceKey) != ptrString(cur.WorkspaceKey) {
			continue
		}
		// An active item passed this check when it became active, so its text parses.
		if n, err := nameOf(o.Text); err == nil && n == name {
			return o.ItemID, nil
		}
	}
	return 0, nil
}

func ptrString(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// Text is the text form of sk, the text of its skill revision: ParseSkill reads it back.
func (sk Skill) Text() string {
	var b strings.Builder
	fmt.Fprintf(&b, "---\nname: %s\ndescription: %s\nrequired-tools: %s\n", sk.Name, sk.Description, sk.RequiredTools)
	if sk.NotAvailable != "" {
		fmt.Fprintf(&b, "not-available: %s\n", sk.NotAvailable)
	}
	fmt.Fprintf(&b, "example: %s\n---\n\n%s\n", sk.Example, sk.Steps)
	return b.String()
}

// SkillDiff is the diff of a skill patch: only the changed lines of next against old,
// each "- " for a removed line and "+ " for an added line. Each run of changed lines starts
// with "@@ line <n>", n the line of old where it starts. The lines that stay are not in it.
// The diff of a new skill ("" for old) adds all its lines.
func SkillDiff(old, next string) string {
	a, b := diffLines(old), diffLines(next)
	// lcs[i][j] is the length of the longest common subsequence of a[i:] and b[j:].
	lcs := make([][]int, len(a)+1)
	for i := range lcs {
		lcs[i] = make([]int, len(b)+1)
	}
	for i := len(a) - 1; i >= 0; i-- {
		for j := len(b) - 1; j >= 0; j-- {
			if a[i] == b[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else {
				lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
			}
		}
	}
	var out []string
	same := true // the last line was a line that stays
	i, j := 0, 0
	for i < len(a) || j < len(b) {
		if i < len(a) && j < len(b) && a[i] == b[j] {
			i, j, same = i+1, j+1, true
			continue
		}
		if same {
			out = append(out, fmt.Sprintf("@@ line %d", i+1))
			same = false
		}
		if i < len(a) && (j == len(b) || lcs[i+1][j] >= lcs[i][j+1]) {
			out = append(out, "- "+a[i])
			i++
			continue
		}
		out = append(out, "+ "+b[j])
		j++
	}
	return strings.Join(out, "\n")
}

// diffLines are the lines of s, with no last empty line; none for "".
func diffLines(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(s, "\n"), "\n")
}
