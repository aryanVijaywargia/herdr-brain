package brained

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/aryanVijaywargia/herdr-brain/internal/bindings"
	"github.com/aryanVijaywargia/herdr-brain/internal/herdr"
	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

// S13, S32: on start, brained follows every live pane and gives each live agent a baseline
// with a gap, also an agent that works already. It invents no text: a baseline writes none.
func TestStartReconcile(t *testing.T) {
	h := newHarness(t, bindings.KindMain)
	h.f.addPane("w1:p1", "claude") // works already (S32)
	h.f.addPane("w1:p2", "")
	h.f.addPane("w2:p1", "codex")
	h.start(t)
	subs := h.f.statusSubs(t, 3)
	for _, id := range []string{"w1:p1", "w1:p2", "w2:p1"} {
		if subs[id] == nil {
			t.Errorf("no status subscription for %s", id)
		}
	}
	got := map[string]Step{}
	for range 2 {
		s := h.step(t)
		got[s.PaneID] = s
	}
	for _, id := range []string{"w1:p1", "w2:p1"} {
		wantStep(t, got[id], Baseline, id, GapStart, "")
		if got[id].Event != nil || !got[id].Learning.On {
			t.Errorf("baseline %s = %+v, want no event and learning on", id, got[id])
		}
	}
}

// S39: when brained falls behind, it drops the waiting events and reconciles: a baseline
// with a gap, not the missed changes.
func TestBehind(t *testing.T) {
	h := newHarness(t, bindings.KindMain)
	h.f.addPane("w1:p1", "claude")
	release := make(chan struct{})
	blocked := &blockOnce{release: release, next: recorder{h.steps}}
	h.d.handler = blocked
	h.d.queue = 1
	dropped := make(chan struct{})
	var once sync.Once
	h.d.onBehind = func() { once.Do(func() { close(dropped) }) }
	h.start(t)
	st := h.f.statusSubs(t, 1)["w1:p1"]
	wantStep(t, h.step(t), Baseline, "w1:p1", GapStart, "")
	st.send(statusLine("w1:p1", "idle")) // the handler blocks on this change
	for range 3 {
		st.send(statusLine("w1:p1", "working"))
	}
	select {
	case <-dropped:
	case <-time.After(wait):
		t.Fatal("no event was dropped")
	}
	close(release)
	wantStep(t, h.step(t), Change, "w1:p1", "", "")
	for {
		s := h.step(t)
		if s.Kind == Change {
			continue // a change that was in the queue before the drop
		}
		wantStep(t, s, Baseline, "w1:p1", GapBehind, "")
		return
	}
}

// A status stream that herdr ends while the pane lives gets a new subscription and a
// baseline with a gap.
func TestStatusStreamEnds(t *testing.T) {
	h := newHarness(t, bindings.KindMain)
	h.f.addPane("w1:p1", "claude")
	h.start(t)
	st := h.f.statusSubs(t, 1)["w1:p1"]
	wantStep(t, h.step(t), Baseline, "w1:p1", GapStart, "")
	_ = st.conn.Close()
	h.f.statusSubs(t, 1)["w1:p1"].send(statusLine("w1:p1", "working"))
	wantStep(t, h.step(t), Baseline, "w1:p1", GapBehind, "")
	wantStep(t, h.step(t), Change, "w1:p1", "", "")
}

// A failed pane.list at the start is tried again at the next tick, with the start gap.
func TestStartListFails(t *testing.T) {
	h := newHarness(t, bindings.KindMain)
	h.f.addPane("w1:p1", "claude")
	h.f.setFailList(true)
	h.start(t)
	h.check(t) // pane.list fails again
	h.f.setFailList(false)
	h.check(t)
	h.f.statusSubs(t, 1)
	wantStep(t, h.step(t), Baseline, "w1:p1", GapStart, "")
}

// A main stream that fails with another error than its end gets a new connection and a
// reconcile with GapConnect.
func TestReconnect(t *testing.T) {
	h := newHarness(t, bindings.KindMain)
	h.f.addPane("w1:p1", "claude")
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
	old := l.main
	if err := l.item(ctx, item{src: l.main, err: syscall.ECONNRESET}); err != nil {
		t.Fatalf("item = %v, want a reconnect", err)
	}
	if main := h.f.sub(t); !slices.Equal(main.subs, mainSubs()) || l.main == old {
		t.Errorf("new subscription %+v, want a new main stream", main.subs)
	}
	wantStep(t, h.step(t), Baseline, "w1:p1", GapConnect, "")
}

// blockOnce blocks the first Change until release closes.
type blockOnce struct {
	release chan struct{}
	next    Handler
	done    bool
}

func (b *blockOnce) Handle(ctx context.Context, s Step) error {
	if s.Kind == Change && !b.done {
		b.done = true
		<-b.release
	}
	return b.next.Handle(ctx, s)
}

// A second brained of the same herdr session exits with a clear line. Another session runs.
func TestSecondCopy(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	unlock, err := lock(dir, "s1")
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	environ := []string{"HERDR_SESSION=s1", "HERDR_SOCKET_PATH=/tmp/none", "HERDR_PLUGIN_STATE_DIR=" + dir, "HERDR_PLUGIN_ID=brain"}
	err = Main(context.Background(), environ, nil)
	if !errors.Is(err, ErrRunning) || !strings.Contains(err.Error(), `brained already runs for herdr session "s1"`) {
		t.Errorf("second Main = %v, want ErrRunning for s1", err)
	}
	other, err := lock(dir, "s2")
	if err != nil {
		t.Fatalf("lock of another session = %v", err)
	}
	other()
}

// D7: brained stops on the end of the event socket, and at the next check after Brain is
// disabled or unlinked.
func TestStops(t *testing.T) {
	tests := []struct {
		name    string
		act     func(t *testing.T, h *harness, main *fakeSub)
		message string
	}{
		{"socket end", func(_ *testing.T, _ *harness, main *fakeSub) { _ = main.conn.Close() }, "herdr closed the event socket"},
		{"disabled", func(t *testing.T, h *harness, _ *fakeSub) {
			h.f.setPlugins(`[{"plugin_id":"brain","enabled":false}]`)
			h.check(t)
		}, "plugin brain is disabled"},
		{"unlinked", func(t *testing.T, h *harness, _ *fakeSub) {
			h.f.setPlugins(`[{"plugin_id":"other","enabled":true}]`)
			h.check(t)
		}, "plugin brain is unlinked"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, bindings.KindMain)
			h.f.addPane("w1:p1", "claude")
			main := h.start(t)
			h.f.statusSubs(t, 1)
			h.step(t)
			tt.act(t, h, main)
			err := h.end(t)
			if !errors.Is(err, ErrStop) || !strings.Contains(err.Error(), tt.message) {
				t.Errorf("run = %v, want ErrStop: %s", err, tt.message)
			}
		})
	}
}

// A pane that appears later gets its status subscription; its agent gets a baseline with no
// gap, and its status changes reach the handler.
func TestLatePane(t *testing.T) {
	h := newHarness(t, bindings.KindMain)
	main := h.start(t)
	h.f.addPane("w1:p3", "")
	main.send(`{"event":"pane_created","data":{"type":"pane_created","pane":{"pane_id":"w1:p3","workspace_id":"w1","tab_id":"w1:t1"}}}`)
	st := h.f.statusSubs(t, 1)["w1:p3"]
	if st == nil {
		t.Fatal("no status subscription for w1:p3")
	}
	main.send(paneLine("pane_agent_detected", "w1:p3"))
	s := h.step(t)
	wantStep(t, s, Baseline, "w1:p3", GapNone, "")
	if s.Event == nil || s.Event.Kind != herdr.PaneAgentDetected {
		t.Errorf("baseline event = %+v, want pane.agent_detected", s.Event)
	}
	st.send(statusLine("w1:p3", "working"))
	wantStep(t, h.step(t), Change, "w1:p3", "", "")
	main.send(paneLine("pane_closed", "w1:p3"))
	wantStep(t, h.step(t), End, "w1:p3", "", ReasonGone)
}

// An unbound session and a test store (learning off by default, §12.1) give no steps. A
// later binding or switch turns capture on with a baseline and a gap.
func TestNoCaptureWhenOff(t *testing.T) {
	h := newHarness(t, bindings.KindTest)
	h.f.addPane("w1:p1", "claude")
	main := h.start(t)
	main.send(paneLine("pane_agent_detected", "w1:p1")) // the pane is followed, but off
	h.check(t)
	h.setLearning(t, store.LearningSetting{Scope: store.LearningStore, On: true, Changed: time.Now()})
	h.check(t)
	h.f.statusSubs(t, 1)
	wantStep(t, h.step(t), Baseline, "w1:p1", GapLearning, "")
}

// Off-time text reaches no store write (E9-S4, HLD §8.3). capture follows the Handler rule
// and writes the text of each change in a real store transaction.
func TestOffTime(t *testing.T) {
	t1 := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	off := store.LearningSetting{Scope: store.LearningPane, HerdrSession: "s1", Target: "w1:p1", On: false, Changed: t1}
	on := off
	on.On, on.Changed = true, t1.Add(time.Second)
	tests := []struct {
		name string
		run  func(t *testing.T, h *harness, c *capture, st *fakeSub)
		want []string
	}{
		{"prompt while off", func(t *testing.T, h *harness, _ *capture, st *fakeSub) {
			h.setLearning(t, off)
			h.check(t)
			wantStep(t, h.step(t), End, "w1:p1", "", ReasonLearningOff)
			// The prompt of this time makes a status change, but brained closed the stream: no
			// read. The next step is the baseline after the switch.
			st.send(statusLine("w1:p1", "working"))
			h.setLearning(t, on)
			h.check(t)
			h.f.statusSubs(t, 1)["w1:p1"].send(statusLine("w1:p1", "idle"))
			wantStep(t, h.step(t), Baseline, "w1:p1", GapLearning, "")
			wantStep(t, h.step(t), Change, "w1:p1", "", "")
		}, []string{"later text"}},
		{"off during a pending read", func(t *testing.T, h *harness, c *capture, st *fakeSub) {
			c.reading = func() { h.setLearning(t, off) }
			st.send(statusLine("w1:p1", "working"))
			wantStep(t, h.step(t), Change, "w1:p1", "", "")
		}, nil},
		{"off, new text, on, new event", func(t *testing.T, h *harness, c *capture, st *fakeSub) {
			h.setLearning(t, off)
			h.setLearning(t, on) // no check in between: brained did not see the off time
			c.text <- "text of the off time"
			st.send(statusLine("w1:p1", "working"))
			wantStep(t, h.step(t), Change, "w1:p1", "", "")
			st.send(statusLine("w1:p1", "idle"))
			wantStep(t, h.step(t), Change, "w1:p1", "", "")
		}, []string{"later text"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, bindings.KindMain)
			h.f.addPane("w1:p1", "claude")
			c := &capture{next: recorder{h.steps}, text: make(chan string, 4), base: map[string]Learning{}}
			h.d.handler = c
			h.start(t)
			st := h.f.statusSubs(t, 1)["w1:p1"]
			wantStep(t, h.step(t), Baseline, "w1:p1", GapStart, "")
			tt.run(t, h, c, st)
			if strings.Join(c.writes, "|") != strings.Join(tt.want, "|") {
				t.Errorf("writes = %q, want %q", c.writes, tt.want)
			}
		})
	}
}

// capture is a Handler that follows the rule of the Handler doc, as E11-S3 must. A read
// is the next string of text, or "later text".
type capture struct {
	next    Handler
	text    chan string
	reading func() // called during each read
	base    map[string]Learning
	writes  []string
}

func (c *capture) Handle(ctx context.Context, s Step) error {
	switch s.Kind {
	case Baseline:
		c.base[s.PaneID] = s.Learning
	case End:
		delete(c.base, s.PaneID)
	case Change:
		if err := c.change(ctx, s); err != nil {
			return err
		}
	}
	return c.next.Handle(ctx, s)
}

func (c *capture) change(ctx context.Context, s Step) error {
	text := "later text"
	select {
	case text = <-c.text:
	default:
	}
	if c.reading != nil {
		c.reading()
	}
	st, err := store.Open(ctx, s.Gate.Binding.Store, busyTimeout)
	if err != nil {
		return err
	}
	defer st.Close() //nolint:errcheck // test
	return st.Write(ctx, func(tx *sql.Tx) error {
		now, err := s.Gate.Learning(ctx, tx, s.PaneID, s.WorkspaceID)
		if err != nil {
			return err
		}
		base, ok := c.base[s.PaneID]
		if !ok || !now.On || now != base {
			c.base[s.PaneID] = now // this read is the new baseline, with a gap
			return nil
		}
		c.writes = append(c.writes, text)
		return nil
	})
}

// A tick starts an extraction pass of the bound store. No second pass starts while one runs,
// or less than extractEvery after the last start (E13-S2).
func TestExtractTrigger(t *testing.T) {
	h := newHarness(t, bindings.KindMain)
	passes := make(chan Gate, 4)
	release := make(chan struct{})
	h.d.extract = func(_ context.Context, g Gate) error {
		passes <- g
		<-release
		return nil
	}
	h.start(t)
	h.check(t)
	select {
	case g := <-passes:
		if g.Binding.Store != h.store {
			t.Errorf("pass store = %s, want %s", g.Binding.Store, h.store)
		}
	case <-time.After(wait):
		t.Fatal("no extraction pass")
	}
	h.check(t) // the pass runs
	close(release)
	h.check(t)
	h.check(t) // the pass ended less than extractEvery ago
	select {
	case <-passes:
		t.Error("a second pass started")
	default:
	}
}

// A pass of a store with no extractor CLI calls nothing.
func TestExtractPassNoCLI(t *testing.T) {
	g := NewGate(bindings.Binding{Session: "s1", Store: t.TempDir(), Kind: bindings.KindMain})
	if err := extractPass(context.Background(), g); err != nil {
		t.Fatal(err)
	}
}
