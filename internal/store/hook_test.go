package store

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"testing"
	"time"
)

// A hook record joins the open segment of its pane, and a forget of the session drops its
// command records with its observations (E11-S5, E11-S3).
func TestHookRecordsForget(t *testing.T) {
	ctx := context.Background()
	s := openM1(t)
	key := SessionKey{HerdrSession: "default", Harness: "claude", Kind: KeyCapture, ID: "cap-1"}
	write(t, s, func(tx *sql.Tx) error {
		if _, seg, err := HookSegment(ctx, tx, "default", "claude", "w1:p1", ""); err != nil || seg != 0 {
			t.Errorf("HookSegment with no session = %d, %v, want 0", seg, err)
		}
		id, err := OpenSession(ctx, tx, key, "w1:p1", "", testNow)
		if err != nil {
			return err
		}
		seg, err := StartSegment(ctx, tx, id, "w1:p1", "", "visible", testNow)
		if err != nil {
			return err
		}
		if err := EnrichSession(ctx, tx, key, "agent-1"); err != nil {
			return err
		}
		for _, tc := range []struct {
			agent string
			want  int64
		}{{"", seg}, {"agent-1", seg}, {"agent-2", 0}} {
			if n, got, err := HookSegment(ctx, tx, "default", "claude", "w1:p1", tc.agent); err != nil || got != tc.want ||
				got != 0 && n != id {
				t.Errorf("HookSegment(%q) = %d, %d, %v, want %d", tc.agent, n, got, err, tc.want)
			}
		}
		pane, err := AddObservation(ctx, tx, Observation{CaptureIdentity: "p", Source: SourceHerdr, SegmentID: seg,
			Event: "idle", Excerpt: "⏺ Bash(ls)", Status: ObservationComplete, Sender: SenderAssistant, Counts: true, At: testNow})
		if err != nil {
			return err
		}
		hook, err := AddObservation(ctx, tx, Observation{CaptureIdentity: "h", Source: SourceHook, SegmentID: seg,
			Event: "tool", Excerpt: "ls", Status: ObservationComplete, Sender: SenderTool, Counts: true, At: testNow})
		if err != nil {
			return err
		}
		exit := 2
		if err := AddCommandRecord(ctx, tx, CommandRecord{ObservationID: hook, Argv: "ls", CWD: "/w", ExitStatus: &exit}); err != nil {
			return err
		}
		cs, err := Counterparts(ctx, tx, id, SourceHerdr, testNow.Add(time.Minute), 2*time.Minute)
		if err != nil || len(cs) != 1 || cs[0].ID != pane || cs[0].Sender != SenderAssistant || cs[0].Event != "idle" {
			t.Errorf("Counterparts = %+v, %v, want the pane row", cs, err)
		}
		if cs, err := Counterparts(ctx, tx, id, SourceHerdr, testNow.Add(time.Hour), 2*time.Minute); err != nil || len(cs) != 0 {
			t.Errorf("Counterparts outside the window = %+v, %v, want none", cs, err)
		}
		if err := Enrich(ctx, tx, pane, hook); err != nil {
			return err
		}
		// An enriched row and its hook are not counterparts again.
		for _, src := range []string{SourceHerdr, SourceHook} {
			if cs, err := Counterparts(ctx, tx, id, src, testNow, time.Minute); err != nil || len(cs) != 0 {
				t.Errorf("Counterparts of %s after Enrich = %+v, %v, want none", src, cs, err)
			}
		}
		obs, err := Observations(ctx, tx)
		if err != nil {
			return err
		}
		if len(obs) != 2 || obs[0].EnrichedBy != hook || obs[1].Counts {
			t.Errorf("observations = %+v, want the pane row enriched by the hook, and the hook not counted", obs)
		}
		cmds, err := CommandRecords(ctx, tx)
		if err != nil || len(cmds) != 1 || cmds[0].Argv != "ls" || cmds[0].ExitStatus == nil || *cmds[0].ExitStatus != 2 {
			t.Errorf("CommandRecords = %+v, %v", cmds, err)
		}
		return ForgetSession(ctx, tx, id)
	})
	write(t, s, func(tx *sql.Tx) error {
		obs, err := Observations(ctx, tx)
		if err != nil {
			return err
		}
		cmds, err := CommandRecords(ctx, tx)
		if err != nil {
			return err
		}
		if len(obs) != 0 || len(cmds) != 0 {
			t.Errorf("after the forget: %+v, %+v, want nothing", obs, cmds)
		}
		return nil
	})
}

// #349: HookPrompts gives the prompt records of one session: no pane row, no tool or stop
// record, no Brain-origin text and no record of another session.
func TestHookPrompts(t *testing.T) {
	ctx := context.Background()
	s := openM1(t)
	write(t, s, func(tx *sql.Tx) error {
		var segs []int64
		var ids []int64
		for _, k := range []string{"cap-1", "cap-2"} {
			id, err := OpenSession(ctx, tx, SessionKey{HerdrSession: "default", Harness: "codex", Kind: KeyCapture, ID: k}, "w1:p1", "", testNow)
			if err != nil {
				return err
			}
			seg, err := StartSegment(ctx, tx, id, "w1:p1", "", "recent_unwrapped", testNow)
			if err != nil {
				return err
			}
			ids, segs = append(ids, id), append(segs, seg)
		}
		var want []int64
		for i, o := range []Observation{
			{Source: SourceHook, SegmentID: segs[0], Event: "prompt", Excerpt: "fix the bug", Sender: SenderUnconfirmed},
			{Source: SourceHerdr, SegmentID: segs[0], Event: "working", Excerpt: "› fix the bug", Sender: SenderUnconfirmed},
			{Source: SourceHook, SegmentID: segs[0], Event: "tool", Excerpt: "npm test", Sender: SenderTool},
			{Source: SourceHook, SegmentID: segs[0], Event: "stop", Excerpt: "Fixed.", Sender: SenderAssistant},
			{Source: SourceHook, SegmentID: segs[0], Event: "prompt", Excerpt: "a brief", Sender: SenderUnconfirmed, BrainOrigin: true},
			{Source: SourceHook, SegmentID: segs[1], Event: "prompt", Excerpt: "other session", Sender: SenderUnconfirmed},
			{Source: SourceHook, SegmentID: segs[0], Event: "prompt", Excerpt: "run the tests", Sender: SenderUnconfirmed, Truncated: true},
		} {
			o.CaptureIdentity, o.Status, o.Counts, o.At = fmt.Sprint(i), ObservationComplete, true, testNow
			id, err := AddObservation(ctx, tx, o)
			if err != nil {
				return err
			}
			if o.Source == SourceHook && o.SegmentID == segs[0] && o.Event == "prompt" && !o.BrainOrigin {
				want = append(want, id)
			}
		}
		ps, err := HookPrompts(ctx, tx, ids[0])
		var got []int64
		for _, p := range ps {
			got = append(got, p.ID)
		}
		if err != nil || !slices.Equal(got, want) || !ps[1].Truncated || ps[0].Excerpt != "fix the bug" {
			t.Errorf("HookPrompts = %+v, %v, want the rows %v", ps, err, want)
		}
		return nil
	})
}
