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

	"github.com/aryanVijaywargia/herdr-brain/internal/bindings"
	"github.com/aryanVijaywargia/herdr-brain/internal/fingerprint"
	"github.com/aryanVijaywargia/herdr-brain/internal/herdr"
	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

const (
	codexRun1 = "01a0ec3e-5e95-7253-aa8d-e7d232139183" // the agent session ids of the E11-S1b test
	codexRun2 = "01a0ec41-83c7-7463-a232-9faa89066336"
)

// capture runs one Capture of one binding against a fake herdr, one step at a time.
type capHarness struct {
	f     *fakeHerdr
	c     *Capture
	gate  Gate
	clock time.Time
}

// newCapture binds herdr session session to the store in dir and returns its Capture, with the
// noise lines of the shipped profiles.
func newCapture(t *testing.T, session, dir string) *capHarness {
	t.Helper()
	f := newFakeHerdr(t)
	b := bindings.Binding{Session: session, Socket: f.path, Store: dir, Kind: bindings.KindMain, Created: time.Now()}
	k := &capHarness{f: f, c: NewCapture(herdr.Socket{Path: f.path}, nil), gate: NewGate(b),
		clock: time.Date(2026, 9, 29, 10, 0, 0, 0, time.UTC)}
	k.c.now = func() time.Time {
		k.clock = k.clock.Add(time.Second)
		return k.clock
	}
	t.Cleanup(func() { onlyReads(t, f) })
	return k
}

// onlyReads checks the AC that Brain sends no keys and no scroll to any pane: the fake herdr
// saw only reads.
func onlyReads(t *testing.T, f *fakeHerdr) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	allowed := []string{"pane.get", "pane.read visible", "pane.read recent_unwrapped", "pane.process_info", "pane.list", "plugin.list", "events.subscribe", "agent.get"}
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

// text returns the excerpts of the text observations, one for each read (its parts joined
// with a line break), and the events of the gaps.
func (r rows) text() (excerpts, gaps []string) {
	var last string
	for _, o := range r.obs {
		// The capture identity of a part is the identity of its read and the part number.
		read := o.CaptureIdentity[:strings.LastIndex(o.CaptureIdentity, "/")]
		switch {
		case o.Status == store.ObservationGap:
			gaps = append(gaps, o.Event)
		case len(excerpts) > 0 && read == last:
			excerpts[len(excerpts)-1] += "\n" + o.Excerpt
		default:
			excerpts = append(excerpts, o.Excerpt)
		}
		last = read
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

// s31 is one recorded task of a harness through working, blocked, working and the end status.
type s31 struct {
	harness, session string
	source           string
	files, statuses  []string
	prompt           string   // the prompt of the task, on the screen once
	want             []string // text of the task that the excerpts keep
	noise            []string // screen text that the excerpts drop
}

// Working, blocked, working, idle in one task. Each read keeps its new part, the prompt
// is kept once, and every excerpt line is text of a read: Brain invents no text.
func TestCaptureS31(t *testing.T) {
	for _, tc := range []s31{
		{harness: "codex", session: codexRun2, source: herdr.ReadRecentUnwrapped,
			files: []string{"codex-s31-0-base.txt", "codex-s31-1-working.txt", "codex-s31-2-blocked.txt",
				"codex-s31-3-working.txt", "codex-s31-4-done.txt"},
			statuses: []string{"working", "blocked", "working", "done"},
			prompt:   "› Run touch made.txt",
			want:     []string{"Would you like to run", "• Ran touch made.txt", "successfully"},
			// The choices of the approval dialog and its echo line are not user text (#281).
			noise: []string{"Context", "Working (", "Ask Codex", "Yes, proceed", "don't ask again", "✔ You approved codex"}},
		{harness: "claude", source: herdr.ReadVisible,
			files: []string{"claude-s31-0-base.txt", "claude-s31-1-working.txt", "claude-s31-2-blocked.txt",
				"claude-s31-3-working.txt", "claude-s31-4-idle.txt"},
			statuses: []string{"working", "blocked", "working", "idle"},
			want:     []string{"Do you want to create hi.txt?", "Wrote 1 line to hi.txt", "⏺ Done."},
			noise:    []string{"thinking)", "ctx:", "manual mode"}},
	} {
		t.Run(tc.harness, func(t *testing.T) { testS31(t, tc) })
	}
}

func testS31(t *testing.T, tc s31) {
	k := newCapture(t, "s1", t.TempDir())
	k.pane("w1:p1", tc.harness, tc.session)
	k.reads(t, "w1:p1", tc.files...)
	k.must(t, Baseline, "w1:p1", "", GapNone)
	for _, st := range tc.statuses {
		k.must(t, Change, "w1:p1", st, GapNone)
	}
	r := k.rows(t)
	if len(r.sessions) != 1 {
		t.Fatalf("sessions = %+v, want one", r.sessions)
	}
	if len(r.segments) != 1 || r.segments[0].ReadSource != tc.source {
		t.Fatalf("segments = %+v, want one %s segment", r.segments, tc.source)
	}
	excerpts, gaps := r.text()
	if len(gaps) != 0 {
		t.Errorf("gaps = %v, want none", gaps)
	}
	for _, o := range r.obs {
		// The Claude screen after the approval dialog shows again the text that the dialog hid.
		if o.Status != store.ObservationComplete {
			t.Errorf("observation %q status = %s, want complete", o.Excerpt, o.Status)
		}
	}
	all := strings.Join(excerpts, "\n")
	if tc.prompt != "" {
		if n := strings.Count(all, tc.prompt); n != 1 {
			t.Errorf("the prompt is kept %d times, want 1 (no invented user turn)", n)
		}
	}
	for _, want := range tc.want {
		if !strings.Contains(all, want) {
			t.Errorf("excerpts miss %q", want)
		}
	}
	var reads []string
	for _, f := range tc.files {
		reads = append(reads, fixture(t, f))
	}
	for _, l := range strings.Split(all, "\n") {
		if !slices.ContainsFunc(reads, func(r string) bool { return strings.Contains(r, l) }) {
			t.Errorf("excerpt line %q is in no read", l)
		}
	}
	for _, noise := range tc.noise {
		if strings.Contains(all, noise) {
			t.Errorf("excerpts hold the noise %q", noise)
		}
	}
}

// An agent already works when brained attaches. The baseline at start stores no text of
// the screen, only the start gap, so the prompt of the running task is not invented as a
// start message. The next read keeps only its new part.
func TestCaptureS32(t *testing.T) {
	k := newCapture(t, "s1", t.TempDir())
	k.pane("w1:p1", "codex", codexRun2)
	k.reads(t, "w1:p1", "codex-s31-1-working.txt", "codex-s31-2-blocked.txt")
	k.must(t, Baseline, "w1:p1", "", GapStart)
	if excerpts, gaps := k.rows(t).text(); len(excerpts) != 0 || !slices.Equal(gaps, []string{string(GapStart)}) {
		t.Fatalf("after the baseline: excerpts %q, gaps %v, want only the start gap", excerpts, gaps)
	}
	k.must(t, Change, "w1:p1", "blocked", GapNone)
	excerpts, gaps := k.rows(t).text()
	if !slices.Equal(gaps, []string{string(GapStart)}) {
		t.Errorf("gaps = %v, want only the start gap", gaps)
	}
	all := strings.Join(excerpts, "\n")
	for _, old := range []string{"› Run touch made.txt", "› Delete blob.bin"} {
		if strings.Contains(all, old) {
			t.Errorf("excerpts hold %q of the screen before the attach: an invented start message", old)
		}
	}
	if !strings.Contains(all, "Would you like to run") {
		t.Errorf("excerpts = %q, want the new part of the blocked read", excerpts)
	}
}

// The pane exits before the read returns. The read is dropped with a gap, and a new
// agent in a pane with the same id gets none of its text.
func TestCaptureS33(t *testing.T) {
	k := newCapture(t, "s1", t.TempDir())
	k.pane("w1:p1", "codex", codexRun1)
	k.reads(t, "w1:p1", "codex-s31-0-base.txt", "codex-s31-1-working.txt")
	k.must(t, Baseline, "w1:p1", "", GapNone)
	k.f.onRead = func(f *fakeHerdr) { delete(f.panes, "w1:p1"); f.onRead = nil }
	k.must(t, Change, "w1:p1", "working", GapNone)
	k.must(t, End, "w1:p1", "", GapNone)

	k.pane("w1:p1", "codex", codexRun2)
	k.f.mu.Lock()
	k.f.reads["w1:p1"] = nil
	k.f.mu.Unlock()
	k.reads(t, "w1:p1", "codex-s31-2-blocked.txt", "codex-s31-3-working.txt")
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
		if strings.Contains(e, "› Run touch made.txt") {
			t.Errorf("excerpt %q: text of the dropped read went to the new agent", e)
		}
	}
	if len(excerpts) != 1 || !strings.Contains(excerpts[0], "• Ran touch made.txt") {
		t.Errorf("excerpts = %q, want the new part of run 2 only", excerpts)
	}
}

// The pane moves to another workspace while a read is pending. The read is dropped
// with a gap in the old segment, and the pane's next baseline starts a new segment in the
// new workspace.
func TestCaptureS36(t *testing.T) {
	k := newCapture(t, "s1", t.TempDir())
	k.pane("w1:p1", "codex", codexRun2)
	k.reads(t, "w1:p1", "codex-s31-0-base.txt", "codex-s31-1-working.txt")
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

// The end of the old pane id after a move does not end the session and segment of the pane
// with the new id.
func TestCaptureMovedEnd(t *testing.T) {
	k := newCapture(t, "s1", t.TempDir())
	k.pane("w1:p1", "codex", codexRun2)
	k.reads(t, "w1:p1", "codex-s31-0-base.txt")
	k.must(t, Baseline, "w1:p1", "", GapNone)
	k.f.mu.Lock()
	k.f.panes["w2:p1"] = &fakePane{ws: "w2", agent: "codex", session: codexRun2}
	delete(k.f.panes, "w1:p1")
	k.f.reads["w2:p1"] = k.f.reads["w1:p1"]
	k.f.mu.Unlock()
	k.must(t, Baseline, "w2:p1", "", GapNone)
	k.must(t, End, "w1:p1", "", GapNone)
	r := k.rows(t)
	if len(r.segments) != 2 || !r.segments[1].Ended.IsZero() || !r.sessions[0].Ended.IsZero() {
		t.Errorf("rows = %+v, want the session and the w2:p1 segment open", r)
	}
	k.must(t, End, "w2:p1", "", GapNone)
	if r = k.rows(t); r.sessions[0].Ended.IsZero() {
		t.Errorf("session %+v is open after the end of w2:p1", r.sessions[0])
	}
}

// An end that fails to write keeps the pane state, so the next end step closes the session.
func TestCaptureEndRetry(t *testing.T) {
	k := newCapture(t, "s1", t.TempDir())
	k.pane("w1:p1", "codex", codexRun2)
	k.reads(t, "w1:p1", "codex-s31-0-base.txt")
	k.must(t, Baseline, "w1:p1", "", GapNone)
	s := Step{Kind: End, PaneID: "w1:p1", WorkspaceID: "w1", Reason: ReasonGone, Gate: k.gate}
	s.Gate.Binding.Store = filepath.Join(t.TempDir(), "missing", "dir")
	if err := k.c.Handle(context.Background(), s); err == nil {
		t.Fatal("end with a bad store: no error")
	}
	k.must(t, End, "w1:p1", "", GapNone)
	if r := k.rows(t); r.sessions[0].Ended.IsZero() {
		t.Errorf("session %+v is open after the second end", r.sessions[0])
	}
}

// A repaint gives no observation. A screen that scrolled the old text away is partial.
// TestCaptureS38Resize has the recorded resize reads.
func TestCaptureS38(t *testing.T) {
	k := newCapture(t, "s1", t.TempDir())
	k.pane("w1:p1", "claude", "")
	k.reads(t, "w1:p1", "claude-s38-0-base.txt", "claude-s38-1-idle.txt", "claude-s38-2-repaint.txt")
	k.must(t, Baseline, "w1:p1", "", GapNone)
	k.must(t, Change, "w1:p1", "idle", GapNone)
	k.must(t, Change, "w1:p1", "idle", GapNone)
	r := k.rows(t)
	if len(r.segments) != 1 || r.segments[0].ReadSource != herdr.ReadVisible {
		t.Fatalf("segments = %+v, want one visible segment (lower coverage)", r.segments)
	}
	if len(r.obs) != 1 || r.obs[0].Status != store.ObservationPartial || !strings.Contains(r.obs[0].Excerpt, "  50") {
		t.Errorf("observations = %+v, want one partial with the new screen", r.obs)
	}
	if len(r.sessions) != 1 || r.sessions[0].Key.Kind != store.KeyCapture {
		t.Errorf("sessions = %+v, want one with a capture id", r.sessions)
	}
}

// A screen that lost the old text and shows nothing new is a gap.
func TestCaptureScreenLost(t *testing.T) {
	k := newCapture(t, "s1", t.TempDir())
	k.pane("w1:p1", "claude", "")
	k.reads(t, "w1:p1", "claude-s38-0-base.txt", "claude-visible-empty.txt")
	k.must(t, Baseline, "w1:p1", "", GapNone)
	k.must(t, Change, "w1:p1", "idle", GapNone)
	if excerpts, gaps := k.rows(t).text(); len(excerpts) != 0 || !slices.Equal(gaps, []string{gapLost}) {
		t.Errorf("excerpts %q, gaps %v, want only a %q gap", excerpts, gaps, gapLost)
	}
}

// Recorded reads of a pane that a herdr client resized (bugs #199, #203). A rewrap to a
// narrower pane and back gives no observation, and a screen too small for the old text is a
// gap. A screen that grows again shows the old text with no count. A new turn after a resize
// or at the same width keeps only the new turn.
func TestCaptureS38Resize(t *testing.T) {
	for _, tc := range []struct {
		harness, prefix string
		resize          []string // reads after the base: rewraps, a lost screen, a screen that grows
		gaps            []string
		turn            []string // reads before a read with a new turn, the last name
		prompt, reply   string
	}{
		// The second small read is a read of the same screen: the screen that grows is still
		// the screen before the small one.
		{harness: "claude", prefix: "claude-s38-resize-", resize: []string{"1-narrow", "2-wide", "3-small", "3-small", "4-wide"},
			gaps: []string{gapLost}, turn: []string{"0-base", "1-narrow", "5-narrow-ready"},
			prompt: "❯ Reply with only the word ready.", reply: "⏺ ready"},
		{harness: "codex", prefix: "codex-s38-", resize: []string{"1-narrow", "2-wide"},
			turn:   []string{"0-base", "1-narrow", "2-wide", "3-narrow-ready"},
			prompt: "› Reply with only the word ready.", reply: "• ready"},
	} {
		t.Run(tc.harness, func(t *testing.T) {
			k := newCapture(t, "s1", t.TempDir())
			k.pane("w1:p1", tc.harness, "")
			k.reads(t, "w1:p1", tc.prefix+"0-base.txt")
			for _, f := range tc.resize {
				k.reads(t, "w1:p1", tc.prefix+f+".txt")
			}
			k.must(t, Baseline, "w1:p1", "", GapNone)
			for range tc.resize {
				k.must(t, Change, "w1:p1", "idle", GapNone)
			}
			r := k.rows(t)
			if _, gaps := r.text(); !slices.Equal(gaps, tc.gaps) {
				t.Errorf("after the resizes: gaps %v, want %v", gaps, tc.gaps)
			}
			for _, o := range r.obs {
				if o.Counts {
					t.Errorf("after the resizes: observation %q counts, want no count", o.Excerpt)
				}
			}

			next := tc.turn[len(tc.turn)-1]
			for _, from := range tc.turn[:len(tc.turn)-1] {
				k := newCapture(t, "s1", t.TempDir())
				k.pane("w1:p1", tc.harness, "")
				k.reads(t, "w1:p1", tc.prefix+from+".txt", tc.prefix+next+".txt")
				k.must(t, Baseline, "w1:p1", "", GapNone)
				k.must(t, Change, "w1:p1", "idle", GapNone)
				if excerpts, _ := k.rows(t).text(); !slices.Equal(excerpts, []string{tc.prompt + "\n" + tc.reply}) {
					t.Errorf("%s, then %s: excerpts = %q, want only the prompt and the reply", from, next, excerpts)
				}
			}
		})
	}
}

// newPart keeps new lines that only look like a rewrap: a line that is only the start of an
// old line, and a new turn after the last old line that repeats a rewrapped old prompt.
func TestNewPartRewrap(t *testing.T) {
	for _, tc := range []struct {
		name      string
		old, read []string
		want      []int
	}{
		{"line start", []string{"• Done.", "› x"}, []string{"• Done.", "› x", "• Done"}, []int{2}},
		{"codex repeat", []string{"› alpha beta", "• r1"},
			[]string{"› alpha", "  beta", "• r1", "› alpha", "  beta", "• r2"}, []int{3, 4, 5}},
		{"claude repeat", []string{"❯ alpha beta", "⏺ r1"},
			[]string{"❯ alpha", "  beta", "⏺ r1", "❯ alpha", "  beta", "⏺ r2"}, []int{3, 4, 5}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if idx, _ := newPart(tc.old, nil, read{lines: tc.read}); !slices.Equal(idx, tc.want) {
				t.Errorf("newPart = %v, want %v", idx, tc.want)
			}
		})
	}
}

// The same text in two real turns gives two occurrences.
func TestCaptureS40(t *testing.T) {
	for _, tc := range []struct{ harness, session, prefix, prompt, reply, status string }{
		{"codex", codexRun2, "codex-s40", "› Reply with only the word ready.", "• ready", "done"},
		{"claude", "", "claude-s40", "❯ Reply with only the word ready.", "⏺ ready", "idle"},
	} {
		t.Run(tc.harness, func(t *testing.T) {
			k := newCapture(t, "s1", t.TempDir())
			k.pane("w1:p1", tc.harness, tc.session)
			k.reads(t, "w1:p1", tc.prefix+"-0-base.txt", tc.prefix+"-1-"+tc.status+".txt", tc.prefix+"-2-"+tc.status+".txt")
			k.must(t, Baseline, "w1:p1", "", GapNone)
			k.must(t, Change, "w1:p1", tc.status, GapNone)
			k.must(t, Change, "w1:p1", tc.status, GapNone)
			excerpts, _ := k.rows(t).text()
			if len(excerpts) != 2 {
				t.Fatalf("excerpts = %q, want 2", excerpts)
			}
			if !strings.Contains(excerpts[0], tc.reply) || strings.Count(excerpts[1], tc.prompt) != 1 ||
				strings.Count(excerpts[1], tc.reply) != 1 {
				t.Errorf("excerpts = %q, want the reply, then the prompt and the reply once", excerpts)
			}
		})
	}
}

// A pane read writes a prompt index row for each stored prompt part, in the transaction of its
// observation: the hash of the prompt without its sender mark, the same hash as the hook prompt
// of the same text. A reply gets no row (E14-S1).
func TestCapturePromptIndex(t *testing.T) {
	for _, tc := range []struct{ harness, session, prefix, status string }{
		{"codex", codexRun2, "codex-s40", "done"},
		{"claude", "", "claude-s40", "idle"},
	} {
		t.Run(tc.harness, func(t *testing.T) {
			k := newCapture(t, "s1", t.TempDir())
			k.pane("w1:p1", tc.harness, tc.session)
			k.reads(t, "w1:p1", tc.prefix+"-0-base.txt", tc.prefix+"-1-"+tc.status+".txt", tc.prefix+"-2-"+tc.status+".txt")
			k.must(t, Baseline, "w1:p1", "", GapNone)
			k.must(t, Change, "w1:p1", tc.status, GapNone)
			k.must(t, Change, "w1:p1", tc.status, GapNone)
			const prompt = "Reply with only the word ready."
			var es []store.PromptEntry
			k.tx(t, func(tx *sql.Tx) (err error) {
				es, err = store.PromptEntries(context.Background(), tx)
				return err
			})
			senders := map[int64]store.Sender{}
			for _, o := range k.rows(t).obs {
				senders[o.ID] = o.Sender
			}
			found := false
			for _, e := range es {
				if senders[e.ObservationID] != store.SenderUnconfirmed {
					t.Errorf("row %+v is of a %s observation, want prompts only", e, senders[e.ObservationID])
				}
				if e.Hash == fingerprint.Prompt(prompt) && e.Text == prompt {
					found = true
				}
			}
			if !found {
				t.Errorf("prompt index = %+v, want a row of %q without its mark", es, prompt)
			}
			if h := fingerprint.Prompt(HookPrompt(tc.harness, prompt)); h != fingerprint.Prompt(prompt) {
				t.Errorf("hook prompt hash = %s, want the pane hash", h)
			}
		})
	}
}

// newPart keeps a repeat above the old text when the last old line stays, and drops
// text that a closed dialog hid.
func TestNewPartRepeatAbove(t *testing.T) {
	for _, tc := range []struct {
		name            string
		back, old, read []string
		want            []string
	}{
		{"repeat", []string{"P", "A"}, []string{"A"}, []string{"P", "A"}, []string{"P"}},
		{"dialog", []string{"36", "37", "W"}, []string{"37", "W", "D"}, []string{"36", "37", "W", "N"}, []string{"N"}},
	} {
		idx, _ := newPart(tc.old, tc.back, read{lines: tc.read})
		var got []string
		for _, i := range idx {
			got = append(got, tc.read[i])
		}
		if !slices.Equal(got, tc.want) {
			t.Errorf("%s: newPart = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// Before the first prompt of a Codex run, herdr gives no agent session id: the session gets a
// capture id, stored before any evidence. The id that comes later keeps that key. After an
// end, the id gives a new session: the two are not merged.
func TestCaptureNativeIDLater(t *testing.T) {
	k := newCapture(t, "s1", t.TempDir())
	k.pane("w1:p1", "codex", "")
	k.reads(t, "w1:p1", "codex-s31-0-base.txt", "codex-s31-1-working.txt", "codex-s31-2-blocked.txt")
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
	k.reads(t, "w1:p1", "codex-s31-0-base.txt", "codex-s31-2-blocked.txt", "codex-s31-3-working.txt")
	k.must(t, Baseline, "w1:p1", "", GapNone)
	k.must(t, Baseline, "w1:p1", "", GapConnect)
	k.must(t, Change, "w1:p1", "working", GapNone)
	r := k.rows(t)
	if len(r.sessions) != 1 || len(r.segments) != 2 || r.segments[0].Ended.IsZero() {
		t.Fatalf("rows = %+v, want one session with two segments", r)
	}
	excerpts, gaps := r.text()
	if !slices.Equal(gaps, []string{string(GapConnect)}) || len(excerpts) != 1 || strings.Contains(excerpts[0], "Would you like") {
		t.Errorf("observations = %+v, want the gap, then only text after the new baseline", r.obs)
	}
	for _, o := range r.obs {
		if o.SegmentID != r.segments[1].ID {
			t.Errorf("observation %+v is not in the new segment", o)
		}
	}
}

// A learning switch after the baseline drops the read: it is the new baseline, with a gap.
func TestCaptureLearningSwitch(t *testing.T) {
	k := newCapture(t, "s1", t.TempDir())
	k.pane("w1:p1", "codex", codexRun2)
	k.reads(t, "w1:p1", "codex-s31-0-base.txt", "codex-s31-1-working.txt", "codex-s31-2-blocked.txt")
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
		if strings.Contains(e, "› Run touch made.txt") {
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
		k.reads(t, "w1:p1", "codex-s31-0-base.txt", "codex-s31-1-working.txt")
		k.reads(t, "w1:p2", "claude-s38-0-base.txt", "claude-s38-1-idle.txt")
		for _, id := range []string{"w1:p1", "w1:p2"} {
			k.must(t, Baseline, id, "", GapNone)
			k.must(t, Change, id, "working", GapNone)
		}
	}
	r := a.rows(t)
	if excerpts, _ := r.text(); len(r.sessions) != 4 || len(excerpts) != 4 {
		t.Fatalf("rows = %+v, want 4 sessions and 4 read observations", r)
	}
	// Enrich: the claude agent of s1 gets an id; end: the codex agent of s1 ends.
	a.pane("w1:p2", "claude", "6a345ec1-3050-4e61-b736-d0e9d2edab82")
	a.must(t, Change, "w1:p2", "idle", GapNone)
	a.must(t, End, "w1:p1", "", GapNone)
	r = a.rows(t)
	var forget int64
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
			forget = s.ID
		}
	}
	a.tx(t, func(tx *sql.Tx) error { return store.ForgetSession(context.Background(), tx, forget) })
	r = a.rows(t)
	var s2 int
	for _, s := range r.sessions {
		if s.Key.HerdrSession == "s2" && s.Generation == 0 {
			s2++
		}
	}
	// Forget keeps the session row: it holds no text (E11-S6).
	if excerpts, _ := r.text(); len(r.sessions) != 4 || s2 != 2 || len(excerpts) != 3 {
		t.Errorf("after forget: %d sessions (%d of s2 intact), %d read observations, want 4 (2 of s2) and 3", len(r.sessions), s2, len(excerpts))
	}
}

// The excerpt filter drops secrets and cuts to 500 characters.
func TestFilter(t *testing.T) {
	if got := redact("", []string{"key AKIAIOSFODNN7EXAMPLE here"}); got[0] != "key [secret] here" {
		t.Errorf("redact = %q, want the key dropped", got)
	}
	got, cut := filter(strings.Repeat("é", 600))
	if len([]rune(got)) != excerptLimit || !cut {
		t.Errorf("filter of 600 characters = %d characters, cut %v, want %d, true", len([]rune(got)), cut, excerptLimit)
	}
}

// forgetOnRead makes the next pane.read of k run brain forget --session on the session with
// id agentID, as a forget that commits while the read waits for its write.
func (k *capHarness) forgetOnRead(t *testing.T, agentID string, n int) {
	t.Helper()
	k.f.mu.Lock()
	defer k.f.mu.Unlock()
	k.f.onRead = func(f *fakeHerdr) {
		if n--; n == 0 {
			f.onRead = nil
		}
		k.tx(t, func(tx *sql.Tx) error {
			ctx := context.Background()
			ss, err := store.SessionsByID(ctx, tx, k.gate.Binding.Session, agentID)
			if err != nil || len(ss) != 1 {
				t.Errorf("SessionsByID(%s) = %+v, %v, want one session", agentID, ss, err)
				return err
			}
			return store.ForgetSession(ctx, tx, ss[0].ID)
		})
	}
}

// AC (E11-S6): a forget while a pane read waits before its commit, then the release: the read
// stores nothing, the forgotten text stays absent, and the read after the new baseline is
// stored. After a restart the screen that still shows the forgotten text is only a baseline:
// the replay stores nothing, and new text after it is stored.
func TestCaptureForgetDuringRead(t *testing.T) {
	dir := t.TempDir()
	k := newCapture(t, "s1", dir)
	k.pane("w1:p1", "codex", codexRun2)
	k.reads(t, "w1:p1", "codex-s31-0-base.txt", "codex-s31-1-working.txt", "codex-s31-2-blocked.txt",
		"codex-s31-3-working.txt", "codex-s31-4-done.txt")
	k.must(t, Baseline, "w1:p1", "", GapStart)
	k.must(t, Change, "w1:p1", "working", GapNone)
	if excerpts, _ := k.rows(t).text(); len(excerpts) != 1 || !strings.Contains(excerpts[0], "Run touch made.txt") {
		t.Fatalf("excerpts before forget = %q, want the prompt", excerpts)
	}
	k.forgetOnRead(t, codexRun2, 1)
	k.must(t, Change, "w1:p1", "blocked", GapNone) // the read of the blocked screen waits for the forget
	r := k.rows(t)
	if excerpts, gaps := r.text(); len(excerpts) != 0 || !slices.Equal(gaps, []string{gapForget}) {
		t.Fatalf("after the forget: excerpts %q, gaps %q, want only a %q gap", excerpts, gaps, gapForget)
	}
	k.must(t, Change, "w1:p1", "done", GapNone)
	excerpts, _ := k.rows(t).text()
	if len(excerpts) != 1 || !strings.Contains(excerpts[0], "with escalated permissions successfully") {
		t.Fatalf("excerpts after the new baseline = %q, want the new text", excerpts)
	}
	forgotten := []string{"Run touch made.txt and request", "Would you like to run", "You approved codex"}
	noText := func(k *capHarness, when string) {
		for _, o := range k.rows(t).obs {
			for _, f := range forgotten {
				if strings.Contains(o.Excerpt, f) {
					t.Errorf("%s: observation %+v holds forgotten text %q", when, o, f)
				}
			}
		}
	}
	noText(k, "after the forget")

	// Restart: a new brained reads the same screen again.
	k2 := newCapture(t, "s1", dir)
	k2.pane("w1:p1", "codex", codexRun2)
	k2.reads(t, "w1:p1", "codex-s31-3-working.txt", "codex-s31-3-working.txt")
	k2.must(t, Baseline, "w1:p1", "", GapStart)
	k2.must(t, Change, "w1:p1", "idle", GapNone)
	if excerpts, _ := k2.rows(t).text(); len(excerpts) != 1 {
		t.Fatalf("excerpts after the replay = %q, want only the one of before", excerpts)
	}
	noText(k2, "after the replay")
	k2.f.mu.Lock()
	k2.f.reads["w1:p1"] = []string{fixture(t, "codex-s31-3-working.txt") + "\n• New text after the forget.\n"}
	k2.f.mu.Unlock()
	k2.must(t, Change, "w1:p1", "working", GapNone)
	if excerpts, _ := k2.rows(t).text(); len(excerpts) != 2 || !strings.Contains(excerpts[1], "New text after the forget") {
		t.Errorf("excerpts after new text = %q, want the new text stored", excerpts)
	}
}

// A forget while a baseline read waits, twice: the read that shows the forgotten text is no
// base, and a read after the forgets is. A later change stores only the text that follows it
// (E11-S6, review finding 2).
func TestCaptureForgetDuringBaseline(t *testing.T) {
	k := newCapture(t, "s1", t.TempDir())
	k.pane("w1:p1", "codex", codexRun2)
	k.reads(t, "w1:p1", "codex-s31-0-base.txt", "codex-s31-1-working.txt", "codex-s31-1-working.txt",
		"codex-s31-2-blocked.txt", "codex-s31-3-working.txt", "codex-s31-4-done.txt")
	k.must(t, Baseline, "w1:p1", "", GapStart)
	k.forgetOnRead(t, codexRun2, 2)
	k.must(t, Baseline, "w1:p1", "", GapConnect) // the first two reads wait for a forget each
	k.must(t, Change, "w1:p1", "working", GapNone)
	k.must(t, Change, "w1:p1", "done", GapNone)
	excerpts, gaps := k.rows(t).text()
	for _, o := range excerpts {
		for _, f := range []string{"Run touch made.txt and request", "Would you like to run"} {
			if strings.Contains(o, f) {
				t.Errorf("excerpt %q holds text %q that the screen showed before the forget", o, f)
			}
		}
	}
	if len(excerpts) == 0 || !strings.Contains(excerpts[len(excerpts)-1], "with escalated permissions successfully") {
		t.Errorf("excerpts = %q, want the new text last", excerpts)
	}
	if !slices.Equal(gaps, []string{string(GapConnect)}) {
		t.Errorf("gaps = %q, want only the reconnect", gaps)
	}
}

// A forget while a change read waits, when a learning switch makes that read a new base: the
// read is no base, and the text that the screen showed at the forget is not stored (E11-S6).
func TestCaptureForgetDuringLearningSwitch(t *testing.T) {
	k := newCapture(t, "s1", t.TempDir())
	k.pane("w1:p1", "codex", codexRun2)
	k.reads(t, "w1:p1", "codex-s31-0-base.txt", "codex-s31-1-working.txt", "codex-s31-2-blocked.txt",
		"codex-s31-3-working.txt", "codex-s31-4-done.txt")
	k.must(t, Baseline, "w1:p1", "", GapNone)
	k.must(t, Change, "w1:p1", "working", GapNone)
	base := k.learning(t, "w1:p1", "w1")
	for _, on := range []bool{false, true} {
		k.tx(t, func(tx *sql.Tx) error {
			return store.SetLearning(context.Background(), tx, store.LearningSetting{Scope: store.LearningPane,
				HerdrSession: "s1", Target: "w1:p1", On: on, Changed: time.Now()})
		})
	}
	k.forgetOnRead(t, codexRun2, 1)
	s := Step{Kind: Change, PaneID: "w1:p1", WorkspaceID: "w1", Gate: k.gate, Learning: base,
		Event: &herdr.Event{Status: "blocked"}}
	if err := k.c.Handle(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	k.must(t, Change, "w1:p1", "working", GapNone)
	excerpts, _ := k.rows(t).text()
	for _, e := range excerpts {
		for _, f := range []string{"Run touch made.txt and request", "Would you like to run", "You approved codex"} {
			if strings.Contains(e, f) {
				t.Errorf("excerpt %q holds text %q that the screen showed at the forget", e, f)
			}
		}
	}
}

// #258: a Claude 2.1.292 task with the start banner, a tip that wraps and the diff panel
// beside the conversation (made reads, testdata/claude-i258-*). The observations hold the
// prompt and the reply with their senders, and no banner, tip, mode line or panel text.
func TestCaptureScreenChrome(t *testing.T) {
	k := newCapture(t, "s1", t.TempDir())
	k.pane("w1:p1", "claude", "")
	k.reads(t, "w1:p1", "claude-i258-0-base.txt", "claude-i258-1-working.txt", "claude-i258-2-idle.txt")
	k.must(t, Baseline, "w1:p1", "", GapNone)
	k.must(t, Change, "w1:p1", "working", GapNone)
	k.must(t, Change, "w1:p1", "idle", GapNone)
	type obs struct {
		sender  store.Sender
		excerpt string
	}
	var got []obs
	for _, o := range k.rows(t).obs {
		got = append(got, obs{o.Sender, o.Excerpt})
	}
	want := []obs{
		{store.SenderUnconfirmed, "❯ Add multiply to src/math.js with a test."},
		{store.SenderAssistant, "⏺ Update(src/math.js)"},
		{store.SenderTool, "  ⎿  Added 4 lines"},
		{store.SenderAssistant, "⏺ Added multiply and its test."},
	}
	if !slices.Equal(got, want) {
		t.Errorf("observations = %q, want %q", got, want)
	}
}
