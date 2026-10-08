package cli

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/aryanVijaywargia/herdr-brain/internal/herdr"
	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

// E20-S10 #275 AC 2, AC 3: panes moved, split and closed during a session. A receipt joins
// its agent session, not its pane: the agent of w14:p1 moved to w15:p4 keeps its receipt, the
// closed pane w14:p2 is not live, and the new agent of the split pane w14:p3 has no receipt.
// brain status and brain doctor report only that gap, and brain status counts the capture
// gaps of the moved agent's session.
func TestStatusDoctorPaneChanges(t *testing.T) {
	f := newDoctorFixture(t)
	receipt(t, f.store, "default", "claude", "w14:p1", "s-moved", store.ResultEmitted, "")
	receipt(t, f.store, "default", "claude", "w14:p2", "s-closed", store.ResultEmitted, "")
	f.herdr.panes = []herdr.Pane{agent("w15:p4", "claude", "s-moved"), agent("w14:p3", "claude", "s-split")}
	// Capture of the moved agent: its text, the gap of the move and the gap of a lost event.
	s := openStore(t, f.store)
	write(t, s, func(tx *sql.Tx) error {
		k := store.SessionKey{HerdrSession: "default", Harness: "claude", Kind: store.KeyAgent, ID: "s-moved"}
		_, _, err := capture(context.Background(), tx, k, "w14:p1", "ws_a14", testNow,
			store.ObservationComplete, store.ObservationGap, store.ObservationGap)
		return err
	})
	closeStore(t, s)

	out := f.mustStatus(t)
	for _, want := range []string{"claude: 2 live: 1 emitted, 1 unknown\n", "    w14:p3 unknown: no receipt for its agent session\n",
		"claude: sessions observed 1, capture gaps 2;"} {
		if !strings.Contains(out, want) {
			t.Errorf("brain status:\n%s\nwant %q", out, want)
		}
	}
	for _, pane := range []string{"w15:p4", "w14:p2"} {
		if strings.Contains(out, "    "+pane+" ") {
			t.Errorf("brain status:\n%s\nwant no gap line of %s", out, pane)
		}
	}
	f.failed(t, "", "claude: hook not trusted or not firing: 1 live agent(s) with no Brain receipt (w14:p3)")
}
