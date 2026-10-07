package cli

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
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

// maxRecall is the most items that one recall prints (E20-S2 AC).
const maxRecall = 10

// Recall runs `brain recall` with args after "recall" (E20-S2, §8.6). It prints the items that
// the brief of the calling pane can deliver and whose text matches the words, best match
// first, with the brief's marks. It records the item revisions that it printed on the
// receipt of the pane's agent session, so outcomes count them as delivered (§5.4).
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
		items, revision, err := recallItems(ctx, tx, c, words)
		if err != nil {
			return err
		}
		return printRecall(ctx, tx, e, c, items, revision, strings.Join(words, " "))
	})
}

// recallItems returns the items that the brief of c's pane can deliver and that match words,
// best match first, and the revision of each delivered item. brain recall and the recall
// popup (E20-S3) both search with it.
func recallItems(ctx context.Context, tx *sql.Tx, c *Call, words []string) ([]brief.Item, map[int64]int64, error) {
	key, err := briefKey(c)
	if err != nil {
		return nil, nil, err
	}
	items, revision, err := output.Items(ctx, tx, key)
	if err != nil {
		return nil, nil, err
	}
	return bestMatches(items, words), revision, nil
}

// printRecall prints items, the recall result of query, and then records it in tx: a failed
// print rolls the record back, so a revision that the agent did not get never counts.
func printRecall(ctx context.Context, tx *sql.Tx, e Env, c *Call, items []brief.Item, revision map[int64]int64, query string) error {
	if len(items) == 0 {
		// [NEW] the no-match form.
		_, err := fmt.Fprintf(e.Stdout, "no Brain item matches %q\n", query)
		return err
	}
	text, err := brief.Entries(items)
	if err != nil {
		return err
	}
	// [NEW] the output form: one heading line, then the items as the brief shows them.
	if _, err := fmt.Fprintf(e.Stdout, "Brain items for %q, best match first:\n%s", query, text); err != nil {
		return err
	}
	recorded, err := recordRecall(ctx, tx, c, items, revision, e.Now())
	if err != nil || recorded {
		return err
	}
	// No receipt ties the pane's agent session, so no outcome could count the recall. [NEW]
	// the warning form.
	_, err = fmt.Fprintln(e.Stderr, "brain: warning: no Brain receipt for the agent session of this pane; this recall is not recorded")
	return err
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

// recordRecall records the revisions of items on the newest receipt of the calling pane's
// agent session. It returns false when there is no such receipt.
func recordRecall(ctx context.Context, tx *sql.Tx, c *Call, items []brief.Item, revision map[int64]int64, now time.Time) (bool, error) {
	id, ok, err := store.RecallReceipt(ctx, tx, c.Binding.Session, c.PaneID, c.AgentSession)
	if err != nil || !ok {
		return false, err
	}
	refs := make([]store.RevisionRef, len(items))
	for i, it := range items {
		refs[i] = store.RevisionRef{Item: it.ID, Revision: revision[it.ID]}
	}
	_, err = store.InsertRecall(ctx, tx, store.Recall{DeliveryID: id, ItemRevisions: refs, At: now})
	return err == nil, err
}

// bestMatches returns the items of items that match words, the best match first, at most
// maxRecall. [NEW] the match rule: an item scores one point for each word that starts a word
// of its text, case aside; more points first, then the lower item id. An item with no point
// does not match.
func bestMatches(items []brief.Item, words []string) []brief.Item {
	type scored struct {
		it    brief.Item
		score int
	}
	var ss []scored
	for _, it := range items {
		text := recallWords(it.Text)
		n := 0
		for _, w := range words {
			if slices.ContainsFunc(text, func(t string) bool { return strings.HasPrefix(t, w) }) {
				n++
			}
		}
		if n > 0 {
			ss = append(ss, scored{it, n})
		}
	}
	slices.SortFunc(ss, func(a, b scored) int {
		return cmp.Or(cmp.Compare(b.score, a.score), cmp.Compare(a.it.ID, b.it.ID))
	})
	out := make([]brief.Item, min(len(ss), maxRecall))
	for i := range out {
		out[i] = ss[i].it
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
