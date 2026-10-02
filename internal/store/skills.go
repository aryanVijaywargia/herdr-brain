package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// Skill is the reviewed content of a skill item (§8.2): the header of its SKILL.md and the
// steps. The text of a skill revision is the whole file, so the file that the compiler writes
// is the text that you approved.
//
// [NEW] the text form (E16-S1 tested it):
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
// [NEW] the form; it keeps the "<name>: <description>" entry of the brief clear.
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

// ErrSkillName means that another active skill in the same scope has the name already (§8.2).
var ErrSkillName = errors.New("another active skill in this scope has this name")

// ActiveSkills reads the current revision of each active skill item, in item id order.
func ActiveSkills(ctx context.Context, tx *sql.Tx) ([]ItemRevision, error) {
	rows, err := tx.QueryContext(ctx, `SELECT `+revisionColumns+` FROM item_revisions r
		JOIN items i ON r.item_id = i.id AND r.revision = i.current_revision
		WHERE i.state = ? AND i.kind = ? ORDER BY i.id`, StateActive, KindSkill)
	if err != nil {
		return nil, fmt.Errorf("read active skills: %w", err)
	}
	defer rows.Close() //nolint:errcheck // read-only: rows.Err reports the read errors
	var rs []ItemRevision
	for rows.Next() {
		r, err := scanRevision(rows)
		if err != nil {
			return nil, fmt.Errorf("read active skills: %w", err)
		}
		rs = append(rs, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read active skills: %w", err)
	}
	return rs, nil
}

// CheckSkill checks the current revision of item, when it is an active skill: its text must
// be a skill text, and no other active skill in the same scope (one workspace key, or global)
// may have its name (§8.2). WriteRevision calls it after each revision write, in the same
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
	all, err := ActiveSkills(ctx, tx)
	if err != nil {
		return err
	}
	for _, o := range all {
		if o.ItemID == item || o.Scope != cur.Scope || ptrString(o.WorkspaceKey) != ptrString(cur.WorkspaceKey) {
			continue
		}
		// An active skill passed this check when it became active, so its text parses.
		if other, err := ParseSkill(o.Text); err == nil && other.Name == sk.Name {
			return fmt.Errorf("skill %q: item %d: %w", sk.Name, o.ItemID, ErrSkillName)
		}
	}
	return nil
}

func ptrString(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
