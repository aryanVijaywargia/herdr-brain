package cli

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

// AC proposal: the review pane shows a merge proposal as brain review does: what it
// merges, the new text, each old text with its id, and the characters saved. Its permission
// words need the y confirm, as for other proposals.
func TestMergePane(t *testing.T) {
	f := newFixture(t)
	ws := "ws_a14"
	a := f.addActive(t, &ws, "use pnpm in this repo")
	b := f.addActive(t, &ws, "install packages with pnpm")
	var pid int64
	f.storeWrite(t, func(ctx context.Context, tx *sql.Tx) (err error) {
		base := int64(1)
		pid, err = store.InsertProposal(ctx, tx, store.Proposal{ItemID: &a, BaseRevision: &base,
			Text: "use pnpm; allow no npm", Scope: store.ScopeWorkspace, Target: "workspace w14 (shop)",
			Diff:  fmt.Sprintf("- item %d: use pnpm in this repo\n- item %d: install packages with pnpm\n+ use pnpm; allow no npm", a, b),
			Merge: []store.RevisionRef{{Item: a, Revision: 1}, {Item: b, Revision: 1}}, Status: store.ProposalPending, Created: testNow})
		return err
	})
	screen, err := f.pane(t, pid, typed("an"))
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{fmt.Sprintf("merges items %d and %d into item %d", a, b, a), "use pnpm; allow no npm",
		fmt.Sprintf("- item %d: install packages with pnpm", b), "saves:    25 characters", "press y to approve"} {
		if !strings.Contains(screen, w) {
			t.Errorf("pane screen =\n%s\nwant %q", screen, w)
		}
	}
	if p := f.rows(t).proposal(t, pid); p.Status != store.ProposalPending {
		t.Errorf("proposal after n = %s, want pending", p.Status)
	}
}

// AC forget: forget of an item that an approved merge retired names the kept item
// first and keeps its merged text. After an undo of the merge, or when the kept item is
// forgotten already, forget prints no such line.
func TestForgetNamesMergeKept(t *testing.T) {
	for _, c := range []struct {
		name string
		then func(t *testing.T, f *fixture, kept int64)
		line bool
	}{
		{name: "approved", line: true},
		{name: "undone", then: func(t *testing.T, f *fixture, kept int64) { f.mustUndo(t, fmt.Sprint(kept)) }},
		{name: "kept forgotten", then: func(t *testing.T, f *fixture, kept int64) { f.mustForget(t, clock(), fmt.Sprint(kept)) }},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			ws := "ws_a14"
			a := f.addActive(t, &ws, "use pnpm in this repo")
			b := f.addActive(t, &ws, "install packages with pnpm")
			var pid int64
			f.storeWrite(t, func(ctx context.Context, tx *sql.Tx) (err error) {
				base := int64(1)
				pid, err = store.InsertProposal(ctx, tx, store.Proposal{ItemID: &a, BaseRevision: &base,
					Text: "use pnpm; install packages with pnpm", Scope: store.ScopeWorkspace, Target: "workspace w14 (shop)",
					Diff:  fmt.Sprintf("- item %d: use pnpm in this repo\n- item %d: install packages with pnpm\n+ use pnpm; install packages with pnpm", a, b),
					Merge: []store.RevisionRef{{Item: a, Revision: 1}, {Item: b, Revision: 1}}, Status: store.ProposalPending, Created: testNow})
				return err
			})
			f.mustReview(t, fmt.Sprint(pid), "--approve")
			if c.then != nil {
				c.then(t, f, a)
			}
			out := f.mustForget(t, clock(), fmt.Sprint(b))
			line := fmt.Sprintf("kept: item %d holds merged text from item %d (workspace w14 (shop)); run brain forget %d to remove it\n", a, b, a)
			if strings.HasPrefix(out, line) != c.line || strings.Contains(out, "merged text") != c.line {
				t.Errorf("Forget(%d) printed\n%s\nwant the kept line first: %v", b, out, c.line)
			}
			if !strings.HasSuffix(out, fmt.Sprintf("forget item %d: done\n", b)) {
				t.Errorf("Forget(%d) printed\n%s\nwant done", b, out)
			}
			if c.line {
				if _, rev := f.rows(t).item(t, a); rev.Text != "use pnpm; install packages with pnpm" {
					t.Errorf("kept item text = %q, want the merged text kept", rev.Text)
				}
				if br := f.brief(t); !strings.Contains(br, "use pnpm; install packages with pnpm") {
					t.Errorf("brief = %q, want the merged text still delivered", br)
				}
			}
		})
	}
}
