package store

import (
	"context"
	"database/sql"
	"slices"
	"strings"
	"testing"
	"time"
)

func testLaunch(hash, socket, pane string) Launch {
	return Launch{IDHash: strings.Repeat(hash, 64), Socket: socket, WorkspaceID: "w1", PaneID: pane, CLI: "claude", Started: testNow, PID: 4242, Server: "1:2:3"}
}

// E24-S3: a launch record reads back by its ID hash; the running records of a herdr server
// are those with no end, and a record ends once.
func TestLaunches(t *testing.T) {
	ctx := context.Background()
	s := openM1(t)
	write(t, s, func(tx *sql.Tx) error {
		var ids []int64
		for _, l := range []Launch{testLaunch("a", "/s1", "w1:p1"), testLaunch("b", "/s1", "w1:p2"), testLaunch("c", "/s2", "w1:p1")} {
			id, err := InsertLaunch(ctx, tx, l)
			if err != nil {
				return err
			}
			ids = append(ids, id)
		}
		got, ok, err := LaunchByHash(ctx, tx, strings.Repeat("a", 64))
		want := testLaunch("a", "/s1", "w1:p1")
		want.ID = ids[0]
		if err != nil || !ok || got != want {
			t.Errorf("LaunchByHash(a) = %+v, %v, %v, want %+v", got, ok, err, want)
		}
		if got, ok, err := LaunchByID(ctx, tx, ids[0]); err != nil || !ok || got != want {
			t.Errorf("LaunchByID(%d) = %+v, %v, %v, want %+v", ids[0], got, ok, err, want)
		}
		if _, ok, err := LaunchByID(ctx, tx, ids[2]+1); ok || err != nil {
			t.Errorf("LaunchByID(none) = %v, %v, want none", ok, err)
		}
		if _, ok, err := LaunchByHash(ctx, tx, strings.Repeat("d", 64)); ok || err != nil {
			t.Errorf("LaunchByHash(d) = %v, %v, want none", ok, err)
		}
		end := testNow.Add(time.Minute)
		for i, r := range []EndReason{EndPaneClosed, EndAgentExit} {
			if err := EndLaunch(ctx, tx, ids[0], r, end.Add(time.Duration(i)*time.Minute)); err != nil {
				t.Errorf("EndLaunch() %d = %v", i, err)
			}
		}
		got, _, err = LaunchByHash(ctx, tx, strings.Repeat("a", 64))
		if err != nil || !got.Ended.Equal(end) || got.EndReason != EndPaneClosed {
			t.Errorf("ended launch = %+v, %v, want the first end, at %v by pane close", got, err, end)
		}
		running, err := RunningLaunches(ctx, tx, "/s1")
		if err != nil || len(running) != 1 || running[0].ID != ids[1] {
			t.Errorf("RunningLaunches(/s1) = %+v, %v, want only launch %d", running, err, ids[1])
		}
		return nil
	})
}

// E24-S5: an ended launch waits for its capture until brained notes it, and only for the
// window from since; a running launch and the launch of another herdr server do not.
func TestLaunchesToCapture(t *testing.T) {
	ctx := context.Background()
	s := openM1(t)
	write(t, s, func(tx *sql.Tx) error {
		var ids []int64
		for _, l := range []Launch{testLaunch("a", "/s1", "w1:p1"), testLaunch("b", "/s1", "w1:p2"),
			testLaunch("c", "/s1", "w1:p3"), testLaunch("d", "/s2", "w1:p1"), testLaunch("e", "/s1", "w1:p4")} {
			l.CodexHome = "/home/me/.codex"
			id, err := InsertLaunch(ctx, tx, l)
			if err != nil {
				return err
			}
			ids = append(ids, id)
		}
		for i, at := range []time.Time{testNow, testNow.Add(-2 * time.Hour), testNow, testNow} {
			if err := EndLaunch(ctx, tx, ids[i], EndAgentExit, at); err != nil {
				return err
			}
		}
		if err := SetLaunchCaptured(ctx, tx, ids[2], testNow); err != nil {
			return err
		}
		got, err := LaunchesToCapture(ctx, tx, "/s1", testNow.Add(-time.Hour))
		if err != nil || len(got) != 1 || got[0].ID != ids[0] || got[0].CodexHome != "/home/me/.codex" || !got[0].Captured.IsZero() {
			t.Errorf("LaunchesToCapture() = %+v, %v, want only launch %d", got, err, ids[0])
		}
		if l, _, err := LaunchByID(ctx, tx, ids[2]); err != nil || !l.Captured.Equal(testNow) {
			t.Errorf("captured launch = %+v, %v, want captured at %v", l, err, testNow)
		}
		return nil
	})
}

// E24-S5: each new native session of a launch is accepted and becomes the current one, as
// after /clear. A known session keeps its first time, and its capture source only rises.
func TestSetLaunchSession(t *testing.T) {
	ctx := context.Background()
	s := openM1(t)
	write(t, s, func(tx *sql.Tx) error {
		id, err := InsertLaunch(ctx, tx, testLaunch("a", "/s1", "w1:p1"))
		if err != nil {
			return err
		}
		other, err := InsertLaunch(ctx, tx, testLaunch("b", "/s1", "w1:p2"))
		if err != nil {
			return err
		}
		for i, c := range []struct {
			session string
			src     CaptureSource
			current string
		}{
			{"s-1", CaptureScreen, "s-1"},
			{"s-1", CaptureHooks, "s-1"},
			{"s-2", CaptureTranscript, "s-2"},
			{"s-1", CaptureScreen, "s-1"}, // no fall back from hooks
			{"s-2", CaptureScreen, "s-2"}, // no fall back from transcript
		} {
			if err := SetLaunchSession(ctx, tx, id, c.session, c.src, testNow.Add(time.Duration(i)*time.Second)); err != nil {
				t.Fatalf("SetLaunchSession(%s, %s) = %v", c.session, c.src, err)
			}
			if l, _, err := LaunchByID(ctx, tx, id); err != nil || l.NativeSessionID != c.current {
				t.Errorf("after SetLaunchSession(%s): current session = %q, %v, want %q", c.session, l.NativeSessionID, err, c.current)
			}
		}
		got, err := LaunchSessions(ctx, tx, id)
		want := []LaunchSession{{"s-1", CaptureHooks, testNow}, {"s-2", CaptureTranscript, testNow.Add(2 * time.Second)}}
		if err != nil || !slices.Equal(got, want) {
			t.Errorf("LaunchSessions() = %+v, %v, want %+v", got, err, want)
		}
		if got, err := LaunchSessions(ctx, tx, other); err != nil || len(got) != 0 {
			t.Errorf("LaunchSessions(other) = %+v, %v, want none", got, err)
		}
		// AddLaunchSession raises a source and adds a session, and keeps the current session.
		if err := AddLaunchSession(ctx, tx, id, "s-3", CaptureScreen, testNow.Add(time.Minute)); err != nil {
			t.Fatal(err)
		}
		if err := AddLaunchSession(ctx, tx, id, "s-2", CaptureHooks, testNow.Add(time.Minute)); err != nil {
			t.Fatal(err)
		}
		if l, _, err := LaunchByID(ctx, tx, id); err != nil || l.NativeSessionID != "s-2" {
			t.Errorf("after AddLaunchSession: current session = %q, %v, want s-2", l.NativeSessionID, err)
		}
		got, err = LaunchSessions(ctx, tx, id)
		want = []LaunchSession{{"s-1", CaptureHooks, testNow}, {"s-2", CaptureHooks, testNow.Add(2 * time.Second)},
			{"s-3", CaptureScreen, testNow.Add(time.Minute)}}
		if err != nil || !slices.Equal(got, want) {
			t.Errorf("LaunchSessions() = %+v, %v, want %+v", got, err, want)
		}
		if err := SetLaunchSession(ctx, tx, id, "", CaptureHooks, testNow); err == nil {
			t.Error(`SetLaunchSession("") = nil, want an error`)
		}
		if err := AddLaunchSession(ctx, tx, id, "", CaptureHooks, testNow); err == nil {
			t.Error(`AddLaunchSession("") = nil, want an error`)
		}
		if err := SetLaunchSession(ctx, tx, other+1, "s-1", CaptureHooks, testNow); err == nil {
			t.Error("SetLaunchSession(no record) = nil, want an error")
		}
		return nil
	})
}
