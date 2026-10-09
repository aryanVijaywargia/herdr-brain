package store

import (
	"cmp"
	"context"
	"database/sql"
	"testing"
	"time"
)

// AC #263: the last extractor call that worked is the newest batch with a model call that
// committed or gave a valid empty result. A failed batch and an empty batch with no call do
// not count.
func TestLastWorkedCall(t *testing.T) {
	s := openM1(t)
	ctx := context.Background()
	last := func() time.Time {
		t.Helper()
		var at time.Time
		write(t, s, func(tx *sql.Tx) (err error) {
			at, err = LastWorkedCall(ctx, tx)
			return err
		})
		return at
	}
	var sid int64
	write(t, s, func(tx *sql.Tx) (err error) {
		k := SessionKey{HerdrSession: "default", Harness: "claude", Kind: KeyAgent, ID: "a1"}
		sid, err = OpenSession(ctx, tx, k, "w1:p1", "ws", testNow)
		return err
	})
	batch := func(capture string, status BatchStatus, at time.Time) {
		t.Helper()
		write(t, s, func(tx *sql.Tx) error {
			b, err := StartBatch(ctx, tx, Batch{SessionID: sid, WorkspaceKey: "ws", FirstCapture: capture,
				LastCapture: capture}, at)
			if err != nil {
				return err
			}
			_, err = EndBatch(ctx, tx, b.ID, status, "", at)
			return err
		})
	}
	if at := last(); !at.IsZero() {
		t.Errorf("LastWorkedCall() = %v with no batch, want the zero time", at)
	}
	batch("c1", BatchFailed, testNow)
	if at := last(); !at.IsZero() {
		t.Errorf("LastWorkedCall() = %v after a failed batch, want the zero time", at)
	}
	batch("c2", BatchCommitted, testNow.Add(time.Minute))
	write(t, s, func(tx *sql.Tx) error {
		return EmptyBatch(ctx, tx, Batch{SessionID: sid, WorkspaceKey: "ws", FirstCapture: "c3", LastCapture: "c3"},
			testNow.Add(2*time.Minute))
	})
	if at := last(); !at.Equal(testNow.Add(time.Minute)) {
		t.Errorf("LastWorkedCall() = %v, want the committed batch at %v", at, testNow.Add(time.Minute))
	}
}

// AC #355: the daily proposal count takes a tool draft and the script proposal beside it as one
// proposal, and each other proposal from a candidate as before. Your own proposal, with no
// candidate, and a proposal before since do not count.
func TestExtractorProposalsSince(t *testing.T) {
	e := newEvidenceStore(t)
	ctx := context.Background()
	chain := e.mustAttach(t, Candidate{ClaimID: e.claim(t, "make it a brain tool"), Scope: ScopeWorkspace,
		WorkspaceKey: "shop", Kind: CandidateCorrection}, e.observe(t, "claude", "a1", "shop", SenderUnconfirmed, testNow))
	rule := e.mustAttach(t, Candidate{ClaimID: e.claim(t, "use pnpm"), Scope: ScopeWorkspace,
		WorkspaceKey: "shop", Kind: CandidateCorrection}, e.observe(t, "claude", "a2", "shop", SenderUnconfirmed, testNow))
	write(t, e.Store, func(tx *sql.Tx) error {
		key := "shop"
		if err := InsertWorkspace(ctx, tx, Workspace{Key: key, HerdrSession: "default", HerdrWorkspaceID: "w1",
			Label: key, Status: WorkspaceLive, LastSeen: testNow}); err != nil {
			return err
		}
		item, err := InsertItem(ctx, tx, Item{ClaimID: chain.ClaimID, CurrentRevision: 1, Kind: KindTool,
			Scope: ScopeWorkspace, WorkspaceKey: &key, State: StateDraft, Created: testNow})
		if err != nil {
			return err
		}
		for _, p := range []struct {
			cand int64
			p    Proposal
		}{
			{chain.ID, Proposal{ItemID: &item, Text: "name = \"reset-db-sh\"", Target: "workspace w1 (shop)"}},
			{chain.ID, Proposal{Text: "#!/bin/sh", Target: ScriptTarget}},
			{rule.ID, Proposal{Text: "use pnpm", Target: "workspace w1 (shop)"}},
			{rule.ID, Proposal{Text: "use pnpm, yesterday", Target: "workspace w1 (shop)", Created: testNow.Add(-24 * time.Hour)}},
			{0, Proposal{Text: "your own edit", Target: "workspace w1 (shop)"}},
		} {
			p.p.Scope, p.p.Status, p.p.Created = ScopeWorkspace, ProposalPending, cmp.Or(p.p.Created, testNow)
			if p.cand == 0 {
				_, err = InsertProposal(ctx, tx, p.p)
			} else {
				_, err = ProposeCandidate(ctx, tx, p.cand, p.p)
			}
			if err != nil {
				return err
			}
		}
		return nil
	})
	var n int
	write(t, e.Store, func(tx *sql.Tx) (err error) {
		n, err = ExtractorProposalsSince(ctx, tx, testNow.Add(-time.Hour))
		return err
	})
	if n != 2 {
		t.Errorf("ExtractorProposalsSince() = %d, want 2: the tool draft with its script, and the rule", n)
	}
}
