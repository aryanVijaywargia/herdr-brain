package store

import (
	"context"
	"database/sql"
	"testing"
	"time"
)

// observe opens the session with key k in pane paneID at at, with one observation at obsAt,
// and returns the observation id.
func observe(ctx context.Context, tx *sql.Tx, k SessionKey, paneID string, at, obsAt time.Time) (int64, error) {
	session, err := OpenSession(ctx, tx, k, paneID, "", at)
	if err != nil {
		return 0, err
	}
	seg, err := StartSegment(ctx, tx, session, paneID, "", "visible", at)
	if err != nil {
		return 0, err
	}
	return AddObservation(ctx, tx, Observation{CaptureIdentity: k.HerdrSession + "/" + k.ID, Source: SourceHerdr,
		SegmentID: seg, Event: "idle", Excerpt: "no, use pnpm", Status: ObservationComplete, Counts: true, At: obsAt})
}

// receipt is an emitted receipt of item 1 at revision in binding, pane and agent session at at.
func receipt(binding, pane, agent string, revision int64, at time.Time) Delivery {
	d := Delivery{Profile: "claude", HerdrSession: &binding, PaneID: pane, Result: ResultEmitted,
		ItemRevisions: []RevisionRef{{Item: 1, Revision: revision}}, At: at}
	if agent != "" {
		d.AgentSessionID = &agent
	}
	return d
}

// AC: two bindings with the same pane ids and agent session ids: a receipt of one never ties
// to a session of the other. The last receipt before the observation wins.
func TestTiedReceiptBindings(t *testing.T) {
	ctx := context.Background()
	s := openM1(t)
	write(t, s, func(tx *sql.Tx) error {
		ids := map[string]int64{}
		for _, b := range []string{"default", "work"} {
			obs, err := observe(ctx, tx, SessionKey{HerdrSession: b, Harness: "claude", Kind: KeyAgent, ID: "a"}, "w1:p1",
				testNow, testNow.Add(10*time.Minute))
			if err != nil {
				return err
			}
			ids[b] = obs
		}
		// Only binding default has receipts: revision 1, then 2 after a compaction, then 3 after
		// the observation.
		for i, at := range []time.Duration{time.Minute, 5 * time.Minute, 20 * time.Minute} {
			if _, err := InsertDelivery(ctx, tx, receipt("default", "w1:p1", "a", int64(i+1), testNow.Add(at))); err != nil {
				return err
			}
		}
		d, ok, err := TiedReceipt(ctx, tx, ids["default"])
		if err != nil || !ok || d.ItemRevisions[0].Revision != 2 {
			t.Errorf("TiedReceipt(default) = %+v, %v, %v, want the receipt of revision 2", d, ok, err)
		}
		if d, ok, err := TiedReceipt(ctx, tx, ids["work"]); err != nil || ok {
			t.Errorf("TiedReceipt(work) = %+v, %v, %v, want none", d, ok, err)
		}
		return nil
	})
}

// A session that herdr gave no agent session id ties by its capture row: the receipts of its
// pane in its binding after the previous session of the pane, whose agent session id no other
// session holds. Two agent session ids among them, or no previous session of the pane: no
// single receipt ties.
func TestTiedReceiptCaptureRow(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		name     string
		receipts []Delivery
		want     int64 // the revision of the tied receipt; 0 for none
	}{
		{"one agent", []Delivery{receipt("default", "w1:p1", "x", 1, testNow.Add(time.Minute))}, 1},
		{"no agent id", []Delivery{receipt("default", "w1:p1", "", 2, testNow.Add(time.Minute))}, 2},
		{"two agents", []Delivery{receipt("default", "w1:p1", "x", 1, testNow.Add(time.Minute)),
			receipt("default", "w1:p1", "y", 2, testNow.Add(2*time.Minute))}, 0},
		{"another session's agent", []Delivery{receipt("default", "w1:p1", "old", 1, testNow.Add(time.Minute))}, 0},
		{"before the previous session ended", []Delivery{receipt("default", "w1:p1", "x", 1, testNow.Add(-30*time.Second))}, 0},
		{"another pane", []Delivery{receipt("default", "w1:p2", "x", 1, testNow.Add(time.Minute))}, 0},
		{"another binding", []Delivery{receipt("work", "w1:p1", "x", 1, testNow.Add(time.Minute))}, 0},
		{"no previous session", []Delivery{receipt("default", "w1:p1", "x", 1, testNow.Add(time.Minute))}, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := openM1(t)
			write(t, s, func(tx *sql.Tx) error {
				old := SessionKey{HerdrSession: "default", Harness: "claude", Kind: KeyAgent, ID: "old"}
				oldPane := "w1:p1"
				if c.name == "no previous session" {
					oldPane = "w1:p9"
				}
				if _, err := observe(ctx, tx, old, oldPane, testNow.Add(-time.Hour), testNow.Add(-time.Hour)); err != nil {
					return err
				}
				if err := EndSession(ctx, tx, old, testNow.Add(-10*time.Second)); err != nil {
					return err
				}
				obs, err := observe(ctx, tx, SessionKey{HerdrSession: "default", Harness: "claude", Kind: KeyCapture, ID: "cap-1"},
					"w1:p1", testNow, testNow.Add(10*time.Minute))
				if err != nil {
					return err
				}
				for _, d := range c.receipts {
					if _, err := InsertDelivery(ctx, tx, d); err != nil {
						return err
					}
				}
				d, ok, err := TiedReceipt(ctx, tx, obs)
				if err != nil {
					return err
				}
				var got int64
				if ok {
					got = d.ItemRevisions[0].Revision
				}
				if got != c.want {
					t.Errorf("TiedReceipt revision = %d (ok %v), want %d", got, ok, c.want)
				}
				return nil
			})
		})
	}
}
