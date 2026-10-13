package cli

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

// AC proposal (§9.5): the review pane shows a merge proposal as brain review does: what it
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
