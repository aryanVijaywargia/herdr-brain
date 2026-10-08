package store

import (
	"context"
	"database/sql"
	"errors"
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
			if _, err := AddObservation(ctx, tx, o); err != nil {
				return err
			}
			if err := MoveCursor(ctx, tx, id, SourceHerdr, o.CaptureIdentity); err != nil {
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
		ss, err := SessionsByID(ctx, tx, "default", "6a345ec1")
		if err != nil || len(ss) != 1 || ss[0].Key != a[1] {
			t.Fatalf("SessionsByID(default, 6a345ec1) = %+v, %v, want the session of %+v", ss, err, a[1])
		}
		return ForgetSession(ctx, tx, ss[0].ID)
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
	// Forget keeps the session row and its segments: they hold no text (E11-S6).
	if len(ss) != 4 || len(segs) != 4 || len(obs) != 3 {
		t.Fatalf("got %d sessions, %d segments, %d observations, want 4, 4 and 3", len(ss), len(segs), len(obs))
	}
	for _, x := range ss {
		switch x.Key {
		case a[0]:
			if x.Ended.IsZero() || x.Generation != 0 {
				t.Errorf("session %+v is open or forgotten, want ended", x)
			}
		case a[1]:
			if x.Generation != 1 {
				t.Errorf("session %+v has generation %d after forget, want 1", x, x.Generation)
			}
		case b[0], b[1]:
			if !x.Ended.IsZero() || x.Generation != 0 || x.Key.Kind == KeyCapture && x.AgentSessionID != "" {
				t.Errorf("session %+v changed by the other binding", x)
			}
		}
	}
	for _, o := range obs {
		if o.CaptureIdentity == "default"+a[1].ID {
			t.Errorf("observation %+v of the forgotten session is kept", o)
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

// A known capture identity adds no row and does not count again (S34). Brain-origin text and
// a gap never count. The cursor keeps the last position of each source, and a forget drops it.
func TestAddObservationOnce(t *testing.T) {
	ctx := context.Background()
	s := openM1(t)
	key := SessionKey{HerdrSession: "default", Harness: "codex", Kind: KeyCapture, ID: "cap-1"}
	write(t, s, func(tx *sql.Tx) error {
		id, err := OpenSession(ctx, tx, key, "w1:p1", "", testNow)
		if err != nil {
			return err
		}
		seg, err := StartSegment(ctx, tx, id, "w1:p1", "", "visible", testNow)
		if err != nil {
			return err
		}
		for i, o := range []Observation{
			{CaptureIdentity: "a", Excerpt: "text", Status: ObservationComplete, Counts: true},
			{CaptureIdentity: "a", Excerpt: "other", Status: ObservationComplete, Counts: true},
			{CaptureIdentity: "b", Excerpt: "brief", Status: ObservationComplete, Counts: true, BrainOrigin: true,
				Sender: SenderAssistant},
		} {
			o.Source, o.SegmentID, o.Event, o.At = SourceHerdr, seg, "idle", testNow
			id, err := AddObservation(ctx, tx, o)
			if err != nil {
				return err
			}
			if added := id != 0; added != (i != 1) {
				t.Errorf("AddObservation %d = %d, want new: %v", i, id, i != 1)
			}
		}
		if seen, err := SeenExcerpt(ctx, tx, seg, SenderUnconfirmed, "text"); err != nil || !seen {
			t.Errorf("SeenExcerpt = %v, %v, want true", seen, err)
		}
		for _, pos := range []string{"a", "b"} {
			if err := MoveCursor(ctx, tx, id, SourceHerdr, pos); err != nil {
				return err
			}
		}
		if pos, err := Cursor(ctx, tx, id, SourceHerdr); err != nil || pos != "b" {
			t.Errorf("Cursor = %q, %v, want b", pos, err)
		}
		if pos, err := Cursor(ctx, tx, id, "hook"); err != nil || pos != "" {
			t.Errorf("Cursor of a source with no row = %q, %v, want empty", pos, err)
		}
		obs, err := Observations(ctx, tx)
		if err != nil {
			return err
		}
		if len(obs) != 2 || obs[0].Excerpt != "text" || !obs[0].Counts || obs[0].Sender != SenderUnconfirmed ||
			obs[1].Counts || !obs[1].BrainOrigin || obs[1].Sender != SenderAssistant {
			t.Errorf("observations = %+v, want text that counts and a Brain-origin text that does not", obs)
		}
		if err := ForgetSession(ctx, tx, id); err != nil {
			return err
		}
		if pos, err := Cursor(ctx, tx, id, SourceHerdr); err != nil || pos != "" {
			t.Errorf("Cursor after forget = %q, %v, want empty", pos, err)
		}
		return nil
	})
}

// Forget of one session raises its capture generation, deletes its evidence and keeps the
// capture identities: a write with the old generation fails, a replay of a deleted identity
// stores nothing, and new evidence after the forget is stored (E11-S6, §6.7).
func TestForgetSessionGeneration(t *testing.T) {
	ctx := context.Background()
	s := openM1(t)
	key := SessionKey{HerdrSession: "default", Harness: "claude", Kind: KeyCapture, ID: "cap-1"}
	var id, seg int64
	write(t, s, func(tx *sql.Tx) (err error) {
		if id, err = OpenSession(ctx, tx, key, "w1:p1", "", testNow); err != nil {
			return err
		}
		if seg, err = StartSegment(ctx, tx, id, "w1:p1", "", "visible", testNow); err != nil {
			return err
		}
		hook, err := AddObservation(ctx, tx, Observation{CaptureIdentity: "h", Source: SourceHook, SegmentID: seg,
			Event: "tool", Excerpt: "ls", Status: ObservationComplete, Sender: SenderTool, At: testNow})
		if err != nil {
			return err
		}
		if err := AddCommandRecord(ctx, tx, CommandRecord{ObservationID: hook, Argv: "ls", CWD: "/w"}); err != nil {
			return err
		}
		if _, err := AddObservation(ctx, tx, Observation{CaptureIdentity: "p", Source: SourceHerdr, SegmentID: seg,
			Event: "idle", Excerpt: "text", Status: ObservationComplete, At: testNow}); err != nil {
			return err
		}
		if e, err := SessionEvidence(ctx, tx, id); err != nil || e != (Evidence{Observations: 2, CommandRecords: 1}) {
			t.Errorf("SessionEvidence = %+v, %v, want 2 observations and 1 command record", e, err)
		}
		return nil
	})
	write(t, s, func(tx *sql.Tx) error {
		gen, err := Generation(ctx, tx, id)
		if err != nil || gen != 0 {
			t.Errorf("Generation before forget = %d, %v, want 0", gen, err)
		}
		if err := ForgetSession(ctx, tx, id); err != nil {
			return err
		}
		if err := CheckGeneration(ctx, tx, id, gen); !errors.Is(err, ErrForgotten) {
			t.Errorf("CheckGeneration with the old generation = %v, want ErrForgotten", err)
		}
		if err := CheckGeneration(ctx, tx, id, gen+1); err != nil {
			t.Errorf("CheckGeneration with the new generation = %v", err)
		}
		if e, err := SessionEvidence(ctx, tx, id); err != nil || e != (Evidence{}) {
			t.Errorf("SessionEvidence after forget = %+v, %v, want none", e, err)
		}
		// A replay of a deleted identity stores nothing; new evidence is stored.
		for _, tc := range []struct {
			identity string
			added    bool
		}{{"p", false}, {"h", false}, {"new", true}} {
			got, err := AddObservation(ctx, tx, Observation{CaptureIdentity: tc.identity, Source: SourceHerdr,
				SegmentID: seg, Event: "idle", Excerpt: "text", Status: ObservationComplete, At: testNow})
			if err != nil || (got != 0) != tc.added {
				t.Errorf("AddObservation(%s) after forget = %d, %v, want added: %v", tc.identity, got, err, tc.added)
			}
		}
		return nil
	})
}

// #317: an ended session with no agent session id and no evidence goes, with its segments, gap
// observations and cursors. A session with text, a hook record, an agent session id or a
// forget stays, and so does an open one.
func TestDropEmptySession(t *testing.T) {
	ctx := context.Background()
	gap := func(id string) Observation {
		return Observation{CaptureIdentity: id, Source: SourceHerdr, Event: "agent detected again", Status: ObservationGap, At: testNow}
	}
	for _, tc := range []struct {
		name string
		obs  []Observation
		// prep runs on the session before its end; nil for none.
		prep func(tx *sql.Tx, k SessionKey, id int64) error
		open bool // the session does not end
		kept bool
	}{
		{name: "no observation"},
		{name: "gaps only", obs: []Observation{gap("g1"), gap("g2")}},
		{name: "pane text", kept: true, obs: []Observation{gap("g1"),
			{CaptureIdentity: "t1", Source: SourceHerdr, Event: "idle", Excerpt: "• done", Status: ObservationComplete, At: testNow}}},
		{name: "hook record only", kept: true, obs: []Observation{
			{CaptureIdentity: "h1", Source: SourceHook, Event: "prompt", Excerpt: "fix it", Status: ObservationComplete, At: testNow}}},
		{name: "agent session id", kept: true, prep: func(tx *sql.Tx, k SessionKey, _ int64) error {
			return EnrichSession(ctx, tx, k, "6a345ec1")
		}},
		{name: "forgotten", kept: true, prep: func(tx *sql.Tx, _ SessionKey, id int64) error {
			return ForgetSession(ctx, tx, id)
		}},
		{name: "open", open: true, kept: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := openM1(t)
			k := SessionKey{HerdrSession: "default", Harness: "codex", Kind: KeyCapture, ID: "cap-1"}
			other := SessionKey{HerdrSession: "default", Harness: "codex", Kind: KeyCapture, ID: "cap-2"}
			var before, after int64
			write(t, s, func(tx *sql.Tx) (err error) {
				if _, err := OpenSession(ctx, tx, other, "w1:p2", "", testNow); err != nil {
					return err
				}
				id, err := OpenSession(ctx, tx, k, "w1:p1", "", testNow)
				if err != nil {
					return err
				}
				seg, err := StartSegment(ctx, tx, id, "w1:p1", "", "visible", testNow)
				if err != nil {
					return err
				}
				for _, o := range tc.obs {
					o.SegmentID = seg
					if _, err := AddObservation(ctx, tx, o); err != nil {
						return err
					}
					if err := MoveCursor(ctx, tx, id, o.Source, o.CaptureIdentity); err != nil {
						return err
					}
				}
				if tc.prep != nil {
					if err := tc.prep(tx, k, id); err != nil {
						return err
					}
				}
				if before, err = Forgets(ctx, tx); err != nil {
					return err
				}
				if !tc.open {
					if err := EndSession(ctx, tx, k, testNow.Add(time.Second)); err != nil {
						return err
					}
				}
				if err := DropEmptySession(ctx, tx, k); err != nil {
					return err
				}
				after, err = Forgets(ctx, tx)
				return err
			})
			var ss []Session
			var segs []Segment
			var c Coverage
			write(t, s, func(tx *sql.Tx) (err error) {
				if ss, err = Sessions(ctx, tx); err != nil {
					return err
				}
				if segs, err = Segments(ctx, tx); err != nil {
					return err
				}
				c, err = Observed(ctx, tx)
				return err
			})
			if before != after {
				t.Errorf("Forgets went from %d to %d", before, after)
			}
			if !tc.kept {
				if len(ss) != 1 || ss[0].Key != other || len(segs) != 0 || c != (Coverage{}) {
					t.Errorf("sessions %+v, segments %+v, coverage %+v: want only the other session", ss, segs, c)
				}
				return
			}
			if len(ss) != 2 || ss[1].Key != k || len(segs) != 1 {
				t.Errorf("sessions %+v, segments %+v: want the session and its segment kept", ss, segs)
			}
		})
	}
}
