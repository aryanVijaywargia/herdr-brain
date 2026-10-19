package store

import (
	"context"
	"database/sql"
	"testing"
)

// AC 1 to 3 (#327): forget of an item removes the key text of a claim whose same_as link
// names the claim of the item, by id and link, when that link is its only use: a restatement
// with no evidence in place (#312) or a merge into the candidate of the item's scope (#279).
// While the linked claim has an item, or a candidate of its own in another scope, or brought
// evidence to a candidate in another scope, that use keeps its key. A candidate of its own in
// the scope of the item does not keep it (#389).
func TestEraseItemLinkedClaim(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		use  func(t *testing.T, e *evidenceStore, claim, linked string) // the uses of the linked claim
		kept bool
	}{
		{"a restatement", func(*testing.T, *evidenceStore, string, string) {}, false},
		{"a merge in the same workspace", func(t *testing.T, e *evidenceStore, claim, linked string) {
			e.mustAttach(t, Candidate{ClaimID: claim, Scope: ScopeWorkspace, WorkspaceKey: "ka", Kind: CandidateCorrection,
				Via: linked}, e.observe(t, "claude", "a1", "ka", SenderUnconfirmed, testNow))
		}, false},
		{"a candidate of its own in the same workspace", func(t *testing.T, e *evidenceStore, _, linked string) {
			e.mustAttach(t, Candidate{ClaimID: linked, Scope: ScopeWorkspace, WorkspaceKey: "ka", Kind: CandidateCorrection},
				e.observe(t, "claude", "a1", "ka", SenderUnconfirmed, testNow))
		}, false},
		{"a candidate of its own in workspace B", func(t *testing.T, e *evidenceStore, _, linked string) {
			e.mustAttach(t, Candidate{ClaimID: linked, Scope: ScopeWorkspace, WorkspaceKey: "kb", Kind: CandidateCorrection},
				e.observe(t, "codex", "b1", "kb", SenderUnconfirmed, testNow))
		}, true},
		{"an item of its own", func(t *testing.T, e *evidenceStore, _, linked string) {
			write(t, e.Store, func(tx *sql.Tx) error {
				_, err := InsertItem(ctx, tx, Item{ClaimID: linked, CurrentRevision: 1, Kind: KindFact, Scope: ScopeGlobal,
					State: StateActive, Created: testNow})
				return err
			})
		}, true},
		{"a merge in workspace B", func(t *testing.T, e *evidenceStore, claim, linked string) {
			e.mustAttach(t, Candidate{ClaimID: claim, Scope: ScopeWorkspace, WorkspaceKey: "kb", Kind: CandidateCorrection,
				Via: linked}, e.observe(t, "codex", "b1", "kb", SenderUnconfirmed, testNow))
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEvidenceStore(t)
			claim := e.claim(t, "use pnpm")
			linked := e.claim(t, "use pnpm, not npm")
			var item int64
			write(t, e.Store, func(tx *sql.Tx) (err error) {
				if err := InsertWorkspace(ctx, tx, Workspace{Key: "ka", HerdrSession: "default", HerdrWorkspaceID: "w1",
					Label: "brain", Status: WorkspaceLive, LastSeen: testNow}); err != nil {
					return err
				}
				if _, err := SameAs(ctx, tx, linked, claim, Match{}); err != nil {
					return err
				}
				item, err = InsertItem(ctx, tx, Item{ClaimID: claim, CurrentRevision: 1, Kind: KindFact, Scope: ScopeWorkspace,
					WorkspaceKey: ptr("ka"), State: StateActive, Created: testNow})
				return err
			})
			tc.use(t, e, claim, linked)
			write(t, e.Store, func(tx *sql.Tx) error {
				if err := EraseItem(ctx, tx, item, testNow); err != nil {
					return err
				}
				if left, err := ItemTextLeft(ctx, tx, item); err != nil || len(left) != 0 {
					t.Errorf("ItemTextLeft = %v, %v, want none", left, err)
				}
				c, err := ClaimByID(ctx, tx, linked)
				if err != nil {
					return err
				}
				if want := (ClaimKey{"in this repo", "use pnpm, not npm"}); tc.kept && c.Key != want {
					t.Errorf("linked claim key %+v, want %+v kept for its other use", c.Key, want)
				}
				if !tc.kept && c.Key != (ClaimKey{}) {
					t.Errorf("linked claim %s keeps its key %+v after the forget", linked, c.Key)
				}
				if c.SameAs != claim {
					t.Errorf("linked claim same_as = %q, want the link to %s kept", c.SameAs, claim)
				}
				return nil
			})
		})
	}
}
