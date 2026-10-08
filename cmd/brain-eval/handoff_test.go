package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/aryanVijaywargia/herdr-brain/internal/bindings"
	"github.com/aryanVijaywargia/herdr-brain/internal/brief"
	"github.com/aryanVijaywargia/herdr-brain/internal/output"
	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

// AC #273 5: the expected brief hash of a probe has the handoff lines of the last session, as
// the hook prints them: an item that you approved after that session started, and a command
// whose last run failed in it.
func TestWantHandoff(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	key, start := "ws_t14", time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	s, err := store.Open(ctx, dir, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close() //nolint:errcheck // test
	text := func(v string) *string { return &v }
	err = s.Write(ctx, func(tx *sql.Tx) error {
		if err := store.InsertWorkspace(ctx, tx, store.Workspace{Key: key, HerdrSession: session, HerdrWorkspaceID: "w14",
			Label: "w14", Status: store.WorkspaceLive, LastSeen: start}); err != nil {
			return err
		}
		// The earlier session in the case workspace, and the failed command of its capture.
		if _, err := store.InsertDelivery(ctx, tx, store.Delivery{Profile: "claude", HerdrSession: text(session),
			PaneID: "w14:p1", AgentSessionID: text("agent-1"), WorkspaceKey: &key, Result: store.ResultEmitted, At: start,
			TranscriptPath: text("/t/agent-1.jsonl")}); err != nil {
			return err
		}
		k := store.SessionKey{HerdrSession: session, Harness: "claude", Kind: store.KeyAgent, ID: "agent-1"}
		sn, err := store.OpenSession(ctx, tx, k, "w14:p1", key, start)
		if err != nil {
			return err
		}
		seg, err := store.StartSegment(ctx, tx, sn, "w14:p1", key, "visible", start)
		if err != nil {
			return err
		}
		o, err := store.AddObservation(ctx, tx, store.Observation{CaptureIdentity: "h1", Source: store.SourceHook,
			SegmentID: seg, Event: "tool", Excerpt: "make", Status: store.ObservationComplete, Sender: store.SenderTool,
			Counts: true, At: start})
		if err != nil {
			return err
		}
		exit := 2
		if err := store.AddCommandRecord(ctx, tx, store.CommandRecord{ObservationID: o, Argv: "make", ExitStatus: &exit}); err != nil {
			return err
		}
		// An item that you approved after that session started.
		at := start.Add(10 * time.Minute)
		r := store.ItemRevision{Text: "use make", Scope: store.ScopeGlobal, State: store.StateDraft,
			Author: store.AuthorBrain, Origin: store.OriginRemember, Created: at}
		id, _, err := store.CreateItem(ctx, tx, store.Item{ClaimID: "c1", Kind: store.KindFact, Created: at}, r)
		if err != nil {
			return err
		}
		base := int64(1)
		p, err := store.InsertProposal(ctx, tx, store.Proposal{ItemID: &id, BaseRevision: &base, Text: "use make",
			Scope: store.ScopeGlobal, Target: "fact", Diff: "+use make", ExpectedBenefit: "b",
			Status: store.ProposalApproved, Created: at, Decided: at})
		if err != nil {
			return err
		}
		r.ItemID, r.State, r.Author, r.Origin, r.ProposalID = id, store.StateActive, store.AuthorYou, store.OriginReview, &p
		_, err = store.WriteRevision(ctx, tx, 1, r)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, outDir)
	if _, err := output.Build(ctx, s, out, nil, func() time.Time { return start }); err != nil {
		t.Fatal(err)
	}
	br, err := output.Read(out, key)
	if err != nil {
		t.Fatal(err)
	}
	exp := expected{found: true, key: key, global: br.Global, workspace: br.Workspace, revisions: br.Revisions}
	r := &Runner{Timeouts: Timeouts{Call: time.Second}}
	hs, err := r.want(ctx, bindings.Binding{Store: dir}, exp, store.Delivery{AgentSessionID: text("agent-2")})
	if err != nil {
		t.Fatal(err)
	}
	line := br.Workspace + "Last session in this workspace: claude, 2026-09-29 12:00, transcript /t/agent-1.jsonl\n"
	sum := func(ws string) string {
		h := sha256.Sum256([]byte(brief.Text(br.Global, ws)))
		return hex.EncodeToString(h[:])
	}
	with := sum(line + "Approved since that session started: [1]\nFailed in that session (last run): make (exit 2)\n")
	if !slices.Contains(hs, with) {
		t.Errorf("want() has no hash of the brief with the handoff lines")
	}
	if slices.Contains(hs, sum(line)) {
		t.Errorf("want() has the hash of the brief without the handoff lines")
	}
}
