package store

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"
)

// reject rejects the claim of c with reason and returns the rejection id.
func (e *evidenceStore) reject(t *testing.T, c Candidate, reason string) int64 {
	t.Helper()
	var id int64
	write(t, e.Store, func(tx *sql.Tx) (err error) {
		id, err = Reject(context.Background(), tx, c, reason, testNow)
		return err
	})
	return id
}

// propose makes a proposal of candidate id through ProposeCandidate.
func (e *evidenceStore) propose(id int64) error {
	return e.Write(context.Background(), func(tx *sql.Tx) error {
		_, err := ProposeCandidate(context.Background(), tx, id, Proposal{Text: "use pnpm", Scope: ScopeGlobal,
			Target: "global", Diff: "+ use pnpm", TriggerPrompts: []string{}, Status: ProposalPending, Created: testNow})
		return err
	})
}

func (e *evidenceStore) rejected(t *testing.T, claim, workspaceKey string) bool {
	t.Helper()
	var held bool
	write(t, e.Store, func(tx *sql.Tx) (err error) {
		held, err = Rejected(context.Background(), tx, claim, workspaceKey)
		return err
	})
	return held
}

func (e *evidenceStore) rejections(t *testing.T) []Rejection {
	t.Helper()
	var rs []Rejection
	write(t, e.Store, func(tx *sql.Tx) (err error) {
		rs, err = Rejections(context.Background(), tx)
		return err
	})
	return rs
}

// AC: reject in "shop" does not hide the claim in "blog".
func TestRejectionPerScope(t *testing.T) {
	e := newEvidenceStore(t)
	claim := e.claim(t, "use pnpm")
	shop := e.mustAttach(t, Candidate{ClaimID: claim, Scope: ScopeWorkspace, WorkspaceKey: "shop", Kind: CandidateCorrection},
		e.observe(t, "claude", "a1", "shop", SenderUnconfirmed, testNow))
	blog := e.mustAttach(t, Candidate{ClaimID: claim, Scope: ScopeWorkspace, WorkspaceKey: "blog", Kind: CandidateCorrection},
		e.observe(t, "claude", "b1", "blog", SenderUnconfirmed, testNow))
	id := e.reject(t, shop, "we use npm in shop")
	if !e.rejected(t, claim, "shop") || e.rejected(t, claim, "blog") || e.rejected(t, claim, "") {
		t.Errorf("rejected in shop, blog, global = %v, %v, %v; want only shop", e.rejected(t, claim, "shop"),
			e.rejected(t, claim, "blog"), e.rejected(t, claim, ""))
	}
	if err := e.propose(shop.ID); !errors.Is(err, ErrRejected) {
		t.Errorf("a proposal in shop: %v, want %v", err, ErrRejected)
	}
	if err := e.propose(blog.ID); err != nil {
		t.Errorf("a proposal in blog: %v, want one", err)
	}
	if again := e.reject(t, shop, "another reason"); again != id {
		t.Errorf("a second reject made rejection %d, want the held %d", again, id)
	}
	rs := e.rejections(t)
	if len(rs) != 1 || rs[0].ID != id || rs[0].Reason != "we use npm in shop" || rs[0].WorkspaceKey != "shop" ||
		rs[0].Scope != ScopeWorkspace || !rs[0].At.Equal(testNow) {
		t.Errorf("rejections = %+v, want rejection %d of shop with the first reason", rs, id)
	}
}

// AC: new evidence after a rejection counts in the rejection, and creates no proposal.
func TestRejectionNewEvidence(t *testing.T) {
	e := newEvidenceStore(t)
	c := Candidate{ClaimID: e.claim(t, "use pnpm"), Scope: ScopeWorkspace, WorkspaceKey: "shop", Kind: CandidateCorrection}
	first := e.mustAttach(t, c, e.observe(t, "claude", "a1", "shop", SenderUnconfirmed, testNow))
	e.reject(t, first, "")
	later := e.mustAttach(t, c, e.observe(t, "codex", "b1", "shop", SenderUnconfirmed, testNow.Add(time.Hour)))
	if later.Count != 2 {
		t.Fatalf("the new evidence gave count %d, want 2: it still attaches", later.Count)
	}
	rs := e.rejections(t)
	if len(rs) != 1 || rs[0].EvidenceCount != 1 || rs[0].Evidence != 2 || rs[0].New != 1 {
		t.Errorf("rejections = %+v, want 1 at the rejection, 2 now, 1 new", rs)
	}
	if err := e.propose(later.ID); !errors.Is(err, ErrRejected) {
		t.Errorf("a proposal after the new evidence: %v, want %v", err, ErrRejected)
	}
	if n := queryInt(t, e.Store, `SELECT count(*) FROM proposals`); n != 0 {
		t.Errorf("proposals = %d, want 0", n)
	}
	// A forget of the older evidence does not hide the new evidence.
	write(t, e.Store, func(tx *sql.Tx) error { return ForgetSession(context.Background(), tx, e.sessions["claude/a1"]) })
	if rs := e.rejections(t); len(rs) != 1 || rs[0].Evidence != 1 || rs[0].New != 1 {
		t.Errorf("after a forget of the older evidence: %+v, want 1 now and 1 new", rs)
	}
}

// A reworded key with a confirmed same_as link has the claim id of the rejected claim, so it
// stays rejected (S23).
func TestRewordedKeyStaysRejected(t *testing.T) {
	e := newEvidenceStore(t)
	old := e.claim(t, "use pnpm")
	c := e.mustAttach(t, Candidate{ClaimID: old, Scope: ScopeGlobal, Kind: CandidateCorrection},
		e.observe(t, "claude", "a1", "shop", SenderUnconfirmed, testNow))
	e.reject(t, c, "no")
	reworded := e.claim(t, "always use pnpm")
	write(t, e.Store, func(tx *sql.Tx) error {
		_, err := SameAs(context.Background(), tx, reworded, old, Match{Clear: true})
		return err
	})
	id := e.claim(t, "always use pnpm")
	if id != old || !e.rejected(t, id, "") {
		t.Errorf("the reworded key has claim id %s (rejected %v), want %s rejected", id, e.rejected(t, id, ""), old)
	}
}

// Only you end a rejection: reset counts the new evidence from now, and reopen makes the claim
// eligible for a proposal again. A reopened rejection stays as history.
func TestReopenAndResetRejection(t *testing.T) {
	ctx := context.Background()
	e := newEvidenceStore(t)
	c := Candidate{ClaimID: e.claim(t, "use pnpm"), Scope: ScopeWorkspace, WorkspaceKey: "shop", Kind: CandidateCorrection}
	e.mustAttach(t, c, e.observe(t, "claude", "a1", "shop", SenderUnconfirmed, testNow))
	first := e.mustAttach(t, c, e.observe(t, "claude", "a2", "shop", SenderUnconfirmed, testNow))
	id := e.reject(t, first, "")
	e.mustAttach(t, c, e.observe(t, "codex", "b1", "shop", SenderUnconfirmed, testNow))
	later := testNow.Add(time.Hour)
	write(t, e.Store, func(tx *sql.Tx) error { return ResetRejection(ctx, tx, id, later) })
	if rs := e.rejections(t); len(rs) != 1 || rs[0].EvidenceCount != 3 || rs[0].New != 0 || !rs[0].At.Equal(later) {
		t.Errorf("after reset: %+v, want 3 at %v and 0 new", rs, later)
	}
	if !e.rejected(t, c.ClaimID, "shop") {
		t.Error("reset ended the rejection, want it held")
	}
	write(t, e.Store, func(tx *sql.Tx) error { return ReopenRejection(ctx, tx, id, later) })
	if e.rejected(t, c.ClaimID, "shop") || len(e.rejections(t)) != 0 {
		t.Error("after reopen the claim is still rejected")
	}
	if err := e.propose(first.ID); err != nil {
		t.Errorf("a proposal after reopen: %v, want one", err)
	}
	write(t, e.Store, func(tx *sql.Tx) error {
		r, err := RejectionByID(ctx, tx, id)
		if err == nil && !r.Reopened.Equal(later) {
			t.Errorf("rejection %d reopened %v, want %v", id, r.Reopened, later)
		}
		if err := ReopenRejection(ctx, tx, id, later); !errors.Is(err, sql.ErrNoRows) {
			t.Errorf("a second reopen: %v, want %v", err, sql.ErrNoRows)
		}
		return err
	})
	if again := e.reject(t, first, "no again"); again == id {
		t.Errorf("a reject after reopen gave the old rejection %d, want a new one", id)
	}
}

// Forget of an item removes your reason from the rejections of its claim in its scope.
func TestEraseItemRejectionReason(t *testing.T) {
	ctx := context.Background()
	e := newEvidenceStore(t)
	claim := e.claim(t, "use pnpm")
	c := e.mustAttach(t, Candidate{ClaimID: claim, Scope: ScopeGlobal, Kind: CandidateCorrection},
		e.observe(t, "claude", "a1", "shop", SenderUnconfirmed, testNow))
	e.reject(t, c, "pnpm breaks the lockfile")
	write(t, e.Store, func(tx *sql.Tx) error {
		item, err := InsertItem(ctx, tx, Item{ClaimID: claim, CurrentRevision: 1, Kind: KindFact, Scope: ScopeGlobal,
			State: StateDraft, Created: testNow})
		if err != nil {
			return err
		}
		if err := EraseItem(ctx, tx, item, testNow); err != nil {
			return err
		}
		if left, err := ItemTextLeft(ctx, tx, item); err != nil || len(left) != 0 {
			t.Errorf("ItemTextLeft = %v, %v, want none", left, err)
		}
		return nil
	})
	if rs := e.rejections(t); len(rs) != 1 || rs[0].Reason != "" {
		t.Errorf("rejections = %+v, want one with no reason: the rejection itself stays", rs)
	}
}
