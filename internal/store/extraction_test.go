package store

import (
	"context"
	"database/sql"
	"testing"
	"time"
)

// AC #263: the last extractor call that worked is the newest batch with a model call that
// committed or gave a valid empty result. A failed batch and an empty batch with no call do
// not count.
func TestLastWorkedCall(t *testing.T) {
	s := openM1(t)
	ctx := context.Background()
	last := func() time.Time {
		t.Helper()
		var at time.Time
		write(t, s, func(tx *sql.Tx) (err error) {
			at, err = LastWorkedCall(ctx, tx)
			return err
		})
		return at
	}
	var sid int64
	write(t, s, func(tx *sql.Tx) (err error) {
		k := SessionKey{HerdrSession: "default", Harness: "claude", Kind: KeyAgent, ID: "a1"}
		sid, err = OpenSession(ctx, tx, k, "w1:p1", "ws", testNow)
		return err
	})
	batch := func(capture string, status BatchStatus, at time.Time) {
		t.Helper()
		write(t, s, func(tx *sql.Tx) error {
			b, err := StartBatch(ctx, tx, Batch{SessionID: sid, WorkspaceKey: "ws", FirstCapture: capture,
				LastCapture: capture}, at)
			if err != nil {
				return err
			}
			_, err = EndBatch(ctx, tx, b.ID, status, "", at)
			return err
		})
	}
	if at := last(); !at.IsZero() {
		t.Errorf("LastWorkedCall() = %v with no batch, want the zero time", at)
	}
	batch("c1", BatchFailed, testNow)
	if at := last(); !at.IsZero() {
		t.Errorf("LastWorkedCall() = %v after a failed batch, want the zero time", at)
	}
	batch("c2", BatchCommitted, testNow.Add(time.Minute))
	write(t, s, func(tx *sql.Tx) error {
		return EmptyBatch(ctx, tx, Batch{SessionID: sid, WorkspaceKey: "ws", FirstCapture: "c3", LastCapture: "c3"},
			testNow.Add(2*time.Minute))
	})
	if at := last(); !at.Equal(testNow.Add(time.Minute)) {
		t.Errorf("LastWorkedCall() = %v, want the committed batch at %v", at, testNow.Add(time.Minute))
	}
}
