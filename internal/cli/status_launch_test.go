package cli

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"github.com/aryanVijaywargia/herdr-brain/internal/adapter"
	"github.com/aryanVijaywargia/herdr-brain/internal/herdr"
	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

// testLaunch adds a running launch record of cli in pane on the herdr server socket, with the
// native session session and its capture source src when session is not "". ended ends it.
func testLaunch(t *testing.T, dir, socket, pane, cli, session string, src store.CaptureSource, ended bool) {
	t.Helper()
	s := openStore(t, dir)
	defer closeStore(t, s)
	ctx := context.Background()
	write(t, s, func(tx *sql.Tx) error {
		ws, _, _ := strings.Cut(pane, ":")
		id, err := store.InsertLaunch(ctx, tx, store.Launch{IDHash: fmt.Sprintf("%x", sha256.Sum256([]byte(socket+pane+cli))), Socket: socket,
			WorkspaceID: ws, PaneID: pane, CLI: cli, Started: testNow})
		if err != nil {
			return err
		}
		if session != "" {
			if err := store.SetLaunchSession(ctx, tx, id, session, src, testNow); err != nil {
				return err
			}
		}
		if ended {
			return store.EndLaunch(ctx, tx, id, store.EndAgentExit, testNow)
		}
		return nil
	})
}

// liveSessions returns the "live sessions" block of the brain status output out.
func liveSessions(out string) string {
	_, block, _ := strings.Cut(out, "\nlive sessions:\n")
	block, _, _ = strings.Cut(block, "\n\n")
	return block
}

// E24-S8 AC: brain status shows the start and the capture source of each live session: a
// running launch of the pane with the source of its current native session, the global hook of
// the CLI with hook capture when every hook record entry is in place, or none with pane-screen
// capture only.
func TestStatusLiveSessions(t *testing.T) {
	f := newDoctorFixture(t)
	codex, err := adapter.LoadFS(testProfiles, "codex")
	if err != nil {
		t.Fatal(err)
	}
	// Codex has no global Brain hook any more.
	if _, err := f.installer(entryBin(t)).Uninstall(codex); err != nil {
		t.Fatal(err)
	}
	testLaunch(t, f.store, f.sock, "w14:p1", "claude", "s-1", store.CaptureHooks, false)
	testLaunch(t, f.store, f.sock, "w14:p3", "codex", "", "", false)
	testLaunch(t, f.store, f.sock, "w14:p4", "codex", "t-old", store.CaptureHooks, true)
	testLaunch(t, f.store, f.sock, "w15:p1", "codex", "t-1", store.CaptureTranscript, false)
	// A launch of another herdr server, and one of another CLI in the pane, do not count.
	testLaunch(t, f.store, "/elsewhere/herdr.sock", "w15:p2", "codex", "t-2", store.CaptureHooks, false)
	testLaunch(t, f.store, f.sock, "w16:p2", "codex", "t-3", store.CaptureHooks, false)
	f.herdr.panes = []herdr.Pane{
		agent("w14:p1", "claude", "s-1"),
		agent("w14:p2", "claude", "s-2"),
		agent("w14:p3", "codex", ""),
		agent("w14:p4", "codex", "t-new"),
		agent("w15:p1", "codex", "t-1"),
		agent("w15:p2", "codex", "t-2"),
		agent("w16:p1", "opencode", "o-1"),
		agent("w16:p2", "claude", "s-3"),
		{ID: "w16:p3", WorkspaceID: "w16", TabID: "w16:t1"},
	}
	want := `  w14:p1 claude: start brain launch, capture hooks
  w14:p2 claude: start global hook, capture hooks
  w14:p3 codex: start brain launch, capture screen
  w14:p4 codex: start none, capture screen
  w15:p1 codex: start brain launch, capture transcript
  w15:p2 codex: start none, capture screen
  w16:p1 opencode: start none, capture screen
  w16:p2 claude: start global hook, capture hooks`
	if got := liveSessions(f.mustStatus(t)); got != want {
		t.Errorf("live sessions:\n%s\nwant:\n%s", got, want)
	}

	// With the hook record entries gone, the global hook delivers but records nothing.
	f.dropRecords(t, "claude")
	if got := liveSessions(f.mustStatus(t)); !strings.Contains(got, "  w14:p2 claude: start global hook, capture screen\n") {
		t.Errorf("live sessions with the Claude hook records off:\n%s\nwant w14:p2 with pane-screen capture", got)
	}

	f.herdr.panes = nil
	if got := liveSessions(f.mustStatus(t)); got != "  no live agent" {
		t.Errorf("live sessions with no agent = %q, want no live agent", got)
	}
	f.herdr.panesErr = fmt.Errorf("timeout")
	if got := liveSessions(f.mustStatus(t)); got != "  unavailable: timeout" {
		t.Errorf("live sessions with no pane list = %q, want unavailable", got)
	}
}
