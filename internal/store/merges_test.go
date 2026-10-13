package store

import (
	"context"
	"database/sql"
	"slices"
	"testing"
)

// mergeStore is a store with workspaces ws-a and ws-b.
func mergeStore(t *testing.T) *Store {
	t.Helper()
	s := openM1(t)
	write(t, s, func(tx *sql.Tx) error {
		for _, k := range []string{"ws-a", "ws-b"} {
			if err := InsertWorkspace(context.Background(), tx, Workspace{Key: k, HerdrSession: "default", HerdrWorkspaceID: k,
				Label: k, Status: WorkspaceLive, LastSeen: testNow}); err != nil {
				return err
			}
		}
		return nil
	})
	return s
}

// mergeItem makes an item of kind in the part of key ("" for global) with claim, in state. A
// skill, tool or automation gets its kind after the write: the merge pass reads only the kind,
// and the write checks the text of those kinds.
func mergeItem(t *testing.T, tx *sql.Tx, key, claim string, kind Kind, state State) int64 {
	t.Helper()
	r := ItemRevision{Text: claim + " text", Scope: ScopeGlobal, State: state, Author: AuthorYou, Origin: OriginReview, Created: testNow}
	if key != "" {
		r.Scope, r.WorkspaceKey = ScopeWorkspace, &key
	}
	id, _, err := CreateItem(context.Background(), tx, Item{ClaimID: claim, Kind: KindFact, Created: testNow}, r)
	if err == nil {
		_, err = tx.Exec(`UPDATE items SET kind = ? WHERE id = ?`, kind, id)
	}
	if err != nil {
		t.Errorf("make item of claim %s: %v", claim, err)
	}
	return id
}

// AC input (§9.5): MergeItems gives the active plain items of one part, and leaves out skills,
// tools, automations, items of another part, items that are not active, and items with a
// waiting proposal, in a waiting merge proposal, or with an exception link or a conflict mark
// at either end.
func TestMergeItems(t *testing.T) {
	s := mergeStore(t)
	ctx := context.Background()
	var want []int64
	write(t, s, func(tx *sql.Tx) error {
		plain := mergeItem(t, tx, "ws-a", "c1", KindFact, StateActive)
		pref := mergeItem(t, tx, "ws-a", "c2", KindPreference, StateActive)
		want = []int64{plain, pref}
		for _, k := range []Kind{KindSkill, KindTool, KindAutomation} {
			mergeItem(t, tx, "ws-a", "k"+string(k), k, StateActive)
		}
		mergeItem(t, tx, "ws-b", "other", KindFact, StateActive)
		mergeItem(t, tx, "", "global", KindFact, StateActive)
		mergeItem(t, tx, "ws-a", "disputed", KindFact, StateDisputed)
		mergeItem(t, tx, "ws-a", "retired", KindFact, StateRetired)
		waits := mergeItem(t, tx, "ws-a", "waits", KindFact, StateActive)
		inMerge := mergeItem(t, tx, "ws-a", "in-merge", KindFact, StateActive)
		kept := mergeItem(t, tx, "ws-a", "kept", KindFact, StateActive)
		for _, p := range []Proposal{
			{ItemID: &waits, BaseRevision: ptr[int64](1)},
			{ItemID: &kept, BaseRevision: ptr[int64](1), Merge: []RevisionRef{{Item: kept, Revision: 1}, {Item: inMerge, Revision: 1}}},
		} {
			p.Text, p.Scope, p.Status, p.Created = "x", ScopeWorkspace, ProposalPending, testNow
			if _, err := InsertProposal(ctx, tx, p); err != nil {
				return err
			}
		}
		g := mergeItem(t, tx, "", "global-linked", KindFact, StateActive)
		exc := mergeItem(t, tx, "ws-a", "exception", KindFact, StateActive)
		conflicted := mergeItem(t, tx, "ws-a", "conflicted", KindFact, StateActive)
		marks := mergeItem(t, tx, "ws-a", "marks", KindFact, StateActive)
		ws := "ws-a"
		for _, r := range []ItemRevision{
			{ItemID: exc, ExceptionToItem: &g, ExceptionToRevision: ptr[int64](1)},
			{ItemID: marks, ConflictsWith: []int64{conflicted}},
		} {
			r.Revision, r.Text, r.Scope, r.WorkspaceKey, r.State = 2, "x", ScopeWorkspace, &ws, StateActive
			r.Author, r.Origin, r.Created = AuthorYou, OriginReview, testNow
			if _, err := WriteRevision(ctx, tx, 1, r); err != nil {
				return err
			}
		}
		return nil
	})
	write(t, s, func(tx *sql.Tx) error {
		got, err := MergeItems(ctx, tx, "ws-a")
		ids := make([]int64, len(got))
		for i, m := range got {
			ids[i] = m.ID
		}
		if err == nil && !slices.Equal(ids, want) {
			t.Errorf("MergeItems(ws-a) = %v, want %v", ids, want)
		}
		return err
	})
}

// AC approve (§9.5): LinkMerged links each retired claim to the kept claim, so a later key of
// a retired item gets a claim that the kept item holds, also when the kept claim has a confirmed
// link of its own. UnlinkMerged puts the links back.
func TestLinkMerged(t *testing.T) {
	s := mergeStore(t)
	ctx := context.Background()
	keys := map[string]ClaimKey{"a": {"s", "rule a"}, "b": {"s", "rule b"}, "c": {"s", "rule c"}}
	ids := map[string]string{}
	var kept int64
	write(t, s, func(tx *sql.Tx) (err error) {
		for n, k := range keys {
			if ids[n], err = ClaimID(ctx, tx, k, testNow); err != nil {
				return err
			}
		}
		// b already means the same as a; the merge keeps the item of b and retires those of a and c.
		if _, err := tx.Exec(`UPDATE claims SET same_as = ?, same_as_status = ? WHERE id = ?`, ids["a"], SameAsConfirmed, ids["b"]); err != nil {
			return err
		}
		kept = mergeItem(t, tx, "ws-a", ids["b"], KindFact, StateActive)
		p, err := InsertProposal(ctx, tx, Proposal{ItemID: &kept, BaseRevision: ptr[int64](1), Text: "x", Scope: ScopeWorkspace,
			Status: ProposalApproved, Created: testNow})
		if err != nil {
			return err
		}
		if err := LinkMerged(ctx, tx, p, ids["b"], []string{ids["a"], ids["c"], "cl_remember"}); err != nil {
			return err
		}
		for _, n := range []string{"a", "c"} {
			id, err := ClaimID(ctx, tx, keys[n], testNow)
			if err != nil {
				return err
			}
			if held, err := ClaimHeld(ctx, tx, id, "ws-a"); err != nil || !held {
				t.Errorf("claim of key %s = %s, held by the kept item %v, %v; want held", n, id, held, err)
			}
		}
		if err := UnlinkMerged(ctx, tx, p); err != nil {
			return err
		}
		for n, want := range map[string]string{"a": ids["a"], "b": ids["a"], "c": ids["c"]} {
			if got, err := ClaimID(ctx, tx, keys[n], testNow); err != nil || got != want {
				t.Errorf("claim of key %s after unlink = %s, %v; want %s", n, got, err, want)
			}
		}
		return nil
	})
}

// AC reject (§9.5): a rejected merge stays with the part of its kept item at the proposal, also
// when a link later moves the item to another workspace (§9.4).
func TestRejectedMergesPart(t *testing.T) {
	s := mergeStore(t)
	ctx := context.Background()
	write(t, s, func(tx *sql.Tx) error {
		a := mergeItem(t, tx, "ws-a", "a", KindFact, StateActive)
		b := mergeItem(t, tx, "ws-a", "b", KindFact, StateActive)
		if _, err := InsertProposal(ctx, tx, Proposal{ItemID: &b, BaseRevision: ptr[int64](1), Text: "x", Scope: ScopeWorkspace,
			Merge: []RevisionRef{{Item: b, Revision: 1}, {Item: a, Revision: 1}}, Status: ProposalRejected, Created: testNow}); err != nil {
			return err
		}
		moved := "ws-b"
		if _, err := WriteRevision(ctx, tx, 1, ItemRevision{ItemID: b, Revision: 2, Text: "b text", Scope: ScopeWorkspace,
			WorkspaceKey: &moved, State: StateActive, Author: AuthorYou, Origin: OriginReview, Created: testNow}); err != nil {
			return err
		}
		for key, want := range map[string][][]int64{"ws-a": {{a, b}}, "ws-b": nil} {
			if got, err := RejectedMerges(ctx, tx, key); err != nil || !slices.EqualFunc(got, want, slices.Equal) {
				t.Errorf("RejectedMerges(%s) = %v, %v; want %v", key, got, err, want)
			}
		}
		return nil
	})
}
