package cli

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"os/exec"
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

// boundArgv approves the tool touch-ran with argv line argv in place of its own, and a binding
// of it; it returns the automation item id.
func (f *automationFixture) boundArgv(t *testing.T, argv string) int64 {
	t.Helper()
	f.approvedTool(t, strings.Replace(bindTool(f.dir), `argv = ["/usr/bin/touch", "ran"]`, argv, 1))
	p := f.add(t)
	f.mustReview(t, fmt.Sprint(p), "--approve")
	return *f.rows(t).proposal(t, p).ItemID
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
// unknown while it runs, and ok clears it. AC: Brain never runs a run again.
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

// AC: a failure shows in status and on the pane label. The run record and the status line
// name the failed step and its exit code as brain run prints them, with no output of the tool
// (#352).
func TestLaunchFailed(t *testing.T) {
	f := newAutomationFixture(t)
	id := f.boundArgv(t, `steps = [["/usr/bin/true"], ["/bin/sh", "-c", "echo out-text; echo err-text >&2; exit 3"]]`)
	run := f.claimRun(t, id, "k")
	h := &fakeRunHerdr{}
	if err := f.launch(context.Background(), f.launchEnv(h), run, nil); err == nil {
		t.Error("launch of a failing tool = nil, want its failure")
	}
	const cause = "step 2: /bin/sh: exit status 3"
	if r := f.run(t, run); r.Result != store.RunFailed || r.Cause != cause {
		t.Errorf("run = %+v, want failed with the cause %q", r, cause)
	}
	if got := h.seen(); len(got) != 2 || got[1] != "w14:p1 touch-ran failed" {
		t.Errorf("labels = %q, want touch-ran failed last", got)
	}
	out, err := f.status(t, f.sock)
	if err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("  last run failed: automation %d (%s): run %d of pane w14:p1, claimed ", id, f.dir, run)
	if !strings.Contains(out, want) || !strings.Contains(out, "; "+cause+"\n") {
		t.Errorf("status = %q, want %q with the cause %q", out, want, cause)
	}
	if strings.Contains(out, "out-text") || strings.Contains(out, "err-text") {
		t.Errorf("status = %q, want no output of the tool", out)
	}
}

// #367 (M2 eval run 5): brain status showed the claim time of a failed run in UTC with no
// zone mark ("claimed 2026-10-09 16:51" at 22:21 local), while review shows local times (#254).
// The run line of status and the last run of brain automation list show local time, in the
// form of the review pane.
func TestRunLocalTime(t *testing.T) {
	inZone(t)
	a := store.Automation{ID: 8, Folder: "/w/notes-api"}
	r := store.AutomationRun{ID: 5, PaneID: "w1:p1", Result: store.RunFailed, Cause: "step 2: npm: exit status 1",
		Claimed: time.Date(2026, 10, 9, 16, 51, 0, 0, time.UTC)}
	want := "  last run failed: automation 8 (/w/notes-api): run 5 of pane w1:p1, claimed 2026-10-09 22:21; step 2: npm: exit status 1"
	if got := runLine(a, r); got != want {
		t.Errorf("runLine() = %q, want %q", got, want)
	}
	f := newAutomationFixture(t)
	_, id := f.bound(t)
	run := f.claimRun(t, id, "k")
	if err := f.launch(context.Background(), f.launchEnv(&fakeRunHerdr{}), run, nil); err != nil {
		t.Fatalf("launch = %v", err)
	}
	s := openStore(t, f.store)
	var as []store.Automation
	write(t, s, func(tx *sql.Tx) (err error) {
		as, err = store.ActiveAutomations(context.Background(), tx)
		return err
	})
	closeStore(t, s)
	if len(as) != 1 || as[0].LastRun.IsZero() {
		t.Fatalf("automations = %+v, want one with a last run", as)
	}
	list := f.mustAutomation(t, "list")
	if want := "last run: ok at " + as[0].LastRun.In(time.Local).Format("2006-01-02 15:04"); !strings.Contains(list, want) {
		t.Errorf("list = %q, want %q", list, want)
	}
}

// The checks before the launch mark skip the run: nothing runs, and the row says skipped.
func TestLaunchSkip(t *testing.T) {
	tests := []struct {
		name   string
		before func(t *testing.T, f *automationFixture, h *fakeRunHerdr, e *Env, id int64)
		paused func(t *testing.T, f *automationFixture, h *fakeRunHerdr, id int64)
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
		{"an agent starts to work while the run waits", nil, func(_ *testing.T, f *automationFixture, h *fakeRunHerdr, _ int64) {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.agents = []herdr.Agent{{PaneID: "w14:p1", WorkspaceID: "w14", Agent: "claude", Status: herdr.StatusWorking, Cwd: f.dir}}
		}},
		{"pause while the run waits", nil, func(t *testing.T, f *automationFixture, _ *fakeRunHerdr, id int64) {
			f.mustAutomation(t, "pause", fmt.Sprint(id))
		}},
		{"removal while the run waits", nil, func(t *testing.T, f *automationFixture, _ *fakeRunHerdr, id int64) {
			f.mustAutomation(t, "remove", fmt.Sprint(id))
		}},
		{"undo while the run waits", nil, func(t *testing.T, f *automationFixture, _ *fakeRunHerdr, id int64) {
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
				paused = func() { tt.paused(t, f, h, id) }
			}
			if err := f.launch(context.Background(), e, run, paused); err == nil {
				t.Error("launch = nil, want the reason of the skip")
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
	id := f.boundArgv(t, `argv = ["/bin/sleep", "30"]`)
	run := f.claimRun(t, id, "k")
	if _, err := f.automation(t, "cancel", fmt.Sprint(run)); !errors.Is(err, store.ErrRunNotActive) {
		t.Errorf("cancel of a run with no launch mark = %v, want ErrRunNotActive", err)
	}
	var out bytes.Buffer
	e := f.env(f.sock, "w14:p1", "w14", &out)
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
	e.Command = launchCommand(run)
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

// AC: a crash after the launch mark leaves unknown, status shows it, and there is no
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

// A launch whose store is not the store of its binding opens nothing, so it cannot make or
// change a store.
func TestLaunchUnbound(t *testing.T) {
	f := newAutomationFixture(t)
	other := t.TempDir()
	err := launcher{Env: f.launchEnv(&fakeRunHerdr{}), pid: 1}.run(context.Background(),
		brained.Launch{Run: 1, Binding: "default", Store: other})
	if err == nil {
		t.Fatal("launch with an unbound store = nil, want a refusal")
	}
	if _, err := os.Stat(filepath.Join(other, "brain.db")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("stat brain.db = %v, want no store in %s", err, other)
	}
}

// AC (no chains): a brain run inside the tool of an automation run is Brain-origin too. The
// tool gets the origin in its environment, and brain run logs that origin.
func TestLaunchOrigin(t *testing.T) {
	f := newAutomationFixture(t)
	id := f.boundArgv(t, `argv = ["/bin/sh", "-c", "printf %s \"$BRAIN_CALL_ORIGIN\" > ran"]`)
	run := f.claimRun(t, id, "k")
	if err := f.launch(context.Background(), f.launchEnv(&fakeRunHerdr{}), run, nil); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(f.dir, "ran"))
	if err != nil || string(b) != "automation" {
		t.Errorf("tool environment origin = %q, %v, want automation", b, err)
	}

	rf := newRunFixture(t)
	rf.approvedTool(t, runTool("write-word", rf.dir, "10", writeWord))
	e := rf.env(rf.sock, "w14:p1", "w14", &bytes.Buffer{})
	e.Environ = append(e.Environ, "HOME="+rf.dir, originEnv+"=automation")
	if err := Run(context.Background(), e, []string{"write-word", "--word", "nested"}); err != nil {
		t.Fatal(err)
	}
	if cs := rf.calls(t); len(cs) != 1 || cs[0].Origin != store.OriginAutomation {
		t.Errorf("tool_calls = %+v, want one call of origin automation", cs)
	}
}

// AC: an event in workspace B runs the tool of the binding of B only, also when workspace A
// has a tool of the same name and an automation of its own.
func TestLaunchOtherWorkspace(t *testing.T) {
	f := newAutomationFixture(t)
	f.bound(t) // workspace A (w14): touch-ran in f.dir
	dirB := t.TempDir()
	f.panes["w15:p1"] = herdr.Pane{ID: "w15:p1", WorkspaceID: "w15", TabID: "w15:t1"}
	s := openStore(t, f.store)
	key := "ws_a15"
	pid, err := saveDraft(context.Background(), s, draft{text: bindTool(dirB), kind: store.KindTool, scope: store.ScopeWorkspace,
		workspaceKey: &key, target: "workspace w15 (blog)"}, testNow)
	closeStore(t, s)
	if err != nil {
		t.Fatal(err)
	}
	inB := func(args ...string) {
		t.Helper()
		e := f.env(f.sock, "w15:p1", "w15", &bytes.Buffer{})
		cmd := Review
		if args[0] == "automation" {
			cmd, args = Automation, args[1:]
		}
		if err := cmd(context.Background(), e, args); err != nil {
			t.Fatalf("%q = %v", args, err)
		}
	}
	inB(fmt.Sprint(pid), "--approve")
	inB("automation", "add", "touch-ran", "--folder", dirB, "--cli", "claude")
	r := f.rows(t)
	p := r.proposals[len(r.proposals)-1].ID
	inB(fmt.Sprint(p), "--approve")
	idB := *f.rows(t).proposal(t, p).ItemID

	sb := openStore(t, f.store)
	var run int64
	write(t, sb, func(tx *sql.Tx) (err error) {
		run, err = store.ClaimRun(context.Background(), tx, store.AutomationRun{AutomationID: idB, EventKey: "k",
			PaneID: "w15:p1", WorkspaceID: "w15", Claimed: testNow})
		return err
	})
	closeStore(t, sb)
	e := f.launchEnv(&fakeRunHerdr{})
	e.Environ = append(e.Environ, "HERDR_PANE_ID=w15:p1", "HERDR_WORKSPACE_ID=w15") // the last entry wins
	if err := f.launch(context.Background(), e, run, nil); err != nil {
		t.Fatalf("launch = %v", err)
	}
	if _, err := os.Stat(filepath.Join(dirB, "ran")); err != nil {
		t.Errorf("the tool of B did not run: %v", err)
	}
	if f.ran(t) {
		t.Error("the tool of A ran, want the tool of B only")
	}
}

// launchCommand is a Command seam whose every process is the launch process of run.
func launchCommand(run int64) func(context.Context, int) (string, error) {
	return func(context.Context, int) (string, error) {
		return fmt.Sprintf("/p/bin/brain automation launch --run %d --binding default --store /s", run), nil
	}
}

// Cancel signals only the launch process of the run. A pid that the OS gave to another
// process, a gone process and a pid of 0 get no signal (kill(0) would signal the caller's
// process group), and the run is cancelled still.
func TestCancelProcess(t *testing.T) {
	tests := []struct {
		name    string
		pid     int
		command func(run int64) func(context.Context, int) (string, error)
		killed  bool
	}{
		{"the launch process", 4242, launchCommand, true},
		{"another command", 4242, func(int64) func(context.Context, int) (string, error) {
			return func(context.Context, int) (string, error) { return "/usr/bin/vim notes.txt", nil }
		}, false},
		{"the launch process of another run", 4242, func(run int64) func(context.Context, int) (string, error) {
			return launchCommand(run + 10)
		}, false},
		{"a gone process", 4242, func(int64) func(context.Context, int) (string, error) {
			return func(context.Context, int) (string, error) { return "", nil }
		}, false},
		{"pid 0", 0, launchCommand, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newAutomationFixture(t)
			_, id := f.bound(t)
			run := f.claimRun(t, id, "k")
			s := openStore(t, f.store)
			write(t, s, func(tx *sql.Tx) error { return store.StartRun(context.Background(), tx, run, tt.pid, testNow) })
			closeStore(t, s)
			var out bytes.Buffer
			e := f.env(f.sock, "w14:p1", "w14", &out)
			e.Command = tt.command(run)
			var killed []int
			e.Kill = func(pid int) error {
				killed = append(killed, pid)
				return nil
			}
			if err := Automation(context.Background(), e, []string{"cancel", fmt.Sprint(run)}); err != nil {
				t.Fatalf("cancel = %v", err)
			}
			if got := len(killed) == 1 && killed[0] == tt.pid; got != tt.killed || len(killed) > 1 {
				t.Errorf("killed = %v, want a signal %v", killed, tt.killed)
			}
			if r := f.run(t, run); r.Result != store.RunCancelled {
				t.Errorf("run = %+v, want cancelled", r)
			}
			if !strings.Contains(out.String(), fmt.Sprintf("run %d of automation %d cancelled", run, id)) {
				t.Errorf("cancel output = %q", out.String())
			}
		})
	}
}

// processCommand reads a real command line with ps, and "" for a pid with no process.
func TestProcessCommand(t *testing.T) {
	got, err := processCommand(context.Background(), os.Getpid())
	if err != nil || !strings.Contains(got, filepath.Base(os.Args[0])) {
		t.Errorf("processCommand(self) = %q, %v, want the test binary", got, err)
	}
	// The pid of a child that ended and was reaped has no process.
	done := exec.Command("/usr/bin/true")
	if err := done.Run(); err != nil {
		t.Fatal(err)
	}
	pid := done.Process.Pid
	if got, err := processCommand(context.Background(), pid); err != nil || got != "" {
		t.Errorf("processCommand(%d) = %q, %v, want \"\"", pid, got, err)
	}
}

// The cause of a failed run is the failed step, or a fixed line for a refusal. The text of
// any other error is no cause: it can hold herdr or store text (#352).
func TestRunResult(t *testing.T) {
	for _, tc := range []struct {
		name   string
		res    ToolResult
		err    error
		result store.RunResult
		cause  string
	}{
		{"ok", ToolResult{}, nil, store.RunOK, ""},
		{"step", ToolResult{Step: "step 2: npm: exit status 1"}, errors.New("tool t: step 2: npm: exit status 1 (exit code 1)"),
			store.RunFailed, "step 2: npm: exit status 1"},
		{"refused", ToolResult{ExitCode: 1}, fmt.Errorf("tool t: the hash changed: %w", ErrRefused), store.RunFailed,
			"the call was refused; nothing ran"},
		{"other", ToolResult{}, errors.New("herdr: token=abc123"), store.RunFailed, ""},
	} {
		if result, cause := runResult(tc.res, tc.err); result != tc.result || cause != tc.cause {
			t.Errorf("%s: runResult() = %s, %q, want %s, %q", tc.name, result, cause, tc.result, tc.cause)
		}
	}
}
