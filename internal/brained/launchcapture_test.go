package brained

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aryanVijaywargia/herdr-brain/internal/bindings"
	"github.com/aryanVijaywargia/herdr-brain/internal/launch"
	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

// The Codex thread ids of the launch capture tests.
const (
	threadA = "019a0000-0000-7000-8000-00000000000a"
	threadB = "019a0000-0000-7000-8000-00000000000b"
)

// minute is the time n minutes after the start of 2026-10-12, as at gives it.
func minute(n int) time.Time { return time.Date(2026, 10, 12, 0, n, 0, 0, time.UTC) }

// launchStart is the start of each test launch.
var launchStart = minute(10)

// launchFix is a bound store and a Codex home for the capture of ended launches.
type launchFix struct {
	t     *testing.T
	g     Gate
	codex string
}

func newLaunchFix(t *testing.T) *launchFix {
	t.Helper()
	b := bindings.Binding{Session: "s1", Socket: filepath.Join(t.TempDir(), "s"), Store: t.TempDir(), Kind: bindings.KindMain,
		Created: launchStart}
	return &launchFix{t: t, g: NewGate(b), codex: t.TempDir()}
}

func (f *launchFix) write(fn func(ctx context.Context, tx *sql.Tx) error) {
	f.t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, f.g.Binding.Store, busyTimeout)
	if err != nil {
		f.t.Fatal(err)
	}
	defer st.Close() //nolint:errcheck // test
	if err := st.Write(ctx, func(tx *sql.Tx) error { return fn(ctx, tx) }); err != nil {
		f.t.Fatal(err)
	}
}

// launch makes the launch record of cli in pane, started at launchStart.
func (f *launchFix) launch(pane, cli string) store.Launch {
	f.t.Helper()
	dir, err := launch.Create(context.Background(), f.g.Binding, "w1", pane, cli, f.codex, launchStart)
	if err != nil {
		f.t.Fatal(err)
	}
	id, err := strconv.ParseInt(filepath.Base(dir), 10, 64)
	if err != nil {
		f.t.Fatal(err)
	}
	var l store.Launch
	f.write(func(ctx context.Context, tx *sql.Tx) (err error) {
		l, _, err = store.LaunchByID(ctx, tx, id)
		return err
	})
	return l
}

// capture opens the capture session of agent session native of cli in pane, as brained keeps
// it, with a segment from minute start and one pane read of text at minute read. It returns the
// session id.
func (f *launchFix) capture(cli, pane, native string, start int, text string, read int) int64 {
	f.t.Helper()
	var session int64
	f.write(func(ctx context.Context, tx *sql.Tx) error {
		var err error
		key := store.SessionKey{HerdrSession: "s1", Harness: cli, Kind: store.KeyAgent, ID: native}
		if session, err = store.OpenSession(ctx, tx, key, pane, "", minute(start)); err != nil {
			return err
		}
		segment, err := store.StartSegment(ctx, tx, session, pane, "", "recent_unwrapped", minute(start))
		if err != nil {
			return err
		}
		_, err = store.AddObservation(ctx, tx, store.Observation{CaptureIdentity: fmt.Sprintf("s1/%d/read", segment),
			Source: store.SourceHerdr, SegmentID: segment, Event: "working", Excerpt: text, Status: store.ObservationComplete,
			Sender: store.SenderUnconfirmed, Counts: true, At: minute(read).Add(30 * time.Second)})
		return err
	})
	return session
}

// rollout writes the rollout file name in the day folder of the Codex home.
func (f *launchFix) rollout(name string, lines ...string) {
	f.t.Helper()
	p := filepath.Join(f.codex, "sessions", "2026", "10", "12", name)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		f.t.Fatal(err)
	}
}

// threadARollout writes the rollout of thread A: a turn of an earlier run, before the launch,
// and a turn of the launch.
func (f *launchFix) threadARollout() {
	f.t.Helper()
	f.rollout("rollout-2026-10-12T00-00-00-"+threadA+".jsonl",
		xMeta(threadA, "/nowhere/api", "cli", nil),
		xUser(2, "old turn before the launch"),
		xAssistant(3, "old reply"),
		xUser(11, "run the api tests"),
		xCall(12, "c1", "make test"),
		xOutput(13, "c1", "Exit code: 2\nOutput:\nFAIL api"),
		xAssistant(14, "make test fails."),
	)
}

// sessions returns the launch sessions of launch id.
func (f *launchFix) sessions(id int64) []store.LaunchSession {
	f.t.Helper()
	var ss []store.LaunchSession
	f.write(func(ctx context.Context, tx *sql.Tx) (err error) {
		ss, err = store.LaunchSessions(ctx, tx, id)
		return err
	})
	return ss
}

// paneObs is one observation with the pane and the capture session of its segment.
type paneObs struct {
	store.Observation
	pane    string
	session int64
}

func (f *launchFix) observations() []paneObs {
	f.t.Helper()
	var out []paneObs
	f.write(func(ctx context.Context, tx *sql.Tx) error {
		obs, err := store.Observations(ctx, tx)
		if err != nil {
			return err
		}
		segs, err := store.Segments(ctx, tx)
		if err != nil {
			return err
		}
		for _, o := range obs {
			i := slices.IndexFunc(segs, func(s store.Segment) bool { return s.ID == o.SegmentID })
			out = append(out, paneObs{o, segs[i].PaneID, segs[i].SessionID})
		}
		return nil
	})
	return out
}

// end ends the launches ls at minute 29, as brained does before a capture pass.
func (f *launchFix) end(ls ...store.Launch) {
	f.t.Helper()
	f.write(func(ctx context.Context, tx *sql.Tx) error {
		for _, l := range ls {
			if err := store.EndLaunch(ctx, tx, l.ID, store.EndAgentExit, minute(29)); err != nil {
				return err
			}
		}
		return nil
	})
}

// run ends the launches ls and runs a capture pass at minute 30.
func (f *launchFix) run(ls ...store.Launch) {
	f.t.Helper()
	f.end(ls...)
	if err := captureLaunches(context.Background(), f.g, minute(0), minute(30)); err != nil {
		f.t.Fatalf("captureLaunches() = %v", err)
	}
}

// AC E24-S5: two Codex panes in one folder, with no Brain hook. Each ended launch reads the
// exact rollout of its own thread, not the newest file of the folder, and its records go to the
// capture session of its own pane. A turn before the launch is not of it. A record that the
// pane showed too enriches the pane record and does not count twice. The launch record shows
// the source transcript.
func TestCaptureLaunchesRollout(t *testing.T) {
	f := newLaunchFix(t)
	la, lb := f.launch("w1:p1", "codex"), f.launch("w1:p2", "codex")
	sa := f.capture("codex", "w1:p1", threadA, 10, "run the api tests", 11)
	sb := f.capture("codex", "w1:p2", threadB, 10, "deploy the web app", 21)
	f.threadARollout()
	// The rollout of thread B is the newest file of the same folder.
	f.rollout("rollout-2026-10-12T00-20-00-"+threadB+".jsonl",
		xMeta(threadB, "/nowhere/api", "cli", nil),
		xUser(21, "deploy the web app"),
		xAssistant(22, "deployed."),
	)
	f.run(la, lb)

	got := map[string]string{}
	var enriched []int64
	for _, o := range f.observations() {
		if o.Source == store.SourceHerdr {
			enriched = append(enriched, o.EnrichedBy)
			continue
		}
		got[o.Excerpt] = fmt.Sprintf("%s %s %d %s counts %v", o.Source, o.pane, o.session, o.Event, o.Counts)
	}
	want := map[string]string{
		"run the api tests":               fmt.Sprintf("transcript w1:p1 %d prompt counts false", sa), // the pane showed it
		"make test":                       fmt.Sprintf("transcript w1:p1 %d tool counts true", sa),
		"Exit code: 2\nOutput:\nFAIL api": fmt.Sprintf("transcript w1:p1 %d tool counts false", sa),
		"make test fails.":                fmt.Sprintf("transcript w1:p1 %d stop counts true", sa),
		"deploy the web app":              fmt.Sprintf("transcript w1:p2 %d prompt counts false", sb),
		"deployed.":                       fmt.Sprintf("transcript w1:p2 %d stop counts true", sb),
	}
	diffMaps(t, got, want)
	if slices.Contains(enriched, 0) {
		t.Errorf("pane records enriched by %v, want each one by its transcript record", enriched)
	}
	for _, c := range []struct {
		l      store.Launch
		thread string
	}{{la, threadA}, {lb, threadB}} {
		want := []store.LaunchSession{{ID: c.thread, Source: store.CaptureTranscript, FirstSeen: minute(10)}}
		if got := f.sessions(c.l.ID); !slices.Equal(got, want) {
			t.Errorf("sessions of the launch of %s = %+v, want %+v", c.l.PaneID, got, want)
		}
	}
	// The pass notes each launch, so the next pass skips it. A pass again after a lost note
	// imports no record twice.
	n := len(f.observations())
	f.write(func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE launches SET captured = NULL`)
		return err
	})
	f.run(la, lb)
	if got := len(f.observations()); got != n {
		t.Errorf("observations after a second pass = %d, want %d", got, n)
	}
}

// AC E24-S5: the launch record shows the capture source of each session. A session that a
// Brain hook reported keeps hooks, and brained reads no rollout of it. A Codex session with no
// rollout, or one that a forget touched, and a Claude session keep the pane-screen capture. A
// session of the pane from before the launch is not of the launch.
func TestCaptureLaunchesSources(t *testing.T) {
	for _, c := range []struct {
		name string
		cli  string
		prep func(f *launchFix, l store.Launch, session int64)
		want store.CaptureSource
	}{
		{name: "hooks", cli: "codex", prep: func(f *launchFix, l store.Launch, _ int64) {
			f.threadARollout()
			f.write(func(ctx context.Context, tx *sql.Tx) error {
				return store.SetLaunchSession(ctx, tx, l.ID, threadA, store.CaptureHooks, minute(10))
			})
		}, want: store.CaptureHooks},
		{name: "no rollout", cli: "codex", want: store.CaptureScreen},
		{name: "unknown format", cli: "codex", prep: func(f *launchFix, _ store.Launch, _ int64) {
			f.rollout("rollout-2026-10-12T00-00-00-"+threadA+".jsonl", `{"timestamp":"2026-10-12T00:11:00Z"}`)
		}, want: store.CaptureScreen},
		{name: "forgotten", cli: "codex", prep: func(f *launchFix, _ store.Launch, session int64) {
			f.threadARollout()
			f.write(func(ctx context.Context, tx *sql.Tx) error { return store.ForgetSession(ctx, tx, session) })
		}, want: store.CaptureScreen},
		{name: "claude", cli: "claude", prep: func(f *launchFix, _ store.Launch, _ int64) { f.threadARollout() },
			want: store.CaptureScreen},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newLaunchFix(t)
			l := f.launch("w1:p1", c.cli)
			f.capture(c.cli, "w1:p1", "019a0000-0000-7000-8000-000000000001", 5, "an earlier run", 6) // before the launch
			session := f.capture(c.cli, "w1:p1", threadA, 10, "run the api tests", 11)
			if c.prep != nil {
				c.prep(f, l, session)
			}
			f.run(l)
			want := []store.LaunchSession{{ID: threadA, Source: c.want, FirstSeen: minute(10)}}
			if got := f.sessions(l.ID); !slices.Equal(got, want) {
				t.Errorf("sessions = %+v, want %+v", got, want)
			}
			for _, o := range f.observations() {
				if o.Source == store.SourceTranscript {
					t.Errorf("observation %q from a transcript, want none", o.Excerpt)
				}
			}
		})
	}
}

// E24-S5: brained reads the rollout when it ends a Codex launch, here the launch of a pane that
// is gone at its start, beside the loop. It looks in the Codex home of the launch, not in its
// own (review finding 7).
func TestLaunchEndReadsRollout(t *testing.T) {
	h := newHarness(t, bindings.KindMain)
	h.codexHome = filepath.Join(t.TempDir(), "pane-codex")
	f := &launchFix{t: t, g: NewGate(bindings.Binding{Session: "s1", Socket: h.f.path, Store: h.store, Kind: bindings.KindMain}),
		codex: h.codexHome}
	h.d.environ = []string{"HOME=" + t.TempDir(), "CODEX_HOME=" + filepath.Join(t.TempDir(), "brained-codex")}
	passes := make(chan error, 4)
	h.d.onCaptured = func(err error) { passes <- err }
	h.f.addPane("w1:p1", "claude")
	gone := h.launchIn(t, h.f.path, "w1:p9", "codex", launchStart)
	f.capture("codex", "w1:p9", threadA, 10, "run the api tests", 11)
	f.threadARollout()
	h.start(t)
	h.f.statusSubs(t, 1)
	wantStep(t, h.step(t), Baseline, "w1:p1", GapStart, "") // the reconcile ended the launch
	select {
	case err := <-passes:
		if err != nil {
			t.Fatalf("capture pass = %v", err)
		}
	case <-time.After(wait):
		t.Fatal("no capture pass")
	}
	r := h.record(t, gone)
	want := []store.LaunchSession{{ID: threadA, Source: store.CaptureTranscript, FirstSeen: minute(10)}}
	if got := f.sessions(r.ID); !slices.Equal(got, want) || r.Captured.IsZero() {
		t.Errorf("sessions = %+v, captured %v; want %+v and a capture time", got, r.Captured, want)
	}
}

// E24-S5 review finding 1: capture keeps no segment while learning is off, so the rollout
// turns of that time stay out, also when learning is on again at the end of the launch.
func TestCaptureLaunchesLearningOffGap(t *testing.T) {
	f := newLaunchFix(t)
	l := f.launch("w1:p1", "codex")
	f.capture("codex", "w1:p1", threadA, 10, "allowed before", 11)
	key := store.SessionKey{HerdrSession: "s1", Harness: "codex", Kind: store.KeyAgent, ID: threadA}
	f.write(func(ctx context.Context, tx *sql.Tx) error {
		if err := store.SetLearning(ctx, tx, store.LearningSetting{Scope: store.LearningPane, HerdrSession: "s1",
			Target: "w1:p1", On: false, Changed: minute(12)}); err != nil {
			return err
		}
		return store.EndSession(ctx, tx, key, minute(12))
	})
	f.write(func(ctx context.Context, tx *sql.Tx) error {
		return store.SetLearning(ctx, tx, store.LearningSetting{Scope: store.LearningPane, HerdrSession: "s1",
			Target: "w1:p1", On: true, Changed: minute(20)})
	})
	f.capture("codex", "w1:p1", threadA, 20, "allowed after", 21)
	f.rollout("rollout-2026-10-12T00-00-00-"+threadA+".jsonl", xMeta(threadA, "/work", "cli", nil),
		xUser(11, "allowed before"), xAssistant(11, "before reply"),
		xUser(15, "private while learning off"), xAssistant(16, "private reply"),
		xUser(21, "allowed after"), xAssistant(22, "after reply"),
		xUser(19, "a turn across the switch"), xAssistant(21, "its reply"))
	f.run(l)
	var got []string
	for _, o := range f.observations() {
		if o.Source == store.SourceTranscript {
			got = append(got, o.Excerpt)
		}
	}
	if want := []string{"allowed before", "before reply", "allowed after", "after reply"}; !slices.Equal(got, want) {
		t.Errorf("transcript records = %q, want %q", got, want)
	}
}

// E24-S5 review finding 5: a rollout whose name has the thread id, but whose session_meta
// names another thread, stays out.
func TestCaptureLaunchesWrongRolloutHeader(t *testing.T) {
	f := newLaunchFix(t)
	l := f.launch("w1:p1", "codex")
	f.capture("codex", "w1:p1", threadA, 10, "pane A", 11)
	f.rollout("rollout-2026-10-12T00-00-00-"+threadA+".jsonl", xMeta(threadB, "/other/workspace", "cli", nil),
		xUser(11, "text of thread B"), xAssistant(12, "reply of thread B"))
	f.run(l)
	for _, o := range f.observations() {
		if o.Source == store.SourceTranscript {
			t.Errorf("observation %q from the rollout of another thread", o.Excerpt)
		}
	}
	if got := f.sessions(l.ID); len(got) != 1 || got[0].Source != store.CaptureScreen {
		t.Errorf("sessions = %+v, want thread A from the screen", got)
	}
}

// E24-S5 review finding 3: a pass that a stop cuts short leaves the launch for the next pass,
// which captures it.
func TestCaptureLaunchesRetry(t *testing.T) {
	f := newLaunchFix(t)
	l := f.launch("w1:p1", "codex")
	f.capture("codex", "w1:p1", threadA, 10, "screen", 11)
	f.threadARollout()
	f.end(l)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := captureLaunches(ctx, f.g, minute(0), minute(30)); !errors.Is(err, context.Canceled) {
		t.Fatalf("captureLaunches(canceled) = %v, want %v", err, context.Canceled)
	}
	if got := f.sessions(l.ID); len(got) != 0 {
		t.Fatalf("sessions after a canceled pass = %+v, want none", got)
	}
	f.run(l)
	if got := f.sessions(l.ID); len(got) != 1 || got[0].Source != store.CaptureTranscript {
		t.Errorf("sessions after the next pass = %+v, want thread A from the transcript", got)
	}
	// A launch that ended before the window of the pass is not tried again.
	other := f.launch("w1:p2", "codex")
	f.write(func(ctx context.Context, tx *sql.Tx) error {
		return store.EndLaunch(ctx, tx, other.ID, store.EndAgentExit, minute(1))
	})
	if err := captureLaunches(context.Background(), f.g, minute(2), minute(30)); err != nil {
		t.Fatal(err)
	}
	f.write(func(ctx context.Context, tx *sql.Tx) error {
		got, _, err := store.LaunchByID(ctx, tx, other.ID)
		if err == nil && !got.Captured.IsZero() {
			t.Errorf("launch that ended before the window: captured %v, want not tried", got.Captured)
		}
		return err
	})
}

// E24-S5 review finding 8: the pass leaves the current session of the launch as the hooks set
// it, also when an older capture session comes later by id.
func TestCaptureLaunchesKeepsCurrentSession(t *testing.T) {
	f := newLaunchFix(t)
	l := f.launch("w1:p1", "codex")
	for i, id := range []string{threadA, threadB, threadA} {
		f.capture("codex", "w1:p1", id, 10+i*5, "pane", 11+i*5)
		f.write(func(ctx context.Context, tx *sql.Tx) error {
			return store.SetLaunchSession(ctx, tx, l.ID, id, store.CaptureHooks, minute(10+i*5))
		})
	}
	f.run(l)
	f.write(func(ctx context.Context, tx *sql.Tx) error {
		got, _, err := store.LaunchByID(ctx, tx, l.ID)
		if err == nil && got.NativeSessionID != threadA {
			t.Errorf("current session after the pass = %s, want %s", got.NativeSessionID, threadA)
		}
		return err
	})
}
