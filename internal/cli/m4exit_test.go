package cli

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/aryanVijaywargia/herdr-brain/internal/output"
	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

// learnedProposal records the learned case of E17-S10 in 3 sessions of workspace w14 (the
// claude agent stops, then brain run runs the approved tool touch-ran in its folder) and runs
// one extraction pass with the fake extractor. It returns the one automation proposal.
func (f *automationFixture) learnedProposal(t *testing.T) store.Proposal {
	t.Helper()
	f.approvedTool(t, bindTool(f.dir))
	for _, id := range []string{"s1", "s2", "s3"} {
		f.stopSession(t, id)
	}
	if calls := f.pass(t); calls != 0 {
		t.Fatalf("pass made %d extractor calls, want none", calls)
	}
	ps := f.automationProposals(t)
	if len(ps) != 1 || ps[0].Scope != store.ScopeWorkspace {
		t.Fatalf("proposals = %+v, want one workspace proposal", ps)
	}
	it, _ := f.rows(t).item(t, *ps[0].ItemID)
	if it.WorkspaceKey == nil || *it.WorkspaceKey != "ws_a14" {
		t.Fatalf("item workspace = %v, want ws_a14", it.WorkspaceKey)
	}
	return ps[0]
}

// stopClaim claims the runs of a claude stop in pane w14:p1, in the fixture's folder, with
// event key key, as brained does at a joined stop (§10.4 step 2): each active automation of
// the workspace, CLI and folder that may run gets one row per event key. It returns the new
// runs.
func (f *automationFixture) stopClaim(t *testing.T, key string) []int64 {
	t.Helper()
	s := openStore(t, f.store)
	defer closeStore(t, s)
	var runs []int64
	write(t, s, func(tx *sql.Tx) error {
		ctx := context.Background()
		as, err := store.ActiveAutomations(ctx, tx)
		if err != nil {
			return err
		}
		for _, a := range as {
			if a.WorkspaceKey != "ws_a14" || a.CLI != "claude" || !a.In(f.dir, f.dir) {
				continue
			}
			if _, err := output.AutomationMayRun(ctx, tx, filepath.Join(f.store, outDir), a.ID); errors.Is(err, output.ErrMayNotRun) {
				continue
			} else if err != nil {
				return err
			}
			id, err := store.ClaimRun(ctx, tx, store.AutomationRun{AutomationID: a.ID, EventKey: key, PaneID: "w14:p1",
				WorkspaceID: "w14", Claimed: testNow})
			if err != nil {
				return err
			}
			if id != 0 {
				runs = append(runs, id)
			}
		}
		return nil
	})
	return runs
}

// stopCalls returns the learned-case occurrences of the store (E17-S10).
func (f *automationFixture) stopCalls(t *testing.T) []store.StopCall {
	t.Helper()
	s := openStore(t, f.store)
	defer closeStore(t, s)
	var cs []store.StopCall
	write(t, s, func(tx *sql.Tx) (err error) {
		cs, err = store.StopCalls(context.Background(), tx, "default")
		return err
	})
	return cs
}

// M4 exit (E17-S12), §10.4 "M4 must prove that one learned automation runs", offline with the
// fake herdr and the fake extractor. A recorded learned case gives one scoped proposal. Nothing
// runs before the approval. After the approval, one stop event runs the fixture tool once, and
// a duplicate of the event does not; the Brain-origin run teaches nothing (S51). A rejection
// stops a new proposal in that scope. The joined stop and its event key are brained's part
// (TestAutomationStop, TestS51Automation); here the claim is the same store transaction.
func TestM4LearnedAutomation(t *testing.T) {
	t.Run("approve", func(t *testing.T) {
		f := newAutomationFixture(t)
		p := f.learnedProposal(t)
		h := &fakeRunHerdr{}

		if runs := f.stopClaim(t, "w14:p1|term_1|s9|5"); len(runs) != 0 {
			t.Fatalf("claims before the approval = %v, want none", runs)
		}
		if f.ran(t) {
			t.Fatal("the tool ran before the approval")
		}

		f.mustReview(t, fmt.Sprint(p.ID), "--approve")
		learned := len(f.stopCalls(t))
		key := "w14:p1|term_1|s9|7"
		runs := f.stopClaim(t, key)
		if len(runs) != 1 {
			t.Fatalf("claims of the stop = %v, want one", runs)
		}
		if err := f.launch(context.Background(), f.launchEnv(h), runs[0], nil); err != nil {
			t.Fatalf("launch = %v", err)
		}
		if !f.ran(t) {
			t.Fatal("the tool did not run after the stop")
		}
		if r := f.run(t, runs[0]); r.Result != store.RunOK || r.AutomationID != *p.ItemID {
			t.Errorf("run = %+v, want ok of automation %d", r, *p.ItemID)
		}
		if err := os.Remove(filepath.Join(f.dir, "ran")); err != nil {
			t.Fatal(err)
		}

		if again := f.stopClaim(t, key); len(again) != 0 {
			t.Errorf("claims of the duplicate event = %v, want none", again)
		}
		if f.ran(t) {
			t.Error("the tool ran again at the duplicate event")
		}
		var auto int
		for _, c := range f.calls(t) {
			if c.Origin == store.OriginAutomation {
				auto++
				if c.Result != store.CallRan {
					t.Errorf("automation call = %+v, want ran", c)
				}
			}
		}
		if auto != 1 {
			t.Errorf("automation calls = %d, want 1", auto)
		}

		// The Brain-origin run is no learned case, and the next pass proposes nothing.
		if got := len(f.stopCalls(t)); got != learned {
			t.Errorf("learned-case occurrences = %d after the run, want %d", got, learned)
		}
		if calls := f.pass(t); calls != 0 || len(f.automationProposals(t)) != 0 {
			t.Errorf("pass after the run: %d calls, proposals %+v; want none", calls, f.automationProposals(t))
		}
	})

	t.Run("reject", func(t *testing.T) {
		f := newAutomationFixture(t)
		p := f.learnedProposal(t)
		f.mustReview(t, fmt.Sprint(p.ID), "--reject")
		if runs := f.stopClaim(t, "w14:p1|term_1|s9|7"); len(runs) != 0 || f.ran(t) {
			t.Errorf("claims after the rejection = %v, want none and no run", runs)
		}
		f.stopSession(t, "s4")
		if calls := f.pass(t); calls != 0 || len(f.automationProposals(t)) != 0 {
			t.Errorf("pass after the rejection: %d calls, proposals %+v; want none in that scope", calls, f.automationProposals(t))
		}
	})
}
