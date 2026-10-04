package extract

import (
	"slices"
	"testing"
	"time"
)

// The proposals of the learned case take turns across workspaces, the key with the oldest
// extractor call first, so one workspace cannot use the whole daily cap (§7.3).
func TestRoundRobin(t *testing.T) {
	bcs := []bindingCandidate{{id: 1, key: "A"}, {id: 2, key: "A"}, {id: 3, key: "A"}, {id: 4, key: "B"}, {id: 5, key: "C"}}
	last := map[string]time.Time{"A": t0.Add(time.Hour), "B": t0}
	var got []int64
	for _, bc := range roundRobin(bcs, last) {
		got = append(got, bc.id)
	}
	if want := []int64{5, 4, 1, 2, 3}; !slices.Equal(got, want) {
		t.Errorf("roundRobin() = %v, want %v", got, want)
	}
}
