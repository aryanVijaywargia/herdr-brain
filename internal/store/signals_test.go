package store

import (
	"context"
	"database/sql"
	"testing"
	"time"
)

// The signal rules read each observation with its session, its workspace and its command
// record, in time order (E12-S1).
func TestSignalInputs(t *testing.T) {
	ctx := context.Background()
	s := openM1(t)
	key := SessionKey{HerdrSession: "default", Harness: "codex", Kind: KeyAgent, ID: "agent-1"}
	var seg, prompt, tool int64
	write(t, s, func(tx *sql.Tx) error {
		id, err := OpenSession(ctx, tx, key, "w1:p1", "ws-1", testNow)
		if err != nil {
			return err
		}
		if seg, err = StartSegment(ctx, tx, id, "w1:p1", "ws-1", "recent_unwrapped", testNow); err != nil {
			return err
		}
		// The tool row is stored first but is later in time.
		if tool, err = AddObservation(ctx, tx, Observation{CaptureIdentity: "t", Source: SourceHook, SegmentID: seg,
			Event: "tool", Excerpt: "go test", Status: ObservationComplete, Sender: SenderTool, Counts: true,
			At: testNow.Add(time.Second)}); err != nil {
			return err
		}
		exit := 1
		if err := AddCommandRecord(ctx, tx, CommandRecord{ObservationID: tool, Argv: "go test", CWD: "/w", ExitStatus: &exit}); err != nil {
			return err
		}
		prompt, err = AddObservation(ctx, tx, Observation{CaptureIdentity: "p", Source: SourceHerdr, SegmentID: seg,
			Event: "working", Excerpt: "run the tests", Status: ObservationComplete, Sender: SenderUnconfirmed,
			Counts: true, At: testNow})
		return err
	})
	var ins []SignalInput
	write(t, s, func(tx *sql.Tx) (err error) {
		ins, err = SignalInputs(ctx, tx)
		return err
	})
	if len(ins) != 2 {
		t.Fatalf("SignalInputs = %+v, want 2 rows", ins)
	}
	p, c := ins[0], ins[1]
	if p.ID != prompt || p.Command != nil || p.WorkspaceKey != "ws-1" || p.SessionID == 0 || p.Excerpt != "run the tests" ||
		p.Sender != SenderUnconfirmed {
		t.Errorf("SignalInputs[0] = %+v, want the prompt with no command record", p)
	}
	if c.ID != tool || c.Command == nil || c.Command.ObservationID != tool || c.Command.Argv != "go test" ||
		c.Command.ExitStatus == nil || *c.Command.ExitStatus != 1 || c.SessionID != p.SessionID {
		t.Errorf("SignalInputs[1] = %+v (command %+v), want the tool row with its command record", c, c.Command)
	}
}
