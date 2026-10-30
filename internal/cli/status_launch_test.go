package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aryanVijaywargia/herdr-brain/internal/adapter"
	"github.com/aryanVijaywargia/herdr-brain/internal/herdr"
	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

// testLaunch adds a running launch record of cli in pane on the herdr server socket, with the
// native session session and its capture source src when session is not "". ended ends it. It
// returns the record id.
func testLaunch(t *testing.T, dir, socket, pane, cli, session string, src store.CaptureSource, ended bool) (id int64) {
	t.Helper()
	s := openStore(t, dir)
	defer closeStore(t, s)
	ctx := context.Background()
	write(t, s, func(tx *sql.Tx) (err error) {
		ws, _, _ := strings.Cut(pane, ":")
		id, err = store.InsertLaunch(ctx, tx, store.Launch{IDHash: fmt.Sprintf("%x", sha256.Sum256([]byte(socket+pane+cli))), Socket: socket,
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
	return id
}

// launchReceipt adds a receipt with result r of the start hook of launch record id of cli in
// pane, for agent session session (E24-S4).
func launchReceipt(t *testing.T, dir string, id int64, cli, pane, session string, r store.Result) {
	t.Helper()
	s := openStore(t, dir)
	defer closeStore(t, s)
	write(t, s, func(tx *sql.Tx) error {
		_, err := store.InsertDelivery(context.Background(), tx, store.Delivery{Profile: cli, HerdrSession: ptr("default"),
			PaneID: pane, AgentSessionID: ptr(session), Result: r, At: testNow, LaunchID: &id})
		return err
	})
}

// liveSessions returns the "live sessions" block of the brain status output out.
func liveSessions(out string) string {
	_, block, _ := strings.Cut(out, "\nlive sessions:\n")
	block, _, _ = strings.Cut(block, "\n\n")
	return block
}

// E24-S8 AC: brain status shows the start, the capture source, the brief and the tools of each
// live session: a running launch of the pane with the source of its current native session,
// the global hook of the CLI with hook capture when every hook record entry is in place, or
// none with pane-screen capture only. The brief comes from the receipt of the launch for its
// current session, else of the agent session; the tools are on for a launch only.
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
	l1 := testLaunch(t, f.store, f.sock, "w14:p1", "claude", "s-1", store.CaptureHooks, false)
	l3 := testLaunch(t, f.store, f.sock, "w14:p3", "codex", "", "", false)
	testLaunch(t, f.store, f.sock, "w14:p4", "codex", "t-old", store.CaptureHooks, true)
	l5 := testLaunch(t, f.store, f.sock, "w15:p1", "codex", "t-1", store.CaptureTranscript, false)
	launchReceipt(t, f.store, l1, "claude", "w14:p1", "s-1", store.ResultEmitted)
	// The launch has no native session yet: the agent session of herdr names the receipt.
	launchReceipt(t, f.store, l3, "codex", "w14:p3", "t-9", store.ResultEmitted)
	// The receipt of an older session of the launch does not count for its current session.
	launchReceipt(t, f.store, l5, "codex", "w15:p1", "t-0", store.ResultEmitted)
	// A session of a global hook has the receipt of its agent session.
	receipt(t, f.store, "default", "claude", "w14:p2", "s-2", store.ResultWithheld, "not a member")
	// A launch of another herdr server, and one of another CLI in the pane, do not count.
	testLaunch(t, f.store, "/elsewhere/herdr.sock", "w15:p2", "codex", "t-2", store.CaptureHooks, false)
	testLaunch(t, f.store, f.sock, "w16:p2", "codex", "t-3", store.CaptureHooks, false)
	f.herdr.panes = []herdr.Pane{
		agent("w14:p1", "claude", "s-1"),
		agent("w14:p2", "claude", "s-2"),
		agent("w14:p3", "codex", "t-9"),
		agent("w14:p4", "codex", "t-new"),
		agent("w15:p1", "codex", "t-1"),
		agent("w15:p2", "codex", "t-2"),
		agent("w16:p1", "opencode", "o-1"),
		agent("w16:p2", "claude", "s-3"),
		{ID: "w16:p3", WorkspaceID: "w16", TabID: "w16:t1"},
	}
	want := `  w14:p1 claude: start brain launch, capture hooks, brief delivered, tools on
  w14:p2 claude: start global hook, capture hooks, brief not delivered (withheld: not a member), tools off
  w14:p3 codex: start brain launch, capture screen, brief delivered, tools on
  w14:p4 codex: start none, capture screen, brief not delivered (no receipt), tools off
  w15:p1 codex: start brain launch, capture transcript, brief not delivered (no receipt), tools on
  w15:p2 codex: start none, capture screen, brief not delivered (no receipt), tools off
  w16:p1 opencode: start none, capture screen, brief not delivered (no receipt), tools off
  w16:p2 claude: start global hook, capture hooks, brief not delivered (no receipt), tools off`
	if got := liveSessions(f.mustStatus(t)); got != want {
		t.Errorf("live sessions:\n%s\nwant:\n%s", got, want)
	}

	// With the hook record entries gone, the global hook delivers but records nothing.
	f.dropRecords(t, "claude")
	if got := liveSessions(f.mustStatus(t)); !strings.Contains(got, "  w14:p2 claude: start global hook, capture screen,") {
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

// Review of #448, finding 4: a hook record entry that you edited may not run, so the global
// hook gives no hook capture.
func TestStatusEditedRecordCapture(t *testing.T) {
	f := newDoctorFixture(t)
	file := filepath.Join(f.home, ".claude", "settings.json")
	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, bytes.ReplaceAll(b, []byte("--event tool"), []byte("--event tool --bad")), 0o600); err != nil {
		t.Fatal(err)
	}
	f.herdr.panes = []herdr.Pane{agent("w14:p1", "claude", "s-1")}
	out := f.mustStatus(t)
	if got := liveSessions(out); got != "  w14:p1 claude: start global hook, capture screen, brief not delivered (no receipt), tools off" {
		t.Errorf("live sessions = %q, want the global hook with pane-screen capture", got)
	}
	if !strings.Contains(out, "  claude 2.1.283: configured, unverified; hook records on, but you edited an entry\n") {
		t.Errorf("brain status:\n%s\nwant the edited hook records", out)
	}
}

// Review of #448, finding 5: the global hook start comes from the hook config, also for a CLI
// that is not on the PATH of brain status.
func TestStatusGlobalHookWithoutPath(t *testing.T) {
	f := newDoctorFixture(t)
	if err := os.Remove(filepath.Join(filepath.Dir(f.store), "bin", "claude")); err != nil {
		t.Fatal(err)
	}
	f.herdr.panes = []herdr.Pane{agent("w14:p1", "claude", "s-1")}
	if got := liveSessions(f.mustStatus(t)); got != "  w14:p1 claude: start global hook, capture hooks, brief not delivered (no receipt), tools off" {
		t.Errorf("live sessions = %q, want the global hook with hook capture", got)
	}
}
