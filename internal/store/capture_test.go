package store

import (
	"context"
	"database/sql"
	"testing"
	"time"
)

// Two bindings with the same pane ids and the same agent session ids, plus an agent with no
// agent session id: a close, an enrich or a forget in one binding leaves the other intact
// (E11-S3).
func TestCaptureBindingsApart(t *testing.T) {
	ctx := context.Background()
	s := openM1(t)
	keys := func(session string) []SessionKey {
		return []SessionKey{
			{HerdrSession: session, Harness: "codex", Kind: KeyAgent, ID: "01a0ec3e-5e95-7253-aa8d-e7d232139183"},
			{HerdrSession: session, Harness: "claude", Kind: KeyCapture, ID: "cap-1"},
		}
	}
	a, b := keys("default"), keys("work")
	write(t, s, func(tx *sql.Tx) error {
		for _, k := range append(append([]SessionKey{}, a...), b...) {
			id, err := OpenSession(ctx, tx, k, "w1:p1", "", testNow)
			if err != nil {
				return err
			}
			seg, err := StartSegment(ctx, tx, id, "w1:p1", "", "visible", testNow)
			if err != nil {
				return err
			}
			o := Observation{CaptureIdentity: k.HerdrSession + k.ID, Source: SourceHerdr, SegmentID: seg,
				Event: "idle", Excerpt: "text", Status: ObservationComplete, At: testNow}
			if err := AddObservation(ctx, tx, o); err != nil {
				return err
			}
		}
		later := testNow.Add(time.Minute)
		if err := EndSession(ctx, tx, a[0], later); err != nil {
			return err
		}
		if err := EnrichSession(ctx, tx, a[1], "6a345ec1"); err != nil {
			return err
		}
		return ForgetSession(ctx, tx, a[1])
	})
	var ss []Session
	var segs []Segment
	var obs []Observation
	write(t, s, func(tx *sql.Tx) (err error) {
		if ss, err = Sessions(ctx, tx); err != nil {
			return err
		}
		if segs, err = Segments(ctx, tx); err != nil {
			return err
		}
		obs, err = Observations(ctx, tx)
		return err
	})
	if len(ss) != 3 || len(segs) != 3 || len(obs) != 3 {
		t.Fatalf("got %d sessions, %d segments, %d observations, want 3 of each", len(ss), len(segs), len(obs))
	}
	for _, x := range ss {
		switch x.Key {
		case a[0]:
			if x.Ended.IsZero() {
				t.Errorf("session %+v is open, want ended", x)
			}
		case b[0], b[1]:
			if !x.Ended.IsZero() || x.Key.Kind == KeyCapture && x.AgentSessionID != "" {
				t.Errorf("session %+v changed by the other binding", x)
			}
		default:
			t.Errorf("session %+v is not forgotten", x)
		}
	}
}

// An agent key opens its row again; a later id on a capture key merges no two sessions.
func TestOpenSessionAgain(t *testing.T) {
	ctx := context.Background()
	s := openM1(t)
	agent := SessionKey{HerdrSession: "default", Harness: "codex", Kind: KeyAgent, ID: "x"}
	capture := SessionKey{HerdrSession: "default", Harness: "codex", Kind: KeyCapture, ID: "cap-1"}
	write(t, s, func(tx *sql.Tx) error {
		first, err := OpenSession(ctx, tx, agent, "w1:p1", "", testNow)
		if err != nil {
			return err
		}
		if err := EndSession(ctx, tx, agent, testNow); err != nil {
			return err
		}
		again, err := OpenSession(ctx, tx, agent, "w2:p1", "k2", testNow)
		if err != nil {
			return err
		}
		if again != first {
			t.Errorf("OpenSession again = %d, want %d", again, first)
		}
		if _, err := OpenSession(ctx, tx, capture, "w1:p1", "", testNow); err != nil {
			return err
		}
		if err := EnrichSession(ctx, tx, capture, "x"); err != nil {
			return err
		}
		if _, err := OpenSession(ctx, tx, capture, "w1:p1", "", testNow); err == nil {
			t.Error("OpenSession of a used capture key = nil, want an error")
		}
		return nil
	})
	write(t, s, func(tx *sql.Tx) error {
		ss, err := Sessions(ctx, tx)
		if err != nil {
			return err
		}
		if len(ss) != 2 || !ss[0].Ended.IsZero() || ss[0].PaneID != "w2:p1" || ss[0].WorkspaceKey != "k2" ||
			ss[1].Key != capture || ss[1].AgentSessionID != "x" {
			t.Errorf("sessions = %+v, want the agent session open in w2:p1, and the capture session with id x", ss)
		}
		return nil
	})
}
