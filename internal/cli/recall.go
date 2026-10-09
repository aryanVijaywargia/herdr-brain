package cli

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/aryanVijaywargia/herdr-brain/internal/brief"
	"github.com/aryanVijaywargia/herdr-brain/internal/output"
	"github.com/aryanVijaywargia/herdr-brain/internal/store"
	"github.com/aryanVijaywargia/herdr-brain/internal/workspace"
)

const recallUsage = `usage: brain recall <words>`

// maxRecall is the most results that one recall prints, items, skills and tools together
// (E20-S2 AC, #344).
const maxRecall = 10

// Recall runs `brain recall` with args after "recall" (E20-S2, §8.6). It prints the items, the
// skills and the tools that the brief of the calling pane can deliver and whose text matches the
// words, best match first, as the brief shows them. It records the revisions that it printed on
// the receipt of the pane's agent session, so outcomes count them as delivered (§5.4).
func Recall(ctx context.Context, e Env, args []string) (err error) {
	words := recallWords(strings.Join(args, " "))
	if len(words) == 0 {
		// One line, as every refusal of recall (§1.2).
		return fmt.Errorf("want words to match; %s", recallUsage)
	}
	c, err := e.Open(ctx)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, c.Close()) }()
	return c.Store.Write(ctx, func(tx *sql.Tx) error {
		// The pane can move to another workspace while the write waits for the store lock, so
		// check it again under the lock, before the read (§5.1).
		if err := e.recheck(ctx, c); err != nil {
			return err
		}
		ms, err := recallItems(ctx, tx, c, words)
		if err != nil {
			return err
		}
		return printRecall(ctx, tx, e, c, ms, strings.Join(words, " "))
	})
}

// recallMatch is one result of a recall: an item, a skill or a tool that the brief of the pane
// can deliver, and the revision that it shows. kind is store.KindSkill for a skill,
// store.KindTool for a tool, and "" for an item.
type recallMatch struct {
	kind  store.Kind
	ref   store.RevisionRef
	item  brief.Item
	skill brief.Skill
	tool  brief.Tool
}

// text is the text that the words of a recall match: the text of an item, the name and the
// description of a skill or a tool (#344).
func (m recallMatch) text() string {
	switch m.kind {
	case store.KindSkill:
		return m.skill.Name + " " + m.skill.Description
	case store.KindTool:
		return m.tool.Name + " " + m.tool.Description
	}
	return m.item.Text
}

// recallable returns what the brief of c's pane can deliver: its items, then its skills, then
// its tools.
func recallable(ctx context.Context, tx *sql.Tx, c *Call) ([]recallMatch, error) {
	key, err := briefKey(c)
	if err != nil {
		return nil, err
	}
	r, err := output.Recall(ctx, tx, filepath.Join(c.Binding.Store, outDir), key)
	if err != nil {
		return nil, err
	}
	ms := make([]recallMatch, 0, len(r.Items)+len(r.Skills)+len(r.Tools))
	for _, it := range r.Items {
		ms = append(ms, recallMatch{ref: store.RevisionRef{Item: it.ID, Revision: r.Revision[it.ID]}, item: it})
	}
	for _, sk := range r.Skills {
		ms = append(ms, recallMatch{kind: store.KindSkill, ref: store.RevisionRef{Item: sk.ID, Revision: r.Revision[sk.ID]},
			skill: sk.Skill})
	}
	for _, t := range r.Tools {
		ms = append(ms, recallMatch{kind: store.KindTool, ref: store.RevisionRef{Item: t.ID, Revision: r.Revision[t.ID]},
			tool: t.Tool})
	}
	return ms, nil
}

// recallItems returns the items, skills and tools that the brief of c's pane can deliver and
// that match words, best match first. brain recall and the recall popup (E20-S3) both search
// with it.
func recallItems(ctx context.Context, tx *sql.Tx, c *Call, words []string) ([]recallMatch, error) {
	ms, err := recallable(ctx, tx, c)
	if err != nil {
		return nil, err
	}
	return bestMatches(ms, words), nil
}

// itemsLabel is the heading of a run of items that comes after a run of skills or tools in a
// recall: without it, the items would read as part of that list. [NEW] the heading.
const itemsLabel = "### Items\n"

// printRecall prints ms, the recall result of query, and then records it in tx: a failed print
// rolls the record back, so a revision that the agent did not get never counts.
func printRecall(ctx context.Context, tx *sql.Tx, e Env, c *Call, ms []recallMatch, query string) error {
	if len(ms) == 0 {
		// [NEW] the no-match form.
		_, err := fmt.Fprintf(e.Stdout, "no Brain item matches %q\n", query)
		return err
	}
	text, err := recallText(ms)
	if err != nil {
		return err
	}
	// [NEW] the output form: one heading line, then the results as the brief shows them.
	if _, err := fmt.Fprintf(e.Stdout, "Brain items for %q, best match first:\n%s", query, text); err != nil {
		return err
	}
	recorded, err := recordRecall(ctx, tx, c, ms, e.Now())
	if err != nil || recorded {
		return err
	}
	// No receipt ties the pane's agent session, so no outcome could count the recall. [NEW]
	// the warning form.
	_, err = fmt.Fprintln(e.Stderr, "brain: warning: no Brain receipt for the agent session of this pane; this recall is not recorded")
	return err
}

// recallText renders ms in their order, best match first (#344). Each run of results of one
// kind shows as the brief shows it: items as the item entries, skills as the "Skills" list and
// tools as the "Tools" catalog, each list with its heading and its instruction line. A run of
// items after another kind gets the itemsLabel heading.
func recallText(ms []recallMatch) (string, error) {
	var b strings.Builder
	for len(ms) > 0 {
		n := 1
		for n < len(ms) && ms[n].kind == ms[0].kind {
			n++
		}
		run := ms[:n]
		ms = ms[n:]
		switch run[0].kind {
		case store.KindSkill:
			skills := make([]brief.Skill, n)
			for i, m := range run {
				skills[i] = m.skill
			}
			b.WriteString(brief.Skills(skills))
		case store.KindTool:
			tools := make([]brief.Tool, n)
			for i, m := range run {
				tools[i] = m.tool
			}
			b.WriteString(brief.Tools(tools))
		default:
			items := make([]brief.Item, n)
			for i, m := range run {
				items[i] = m.item
			}
			text, err := brief.Entries(items)
			if err != nil {
				return "", err
			}
			if b.Len() > 0 {
				b.WriteString(itemsLabel)
			}
			b.WriteString(text)
		}
	}
	return b.String(), nil
}

// briefKey is the workspace key whose part the brief of the calling pane delivers, or "" for
// the global part only: an unmapped workspace, or one whose root check failed (§8.3 step 4).
func briefKey(c *Call) (string, error) {
	m, err := c.Workspace.Lookup(c.WorkspaceID)
	if err != nil {
		return "", err
	}
	if m.State != workspace.Mapped {
		return "", nil
	}
	return m.Entry.Key, nil
}

// recordRecall records the revisions of ms, items, skills and tools, on the newest receipt of
// the calling pane's agent session. It returns false when there is no such receipt.
func recordRecall(ctx context.Context, tx *sql.Tx, c *Call, ms []recallMatch, now time.Time) (bool, error) {
	id, ok, err := store.RecallReceipt(ctx, tx, c.Binding.Session, c.PaneID, c.AgentSession)
	if err != nil || !ok {
		return false, err
	}
	refs := make([]store.RevisionRef, len(ms))
	for i, m := range ms {
		refs[i] = m.ref
	}
	_, err = store.InsertRecall(ctx, tx, store.Recall{DeliveryID: id, ItemRevisions: refs, At: now})
	return err == nil, err
}

// bestMatches returns the results of ms that match words, the best match first, at most
// maxRecall of items, skills and tools together. [NEW] the match rule: a result scores one point
// for each word that starts a word of its text, case aside; more points first, then the lower
// item id. A result with no point does not match.
func bestMatches(ms []recallMatch, words []string) []recallMatch {
	type scored struct {
		m     recallMatch
		score int
	}
	var ss []scored
	for _, m := range ms {
		text := recallWords(m.text())
		n := 0
		for _, w := range words {
			if slices.ContainsFunc(text, func(t string) bool { return strings.HasPrefix(t, w) }) {
				n++
			}
		}
		if n > 0 {
			ss = append(ss, scored{m, n})
		}
	}
	slices.SortFunc(ss, func(a, b scored) int {
		return cmp.Or(cmp.Compare(b.score, a.score), cmp.Compare(a.m.ref.Item, b.m.ref.Item))
	})
	out := make([]recallMatch, min(len(ss), maxRecall))
	for i := range out {
		out[i] = ss[i].m
	}
	return out
}

// recallWords splits s into its words in lower case, each word once, in the order of s. A
// word is a run of letters and digits.
func recallWords(s string) []string {
	var words []string
	for _, w := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}) {
		if !slices.Contains(words, w) {
			words = append(words, w)
		}
	}
	return words
}
