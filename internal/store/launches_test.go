package store

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"
)

func testLaunch(hash, socket, pane string) Launch {
	return Launch{IDHash: strings.Repeat(hash, 64), Socket: socket, WorkspaceID: "w1", PaneID: pane, CLI: "claude", Started: testNow}
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

// E24-S3: the first native session id of a launch stays; a later one does not replace it.
func TestSetLaunchSession(t *testing.T) {
	ctx := context.Background()
	s := openM1(t)
	write(t, s, func(tx *sql.Tx) error {
		id, err := InsertLaunch(ctx, tx, testLaunch("a", "/s1", "w1:p1"))
		if err != nil {
			return err
		}
		for _, c := range []struct{ set, want string }{{"s-1", "s-1"}, {"s-1", "s-1"}, {"s-2", "s-1"}} {
			if got, err := SetLaunchSession(ctx, tx, id, c.set); got != c.want || err != nil {
				t.Errorf("SetLaunchSession(%s) = %q, %v, want %q", c.set, got, err, c.want)
			}
		}
		if _, err := SetLaunchSession(ctx, tx, id, ""); err == nil {
			t.Error(`SetLaunchSession("") = nil, want an error`)
		}
		if _, err := SetLaunchSession(ctx, tx, id+1, "s-1"); err == nil {
			t.Error("SetLaunchSession(no record) = nil, want an error")
		}
		return nil
	})
}
