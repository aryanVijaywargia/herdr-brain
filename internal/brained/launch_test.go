package brained

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aryanVijaywargia/herdr-brain/internal/bindings"
	"github.com/aryanVijaywargia/herdr-brain/internal/herdr"
	"github.com/aryanVijaywargia/herdr-brain/internal/launch"
	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

// testLaunch is a launch record that a test made, and its folder.
type testLaunch struct {
	dir, hash string
}

// launchIn makes a launch record of cli in pane of the herdr server socket in the store of h,
// started at at.
func (h *harness) launchIn(t *testing.T, socket, pane, cli string, at time.Time) testLaunch {
	t.Helper()
	ws, _, _ := strings.Cut(pane, ":")
	b := bindings.Binding{Session: "s1", Socket: socket, Store: h.store, Kind: bindings.KindMain, Created: at}
	dir, err := launch.Create(context.Background(), b, ws, pane, cli, h.codexHome, at)
	if err != nil {
		t.Fatal(err)
	}
	id, err := os.ReadFile(filepath.Join(dir, "id"))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := hex.DecodeString(string(id))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	return testLaunch{dir: dir, hash: hex.EncodeToString(sum[:])}
}

// record reads the launch record of l.
func (h *harness) record(t *testing.T, l testLaunch) store.Launch {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, h.store, busyTimeout)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close() //nolint:errcheck // test
	var r store.Launch
	err = st.Write(ctx, func(tx *sql.Tx) error {
		var ok bool
		var err error
		r, ok, err = store.LaunchByHash(ctx, tx, l.hash)
		if err == nil && !ok {
			err = errors.New("no launch record")
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// wantEnded checks that l ended with reason, and that its folder is gone.
func (h *harness) wantEnded(t *testing.T, l testLaunch, reason store.EndReason) {
	t.Helper()
	if r := h.record(t, l); r.Ended.IsZero() || r.EndReason != reason {
		t.Errorf("launch of %s = ended %v by %q, want an end by %q", r.PaneID, r.Ended, r.EndReason, reason)
	}
	if _, err := os.Stat(l.dir); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("launch folder %s: %v, want it deleted", l.dir, err)
	}
}

// wantRunning checks that l has no end, and that its folder is there.
func (h *harness) wantRunning(t *testing.T, l testLaunch) {
	t.Helper()
	if r := h.record(t, l); !r.Ended.IsZero() || r.EndReason != "" {
		t.Errorf("launch of %s = ended %v by %q, want it running", r.PaneID, r.Ended, r.EndReason)
	}
	if _, err := os.Stat(l.dir); err != nil {
		t.Errorf("launch folder %s: %v, want it kept", l.dir, err)
	}
}

func agentGoneLine(pane string) string {
	ws, _, _ := strings.Cut(pane, ":")
	return fmt.Sprintf(`{"event":"pane_agent_detected","data":{"agent":"claude","pane_id":%q,"workspace_id":%q,"released":true}}`, pane, ws)
}

// AC 2 E24-S3: with a fake herdr, brained ends the launch record of a pane on its close, its
// exit, the exit of its agent and its move to another workspace, and deletes the launch
// folder. The launch of another pane keeps running.
func TestLaunchEnds(t *testing.T) {
	for _, c := range []struct {
		name   string
		event  func(t *testing.T, h *harness, main *fakeSub, l testLaunch)
		reason store.EndReason
		step   Reason
	}{
		{name: "pane close", event: func(_ *testing.T, _ *harness, main *fakeSub, _ testLaunch) {
			main.send(paneLine("pane_closed", "w1:p1"))
		}, reason: store.EndPaneClosed, step: ReasonGone},
		{name: "pane exit", event: func(_ *testing.T, _ *harness, main *fakeSub, _ testLaunch) {
			main.send(paneLine("pane_exited", "w1:p1"))
		}, reason: store.EndPaneExited, step: ReasonGone},
		{name: "agent exit", event: func(_ *testing.T, h *harness, main *fakeSub, _ testLaunch) {
			h.f.mu.Lock()
			h.f.panes["w1:p1"].agent = ""
			h.f.mu.Unlock()
			main.send(agentGoneLine("w1:p1"))
		}, reason: store.EndAgentExit, step: ReasonAgentLeft},
		{name: "move", event: func(t *testing.T, h *harness, main *fakeSub, l testLaunch) {
			h.f.mu.Lock()
			h.f.panes["w2:p1"] = h.f.panes["w1:p1"]
			h.f.panes["w2:p1"].ws = "w2"
			delete(h.f.panes, "w1:p1")
			h.f.moved["w1:p1"] = "w2:p1"
			h.f.mu.Unlock()
			// The check refuses the moved pane before brained sees the close of its old id.
			var r *launch.RefusedError
			if err := checkLaunch(h, l); !errors.As(err, &r) || r.Reason != launch.Moved {
				t.Errorf("Check() of the moved pane = %v, want refused: %s", err, launch.Moved)
			}
			main.send(paneLine("pane_closed", "w1:p1"))
		}, reason: store.EndPaneClosed, step: ReasonGone},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t, bindings.KindMain)
			h.f.addPane("w1:p1", "claude")
			h.f.addPane("w1:p2", "claude")
			before := time.Now().Add(-time.Second)
			l1 := h.launchIn(t, h.f.path, "w1:p1", "claude", before)
			l2 := h.launchIn(t, h.f.path, "w1:p2", "claude", before)
			main := h.start(t)
			h.f.statusSubs(t, 2)
			for range 2 {
				h.step(t) // the start baselines
			}
			if err := checkLaunch(h, l1); err != nil {
				t.Fatalf("Check() of the running launch = %v, want nil", err)
			}
			c.event(t, h, main, l1)
			wantStep(t, h.step(t), End, "w1:p1", "", c.step) // brained ends the launches before this step
			h.wantEnded(t, l1, c.reason)
			h.wantRunning(t, l2)
		})
	}
}

// An agent exit ends only the launches that started before herdr detected the agent: the late
// exit of an older agent in the pane does not end a new launch whose agent herdr has not
// detected yet.
func TestLaunchOlderAgentExit(t *testing.T) {
	h := newHarness(t, bindings.KindMain)
	h.f.addPane("w1:p1", "claude")
	main := h.start(t)
	h.f.statusSubs(t, 1)
	wantStep(t, h.step(t), Baseline, "w1:p1", GapStart, "") // brained has seen the agent
	l := h.launchIn(t, h.f.path, "w1:p1", "claude", time.Now().Add(time.Second))
	main.send(agentGoneLine("w1:p1"))
	wantStep(t, h.step(t), End, "w1:p1", "", ReasonAgentLeft)
	h.wantRunning(t, l)
}

// AC 4 E24-S3: at its start, brained ends the launch records of panes that are gone, and
// deletes their folders. A launch of a live pane, a launch of another herdr server that
// shares the store, and a launch that started after brained listed the panes keep running.
func TestLaunchRestart(t *testing.T) {
	h := newHarness(t, bindings.KindMain)
	h.f.addPane("w1:p1", "claude")
	before := time.Now().Add(-time.Second)
	live := h.launchIn(t, h.f.path, "w1:p1", "claude", before)
	gone := h.launchIn(t, h.f.path, "w1:p9", "codex", before)
	other := h.launchIn(t, filepath.Join(t.TempDir(), "other.sock"), "w1:p9", "claude", before)
	late := h.launchIn(t, h.f.path, "w1:p8", "claude", time.Now().Add(time.Hour))
	h.start(t)
	h.f.statusSubs(t, 1)
	wantStep(t, h.step(t), Baseline, "w1:p1", GapStart, "") // the reconcile ended the launches before this step
	h.wantEnded(t, gone, store.EndPaneGone)
	h.wantRunning(t, live)
	h.wantRunning(t, other)
	h.wantRunning(t, late)
	var r *launch.RefusedError
	if err := checkLaunch(h, live); err != nil {
		t.Errorf("Check() of the live launch = %v, want nil", err)
	}
	if err := checkLaunch(h, gone); !errors.As(err, &r) || r.Reason != launch.Unknown {
		t.Errorf("Check() of the ended launch = %v, want refused: %s (its folder is gone)", err, launch.Unknown)
	}
}

// checkLaunch runs the launch check of l against the fake herdr of h.
func checkLaunch(h *harness, l testLaunch) error {
	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	defer cancel()
	_, err := launch.Check(ctx, h.d.bindingsFile, l.dir, nil)
	return err
}

// The store keeps the start of a launch to the millisecond. A launch that started after the
// time of an agent detection or a pane list, in the same millisecond, is not older than it: an
// agent exit or a reconcile does not end it.
func TestLaunchSameMillisecond(t *testing.T) {
	for _, kind := range []string{"agent exit", "pane list"} {
		t.Run(kind, func(t *testing.T) {
			h := newHarness(t, bindings.KindMain)
			base := time.Date(2026, 10, 29, 1, 0, 0, 0, time.UTC)
			observed := base.Add(500 * time.Microsecond)
			h.d.now = func() time.Time { return observed }
			newer := h.launchIn(t, h.f.path, "w1:p1", "claude", base.Add(600*time.Microsecond))
			g := NewGate(bindings.Binding{Session: "s1", Socket: h.f.path, Store: h.store, Kind: bindings.KindMain})
			l := newLoop(h.d)
			defer l.wg.Wait()
			l.gate = &g
			if kind == "agent exit" {
				l.agentDetected(context.Background(), &pane{agent: true, detected: observed},
					herdr.Event{Kind: herdr.PaneAgentDetected, PaneID: "w1:p1", Agent: "claude", Released: true})
			} else if err := l.reconcile(context.Background(), GapStart); err != nil {
				t.Fatal(err)
			}
			h.wantRunning(t, newer)
		})
	}
}

// A repeated detection of the agent in a pane keeps the time of its first detection, so the
// exit of that agent does not end a launch that started after it.
func TestLaunchRepeatedDetection(t *testing.T) {
	h := newHarness(t, bindings.KindMain)
	base := time.Date(2026, 10, 29, 1, 0, 0, 0, time.UTC)
	calls := 0
	h.d.now = func() time.Time {
		calls++
		if calls <= 2 { // the start pane list and the end of its launches
			return base
		}
		return base.Add(2 * time.Second)
	}
	h.f.addPane("w1:p1", "claude")
	main := h.start(t)
	h.f.statusSubs(t, 1)
	wantStep(t, h.step(t), Baseline, "w1:p1", GapStart, "")
	newer := h.launchIn(t, h.f.path, "w1:p1", "claude", base.Add(time.Second))
	main.send(paneLine("pane_agent_detected", "w1:p1")) // the old agent again
	wantStep(t, h.step(t), Baseline, "w1:p1", GapNone, "")
	main.send(agentGoneLine("w1:p1"))
	wantStep(t, h.step(t), End, "w1:p1", "", ReasonAgentLeft)
	h.wantRunning(t, newer)
}

// A crash after the end of a launch and before the delete of its folder leaves the folder. The
// next reconcile deletes it.
func TestLaunchFolderAfterCrash(t *testing.T) {
	h := newHarness(t, bindings.KindMain)
	ended := h.launchIn(t, h.f.path, "w1:p1", "claude", time.Now().Add(-time.Hour))
	id := h.record(t, ended).ID
	ctx := context.Background()
	st, err := store.Open(ctx, h.store, busyTimeout)
	if err != nil {
		t.Fatal(err)
	}
	err = st.Write(ctx, func(tx *sql.Tx) error { return store.EndLaunch(ctx, tx, id, store.EndPaneClosed, time.Now()) })
	if err := errors.Join(err, st.Close()); err != nil {
		t.Fatal(err)
	}
	l := newLoop(h.d)
	defer l.wg.Wait()
	if err := l.check(ctx); err != nil {
		t.Fatal(err)
	}
	if err := l.reconcile(ctx, GapStart); err != nil {
		t.Fatal(err)
	}
	h.wantEnded(t, ended, store.EndPaneClosed)
}
