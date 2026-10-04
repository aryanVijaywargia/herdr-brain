package brained

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"slices"
	"syscall"
	"testing"
	"time"

	"github.com/aryanVijaywargia/herdr-brain/internal/bindings"
	"github.com/aryanVijaywargia/herdr-brain/internal/herdr"
	"github.com/aryanVijaywargia/herdr-brain/internal/output"
	"github.com/aryanVijaywargia/herdr-brain/internal/paths"
	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

// launched is one start of a launch process by brained.
type launched struct {
	environ []string
	l       Launch
}

// autoHarness is a brained of herdr session s1 whose store maps w1 (ws_1) and w2 (ws_2). Each
// workspace has an approved tool that runs in its folder, and an automation of it on the
// stops of a claude agent in that folder.
type autoHarness struct {
	*harness
	folder   map[string]string // the pinned folder of each workspace key
	auto     map[string]int64  // the automation of each workspace key
	launches chan launched
}

func newAutoHarness(t *testing.T, kind bindings.Kind) *autoHarness {
	t.Helper()
	h := &autoHarness{harness: newHarness(t, kind), folder: map[string]string{}, auto: map[string]int64{},
		launches: make(chan launched, 16)}
	h.d.now = time.Now
	h.d.environ = []string{"HOME=/home/me", "HERDR_PANE_ID=w1:p1", "HERDR_WORKSPACE_ID=w1", "HERDR_SESSION=s1"}
	h.d.launch = func(_ context.Context, environ []string, l Launch) error {
		h.launches <- launched{environ, l}
		return nil
	}
	proj, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(filepath.Dir(h.f.path), "session.json"), map[string]any{"version": 3,
		"workspaces": []map[string]string{{"id": "w1", "identity_cwd": proj}, {"id": "w2", "identity_cwd": proj}}})
	writeFile(t, paths.WorkspaceMap(h.store), map[string]any{"version": 1, "entries": []map[string]string{
		{"key": "ws_1", "session": "s1", "workspace_id": "w1", "label": "a", "project": proj},
		{"key": "ws_2", "session": "s1", "workspace_id": "w2", "label": "b", "project": proj}}})
	now := time.Date(2026, 10, 5, 9, 0, 0, 0, time.UTC)
	h.tx(t, func(ctx context.Context, tx *sql.Tx) error {
		for _, ws := range []string{"1", "2"} {
			key := "ws_" + ws
			h.folder[key] = filepath.Join(proj, "f"+ws)
			if err := store.InsertWorkspace(ctx, tx, store.Workspace{Key: key, HerdrSession: "s1", HerdrWorkspaceID: "w" + ws,
				Label: key, Status: store.WorkspaceLive, LastSeen: now}); err != nil {
				return err
			}
			contract := fmt.Sprintf("name = \"lint\"\ndescription = \"Lint\"\ncwd = %q\ntimeout_s = 10\neffects = \"none\"\n"+
				"read_only = true\nargv = [\"/usr/bin/true\"]\n", h.folder[key])
			tool, _, err := store.CreateItem(ctx, tx, store.Item{ClaimID: "t" + ws, Kind: store.KindTool, Created: now},
				store.ItemRevision{Text: contract, Scope: store.ScopeWorkspace, WorkspaceKey: &key, State: store.StateActive,
					Author: store.AuthorYou, Origin: store.OriginReview, Created: now})
			if err != nil {
				return err
			}
			tl, err := store.ToolByID(ctx, tx, tool)
			if err != nil {
				return err
			}
			b := store.Binding{Tool: "lint", ToolID: tool, ContractHash: tl.ContractHash, Folder: h.folder[key], CLI: "claude",
				Trigger: "working → idle or working → done"}
			h.auto[key], _, err = store.CreateItem(ctx, tx, store.Item{ClaimID: "a" + ws, Kind: store.KindAutomation, Created: now},
				store.ItemRevision{Text: b.Text(), Scope: store.ScopeWorkspace, WorkspaceKey: &key, State: store.StateActive,
					Author: store.AuthorYou, Origin: store.OriginReview, Created: now})
			if err != nil {
				return err
			}
		}
		return nil
	})
	st := h.open(t)
	defer st.Close() //nolint:errcheck // test
	if _, err := output.Build(context.Background(), st, filepath.Join(h.store, outDir), nil, func() time.Time { return now }); err != nil {
		t.Fatalf("Build() = %v", err)
	}
	return h
}

func (h *autoHarness) open(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), h.store, busyTimeout)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func (h *autoHarness) tx(t *testing.T, fn func(ctx context.Context, tx *sql.Tx) error) {
	t.Helper()
	st := h.open(t)
	defer st.Close() //nolint:errcheck // test
	if err := st.Write(context.Background(), func(tx *sql.Tx) error { return fn(context.Background(), tx) }); err != nil {
		t.Fatal(err)
	}
}

// agent adds pane id of workspace ws with a claude agent of session session in the folder of
// key, at status and seq.
func (h *autoHarness) agent(id, key, session, status string, seq uint64) {
	h.f.addPane(id, "claude")
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	p := h.f.panes[id]
	p.session, p.cwd, p.status, p.seq = session, h.folder[key], status, seq
}

// set changes the agent.get state of pane id.
func (h *autoHarness) set(id, status string, seq uint64, terminal string) {
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	p := h.f.panes[id]
	p.status, p.seq, p.terminal = status, seq, terminal
}

// event sends status event status of pane id on st, and waits for its Change step: brained
// handled the automation part of the event before it.
func (h *autoHarness) event(t *testing.T, st *fakeSub, id, status string) {
	t.Helper()
	st.send(statusLine(id, status))
	wantStep(t, h.step(t), Change, id, "", "")
}

// launch returns the next launch, or fails.
func (h *autoHarness) launch(t *testing.T) launched {
	t.Helper()
	select {
	case l := <-h.launches:
		return l
	case <-time.After(wait):
		t.Fatal("no launch")
		return launched{}
	}
}

// noLaunch fails when brained started a launch process.
func (h *autoHarness) noLaunch(t *testing.T) {
	t.Helper()
	select {
	case l := <-h.launches:
		t.Errorf("launch %+v, want none", l.l)
	default:
	}
}

func (h *autoHarness) runs(t *testing.T) []store.AutomationRun {
	t.Helper()
	var rs []store.AutomationRun
	h.tx(t, func(ctx context.Context, tx *sql.Tx) (err error) {
		rs, err = store.AutomationRuns(ctx, tx)
		return err
	})
	return rs
}

// startPane starts brained with pane w1:p1 and returns its status stream after the baseline.
func (h *autoHarness) startPane(t *testing.T, status string) *fakeSub {
	t.Helper()
	h.agent("w1:p1", "ws_1", "s1", status, 1)
	h.start(t)
	st := h.f.statusSubs(t, 1)["w1:p1"]
	wantStep(t, h.step(t), Baseline, "w1:p1", GapStart, "")
	return st
}

// Method P (M0 E17-S1, §10.4 step 1): a joined working, then a joined idle, is one stop. It
// claims one run and starts one launch process with the pane of the event. A duplicate of the
// stop joins at rest and claims nothing (S51). A user stop (Esc) gives the same change, so it
// runs (owner, 2026-10-05).
func TestAutomationStop(t *testing.T) {
	h := newAutoHarness(t, bindings.KindMain)
	st := h.startPane(t, "idle")
	h.set("w1:p1", "working", 5, "")
	h.event(t, st, "w1:p1", "working")
	h.noLaunch(t)
	h.set("w1:p1", "idle", 6, "")
	h.event(t, st, "w1:p1", "idle")
	got := h.launch(t)
	rs := h.runs(t)
	if len(rs) != 1 || rs[0].AutomationID != h.auto["ws_1"] || rs[0].EventKey != "w1:p1|term_1|s1|6" ||
		rs[0].PaneID != "w1:p1" || rs[0].WorkspaceID != "w1" || rs[0].Result != store.RunUnknown {
		t.Fatalf("runs = %+v, want one claim of automation %d with key w1:p1|term_1|s1|6", rs, h.auto["ws_1"])
	}
	if got.l != (Launch{Run: rs[0].ID, Binding: "s1", Store: h.store}) {
		t.Errorf("launch = %+v, want run %d of s1 in %s", got.l, rs[0].ID, h.store)
	}
	for _, kv := range []string{"HERDR_PANE_ID=w1:p1", "HERDR_WORKSPACE_ID=w1", "HERDR_SOCKET_PATH=" + h.f.path} {
		if !slices.Contains(got.environ, kv) {
			t.Errorf("launch environ = %q, want %s", got.environ, kv)
		}
	}
	h.event(t, st, "w1:p1", "idle") // a duplicate of the stop
	// A user stop: Esc while working gives working → done in a pane without focus.
	h.set("w1:p1", "working", 7, "")
	h.event(t, st, "w1:p1", "working")
	h.set("w1:p1", "done", 8, "")
	h.event(t, st, "w1:p1", "done")
	h.launch(t)
	h.noLaunch(t)
	if rs := h.runs(t); len(rs) != 2 {
		t.Errorf("runs = %+v, want 2 claims", rs)
	}
}

// Method P cases, each after a joined working of pane w1:p1.
func TestAutomationSkip(t *testing.T) {
	tests := []struct {
		name string
		run  func(t *testing.T, h *autoHarness, st *fakeSub)
		want int // claims
	}{
		{"an event that does not join", func(t *testing.T, h *autoHarness, st *fakeSub) {
			// The agent works again: the idle event does not join and changes no memory.
			h.event(t, st, "w1:p1", "idle")
			h.noLaunch(t)
			h.set("w1:p1", "idle", 6, "")
			h.event(t, st, "w1:p1", "idle")
		}, 1},
		{"no agent session id", func(t *testing.T, h *autoHarness, st *fakeSub) {
			h.f.mu.Lock()
			h.f.panes["w1:p1"].session = ""
			h.f.mu.Unlock()
			h.set("w1:p1", "done", 6, "")
			h.event(t, st, "w1:p1", "done")
		}, 0},
		{"a herdr server restart", func(t *testing.T, h *autoHarness, st *fakeSub) {
			h.set("w1:p1", "idle", 6, "")
			h.event(t, st, "w1:p1", "idle")
			// herdr restarts: state_change_seq starts again, the same agent session resumes in the
			// same pane, and terminal_id changes. The same seq is a new stop.
			h.set("w1:p1", "working", 5, "term_2")
			h.event(t, st, "w1:p1", "working")
			h.set("w1:p1", "idle", 6, "term_2")
			h.event(t, st, "w1:p1", "idle")
		}, 2},
		{"a codex agent", func(t *testing.T, h *autoHarness, st *fakeSub) {
			h.f.mu.Lock()
			h.f.panes["w1:p1"].agent = "codex"
			h.f.mu.Unlock()
			h.set("w1:p1", "idle", 6, "")
			h.event(t, st, "w1:p1", "idle")
		}, 0},
		{"another folder", func(t *testing.T, h *autoHarness, st *fakeSub) {
			h.f.mu.Lock()
			h.f.panes["w1:p1"].cwd = h.folder["ws_2"]
			h.f.mu.Unlock()
			h.set("w1:p1", "idle", 6, "")
			h.event(t, st, "w1:p1", "idle")
		}, 0},
		{"an already-paused binding", func(t *testing.T, h *autoHarness, st *fakeSub) {
			// brained still follows the pane; the claim transaction sees the pause.
			h.tx(t, func(ctx context.Context, tx *sql.Tx) error {
				return store.SetAutomationEnabled(ctx, tx, h.auto["ws_1"], false)
			})
			h.set("w1:p1", "idle", 6, "")
			h.event(t, st, "w1:p1", "idle")
		}, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newAutoHarness(t, bindings.KindMain)
			st := h.startPane(t, "idle")
			h.set("w1:p1", "working", 5, "")
			h.event(t, st, "w1:p1", "working")
			tt.run(t, h, st)
			if rs := h.runs(t); len(rs) != tt.want {
				t.Errorf("runs = %+v, want %d", rs, tt.want)
			}
			for range tt.want {
				h.launch(t)
			}
			h.noLaunch(t)
		})
	}
}

// AC S59: no backfill. At a brained start the current state is a baseline: a stop that came
// while brained was down never runs, but the stop of an agent that works at the start runs.
func TestAutomationBaseline(t *testing.T) {
	h := newAutoHarness(t, bindings.KindMain)
	st := h.startPane(t, "idle") // the agent stopped while brained was down
	h.event(t, st, "w1:p1", "idle")
	h.noLaunch(t)

	h = newAutoHarness(t, bindings.KindMain)
	st = h.startPane(t, "working")
	h.set("w1:p1", "idle", 2, "")
	h.event(t, st, "w1:p1", "idle")
	h.launch(t)
}

// AC: brained starts in a pane of workspace A. A valid event in workspace B claims the
// automation of B only, and its launch process gets the pane of B, not the pane of brained.
func TestAutomationOtherWorkspace(t *testing.T) {
	h := newAutoHarness(t, bindings.KindMain)
	h.agent("w1:p1", "ws_1", "s1", "idle", 1)
	h.agent("w2:p1", "ws_2", "s2", "working", 2)
	h.start(t)
	subs := h.f.statusSubs(t, 2)
	h.step(t)
	h.step(t)
	h.set("w2:p1", "idle", 3, "")
	h.event(t, subs["w2:p1"], "w2:p1", "idle")
	got := h.launch(t)
	rs := h.runs(t)
	if len(rs) != 1 || rs[0].AutomationID != h.auto["ws_2"] || rs[0].PaneID != "w2:p1" {
		t.Fatalf("runs = %+v, want one claim of automation %d of ws_2", rs, h.auto["ws_2"])
	}
	for _, kv := range []string{"HERDR_PANE_ID=w2:p1", "HERDR_WORKSPACE_ID=w2"} {
		if !slices.Contains(got.environ, kv) {
			t.Errorf("launch environ = %q, want %s", got.environ, kv)
		}
	}
	if slices.Contains(got.environ, "HERDR_PANE_ID=w1:p1") {
		t.Errorf("launch environ = %q, want no pane of brained", got.environ)
	}
}

// AC: an event of a test binding claims in the test store and launches with it. A test store
// has learning off, and brained follows the pane for its automation still.
func TestAutomationTestBinding(t *testing.T) {
	h := newAutoHarness(t, bindings.KindTest)
	h.agent("w1:p1", "ws_1", "s1", "working", 1)
	h.f.gets = make(chan string, 16)
	h.start(t)
	st := h.f.statusSubs(t, 1)["w1:p1"]
	select {
	case <-h.f.gets: // the baseline
	case <-time.After(wait):
		t.Fatal("no baseline")
	}
	h.set("w1:p1", "idle", 2, "")
	st.send(statusLine("w1:p1", "idle"))
	got := h.launch(t)
	if got.l.Store != h.store || got.l.Binding != "s1" || !slices.Contains(got.environ, "HERDR_SOCKET_PATH="+h.f.path) {
		t.Errorf("launch = %+v %q, want the test store %s and its socket", got.l, got.environ, h.store)
	}
}

// AC S59: after a reconnect of brained, the current agent state is a baseline. A stop in the
// time between, and a duplicate of it, never run; the next live stop runs.
func TestAutomationReconnect(t *testing.T) {
	h := newAutoHarness(t, bindings.KindMain)
	h.agent("w1:p1", "ws_1", "s1", "working", 1)
	ctx, cancel := context.WithCancel(context.Background())
	l := newLoop(h.d)
	defer func() {
		cancel()
		l.stop()
	}()
	if err := l.start(ctx); err != nil {
		t.Fatal(err)
	}
	h.f.sub(t) // the main stream
	h.f.statusSubs(t, 1)
	wantStep(t, h.step(t), Baseline, "w1:p1", GapStart, "")
	h.set("w1:p1", "idle", 2, "") // the agent stops while the event socket is down
	if err := l.item(ctx, item{src: l.main, err: syscall.ECONNRESET}); err != nil {
		t.Fatalf("item = %v, want a reconnect", err)
	}
	h.f.sub(t)
	wantStep(t, h.step(t), Baseline, "w1:p1", GapConnect, "")
	p := l.panes["w1:p1"]
	event := func(status string) {
		t.Helper()
		if err := l.item(ctx, item{ev: herdr.Event{Kind: herdr.PaneStatusChanged, PaneID: "w1:p1", WorkspaceID: "w1",
			Status: status}, src: p.stream}); err != nil {
			t.Fatal(err)
		}
		wantStep(t, h.step(t), Change, "w1:p1", "", "")
	}
	event("idle") // a late copy of the missed stop
	h.noLaunch(t)
	h.set("w1:p1", "working", 3, "")
	event("working")
	h.set("w1:p1", "idle", 4, "")
	event("idle")
	h.launch(t)
	if rs := h.runs(t); len(rs) != 1 || rs[0].EventKey != "w1:p1|term_1|s1|4" {
		t.Errorf("runs = %+v, want one claim of the live stop at seq 4", rs)
	}
}
