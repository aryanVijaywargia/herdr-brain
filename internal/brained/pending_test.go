package brained

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aryanVijaywargia/herdr-brain/internal/bindings"
)

// #460: a pane has no HERDR_PLUGIN_ variables, so RunningSession finds the state folder as
// herdr 0.9.5 makes it.
func TestStateDir(t *testing.T) {
	for _, c := range []struct {
		name    string
		environ []string
		want    string
	}{
		{"plugin process", []string{"HERDR_PLUGIN_STATE_DIR=/s/plugins/brain", "XDG_STATE_HOME=/x", "HOME=/h"}, "/s/plugins/brain"},
		{"XDG_STATE_HOME", []string{"XDG_STATE_HOME=/x", "HOME=/h"}, "/x/herdr/plugins/brain"},
		{"HOME", []string{"HOME=/h"}, "/h/.local/state/herdr/plugins/brain"},
		{"relative XDG_STATE_HOME", []string{"XDG_STATE_HOME=x", "HOME=/h"}, "/h/.local/state/herdr/plugins/brain"},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := StateDir(c.environ)
			if err != nil || got != c.want {
				t.Errorf("StateDir(%q) = %q, %v; want %q", c.environ, got, err, c.want)
			}
		})
	}
	for _, environ := range [][]string{{"HOME=h"}, {"HERDR_PLUGIN_STATE_DIR=s", "HOME=/h"}} {
		if got, err := StateDir(environ); err == nil {
			t.Errorf("StateDir(%q) = %q, want an error", environ, got)
		}
	}
}

// #460: brain doctor and brain status see the brained of their session, and a check with no
// lock file makes no file.
func TestRunningSession(t *testing.T) {
	state := t.TempDir()
	environ := []string{"XDG_STATE_HOME=" + state, "HOME=/none"}
	dir := filepath.Join(state, "herdr", "plugins", "brain")
	if got, err := RunningSession(environ, "s1"); err != nil || got {
		t.Fatalf("RunningSession with no lock file = %v, %v; want false", got, err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("state folder after the check: %v, want none", err)
	}
	unlock, err := lock(dir, "s1")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := RunningSession(environ, "s1"); err != nil || !got {
		t.Errorf("RunningSession with a brained = %v, %v; want true", got, err)
	}
	if got, err := RunningSession(environ, "s2"); err != nil || got {
		t.Errorf("RunningSession of another session = %v, %v; want false", got, err)
	}
	unlock() // a killed brained drops its lock and leaves the file
	if got, err := RunningSession(environ, "s1"); err != nil || got {
		t.Errorf("RunningSession after the brained ended = %v, %v; want false", got, err)
	}
}

// SetupPending writes and removes the record of one session, and a removal of no record is
// no error.
func TestSetupPending(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	environ := []string{"HERDR_SESSION=s1", "HERDR_SOCKET_PATH=/tmp/none", "HERDR_PLUGIN_STATE_DIR=" + dir, "HERDR_PLUGIN_ID=brain"}
	path := pendingPath(dir, "s1")
	if err := SetupPending(environ, false); err != nil {
		t.Fatalf("SetupPending(false) with no record = %v", err)
	}
	if err := SetupPending(environ, true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("record after SetupPending(true): %v", err)
	}
	if err := SetupPending(environ, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("record after SetupPending(false): %v, want none", err)
	}
}

// pendingHarness is an unbound brained of session s1 whose setup counts the popups.
func pendingHarness(t *testing.T) (h *harness, popups chan struct{}) {
	t.Helper()
	h = newHarness(t, bindings.KindMain)
	if err := bindings.Write(h.d.bindingsFile, nil); err != nil {
		t.Fatal(err)
	}
	popups = make(chan struct{}, 4)
	h.d.setup = func(context.Context) error {
		popups <- struct{}{}
		return nil
	}
	return h, popups
}

// markPending writes the record of brain start for the brained of h.
func markPending(t *testing.T, h *harness) string {
	t.Helper()
	path := pendingPath(h.d.env.stateDir, h.d.env.session)
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// #462: brain start found no workspace, so brained opens the setup popup once, at the first
// check that finds a pane.
func TestPendingSetupOpensOnce(t *testing.T) {
	h, popups := pendingHarness(t)
	path := markPending(t, h)
	h.start(t)
	h.check(t)
	h.check(t) // the first check has ended: the loop takes one tick at a time
	if len(popups) != 0 {
		t.Fatalf("popups with no pane = %d, want 0", len(popups))
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("record with no pane: %v, want it kept", err)
	}
	h.f.addPane("w1:p1", "")
	h.check(t)
	h.check(t)
	h.check(t)
	if len(popups) != 1 {
		t.Errorf("popups = %d, want 1", len(popups))
	}
	if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("record after the popup: %v, want none", err)
	}
}

// #462: a session that got its binding drops the record with no popup, and a brained with no
// record opens none.
func TestPendingSetupNotInBoundSession(t *testing.T) {
	h, popups := pendingHarness(t)
	h.f.addPane("w1:p1", "")
	h.start(t)
	h.check(t)
	h.check(t)
	if len(popups) != 0 {
		t.Fatalf("popups with no record = %d, want 0", len(popups))
	}
	b := bindings.Binding{Session: "s1", Socket: h.f.path, Store: h.store, Kind: bindings.KindMain, Created: time.Now()}
	if err := bindings.Write(h.d.bindingsFile, []bindings.Binding{b}); err != nil {
		t.Fatal(err)
	}
	path := markPending(t, h)
	h.check(t)
	h.check(t)
	if len(popups) != 0 {
		t.Errorf("popups in a bound session = %d, want 0", len(popups))
	}
	if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("record in a bound session: %v, want none", err)
	}
}

// #462: a bindings file that brained cannot read does not say that the session is unbound, so
// brained opens no popup and keeps the record.
func TestPendingSetupBadBindings(t *testing.T) {
	h, popups := pendingHarness(t)
	h.f.addPane("w1:p1", "")
	if err := os.WriteFile(h.d.bindingsFile, []byte("{junk"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := markPending(t, h)
	h.start(t)
	h.check(t)
	h.check(t)
	if len(popups) != 0 {
		t.Errorf("popups with a bad bindings file = %d, want 0", len(popups))
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("record with a bad bindings file: %v, want it kept", err)
	}
}
