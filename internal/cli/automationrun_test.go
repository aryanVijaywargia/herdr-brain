package cli

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aryanVijaywargia/herdr-brain/internal/bindings"
	"github.com/aryanVijaywargia/herdr-brain/internal/brained"
	"github.com/aryanVijaywargia/herdr-brain/internal/herdr"
	"github.com/aryanVijaywargia/herdr-brain/internal/store"
	"github.com/aryanVijaywargia/herdr-brain/internal/workspace"
)

// fakeRunHerdr is the herdr of an automation run: its agents, and the pane label values.
type fakeRunHerdr struct {
	mu     sync.Mutex
	agents []herdr.Agent
	labels []string        // "<pane> <value>"; "" clears
	label  chan<- struct{} // when not nil, gets one value at each label
}

func (h *fakeRunHerdr) Agents(context.Context) ([]herdr.Agent, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.agents, nil
}

func (h *fakeRunHerdr) SetPaneToken(ctx context.Context, id, source, name, value string) error {
	if _, ok := ctx.Deadline(); !ok || source != labelSource || name != labelToken {
		return fmt.Errorf("SetPaneToken(%s, %s): want a deadline, source %s and token %s", source, name, labelSource, labelToken)
	}
	h.mu.Lock()
	h.labels = append(h.labels, strings.TrimSpace(id+" "+value))
	h.mu.Unlock()
	if h.label != nil {
		h.label <- struct{}{}
	}
	return nil
}

func (h *fakeRunHerdr) seen() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.labels...)
}

// claimRun claims a run of automation id for pane w14:p1, as brained does, and returns it.
func (f *automationFixture) claimRun(t *testing.T, id int64, key string) int64 {
	t.Helper()
	s := openStore(t, f.store)
	defer closeStore(t, s)
	var run int64
	write(t, s, func(tx *sql.Tx) (err error) {
		run, err = store.ClaimRun(context.Background(), tx, store.AutomationRun{AutomationID: id, EventKey: key,
			PaneID: "w14:p1", WorkspaceID: "w14", Claimed: testNow})
		return err
	})
	return run
}

// launchEnv is the environment that brained gives the launch process of a run of pane w14:p1.
func (f *automationFixture) launchEnv(h *fakeRunHerdr) Env {
	e := f.env(f.sock, "w14:p1", "w14", &bytes.Buffer{})
	e.Environ = append(e.Environ, "HOME="+f.dir)
	// The sync of the fixture, without its count: a cancel can run beside the launch.
	e.Sync = func(ctx context.Context, s workspace.Session, st *store.Store) error {
		return s.Refresh(ctx, nil, st, []herdr.Workspace{{ID: "w14", Label: "shop"}, {ID: "w15", Label: "blog"},
			{ID: "w16", Label: "new"}}, testNow)
	}
	e.RunHerdr = func(t herdr.Target) (RunHerdr, error) {
		if t.Socket != f.sock {
			return nil, fmt.Errorf("herdr of %s, want %s", t.Socket, f.sock)
		}
		return h, nil
	}
	return e
}

// launch runs the launch process of run in the main store with e.
func (f *automationFixture) launch(ctx context.Context, e Env, run int64, paused func()) error {
	return launcher{Env: e, pid: 4242, paused: paused}.run(ctx, brained.Launch{Run: run, Binding: "default", Store: f.store})
}

func (f *automationFixture) run(t *testing.T, id int64) store.AutomationRun {
	t.Helper()
	s := openStore(t, f.store)
	defer closeStore(t, s)
	var r store.AutomationRun
	write(t, s, func(tx *sql.Tx) (err error) {
		r, err = store.AutomationRunByID(context.Background(), tx, id)
		return err
	})
	return r
}

func (f *automationFixture) toolCalls(t *testing.T) []store.ToolCall {
	t.Helper()
	s := openStore(t, f.store)
	defer closeStore(t, s)
	var cs []store.ToolCall
	write(t, s, func(tx *sql.Tx) (err error) {
		cs, err = store.ToolCalls(context.Background(), tx)
		return err
	})
	return cs
}

// A claimed run resolves its pane, writes its launch mark, runs the bound tool in the pinned
// folder with the brain run checks as a Brain-origin call, and writes ok. The pane label shows
// unknown while it runs, and ok clears it. AC S62: Brain never runs a run again.
func TestLaunchRun(t *testing.T) {
	f := newAutomationFixture(t)
	toolID, id := f.bound(t)
	run := f.claimRun(t, id, "w14:p1|term_1|s1|6")
	h := &fakeRunHerdr{}
	if err := f.launch(context.Background(), f.launchEnv(h), run, nil); err != nil {
		t.Fatalf("launch = %v", err)
	}
	if !f.ran(t) {
		t.Error("the tool did not run in the pinned folder")
	}
	r := f.run(t, run)
	if r.Result != store.RunOK || r.Started.IsZero() || r.Finished.IsZero() || r.PID != 4242 {
		t.Errorf("run = %+v, want ok with a launch mark of process 4242", r)
	}
	cs := f.toolCalls(t)
	if len(cs) != 1 || cs[0].ToolID != toolID || cs[0].Origin != store.OriginAutomation || cs[0].Result != store.CallRan {
		t.Errorf("tool_calls = %+v, want one ran call of tool %d of origin automation", cs, toolID)
	}
	if got := strings.Join(h.seen(), "|"); got != "w14:p1 touch-ran unknown|w14:p1" {
		t.Errorf("labels = %q, want unknown, then cleared", got)
	}
	if err := f.launch(context.Background(), f.launchEnv(h), run, nil); err == nil {
		t.Error("second launch of the run = nil, want a refusal")
	}
	if cs := f.toolCalls(t); len(cs) != 1 {
		t.Errorf("tool_calls = %+v, want no second run", cs)
	}
	list := f.mustAutomation(t, "list")
	if !strings.Contains(list, "last run: ok at ") {
		t.Errorf("list = %q, want the last run ok", list)
	}
}

// AC S51: a failure shows in status and on the pane label.
func TestLaunchFailed(t *testing.T) {
	f := newAutomationFixture(t)
	f.approvedTool(t, strings.Replace(bindTool(f.dir), `argv = ["/usr/bin/touch", "ran"]`, `argv = ["/bin/sh", "-c", "exit 3"]`, 1))
	p := f.add(t)
	f.mustReview(t, fmt.Sprint(p), "--approve")
	id := *f.rows(t).proposal(t, p).ItemID
	run := f.claimRun(t, id, "k")
	h := &fakeRunHerdr{}
	if err := f.launch(context.Background(), f.launchEnv(h), run, nil); err == nil {
		t.Error("launch of a failing tool = nil, want its failure")
	}
	if r := f.run(t, run); r.Result != store.RunFailed {
		t.Errorf("run = %+v, want failed", r)
	}
	if got := h.seen(); len(got) != 2 || got[1] != "w14:p1 touch-ran failed" {
		t.Errorf("labels = %q, want touch-ran failed last", got)
	}
	out, err := f.status(t, f.sock)
	if err != nil {
		t.Fatal(err)
	}
	if want := fmt.Sprintf("  last run failed: automation %d (%s): run %d of pane w14:p1", id, f.dir, run); !strings.Contains(out, want) {
		t.Errorf("status = %q, want %q", out, want)
	}
}

// The checks before the launch mark skip the run: nothing runs, and the row says skipped.
func TestLaunchSkip(t *testing.T) {
	tests := []struct {
		name   string
		before func(t *testing.T, f *automationFixture, h *fakeRunHerdr, e *Env, id int64)
		paused func(t *testing.T, f *automationFixture, id int64)
	}{
		{"AC S59: an agent works in the folder", func(_ *testing.T, f *automationFixture, h *fakeRunHerdr, _ *Env, _ int64) {
			h.agents = []herdr.Agent{{PaneID: "w14:p2", WorkspaceID: "w14", Agent: "codex", Status: herdr.StatusWorking, Cwd: f.dir}}
		}, nil},
		{"the source pane closed", func(_ *testing.T, f *automationFixture, _ *fakeRunHerdr, _ *Env, _ int64) {
			delete(f.panes, "w14:p1")
		}, nil},
		{"the source pane moved", func(_ *testing.T, f *automationFixture, _ *fakeRunHerdr, _ *Env, _ int64) {
			f.panes["w14:p1"] = herdr.Pane{ID: "w20:p1", WorkspaceID: "w20", TabID: "w20:t1"}
		}, nil},
		{"the pane resolves to a test binding", func(t *testing.T, f *automationFixture, _ *fakeRunHerdr, e *Env, _ int64) {
			ts := f.bindLearn(t, "brain-test", bindings.KindTest)
			*e = f.env(ts.sock, "w14:p1", "w14", &bytes.Buffer{})
			e.Environ = append(e.Environ, "HERDR_SESSION=brain-test")
			e.Pane = func(context.Context, herdr.Target, string) (herdr.Pane, error) {
				return herdr.Pane{ID: "w14:p1", WorkspaceID: "w14", TabID: "w14:t1"}, nil
			}
			e.Sync = func(context.Context, workspace.Session, *store.Store) error { return nil }
		}, nil},
		{"pause while the run waits", nil, func(t *testing.T, f *automationFixture, id int64) {
			f.mustAutomation(t, "pause", fmt.Sprint(id))
		}},
		{"removal while the run waits", nil, func(t *testing.T, f *automationFixture, id int64) {
			f.mustAutomation(t, "remove", fmt.Sprint(id))
		}},
		{"undo while the run waits", nil, func(t *testing.T, f *automationFixture, id int64) {
			f.mustUndo(t, fmt.Sprint(id))
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newAutomationFixture(t)
			_, id := f.bound(t)
			run := f.claimRun(t, id, "k")
			h := &fakeRunHerdr{}
			e := f.launchEnv(h)
			if tt.before != nil {
				tt.before(t, f, h, &e, id)
			}
			var paused func()
			if tt.paused != nil {
				paused = func() { tt.paused(t, f, id) }
			}
			if err := f.launch(context.Background(), e, run, paused); err == nil || !strings.Contains(err.Error(), "skipped") {
				t.Errorf("launch = %v, want a skip", err)
			}
			if f.ran(t) {
				t.Error("the tool ran, want no process")
			}
			if r := f.run(t, run); r.Result != store.RunSkipped || !r.Started.IsZero() {
				t.Errorf("run = %+v, want skipped with no launch mark", r)
			}
			if cs := f.toolCalls(t); len(cs) != 0 {
				t.Errorf("tool_calls = %+v, want none", cs)
			}
		})
	}
}

// An agent that works in another folder or another workspace does not skip the run.
func TestLaunchBusyElsewhere(t *testing.T) {
	f := newAutomationFixture(t)
	_, id := f.bound(t)
	run := f.claimRun(t, id, "k")
	h := &fakeRunHerdr{agents: []herdr.Agent{
		{PaneID: "w14:p2", WorkspaceID: "w14", Status: herdr.StatusWorking, Cwd: t.TempDir()},
		{PaneID: "w15:p1", WorkspaceID: "w15", Status: herdr.StatusWorking, Cwd: f.dir},
		{PaneID: "w14:p3", WorkspaceID: "w14", Status: herdr.StatusIdle, Cwd: f.dir},
	}}
	if err := f.launch(context.Background(), f.launchEnv(h), run, nil); err != nil || !f.ran(t) {
		t.Errorf("launch = %v, ran %v, want a run", err, f.ran(t))
	}
}

// AC: brain automation cancel stops an active run and records cancelled. The end that the run
// writes after the stop keeps cancelled. A run with no launch mark cannot be cancelled.
func TestLaunchCancel(t *testing.T) {
	f := newAutomationFixture(t)
	f.approvedTool(t, strings.Replace(bindTool(f.dir), `argv = ["/usr/bin/touch", "ran"]`, `argv = ["/bin/sleep", "30"]`, 1))
	p := f.add(t)
	f.mustReview(t, fmt.Sprint(p), "--approve")
	id := *f.rows(t).proposal(t, p).ItemID
	run := f.claimRun(t, id, "k")
	var out bytes.Buffer
	e := f.env(f.sock, "w14:p1", "w14", &out)
	if err := Automation(context.Background(), e, []string{"cancel", fmt.Sprint(run)}); !errors.Is(err, store.ErrRunNotActive) {
		t.Errorf("cancel of a run with no launch mark = %v, want ErrRunNotActive", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	label := make(chan struct{}, 4)
	h := &fakeRunHerdr{label: label}
	done := make(chan error, 1)
	start := time.Now()
	go func() { done <- f.launch(ctx, f.launchEnv(h), run, nil) }()
	select {
	case <-label: // the launch mark is written, and the tool starts
	case <-time.After(10 * time.Second):
		t.Fatal("the run did not start")
	}
	var killed []int
	e.Kill = func(pid int) error {
		killed = append(killed, pid)
		cancel() // the SIGTERM that the launch process gets
		return nil
	}
	if err := Automation(context.Background(), e, []string{"cancel", fmt.Sprint(run)}); err != nil {
		t.Fatalf("cancel = %v", err)
	}
	if len(killed) != 1 || killed[0] != 4242 {
		t.Errorf("killed = %v, want process 4242", killed)
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the run did not stop")
	}
	if d := time.Since(start); d > 20*time.Second {
		t.Errorf("the run took %v, want a stop before the tool's 30 s", d)
	}
	if r := f.run(t, run); r.Result != store.RunCancelled || r.Active() {
		t.Errorf("run = %+v, want cancelled", r)
	}
	if got := h.seen(); len(got) != 2 || got[1] != "w14:p1 touch-ran cancelled" {
		t.Errorf("labels = %q, want touch-ran cancelled last", got)
	}
	if !strings.Contains(out.String(), fmt.Sprintf("run %d of automation %d cancelled", run, id)) {
		t.Errorf("cancel output = %q", out.String())
	}
}

// AC S62: a crash after the launch mark leaves unknown, status shows it, and there is no
// retry: the launch of the run refuses, and the same event claims nothing again.
func TestLaunchCrash(t *testing.T) {
	f := newAutomationFixture(t)
	_, id := f.bound(t)
	run := f.claimRun(t, id, "k")
	s := openStore(t, f.store)
	// The launch process wrote its mark and died.
	write(t, s, func(tx *sql.Tx) error { return store.StartRun(context.Background(), tx, run, 4242, testNow) })
	closeStore(t, s)
	if err := f.launch(context.Background(), f.launchEnv(&fakeRunHerdr{}), run, nil); err == nil {
		t.Error("launch of a started run = nil, want a refusal")
	}
	if again := f.claimRun(t, id, "k"); again != 0 {
		t.Errorf("claim of the same event = %d, want none", again)
	}
	if f.ran(t) {
		t.Error("the tool ran, want no retry")
	}
	if r := f.run(t, run); r.Result != store.RunUnknown {
		t.Errorf("run = %+v, want unknown", r)
	}
	out, err := f.status(t, f.sock)
	if err != nil {
		t.Fatal(err)
	}
	if want := fmt.Sprintf("  last run unknown: automation %d (%s): run %d of pane w14:p1", id, f.dir, run); !strings.Contains(out, want) ||
		!strings.Contains(out, fmt.Sprintf("brain automation cancel %d stops it", run)) {
		t.Errorf("status = %q, want %q and the cancel hint", out, want)
	}
}

func TestParseLaunch(t *testing.T) {
	l := brained.Launch{Run: 7, Binding: "default", Store: filepath.Join(t.TempDir(), "main")}
	args := l.Args()
	if args[0] != "automation" || args[1] != "launch" {
		t.Fatalf("Args() = %q, want automation launch first", args)
	}
	got, err := brained.ParseLaunch(args[2:])
	if err != nil || got != l {
		t.Errorf("ParseLaunch(Args()) = %+v, %v, want %+v", got, err, l)
	}
	for _, bad := range [][]string{nil, {"--run", "0", "--binding", "d", "--store", "/s"}, {"--run", "1", "--binding", "d", "--store", "s"}} {
		if _, err := brained.ParseLaunch(bad); err == nil {
			t.Errorf("ParseLaunch(%q) = nil, want an error", bad)
		}
	}
}

// The run of an automation is pinned to the tool and the contract hash of its binding:
// another tool of the name, or another contract, is refused and runs nothing.
func TestRunToolPinned(t *testing.T) {
	f := newAutomationFixture(t)
	toolID, _ := f.bound(t)
	hash := f.mustMayRun(t, toolID).ContractHash
	e := f.launchEnv(&fakeRunHerdr{})
	for _, r := range []ToolRun{
		{ToolID: toolID + 100, ContractHash: hash},
		{ToolID: toolID, ContractHash: "other"},
	} {
		r.PaneID, r.WorkspaceID, r.Origin, r.Tool = "w14:p1", "w14", store.OriginAutomation, "touch-ran"
		if _, err := RunTool(context.Background(), e, r); !errors.Is(err, ErrRefused) {
			t.Errorf("RunTool(%d, %s) = %v, want a refusal", r.ToolID, r.ContractHash, err)
		}
	}
	if f.ran(t) {
		t.Error("the tool ran, want no run")
	}
	if _, err := RunTool(context.Background(), e, ToolRun{PaneID: "w14:p1", WorkspaceID: "w14", Origin: store.OriginAutomation,
		Tool: "touch-ran", ToolID: toolID, ContractHash: hash}); err != nil || !f.ran(t) {
		t.Errorf("RunTool(pinned) = %v, want a run", err)
	}
}
