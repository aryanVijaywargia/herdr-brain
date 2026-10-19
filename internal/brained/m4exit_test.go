package brained

import (
	"context"
	"database/sql"
	"testing"

	"github.com/aryanVijaywargia/herdr-brain/internal/bindings"
	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

// M4 exit (E17-S12), the claim part, on one automation in order: a stop claims one run, a
// duplicate of it claims nothing, a stop after a pause claims nothing, a stop after the resume
// claims one run, and a stop after the removal claims nothing. The failure and Brain-origin
// parts are in the cli tests (TestLaunchFailed, TestLaunchOrigin, TestM4LearnedAutomation).
func TestS51Automation(t *testing.T) {
	h := newAutoHarness(t, bindings.KindMain)
	st := h.startPane(t, "idle")
	id := h.auto["ws_1"]
	seq := uint64(1)
	stop := func() {
		t.Helper()
		seq++
		h.set("w1:p1", "working", seq, "")
		h.event(t, st, "w1:p1", "working")
		seq++
		h.set("w1:p1", "idle", seq, "")
		h.event(t, st, "w1:p1", "idle")
	}
	claims := func(want int) {
		t.Helper()
		if rs := h.runs(t); len(rs) != want {
			t.Fatalf("runs = %+v, want %d", rs, want)
		}
	}

	stop()
	h.launch(t)
	h.event(t, st, "w1:p1", "idle") // a duplicate of the stop
	h.noLaunch(t)
	claims(1)

	h.tx(t, func(ctx context.Context, tx *sql.Tx) error { return store.SetAutomationEnabled(ctx, tx, id, false) })
	stop()
	h.noLaunch(t)
	claims(1)

	h.tx(t, func(ctx context.Context, tx *sql.Tx) error { return store.SetAutomationEnabled(ctx, tx, id, true) })
	stop()
	h.launch(t)
	claims(2)

	// brain automation remove retires the item in a new revision.
	h.tx(t, func(ctx context.Context, tx *sql.Tx) error {
		r, err := store.ItemRevisionByKey(ctx, tx, id, 1)
		if err != nil {
			return err
		}
		r.State = store.StateRetired
		_, err = store.WriteRevision(ctx, tx, 1, r)
		return err
	})
	stop()
	h.noLaunch(t)
	claims(2)
}
