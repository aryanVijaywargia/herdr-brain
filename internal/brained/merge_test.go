package brained

import (
	"context"
	"testing"
	"time"

	"github.com/aryanVijaywargia/herdr-brain/internal/bindings"
)

// AC trigger (§9.5): brained starts the merge pass of the bound store at the first tick, and not
// again within a day, as the stale check.
func TestMergeTrigger(t *testing.T) {
	h := newHarness(t, bindings.KindMain)
	passes := make(chan Gate, 4)
	h.d.merge = func(_ context.Context, g Gate) error {
		passes <- g
		return nil
	}
	h.start(t)
	h.check(t)
	select {
	case g := <-passes:
		if g.Binding.Store != h.store {
			t.Errorf("merge pass store = %s, want %s", g.Binding.Store, h.store)
		}
	case <-time.After(wait):
		t.Fatal("no merge pass")
	}
	h.check(t)
	h.check(t)
	select {
	case <-passes:
		t.Error("a second merge pass started within a day")
	default:
	}
}
