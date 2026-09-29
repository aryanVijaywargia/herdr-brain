package brained

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aryanVijaywargia/herdr-brain/internal/adapter"
	"github.com/aryanVijaywargia/herdr-brain/internal/bindings"
	"github.com/aryanVijaywargia/herdr-brain/internal/herdr"
	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

const (
	codexRun1 = "01a0ec3e-5e95-7253-aa8d-e7d232139183" // the agent session ids of M0 E11-S1b
	codexRun2 = "01a0ec41-83c7-7463-a232-9faa89066336"
)

// capture runs one Capture of one binding against a fake herdr, one step at a time.
type capHarness struct {
	f     *fakeHerdr
	c     *Capture
	gate  Gate
	clock time.Time
}

// newCapture binds herdr session session to the store in dir and returns its Capture. Codex
// gets the noise patterns of testdata/noise.
func newCapture(t *testing.T, session, dir string) *capHarness {
	t.Helper()
	f := newFakeHerdr(t)
	b := bindings.Binding{Session: session, Socket: f.path, Store: dir, Kind: bindings.KindMain, Created: time.Now()}
	k := &capHarness{f: f, c: NewCapture(herdr.Socket{Path: f.path}), gate: NewGate(b),
		clock: time.Date(2026, 9, 29, 10, 0, 0, 0, time.UTC)}
	k.c.now = func() time.Time {
		k.clock = k.clock.Add(time.Second)
		return k.clock
	}
	k.c.profile = func(h string) (*adapter.Profile, error) { return adapter.LoadFS(os.DirFS("testdata/noise"), h) }
	t.Cleanup(func() { onlyReads(t, f) })
	return k
}

// onlyReads checks the AC that Brain sends no keys and no scroll to any pane: the fake herdr
// saw only reads.
func onlyReads(t *testing.T, f *fakeHerdr) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	allowed := []string{"pane.get", "pane.read visible", "pane.read recent_unwrapped", "pane.list", "plugin.list", "events.subscribe"}
	for _, c := range f.calls {
		if !slices.Contains(allowed, c) {
			t.Errorf("herdr call %q: Brain only reads", c)
		}
	}
}

// pane adds pane id with agent and agent session id session ("" for none).
func (k *capHarness) pane(id, agent, session string) {
	k.f.addPane(id, agent)
	k.f.mu.Lock()
	defer k.f.mu.Unlock()
	k.f.panes[id].session = session
}

// reads queues the fixture reads of pane id.
func (k *capHarness) reads(t *testing.T, id string, files ...string) {
	t.Helper()
	k.f.mu.Lock()
	defer k.f.mu.Unlock()
	for _, name := range files {
		b, err := os.ReadFile(filepath.Join("testdata", name))
		if err != nil {
			t.Fatal(err)
		}
		k.f.reads[id] = append(k.f.reads[id], string(b))
	}
}

// step gives one step of pane id to the Capture, with the learning state in the store.
func (k *capHarness) step(t *testing.T, kind StepKind, id, status string, gap Gap) error {
	t.Helper()
	ws, _, _ := strings.Cut(id, ":")
	s := Step{Kind: kind, PaneID: id, WorkspaceID: ws, Gap: gap, Gate: k.gate, Learning: k.learning(t, id, ws)}
	if status != "" {
		s.Event = &herdr.Event{Kind: herdr.PaneStatusChanged, PaneID: id, WorkspaceID: ws, Status: status}
	}
	if kind == End {
		s.Reason = ReasonGone
	}
	return k.c.Handle(context.Background(), s)
}

func (k *capHarness) must(t *testing.T, kind StepKind, id, status string, gap Gap) {
	t.Helper()
	if err := k.step(t, kind, id, status, gap); err != nil {
		t.Fatalf("%s %s: %v", kind, id, err)
	}
}

func (k *capHarness) learning(t *testing.T, id, ws string) Learning {
	t.Helper()
	var l Learning
	k.tx(t, func(tx *sql.Tx) (err error) {
		l, err = k.gate.Learning(context.Background(), tx, id, ws)
		return err
	})
	return l
}

func (k *capHarness) tx(t *testing.T, fn func(tx *sql.Tx) error) {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, k.gate.Binding.Store, busyTimeout)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close() //nolint:errcheck // test
	if err := st.Write(ctx, fn); err != nil {
		t.Fatal(err)
	}
}

// rows is what the store holds.
type rows struct {
	sessions []store.Session
	segments []store.Segment
	obs      []store.Observation
}

func (k *capHarness) rows(t *testing.T) rows {
	t.Helper()
	ctx := context.Background()
	var r rows
	k.tx(t, func(tx *sql.Tx) (err error) {
		if r.sessions, err = store.Sessions(ctx, tx); err != nil {
			return err
		}
		if r.segments, err = store.Segments(ctx, tx); err != nil {
			return err
		}
		r.obs, err = store.Observations(ctx, tx)
		return err
	})
	return r
}

// text returns the excerpts of the text observations, and the events of the gaps.
func (r rows) text() (excerpts, gaps []string) {
	for _, o := range r.obs {
		if o.Status == store.ObservationGap {
			gaps = append(gaps, o.Event)
		} else {
			excerpts = append(excerpts, o.Excerpt)
		}
	}
	return excerpts, gaps
}

// fixture returns the lines of a fixture file.
func fixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// S31: working, blocked, working, idle in one task. Each read keeps its new part, the prompt
// is kept once, and every excerpt line is text of a read: Brain invents no text.
func TestCaptureS31(t *testing.T) {
	k := newCapture(t, "s1", t.TempDir())
	k.pane("w1:p1", "codex", codexRun2)
	files := []string{"codex-run2-0-idle.txt", "codex-run2-1-working.txt", "codex-run2-2-blocked.txt",
		"codex-run2-3-working.txt", "codex-run2-4-idle.txt"}
	k.reads(t, "w1:p1", files...)
	k.must(t, Baseline, "w1:p1", "", GapNone)
	for _, st := range []string{"working", "blocked", "working", "idle"} {
		k.must(t, Change, "w1:p1", st, GapNone)
	}
	r := k.rows(t)
	if len(r.sessions) != 1 || r.sessions[0].Key.Kind != store.KeyAgent || r.sessions[0].Key.ID != codexRun2 {
		t.Fatalf("sessions = %+v, want one with the agent session id", r.sessions)
	}
	if len(r.segments) != 1 || r.segments[0].ReadSource != herdr.ReadRecentUnwrapped {
		t.Fatalf("segments = %+v, want one recent_unwrapped segment", r.segments)
	}
	excerpts, gaps := r.text()
	if len(gaps) != 0 || len(r.obs) != 4 {
		t.Fatalf("observations = %+v, want 4 with text and no gap", r.obs)
	}
	var events []string
	for _, o := range r.obs {
		events = append(events, o.Event)
		if o.Status != store.ObservationComplete {
			t.Errorf("observation %q status = %s, want complete", o.Excerpt, o.Status)
		}
	}
	if want := []string{"working", "blocked", "working", "idle"}; !slices.Equal(events, want) {
		t.Errorf("events = %v, want %v", events, want)
	}
	all := strings.Join(excerpts, "\n")
	if n := strings.Count(all, "› List the files"); n != 1 {
		t.Errorf("the prompt is kept %d times, want 1 (no invented user turn):\n%s", n, all)
	}
	for _, want := range []string{"Would you like to run", "• Ran ls -la", "notes-kx.txt is the only text file"} {
		if !strings.Contains(all, want) {
			t.Errorf("excerpts miss %q:\n%s", want, all)
		}
	}
	var reads []string
	for _, f := range files {
		reads = append(reads, fixture(t, f))
	}
	for _, l := range strings.Split(all, "\n") {
		if !slices.ContainsFunc(reads, func(r string) bool { return strings.Contains(r, l) }) {
			t.Errorf("excerpt line %q is in no read", l)
		}
	}
	for _, noise := range []string{"Context", "Working (", "Ask Codex"} {
		if strings.Contains(all, noise) {
			t.Errorf("excerpts hold the noise %q", noise)
		}
	}
}

// S33: the pane exits before the read returns. The read is dropped with a gap, and a new
// agent in a pane with the same id gets none of its text.
func TestCaptureS33(t *testing.T) {
	k := newCapture(t, "s1", t.TempDir())
	k.pane("w1:p1", "codex", codexRun1)
	k.reads(t, "w1:p1", "codex-run2-0-idle.txt", "codex-run2-1-working.txt")
	k.must(t, Baseline, "w1:p1", "", GapNone)
	k.f.onRead = func(f *fakeHerdr) { delete(f.panes, "w1:p1"); f.onRead = nil }
	k.must(t, Change, "w1:p1", "working", GapNone)
	k.must(t, End, "w1:p1", "", GapNone)

	k.pane("w1:p1", "codex", codexRun2)
	k.f.mu.Lock()
	k.f.reads["w1:p1"] = nil
	k.f.mu.Unlock()
	k.reads(t, "w1:p1", "codex-run2-2-blocked.txt", "codex-run2-3-working.txt")
	k.must(t, Baseline, "w1:p1", "", GapNone)
	k.must(t, Change, "w1:p1", "working", GapNone)

	r := k.rows(t)
	excerpts, gaps := r.text()
	if !slices.Equal(gaps, []string{gapGone}) {
		t.Errorf("gaps = %v, want [%s]", gaps, gapGone)
	}
	if len(r.sessions) != 2 || r.sessions[0].Ended.IsZero() || r.sessions[1].Key.ID != codexRun2 {
		t.Fatalf("sessions = %+v, want run 1 ended and run 2", r.sessions)
	}
	for _, e := range excerpts {
		if strings.Contains(e, "› List the files") {
			t.Errorf("excerpt %q: text of the dropped read went to the new agent", e)
		}
	}
	if len(excerpts) != 1 || !strings.Contains(excerpts[0], "• Ran ls -la") {
		t.Errorf("excerpts = %q, want the new part of run 2 only", excerpts)
	}
}

// S36: the pane moves to another workspace while a read is pending. The read is dropped
// with a gap in the old segment, and the pane's next baseline starts a new segment in the
// new workspace.
func TestCaptureS36(t *testing.T) {
	k := newCapture(t, "s1", t.TempDir())
	k.pane("w1:p1", "codex", codexRun2)
	k.reads(t, "w1:p1", "codex-run2-0-idle.txt", "codex-run2-1-working.txt")
	k.must(t, Baseline, "w1:p1", "", GapNone)
	k.f.onRead = func(f *fakeHerdr) {
		f.panes["w2:p1"] = &fakePane{ws: "w2", agent: "codex", session: codexRun2}
		delete(f.panes, "w1:p1")
		f.moved["w1:p1"] = "w2:p1"
		f.reads["w2:p1"] = f.reads["w1:p1"]
		f.onRead = nil
	}
	k.must(t, Change, "w1:p1", "working", GapNone)
	k.must(t, Change, "w1:p1", "working", GapNone) // the old id reads the moved pane: nothing is kept, and the segment has its gap
	k.must(t, Baseline, "w2:p1", "", GapNone)

	r := k.rows(t)
	excerpts, gaps := r.text()
	if len(excerpts) != 0 || !slices.Equal(gaps, []string{gapMoved}) {
		t.Errorf("observations = %+v, want only one %q gap", r.obs, gapMoved)
	}
	if len(r.sessions) != 1 || len(r.segments) != 2 || r.segments[0].PaneID != "w1:p1" || r.segments[1].PaneID != "w2:p1" ||
		r.segments[0].Ended.IsZero() {
		t.Errorf("segments = %+v, want w1:p1 ended, then w2:p1 in the same session", r.segments)
	}
	for _, o := range r.obs {
		if o.SegmentID != r.segments[0].ID {
			t.Errorf("gap %+v is not in the old segment", o)
		}
	}
}

// S38: a repaint and a resize give no observation. A screen that lost text is partial.
func TestCaptureS38(t *testing.T) {
	k := newCapture(t, "s1", t.TempDir())
	k.pane("w1:p1", "claude", "")
	k.reads(t, "w1:p1", "claude-visible-1.txt", "claude-visible-1-repaint.txt", "claude-visible-1-narrow.txt",
		"claude-visible-2-scrolled.txt")
	k.must(t, Baseline, "w1:p1", "", GapNone)
	for _, st := range []string{"idle", "working", "idle"} {
		k.must(t, Change, "w1:p1", st, GapNone)
	}
	r := k.rows(t)
	if len(r.segments) != 1 || r.segments[0].ReadSource != herdr.ReadVisible {
		t.Fatalf("segments = %+v, want one visible segment (lower coverage)", r.segments)
	}
	if len(r.obs) != 1 || r.obs[0].Status != store.ObservationPartial || !strings.Contains(r.obs[0].Excerpt, "Line 8 ") {
		t.Errorf("observations = %+v, want one partial with the new screen", r.obs)
	}
	if len(r.sessions) != 1 || r.sessions[0].Key.Kind != store.KeyCapture {
		t.Errorf("sessions = %+v, want one with a capture id", r.sessions)
	}
}

// S40: the same text in two real turns gives two occurrences.
func TestCaptureS40(t *testing.T) {
	k := newCapture(t, "s1", t.TempDir())
	k.pane("w1:p1", "codex", codexRun2)
	k.reads(t, "w1:p1", "codex-run2-0-idle.txt", "codex-repeat-1.txt", "codex-repeat-2.txt")
	k.must(t, Baseline, "w1:p1", "", GapNone)
	k.must(t, Change, "w1:p1", "idle", GapNone)
	k.must(t, Change, "w1:p1", "idle", GapNone)
	excerpts, _ := k.rows(t).text()
	if len(excerpts) != 2 {
		t.Fatalf("excerpts = %q, want 2", excerpts)
	}
	for _, e := range excerpts {
		if strings.Count(e, "› Run the tests again.") != 1 {
			t.Errorf("excerpt %q, want the prompt once", e)
		}
	}
}

// Before the first prompt of a Codex run, herdr gives no agent session id: the session gets a
// capture id, stored before any evidence. The id that comes later keeps that key. After an
// end, the id gives a new session: the two are not merged.
func TestCaptureNativeIDLater(t *testing.T) {
	k := newCapture(t, "s1", t.TempDir())
	k.pane("w1:p1", "codex", "")
	k.reads(t, "w1:p1", "codex-run2-0-idle.txt", "codex-run2-1-working.txt", "codex-run2-2-blocked.txt")
	k.must(t, Baseline, "w1:p1", "", GapStart)
	r := k.rows(t)
	if len(r.sessions) != 1 || r.sessions[0].Key.Kind != store.KeyCapture || len(r.obs) != 1 || r.obs[0].Event != string(GapStart) {
		t.Fatalf("rows = %+v, want a capture session and its start gap", r)
	}
	k.pane("w1:p1", "codex", codexRun2)
	k.must(t, Change, "w1:p1", "working", GapNone)
	k.must(t, End, "w1:p1", "", GapNone)
	k.must(t, Baseline, "w1:p1", "", GapNone)
	r = k.rows(t)
	if len(r.sessions) != 2 || r.sessions[0].Key.Kind != store.KeyCapture || r.sessions[0].AgentSessionID != codexRun2 ||
		r.sessions[0].Ended.IsZero() || r.sessions[1].Key != (store.SessionKey{HerdrSession: "s1", Harness: "codex", Kind: store.KeyAgent, ID: codexRun2}) {
		t.Errorf("sessions = %+v, want the ended capture session with the later id, then the agent session", r.sessions)
	}
}

// A reconnect keeps the session, starts a new segment and records the gap.
func TestCaptureReconnect(t *testing.T) {
	k := newCapture(t, "s1", t.TempDir())
	k.pane("w1:p1", "codex", codexRun2)
	k.reads(t, "w1:p1", "codex-run2-0-idle.txt", "codex-run2-2-blocked.txt", "codex-run2-3-working.txt")
	k.must(t, Baseline, "w1:p1", "", GapNone)
	k.must(t, Baseline, "w1:p1", "", GapConnect)
	k.must(t, Change, "w1:p1", "working", GapNone)
	r := k.rows(t)
	if len(r.sessions) != 1 || len(r.segments) != 2 || r.segments[0].Ended.IsZero() {
		t.Fatalf("rows = %+v, want one session with two segments", r)
	}
	if len(r.obs) != 2 || r.obs[0].Event != string(GapConnect) || r.obs[1].SegmentID != r.segments[1].ID ||
		strings.Contains(r.obs[1].Excerpt, "Would you like") {
		t.Errorf("observations = %+v, want the gap, then only text after the new baseline", r.obs)
	}
}

// A learning switch after the baseline drops the read: it is the new baseline, with a gap.
func TestCaptureLearningSwitch(t *testing.T) {
	k := newCapture(t, "s1", t.TempDir())
	k.pane("w1:p1", "codex", codexRun2)
	k.reads(t, "w1:p1", "codex-run2-0-idle.txt", "codex-run2-1-working.txt", "codex-run2-2-blocked.txt")
	k.must(t, Baseline, "w1:p1", "", GapNone)
	base := k.learning(t, "w1:p1", "w1")
	for _, on := range []bool{false, true} {
		k.tx(t, func(tx *sql.Tx) error {
			return store.SetLearning(context.Background(), tx, store.LearningSetting{Scope: store.LearningPane,
				HerdrSession: "s1", Target: "w1:p1", On: on, Changed: time.Now()})
		})
	}
	// brained has not seen the switch yet: the step holds the old state.
	s := Step{Kind: Change, PaneID: "w1:p1", WorkspaceID: "w1", Gate: k.gate, Learning: base,
		Event: &herdr.Event{Status: "working"}}
	if err := k.c.Handle(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	k.must(t, Change, "w1:p1", "blocked", GapNone)
	r := k.rows(t)
	excerpts, gaps := r.text()
	if !slices.Equal(gaps, []string{string(GapLearning)}) || len(r.segments) != 2 {
		t.Fatalf("rows = %+v, want a new segment with a learning gap", r)
	}
	for _, e := range excerpts {
		if strings.Contains(e, "› List the files") {
			t.Errorf("excerpt %q holds text of the time with learning off", e)
		}
	}
}

// Two bindings with the same pane ids and the same agent session ids, plus an agent with no
// agent session id, in one store: each gets its own sessions, and an end, an enrich or a
// forget in one binding leaves the other intact.
func TestCaptureTwoBindings(t *testing.T) {
	dir := t.TempDir()
	a, b := newCapture(t, "s1", dir), newCapture(t, "s2", dir)
	for _, k := range []*capHarness{a, b} {
		k.pane("w1:p1", "codex", codexRun2)
		k.pane("w1:p2", "claude", "")
		k.reads(t, "w1:p1", "codex-run2-0-idle.txt", "codex-run2-1-working.txt")
		k.reads(t, "w1:p2", "claude-visible-1.txt", "claude-visible-2-scrolled.txt")
		for _, id := range []string{"w1:p1", "w1:p2"} {
			k.must(t, Baseline, id, "", GapNone)
			k.must(t, Change, id, "working", GapNone)
		}
	}
	r := a.rows(t)
	if len(r.sessions) != 4 || len(r.obs) != 4 {
		t.Fatalf("rows = %+v, want 4 sessions and 4 observations", r)
	}
	// Enrich: the claude agent of s1 gets an id; end: the codex agent of s1 ends.
	a.pane("w1:p2", "claude", "6a345ec1-3050-4e61-b736-d0e9d2edab82")
	a.must(t, Change, "w1:p2", "idle", GapNone)
	a.must(t, End, "w1:p1", "", GapNone)
	r = a.rows(t)
	var forget store.SessionKey
	for _, s := range r.sessions {
		switch {
		case s.Key.HerdrSession == "s2" && (!s.Ended.IsZero() || s.AgentSessionID != "" && s.Key.Kind == store.KeyCapture):
			t.Errorf("session %+v of s2 changed", s)
		case s.Key.HerdrSession == "s1" && s.Key.Kind == store.KeyAgent && s.Ended.IsZero():
			t.Errorf("session %+v of s1 is open, want ended", s)
		case s.Key.HerdrSession == "s1" && s.Key.Kind == store.KeyCapture:
			if s.AgentSessionID == "" {
				t.Errorf("session %+v of s1 is not enriched", s)
			}
			forget = s.Key
		}
	}
	a.tx(t, func(tx *sql.Tx) error { return store.ForgetSession(context.Background(), tx, forget) })
	r = a.rows(t)
	var s2 int
	for _, s := range r.sessions {
		if s.Key.HerdrSession == "s2" {
			s2++
		}
	}
	if len(r.sessions) != 3 || s2 != 2 || len(r.obs) != 3 {
		t.Errorf("after forget: %d sessions (%d of s2), %d observations, want 3 (2 of s2) and 3", len(r.sessions), s2, len(r.obs))
	}
}

// The excerpt filter drops secrets and cuts to 500 characters.
func TestFilter(t *testing.T) {
	got, cut := filter("key AKIAIOSFODNN7EXAMPLE here")
	if strings.Contains(got, "AKIA") || cut {
		t.Errorf("filter = %q, %v, want the key dropped", got, cut)
	}
	got, cut = filter(strings.Repeat("é", 600))
	if len([]rune(got)) != excerptLimit || !cut {
		t.Errorf("filter of 600 characters = %d characters, cut %v, want %d, true", len([]rune(got)), cut, excerptLimit)
	}
}
