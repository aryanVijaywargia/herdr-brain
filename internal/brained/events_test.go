package brained

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aryanVijaywargia/herdr-brain/internal/bindings"
	"github.com/aryanVijaywargia/herdr-brain/internal/herdr"
	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

// E20-S10 #275: herdr loses, repeats and reorders events, and panes split, move and close
// during a session. A live brained runs the real Capture against the fake herdr, so each step
// reaches the store. In each test the captured text stays right, or a gap observation
// records the loss: brain status prints the gap observations as "capture gaps"
// (store.DeliveryGaps).

// tap does each step with the Capture, and then passes it to the test.
type tap struct {
	c     *Capture
	steps chan Step
}

func (t tap) Handle(ctx context.Context, s Step) error {
	err := t.c.Handle(ctx, s)
	t.steps <- s
	return err
}

// live is a brained with a Capture, bound to a main store.
type live struct {
	*harness
	main *fakeSub
	subs map[string]*fakeSub // the status stream of each pane
}

// newLive starts brained with the live codex panes of runs, by pane id, and the screen of each,
// and waits for their start baselines.
func newLive(t *testing.T, runs map[string]string) *live {
	t.Helper()
	h := newHarness(t, bindings.KindMain)
	c := NewCapture(herdr.Socket{Path: h.f.path}, nil)
	var mu sync.Mutex
	clock := time.Date(2026, 10, 8, 10, 0, 0, 0, time.UTC)
	c.now = func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		clock = clock.Add(time.Second)
		return clock
	}
	h.d.handler = tap{c, h.steps}
	l := &live{harness: h}
	for id, run := range runs {
		l.pane(id, run, "• start of "+id)
	}
	l.main = h.start(t)
	l.subs = h.f.statusSubs(t, len(runs))
	for range runs {
		if s := h.step(t); s.Kind != Baseline || s.Gap != GapStart {
			t.Fatalf("step %+v, want a start baseline", s)
		}
	}
	t.Cleanup(func() { onlyReads(t, h.f) })
	return l
}

// pane adds live codex pane id of agent session run, with screen lines.
func (l *live) pane(id, run string, lines ...string) {
	l.f.addPane(id, "codex")
	l.f.mu.Lock()
	l.f.panes[id].session = run
	l.f.mu.Unlock()
	l.screen(id, lines...)
}

// screen sets the screen of pane id; each read gets it until the next call.
func (l *live) screen(id string, lines ...string) {
	l.f.mu.Lock()
	defer l.f.mu.Unlock()
	l.f.reads[id] = []string{strings.Join(lines, "\n") + "\n"}
}

// status sends a status change of pane id on its stream, and waits for its step.
func (l *live) status(t *testing.T, id, status string) {
	t.Helper()
	l.subs[id].send(statusLine(id, status))
	l.step(t)
}

// created sends pane_created of pane id and waits for its status stream.
func (l *live) created(t *testing.T, id string) {
	t.Helper()
	ws, _, _ := strings.Cut(id, ":")
	l.main.send(fmt.Sprintf(`{"event":"pane_created","data":{"type":"pane_created","pane":{"pane_id":%q,"workspace_id":%q,"tab_id":"%s:t1"}}}`,
		id, ws, ws))
	st := l.f.statusSubs(t, 1)[id]
	if st == nil {
		t.Fatalf("no status subscription for %s", id)
	}
	l.subs[id] = st
}

// rows returns what the store holds.
func (l *live) rows(t *testing.T) rows {
	t.Helper()
	ctx := context.Background()
	var r rows
	l.tx(t, func(tx *sql.Tx) (err error) {
		if r.sessions, err = store.Sessions(ctx, tx); err != nil {
			return err
		}
		if r.segments, err = store.Segments(ctx, tx); err != nil {
			return err
		}
		r.obs, err = store.Observations(ctx, tx)
		return err
	})
	wantSenders(t, r)
	return r
}

// wantSenders checks the attribution of each text observation (§6.5): a prompt line has the
// sender not confirmed, and an agent line is assistant text.
func wantSenders(t *testing.T, r rows) {
	t.Helper()
	for _, o := range r.obs {
		want := store.SenderAssistant
		if strings.HasPrefix(o.Excerpt, "›") {
			want = store.SenderUnconfirmed
		}
		if o.Status != store.ObservationGap && o.Sender != want {
			t.Errorf("observation %q has sender %q, want %q", o.Excerpt, o.Sender, want)
		}
	}
}

// holdFirst blocks the first change until release, and closes entered when it blocks.
type holdFirst struct {
	entered, release chan struct{}
	next             Handler
	done             bool
}

func (b *holdFirst) Handle(ctx context.Context, s Step) error {
	if s.Kind == Change && !b.done {
		b.done = true
		close(b.entered)
		<-b.release
	}
	return b.next.Handle(ctx, s)
}

func (l *live) tx(t *testing.T, fn func(tx *sql.Tx) error) {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, l.store, busyTimeout)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close() //nolint:errcheck // test
	if err := st.Write(ctx, fn); err != nil {
		t.Fatal(err)
	}
}

// statusGaps returns the capture gaps that brain status prints for codex.
func (l *live) statusGaps(t *testing.T) int {
	t.Helper()
	var d store.DeliveryGap
	l.tx(t, func(tx *sql.Tx) (err error) {
		d, err = store.DeliveryGaps(context.Background(), tx, "s1", "codex")
		return err
	})
	return d.Gaps
}

// texts returns the excerpts of the text observations of pane id's segments, one line each,
// and the events of their gaps.
func (r rows) texts(id string) (lines, gaps []string) {
	segs := map[int64]bool{}
	for _, g := range r.segments {
		segs[g.ID] = g.PaneID == id
	}
	for _, o := range r.obs {
		switch {
		case !segs[o.SegmentID]:
		case o.Status == store.ObservationGap:
			gaps = append(gaps, o.Event)
		default:
			lines = append(lines, strings.Split(o.Excerpt, "\n")...)
		}
	}
	return lines, gaps
}

// wantLines checks that got holds each of want once, in order, and no other line.
func wantLines(t *testing.T, got []string, want ...string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Errorf("captured lines = %q, want %q", got, want)
	}
}

func wantGaps(t *testing.T, got []string, want ...string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Errorf("gaps = %q, want %q", got, want)
	}
}

// AC 1: herdr loses the status events between two reads. The next read keeps all the new
// text, once: a status change is only a chance to read (§6.2).
func TestEventsLost(t *testing.T) {
	l := newLive(t, map[string]string{"w1:p1": codexRun1})
	l.screen("w1:p1", "• start of w1:p1", "• one")
	l.status(t, "w1:p1", "working")
	// The idle and working events of "two" and "three" are lost.
	l.screen("w1:p1", "• start of w1:p1", "• one", "• two", "• three", "• four")
	l.status(t, "w1:p1", "idle")
	lines, gaps := l.rows(t).texts("w1:p1")
	wantLines(t, lines, "• one", "• two", "• three", "• four")
	wantGaps(t, gaps, string(GapStart))
	if n := l.statusGaps(t); n != 1 {
		t.Errorf("status capture gaps = %d, want 1 (the start)", n)
	}
}

// AC 1, S39: brained falls behind and drops events. It records a gap, takes a new baseline
// and keeps the text after it. No line is stored twice, and status counts the gap.
func TestEventsDropped(t *testing.T) {
	h := newHarness(t, bindings.KindMain)
	c := NewCapture(herdr.Socket{Path: h.f.path}, nil)
	entered, release := make(chan struct{}), make(chan struct{})
	h.d.handler = &holdFirst{entered: entered, release: release, next: tap{c, h.steps}}
	h.d.queue = 1
	dropped := make(chan struct{})
	var once sync.Once
	h.d.onBehind = func() { once.Do(func() { close(dropped) }) }
	l := &live{harness: h}
	l.pane("w1:p1", codexRun1, "• base")
	l.main = h.start(t)
	l.subs = h.f.statusSubs(t, 1)
	wantStep(t, h.step(t), Baseline, "w1:p1", GapStart, "")

	l.screen("w1:p1", "• base", "• one")
	l.subs["w1:p1"].send(statusLine("w1:p1", "working")) // the handler blocks on this change
	select {
	case <-entered:
	case <-time.After(wait):
		t.Fatal("the handler got no change")
	}
	for range 3 {
		l.subs["w1:p1"].send(statusLine("w1:p1", "idle"))
	}
	select {
	case <-dropped:
	case <-time.After(wait):
		t.Fatal("no event was dropped")
	}
	l.screen("w1:p1", "• base", "• one", "• two")
	close(release)
	for s := h.step(t); s.Kind != Baseline; s = h.step(t) {
		wantStep(t, s, Change, "w1:p1", "", "") // a change that was in the queue before the drop
	}
	l.screen("w1:p1", "• base", "• one", "• two", "• three")
	l.status(t, "w1:p1", "working")

	lines, gaps := l.rows(t).texts("w1:p1")
	wantLines(t, lines, "• one", "• two", "• three")
	wantGaps(t, gaps, string(GapStart), string(GapBehind))
	if n := l.statusGaps(t); n != 2 {
		t.Errorf("status capture gaps = %d, want 2", n)
	}
}

// AC 1: herdr repeats events. A repeated status change, pane_created, agent_detected or
// pane_closed adds no text and loses none.
func TestEventsRepeated(t *testing.T) {
	l := newLive(t, map[string]string{"w1:p1": codexRun1})
	l.screen("w1:p1", "• start of w1:p1", "• one")
	l.status(t, "w1:p1", "working")
	l.status(t, "w1:p1", "working")

	// A repeated pane_created is no new pane, and a repeated agent_detected keeps the text that
	// came since the last read.
	l.screen("w1:p1", "• start of w1:p1", "• one", "• two")
	ws := `{"event":"pane_created","data":{"type":"pane_created","pane":{"pane_id":"w1:p1","workspace_id":"w1","tab_id":"w1:t1"}}}`
	l.main.send(ws)
	l.main.send(paneLine("pane_agent_detected", "w1:p1"))
	l.step(t)
	l.screen("w1:p1", "• start of w1:p1", "• one", "• two", "• three")
	l.status(t, "w1:p1", "idle")

	l.main.send(paneLine("pane_closed", "w1:p1"))
	wantStep(t, l.step(t), End, "w1:p1", "", ReasonGone)
	l.main.send(paneLine("pane_closed", "w1:p1"))
	// The next event of the main stream comes after the repeated close.
	l.pane("w1:p2", codexRun2, "• base of w1:p2")
	l.created(t, "w1:p2")
	l.main.send(paneLine("pane_agent_detected", "w1:p2"))
	wantStep(t, l.step(t), Baseline, "w1:p2", GapNone, "")

	r := l.rows(t)
	lines, gaps := r.texts("w1:p1")
	wantLines(t, lines, "• one", "• two", "• three")
	wantGaps(t, gaps, string(GapStart))
	if len(r.sessions) != 2 || r.sessions[0].Ended.IsZero() || !r.sessions[1].Ended.IsZero() {
		t.Errorf("sessions = %+v, want run 1 ended and run 2 open", r.sessions)
	}
}

// AC 1: herdr gives events in another order. The reads come at the handling time, so the text
// stays right. An agent_detected before its pane_created is lost for brained: the first
// change of the pane records a gap, and status counts it.
func TestEventsReordered(t *testing.T) {
	l := newLive(t, map[string]string{"w1:p1": codexRun1})
	l.screen("w1:p1", "• start of w1:p1", "• one", "• two")
	l.status(t, "w1:p1", "idle") // the idle of "two" comes before the working of "one"
	l.status(t, "w1:p1", "working")
	lines, _ := l.rows(t).texts("w1:p1")
	wantLines(t, lines, "• one", "• two")

	l.pane("w1:p2", codexRun2, "• base of w1:p2")
	l.main.send(paneLine("pane_agent_detected", "w1:p2"))
	l.created(t, "w1:p2")
	l.screen("w1:p2", "• base of w1:p2", "• one of w1:p2")
	l.status(t, "w1:p2", "working")
	l.screen("w1:p2", "• base of w1:p2", "• one of w1:p2", "• two of w1:p2")
	l.status(t, "w1:p2", "idle")

	r := l.rows(t)
	lines, gaps := r.texts("w1:p2")
	wantLines(t, lines, "• two of w1:p2")
	wantGaps(t, gaps, gapNoBase)
	if n := l.statusGaps(t); n != 2 {
		t.Errorf("status capture gaps = %d, want 2 (the start of w1:p1 and no base of w1:p2)", n)
	}
	for _, s := range r.sessions {
		if s.PaneID == "w1:p2" && s.Key.ID != codexRun2 {
			t.Errorf("session of w1:p2 = %+v, want run 2", s)
		}
	}
}

// AC 2: a pane splits during a session. The new pane gets its own session, and no text goes
// across the two panes.
func TestPaneSplit(t *testing.T) {
	l := newLive(t, map[string]string{"w1:p1": codexRun1})
	l.pane("w1:p2", codexRun2, "• base of w1:p2")
	l.created(t, "w1:p2")
	l.main.send(paneLine("pane_agent_detected", "w1:p2"))
	wantStep(t, l.step(t), Baseline, "w1:p2", GapNone, "")
	l.screen("w1:p1", "• start of w1:p1", "• left")
	l.screen("w1:p2", "• base of w1:p2", "• right")
	l.status(t, "w1:p2", "working")
	l.status(t, "w1:p1", "working")

	r := l.rows(t)
	left, gaps := r.texts("w1:p1")
	wantLines(t, left, "• left")
	wantGaps(t, gaps, string(GapStart))
	right, gaps := r.texts("w1:p2")
	wantLines(t, right, "• right")
	wantGaps(t, gaps)
	sessions := map[string]string{}
	for _, s := range r.sessions {
		sessions[s.PaneID] = s.Key.ID
	}
	if sessions["w1:p1"] != codexRun1 || sessions["w1:p2"] != codexRun2 {
		t.Errorf("sessions by pane = %v, want run 1 in w1:p1 and run 2 in w1:p2", sessions)
	}
}

// AC 2, S36: a pane moves to another workspace during a session, and gets a new id. The read
// of the old id records a gap in the old segment. The session goes on in a new segment of the
// new id, and new text never goes to the old workspace. The close of the old id comes before
// or after the new id.
func TestPaneMove(t *testing.T) {
	for _, closeFirst := range []bool{false, true} {
		t.Run(fmt.Sprintf("close first %v", closeFirst), func(t *testing.T) {
			l := newLive(t, map[string]string{"w1:p1": codexRun1})
			l.screen("w1:p1", "• start of w1:p1", "• one")
			l.status(t, "w1:p1", "working")

			l.f.mu.Lock()
			l.f.panes["w2:p1"] = l.f.panes["w1:p1"]
			l.f.panes["w2:p1"].ws = "w2"
			delete(l.f.panes, "w1:p1")
			l.f.moved["w1:p1"] = "w2:p1"
			l.f.mu.Unlock()
			l.screen("w2:p1", "• start of w1:p1", "• one", "• two")
			l.status(t, "w1:p1", "working") // the old id has no pane to read: a gap

			if closeFirst {
				l.main.send(paneLine("pane_closed", "w1:p1"))
				wantStep(t, l.step(t), End, "w1:p1", "", ReasonGone)
			}
			l.created(t, "w2:p1")
			l.main.send(paneLine("pane_agent_detected", "w2:p1"))
			wantStep(t, l.step(t), Baseline, "w2:p1", GapNone, "")
			if !closeFirst {
				l.main.send(paneLine("pane_closed", "w1:p1"))
				wantStep(t, l.step(t), End, "w1:p1", "", ReasonGone)
			}
			l.screen("w2:p1", "• start of w1:p1", "• one", "• two", "• three")
			l.status(t, "w2:p1", "idle")

			r := l.rows(t)
			old, gaps := r.texts("w1:p1")
			wantLines(t, old, "• one")
			wantGaps(t, gaps, string(GapStart), gapGone)
			moved, gaps := r.texts("w2:p1")
			wantLines(t, moved, "• three")
			wantGaps(t, gaps)
			if len(r.sessions) != 1 || !r.sessions[0].Ended.IsZero() || r.sessions[0].PaneID != "w2:p1" {
				t.Errorf("sessions = %+v, want run 1 open in w2:p1", r.sessions)
			}
			if len(r.segments) != 2 || r.segments[0].Ended.IsZero() || !r.segments[1].Ended.IsZero() {
				t.Errorf("segments = %+v, want w1:p1 ended and w2:p1 open", r.segments)
			}
			if n := l.statusGaps(t); n != 2 {
				t.Errorf("status capture gaps = %d, want 2 (the start and the move)", n)
			}
		})
	}
}

// AC 2, §6.2 step 9: a pane closes during a session. Its session ends, also when herdr loses
// pane_closed and ends the pane's status stream: the reconcile finds the pane gone.
func TestPaneClose(t *testing.T) {
	for _, lost := range []bool{false, true} {
		t.Run(fmt.Sprintf("close lost %v", lost), func(t *testing.T) {
			l := newLive(t, map[string]string{"w1:p1": codexRun1, "w1:p2": codexRun2})
			l.screen("w1:p1", "• start of w1:p1", "• one")
			l.status(t, "w1:p1", "working")
			l.f.mu.Lock()
			delete(l.f.panes, "w1:p1")
			l.f.mu.Unlock()
			if lost {
				_ = l.subs["w1:p1"].conn.Close() // herdr ends the stream; a close error changes no step
			} else {
				l.main.send(paneLine("pane_closed", "w1:p1"))
			}
			for s := l.step(t); s.PaneID != "w1:p1"; s = l.step(t) {
				wantStep(t, s, Baseline, "w1:p2", GapBehind, "") // the reconcile of the lost close
			}

			r := l.rows(t)
			lines, gaps := r.texts("w1:p1")
			wantLines(t, lines, "• one")
			wantGaps(t, gaps, string(GapStart))
			for _, s := range r.sessions {
				if ended := !s.Ended.IsZero(); ended != (s.PaneID == "w1:p1") {
					t.Errorf("session %+v: ended %v, want only the session of w1:p1 ended", s, ended)
				}
			}
		})
	}
}

// AC 1: herdr loses the release of an agent, and a new agent in the same pane is detected.
// The old session ends, and the new one starts with a "session change" gap: no text of one
// run goes to the other.
func TestEventsLostRelease(t *testing.T) {
	l := newLive(t, map[string]string{"w1:p1": codexRun1})
	l.screen("w1:p1", "• start of w1:p1", "• one")
	l.status(t, "w1:p1", "working")
	l.f.mu.Lock()
	l.f.panes["w1:p1"].session = codexRun2
	l.f.mu.Unlock()
	l.screen("w1:p1", "• start of w1:p1", "• one", "• run 2 base")
	l.main.send(paneLine("pane_agent_detected", "w1:p1"))
	l.step(t)
	l.screen("w1:p1", "• start of w1:p1", "• one", "• run 2 base", "• run 2 text")
	l.status(t, "w1:p1", "working")

	r := l.rows(t)
	if len(r.sessions) != 2 || r.sessions[0].Key.ID != codexRun1 || r.sessions[0].Ended.IsZero() ||
		r.sessions[1].Key.ID != codexRun2 || !r.sessions[1].Ended.IsZero() {
		t.Fatalf("sessions = %+v, want run 1 ended and run 2 open", r.sessions)
	}
	bySession := map[int64]int64{}
	for _, g := range r.segments {
		bySession[g.ID] = g.SessionID
	}
	var run1, run2 []string
	for _, o := range r.obs {
		text := o.Excerpt
		if o.Status == store.ObservationGap {
			text = "gap " + o.Event
		}
		if bySession[o.SegmentID] == r.sessions[0].ID {
			run1 = append(run1, text)
		} else {
			run2 = append(run2, text)
		}
	}
	wantLines(t, run1, "gap "+string(GapStart), "• one")
	wantLines(t, run2, "gap "+gapSession, "• run 2 text")
}

// AC 1: herdr detects the agent again in a pane with no agent session id. Brain cannot tell a
// new run of the same harness, so the text since the last read is a recorded gap, and the
// text after it is kept.
func TestEventsRepeatedNoID(t *testing.T) {
	l := newLive(t, map[string]string{"w1:p1": ""})
	l.screen("w1:p1", "• start of w1:p1", "• one")
	l.status(t, "w1:p1", "working")
	l.screen("w1:p1", "• start of w1:p1", "• one", "• two")
	l.main.send(paneLine("pane_agent_detected", "w1:p1"))
	l.step(t)
	l.screen("w1:p1", "• start of w1:p1", "• one", "• two", "• three")
	l.status(t, "w1:p1", "idle")

	lines, gaps := l.rows(t).texts("w1:p1")
	wantLines(t, lines, "• one", "• three")
	wantGaps(t, gaps, string(GapStart), gapDetected)
	if n := l.statusGaps(t); n != 2 {
		t.Errorf("status capture gaps = %d, want 2", n)
	}
}

// AC 1, AC 3, §6.4: a repeated agent_detected starts no turn. A prompt that the screen still
// shows is one occurrence; the same prompt sent again in a new turn is a real repeat (S40).
func TestEventsRepeatedPrompt(t *testing.T) {
	l := newLive(t, map[string]string{"w1:p1": codexRun1})
	l.screen("w1:p1", "• start of w1:p1", "› fix the bug", "• fixing")
	l.status(t, "w1:p1", "working")
	l.screen("w1:p1", "• start of w1:p1", "› fix the bug", "• fixing", "• fixed")
	l.main.send(paneLine("pane_agent_detected", "w1:p1"))
	l.step(t)
	l.status(t, "w1:p1", "idle")
	l.screen("w1:p1", "• start of w1:p1", "› fix the bug", "• fixing", "• fixed", "› fix the bug", "• fixing again")
	l.status(t, "w1:p1", "working")

	r := l.rows(t)
	lines, gaps := r.texts("w1:p1")
	wantLines(t, lines, "› fix the bug", "• fixing", "• fixed", "› fix the bug", "• fixing again")
	wantGaps(t, gaps, string(GapStart))
	prompts := 0
	for _, o := range r.obs {
		if strings.HasPrefix(o.Excerpt, "› fix the bug") && o.Counts {
			prompts++
		}
	}
	if prompts != 2 {
		t.Errorf("counted prompts = %d, want 2 (one each turn)", prompts)
	}
}

// shellPane is the shell pane of the probe tests (#317).
const shellPane = "w1:p2"

// probeLine is a pane_agent_detected event of agent in shellPane, as herdr sends it; a release
// has the final status and released.
func probeLine(agent string, released bool) string {
	if released {
		return fmt.Sprintf(`{"data":{"agent":%q,"final_status":"idle","pane_id":"w1:p2","released":true,"workspace_id":"w1"},"event":"pane_agent_detected"}`, agent)
	}
	return fmt.Sprintf(`{"data":{"agent":%q,"pane_id":"w1:p2","type":"pane_agent_detected","workspace_id":"w1"},"event":"pane_agent_detected"}`, agent)
}

// job is a command line in shellPane: the agent that herdr detects there, its foreground
// processes as pane.process_info gives them, and the id of their process group, the pid of its
// leader (0: herdr gives none).
type job struct {
	agent, procs string
	group        int
}

// The foreground processes of shellPane, as pane.process_info gives them.
const (
	// procsProbe: brain status runs its codex version probe.
	procsProbe = `[{"name":"brain","argv":["/Users/me/.local/bin/brain","status"],"pid":10},` +
		`{"name":"codex","argv":["codex","--version"],"pid":11}]`
	// procsBrain: the probe has exited, and brain doctor still runs.
	procsBrain = `[{"name":"brain","argv":["brain","doctor"],"pid":12}]`
	// procsShell: brain review and its probe have exited.
	procsShell = `[{"name":"zsh","argv":["-zsh"],"pid":1}]`
	// procsAsh: the probe has exited in an ash login shell.
	procsAsh = `[{"name":"ash","argv":["-ash"],"pid":1}]`
	// procsCodex: a real codex runs.
	procsCodex = `[{"name":"codex","argv":["codex"],"pid":13}]`
	// procsNoArgv: herdr cannot read the argv.
	procsNoArgv = `[{"name":"codex","argv":null,"pid":14}]`
)

// shell adds shellPane, with no agent, and waits for its status stream.
func (l *live) shell(t *testing.T) {
	t.Helper()
	l.f.addPane(shellPane, "")
	l.screen(shellPane, "$ ")
	l.created(t, shellPane)
}

// run sets the agent of shellPane and its foreground processes to those of j; a zero j is a
// pane with no agent.
func (l *live) run(j job) {
	l.f.mu.Lock()
	defer l.f.mu.Unlock()
	l.f.panes[shellPane].agent = j.agent
	l.f.panes[shellPane].procs = j.procs
	l.f.panes[shellPane].group = j.group
}

// detect sends the detection of the agent of j in shellPane and waits for its baseline.
func (l *live) detect(t *testing.T, j job) {
	t.Helper()
	l.run(j)
	l.main.send(probeLine(j.agent, false))
	wantStep(t, l.step(t), Baseline, shellPane, GapNone, "")
}

// release sends the release of agent in shellPane and waits for its end.
func (l *live) release(t *testing.T, agent string) {
	t.Helper()
	l.run(job{})
	l.main.send(probeLine(agent, true))
	wantStep(t, l.step(t), End, shellPane, "", ReasonAgentLeft)
}

// observed returns the sessions observed and the capture gaps that brain status prints for
// harness.
func (l *live) observed(t *testing.T, harness string) store.DeliveryGap {
	t.Helper()
	var d store.DeliveryGap
	l.tx(t, func(tx *sql.Tx) (err error) {
		d, err = store.DeliveryGaps(context.Background(), tx, "s1", harness)
		return err
	})
	return d
}

// #317 AC 1, AC 3: brain status, doctor and review run "codex --version" in a shell pane.
// herdr detects codex for under a second, with no agent session id. At the baseline the probe
// still runs, or only brain runs, or only the shell is left; herdr may also lose a release.
// No probe makes a session, and brain status counts no session observed for them.
func TestEventsShellProbe(t *testing.T) {
	l := newLive(t, map[string]string{"w1:p1": codexRun1})
	l.shell(t)

	l.screen(shellPane, "$ brain status", "codex-cli 0.160.1")
	l.detect(t, job{agent: "codex", procs: procsProbe})
	l.release(t, "codex")
	l.screen(shellPane, "$ brain doctor", "codex-cli 0.160.1")
	l.detect(t, job{agent: "codex", procs: procsBrain})
	// herdr loses this release, and detects the next probe.
	l.screen(shellPane, "$ brain doctor", "codex-cli 0.160.1", "$ brain review", "codex-cli 0.160.1")
	l.detect(t, job{agent: "codex", procs: procsShell})
	l.status(t, shellPane, "idle") // a status change with no base takes a baseline too
	l.release(t, "codex")
	l.detect(t, job{agent: "codex", procs: procsAsh})
	l.release(t, "codex")

	r := l.rows(t)
	if len(r.sessions) != 1 || r.sessions[0].PaneID != "w1:p1" {
		t.Errorf("sessions = %+v, want only the codex session of w1:p1", r.sessions)
	}
	if lines, gaps := r.texts(shellPane); len(lines) != 0 || len(gaps) != 0 {
		t.Errorf("%s holds lines %q and gaps %q, want none", shellPane, lines, gaps)
	}
	if d := l.observed(t, "codex"); d.Sessions != 1 || d.Gaps != 1 {
		t.Errorf("status codex: sessions observed %d, capture gaps %d, want 1 and 1 (the start of w1:p1)", d.Sessions, d.Gaps)
	}
}

// #351 AC 2: brain status in a pipe, and after another brain command on the same line, in a
// shell pane. brain status runs "claude --version", and herdr detects claude there for under a
// second, with no agent session id. The #317 rule saw grep, a process of the pipe, as a
// possible agent, and the baseline opened an empty claude session. Brain's CLI leads the
// process group of the line: no session, and brain status counts no claude session observed.
func TestEventsShellPipe(t *testing.T) {
	l := newLive(t, map[string]string{"w1:p1": codexRun1})
	l.shell(t)

	l.screen(shellPane, "$ brain status | grep -i learning")
	l.detect(t, job{"claude", procsPipe, 20})
	l.release(t, "claude")
	l.detect(t, job{"claude", procsPipeDone, 20})
	l.release(t, "claude")
	l.screen(shellPane, "$ brain review 7 --reject --reason dup; brain status | grep -c claude")
	l.detect(t, job{"claude", procsReview, 30})
	l.release(t, "claude")
	l.detect(t, job{"claude", procsChain, 31})
	l.release(t, "claude")

	r := l.rows(t)
	if len(r.sessions) != 1 || r.sessions[0].PaneID != "w1:p1" {
		t.Errorf("sessions = %+v, want only the codex session of w1:p1", r.sessions)
	}
	if lines, gaps := r.texts(shellPane); len(lines) != 0 || len(gaps) != 0 {
		t.Errorf("%s holds lines %q and gaps %q, want none", shellPane, lines, gaps)
	}
	if d := l.observed(t, "claude"); d.Sessions != 0 || d.Gaps != 0 {
		t.Errorf("status claude: sessions observed %d, capture gaps %d, want none", d.Sessions, d.Gaps)
	}
}

// The foreground processes of a brain command line in shellPane (#351). The native claude
// binary is named by its version [spike/m0/data/e11s1-capture.txt].
const (
	// procsPipe: "brain status | grep -i learning" while brain status runs its claude probe.
	procsPipe = `[{"name":"brain","argv":["/Users/me/.local/bin/brain","status"],"pid":20},` +
		`{"name":"grep","argv":["grep","-i","learning"],"pid":21},{"name":"2.1.295","argv":["claude","--version"],"pid":22}]`
	// procsPipeDone: the probe has exited; brain status and grep still run.
	procsPipeDone = `[{"name":"brain","argv":["/Users/me/.local/bin/brain","status"],"pid":20},` +
		`{"name":"grep","argv":["grep","-i","learning"],"pid":21}]`
	// procsReview: the first command of the line, brain review, with its probe.
	procsReview = `[{"name":"brain","argv":["brain","review","7","--reject","--reason","dup"],"pid":30},` +
		`{"name":"2.1.295","argv":["claude","--version"],"pid":32}]`
	// procsChain: the second command of the line, "brain status | grep -c claude", after its
	// probe: grep names the agent.
	procsChain = `[{"name":"brain","argv":["brain","status"],"pid":31},{"name":"grep","argv":["grep","-c","claude"],"pid":33}]`
)

// #317 AC 2, AC 3: a real codex with no agent session id keeps its session: one left at its
// first prompt with no text (it holds only its baseline), one that ends fast with text, one
// with a hook record only, and one whose argv herdr cannot read. #351 AC 2: a real agent
// that leads its process group keeps its session, also when it runs Brain's CLI as a tool, when
// it runs under a node wrapper and when a script starts it. A real agent in a pipe that Brain's
// CLI leads keeps its session too (#351 review).
func TestEventsRealAgentKept(t *testing.T) {
	for _, tc := range []struct {
		name string
		job  job
		text string // a change shows this new line before the end; "" for none
		hook bool   // brain hook stores a record before the end
	}{
		{name: "first prompt", job: job{agent: "codex", procs: procsCodex}},
		{name: "fast with text", job: job{agent: "codex", procs: procsCodex}, text: "• done"},
		{name: "hook record only", job: job{agent: "codex", procs: procsCodex}, hook: true},
		{name: "no argv", job: job{agent: "codex", procs: procsNoArgv}},
		// #351: herdr gives the process group, and the agent leads it.
		{name: "claude", job: job{"claude", procsClaude, 40}, text: "⏺ done"},
		{name: "claude runs brain", job: job{"claude", procsClaudeTool, 40}, text: "⏺ done"},
		{name: "codex under node", job: job{"codex", procsNode, 50}, text: "• done"},
		{name: "script starts claude", job: job{"claude", procsScript, 60}, text: "⏺ done"},
		{name: "brain pipe into claude", job: job{"claude", procsBrainPipe, 20}, text: "⏺ done"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := newLive(t, map[string]string{"w1:p1": codexRun1})
			l.shell(t)
			l.screen(shellPane, "$ "+tc.job.agent)
			l.detect(t, tc.job)
			if tc.text != "" {
				l.screen(shellPane, "$ "+tc.job.agent, tc.text)
				l.status(t, shellPane, "idle")
			}
			if tc.hook {
				l.hookRecord(t)
			}
			l.release(t, tc.job.agent)

			r := l.rows(t)
			if len(r.sessions) != 2 || r.sessions[1].PaneID != shellPane || r.sessions[1].Key.Kind != store.KeyCapture ||
				r.sessions[1].Ended.IsZero() {
				t.Fatalf("sessions = %+v, want the ended capture session of %s", r.sessions, shellPane)
			}
			want := btoi(tc.text != "" || tc.hook) + btoi(tc.job.agent == "codex") // w1:p1 is a codex session
			if got := l.observed(t, tc.job.agent).Sessions; got != want {
				t.Errorf("status %s: sessions observed %d, want %d", tc.job.agent, got, want)
			}
			lines, _ := r.texts(shellPane)
			switch {
			case tc.text != "":
				wantLines(t, lines, tc.text)
			case tc.hook:
				wantLines(t, lines, "• Ran npm test")
			default:
				wantLines(t, lines)
			}
		})
	}
}

// The foreground processes of a real agent in shellPane, with the pid of their group leader
// (#351). The native claude binary is named by its version [spike/m0/data/e11s1-capture.txt].
const (
	// procsClaude: claude leads its group.
	procsClaude = `[{"name":"2.1.295","argv":["claude"],"pid":40}]`
	// procsClaudeTool: claude runs brain recall in its group.
	procsClaudeTool = `[{"name":"2.1.295","argv":["claude"],"pid":40},{"name":"brain","argv":["brain","recall","tests"],"pid":41}]`
	// procsNode: the npm codex, a node wrapper that leads the group and the codex binary
	// [spike/m0/data/e11s1b-capture.txt].
	procsNode = `[{"name":"codex","argv":["/usr/local/lib/node_modules/@openai/codex/bin/codex-aarch64-apple-darwin"],"pid":51},` +
		`{"name":"node","argv":["node","/usr/local/bin/codex"],"pid":50}]`
	// procsScript: a shell script that starts claude leads the group.
	procsScript = `[{"name":"bash","argv":["bash","./agent.sh"],"pid":60},{"name":"2.1.295","argv":["claude","--model","opus"],"pid":61}]`
	// procsBrainPipe: "brain status | claude -p 'Summarize this status'": brain leads the
	// group, and a real claude runs in it.
	procsBrainPipe = `[{"name":"brain","argv":["brain","status"],"pid":20},` +
		`{"name":"2.1.295","argv":["claude","-p","Summarize this status"],"pid":21}]`
)

// hookRecord stores a tool hook record of codex in shellPane, as brain hook does.
func (l *live) hookRecord(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	l.tx(t, func(tx *sql.Tx) error {
		_, segment, err := store.HookSegment(ctx, tx, "s1", "codex", shellPane, "")
		if err != nil || segment == 0 {
			return fmt.Errorf("no open segment: %w", err)
		}
		_, err = store.AddObservation(ctx, tx, store.Observation{CaptureIdentity: fmt.Sprintf("s1/%d/hook/tool", segment),
			Source: store.SourceHook, SegmentID: segment, Event: "tool", Excerpt: "• Ran npm test", Status: store.ObservationComplete,
			Sender: store.SenderAssistant, Counts: true, At: time.Date(2026, 10, 8, 10, 0, 30, 0, time.UTC)})
		return err
	})
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}
