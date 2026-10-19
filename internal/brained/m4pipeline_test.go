package brained_test

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aryanVijaywargia/herdr-brain/internal/brained"
	"github.com/aryanVijaywargia/herdr-brain/internal/cli"
	"github.com/aryanVijaywargia/herdr-brain/internal/extract"
	"github.com/aryanVijaywargia/herdr-brain/internal/herdr"
	"github.com/aryanVijaywargia/herdr-brain/internal/store"
	"github.com/aryanVijaywargia/herdr-brain/internal/workspace"
)

var pipelineNow = time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC)

// runHerdr is the herdr of the launch process: no agent works, and it takes each pane label.
type runHerdr struct{}

func (runHerdr) Agents(context.Context) ([]herdr.Agent, error)                      { return nil, nil }
func (runHerdr) SetPaneToken(context.Context, string, string, string, string) error { return nil }

// extractor is the fake extractor CLI: it counts its calls and gives no claim.
type extractor struct{ calls int }

func (e *extractor) Run(context.Context, extract.Command) ([]byte, error) {
	e.calls++
	return []byte(`{"claims": []}`), nil
}

// pipeline is the brained of the test, its store and the cli commands of pane w1:p1.
type pipeline struct {
	*brained.Pipeline
}

// env is the environment of a brain command in pane w1:p1 of workspace w1, with environ in
// place of the pane variables when it is not nil.
func (p pipeline) env(environ []string) cli.Env {
	if environ == nil {
		environ = []string{"HERDR_ENV=1", "HERDR_SOCKET_PATH=" + p.Socket(), "HERDR_PANE_ID=w1:p1",
			"HERDR_WORKSPACE_ID=w1", "HERDR_SESSION=s1"}
	}
	return cli.Env{
		Environ:  append(environ, "HOME="+p.Folder()),
		Bindings: p.Bindings(),
		Pane: func(_ context.Context, t herdr.Target, id string) (herdr.Pane, error) {
			if id != "w1:p1" || t.Socket != p.Socket() {
				return herdr.Pane{}, fmt.Errorf("pane %s: %w", id, herdr.ErrPaneNotFound)
			}
			return herdr.Pane{ID: id, WorkspaceID: "w1", TabID: "w1:t1"}, nil
		},
		Sync:     func(context.Context, workspace.Session, *store.Store) error { return nil },
		RunHerdr: func(herdr.Target) (cli.RunHerdr, error) { return runHerdr{}, nil },
		Now:      func() time.Time { return pipelineNow },
		Stdout:   &bytes.Buffer{},
		Stderr:   &bytes.Buffer{},
	}
}

func (p pipeline) brain(t *testing.T, cmd func(context.Context, cli.Env, []string) error, args ...string) {
	t.Helper()
	if err := cmd(context.Background(), p.env(nil), args); err != nil {
		t.Fatalf("brain %q = %v", args, err)
	}
}

func (p pipeline) tx(t *testing.T, fn func(ctx context.Context, tx *sql.Tx) error) {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, p.Store(), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close() //nolint:errcheck // test
	if err := st.Write(ctx, func(tx *sql.Tx) error { return fn(ctx, tx) }); err != nil {
		t.Fatal(err)
	}
}

// session records one session of the learned case (E17-S10): the claude agent of pane w1:p1
// works, its Stop hook records the stop, and then brain run runs tool lint from the pane.
func (p pipeline) session(t *testing.T, id string) {
	t.Helper()
	key := store.SessionKey{HerdrSession: "s1", Harness: "claude", Kind: store.KeyAgent, ID: id}
	p.tx(t, func(ctx context.Context, tx *sql.Tx) error {
		sid, err := store.OpenSession(ctx, tx, key, "w1:p1", "ws_1", pipelineNow.Add(-time.Hour))
		if err != nil {
			return err
		}
		seg, err := store.StartSegment(ctx, tx, sid, "w1:p1", "ws_1", "recent_unwrapped", pipelineNow.Add(-time.Hour))
		if err != nil {
			return err
		}
		for i, o := range []store.Observation{
			{Source: store.SourceHerdr, Event: "working", Excerpt: "Editing main.go", Sender: store.SenderAssistant},
			{Source: store.SourceHook, Event: "stop", Excerpt: "Done.", Sender: store.SenderAssistant},
		} {
			o.CaptureIdentity, o.SegmentID, o.Status, o.Counts = fmt.Sprintf("%s-%d", id, i), seg, store.ObservationComplete, true
			o.At = pipelineNow.Add(time.Duration(i-2) * time.Minute)
			if _, err := store.AddObservation(ctx, tx, o); err != nil {
				return err
			}
		}
		return nil
	})
	p.brain(t, cli.Run, "lint")
	p.removeRan(t)
	p.tx(t, func(ctx context.Context, tx *sql.Tx) error { return store.EndSession(ctx, tx, key, pipelineNow) })
}

// pass runs one extraction pass with the fake extractor, and returns its calls.
func (p pipeline) pass(t *testing.T) int {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, p.Store(), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close() //nolint:errcheck // test
	e := &extractor{}
	if err := (extract.Pass{Store: st, Runner: e, HerdrSession: "s1", DefaultOn: true,
		Now: func() time.Time { return pipelineNow.Add(time.Hour) }}).Run(ctx); err != nil {
		t.Fatalf("pass = %v", err)
	}
	return e.calls
}

// proposals returns the pending proposals of automation items.
func (p pipeline) proposals(t *testing.T) []store.Proposal {
	t.Helper()
	var ps []store.Proposal
	p.tx(t, func(ctx context.Context, tx *sql.Tx) error {
		ids, err := store.PendingProposalIDs(ctx, tx)
		if err != nil {
			return err
		}
		for _, id := range ids {
			pr, err := store.ProposalByID(ctx, tx, id)
			if err != nil || pr.ItemID == nil {
				return err
			}
			it, err := store.ItemByID(ctx, tx, *pr.ItemID)
			if err != nil {
				return err
			}
			if it.Kind == store.KindAutomation {
				ps = append(ps, pr)
			}
		}
		return nil
	})
	return ps
}

func (p pipeline) calls(t *testing.T) []store.ToolCall {
	t.Helper()
	var cs []store.ToolCall
	p.tx(t, func(ctx context.Context, tx *sql.Tx) (err error) {
		cs, err = store.ToolCalls(ctx, tx)
		return err
	})
	return cs
}

func (p pipeline) ran(t *testing.T) bool {
	t.Helper()
	_, err := os.Stat(filepath.Join(p.Folder(), "ran"))
	return err == nil
}

func (p pipeline) removeRan(t *testing.T) {
	t.Helper()
	if err := os.Remove(filepath.Join(p.Folder(), "ran")); err != nil {
		t.Fatal(err)
	}
}

// learned records the learned case in 3 sessions and runs a pass. It returns the one scoped
// automation proposal.
func (p pipeline) learned(t *testing.T) store.Proposal {
	t.Helper()
	for _, id := range []string{"a1", "a2", "a3"} {
		p.session(t, id)
	}
	calls := p.pass(t)
	ps := p.proposals(t)
	if calls != 0 || len(ps) != 1 || ps[0].Scope != store.ScopeWorkspace {
		t.Fatalf("%d extractor calls, proposals %+v; want one workspace proposal and no call", calls, ps)
	}
	b, err := store.ParseBinding(ps[0].Text)
	if err != nil || b.Tool != "lint" || b.Folder != p.Folder() || b.CLI != "claude" {
		t.Fatalf("binding = %+v, %v; want tool lint in %s on the claude stop", b, err, p.Folder())
	}
	return ps[0]
}

// M4 exit (E17-S12): one learned automation runs, in one test with the fake herdr and the
// fake extractor. The recorded learned case of E17-S10 gives one scoped proposal. Before the
// approval, a stop of the agent claims nothing and runs nothing. After the approval, one stop
// event through brained claims one run, and the launch process (the code of
// `brain automation launch`) runs the tool once. A duplicate of the event does
// not run it again. The Brain-origin run triggers nothing and teaches nothing (S51 "no chain").
// A rejection stops a new proposal in that scope.
func TestM4LearnedAutomation(t *testing.T) {
	t.Run("approve", func(t *testing.T) {
		p := pipeline{brained.NewPipeline(t)}
		pr := p.learned(t)
		p.Start(t)

		p.Stop(t)
		p.NoLaunch(t)
		if rs := p.Runs(t); len(rs) != 0 || p.ran(t) {
			t.Fatalf("runs before the approval = %+v, want none and no run", rs)
		}

		p.brain(t, cli.Review, fmt.Sprint(pr.ID), "--approve")
		p.Check(t) // brained sees the automation
		p.Stop(t)
		args, environ := p.Launch(t)
		rs := p.Runs(t)
		if len(rs) != 1 || rs[0].AutomationID != *pr.ItemID {
			t.Fatalf("runs = %+v, want one run of automation %d", rs, *pr.ItemID)
		}
		if err := cli.Automation(context.Background(), p.env(environ), args[1:]); err != nil {
			t.Fatalf("brain %q = %v", args, err)
		}
		if !p.ran(t) {
			t.Fatal("the tool did not run after the stop")
		}
		p.removeRan(t)
		if rs := p.Runs(t); rs[0].Result != store.RunOK {
			t.Errorf("run = %+v, want ok", rs[0])
		}

		p.Duplicate(t)
		p.NoLaunch(t)
		auto := 0
		for _, c := range p.calls(t) {
			if c.Origin == store.OriginAutomation {
				auto++
			}
		}
		if rs := p.Runs(t); len(rs) != 1 || auto != 1 || p.ran(t) {
			t.Errorf("after the duplicate: runs %+v, %d automation calls; want one run and one call", rs, auto)
		}

		// No chain: after the Brain-origin run, brained claims nothing and launches nothing.
		p.Check(t)
		p.NoLaunch(t)
		if rs := p.Runs(t); len(rs) != 1 {
			t.Errorf("runs after the Brain-origin run = %+v, want one", rs)
		}
		// Nothing is learned from it: the next pass makes no call and no proposal.
		if calls := p.pass(t); calls != 0 || len(p.proposals(t)) != 0 {
			t.Errorf("pass after the run: %d calls, proposals %+v; want none", calls, p.proposals(t))
		}
	})

	t.Run("reject", func(t *testing.T) {
		p := pipeline{brained.NewPipeline(t)}
		pr := p.learned(t)
		p.brain(t, cli.Review, fmt.Sprint(pr.ID), "--reject")
		p.Start(t)
		p.Stop(t)
		p.NoLaunch(t)
		if rs := p.Runs(t); len(rs) != 0 || p.ran(t) {
			t.Errorf("runs after the rejection = %+v, want none and no run", rs)
		}
		p.session(t, "a4")
		if calls := p.pass(t); calls != 0 || len(p.proposals(t)) != 0 {
			t.Errorf("pass after the rejection: %d calls, proposals %+v; want none in that scope", calls, p.proposals(t))
		}
	})
}
