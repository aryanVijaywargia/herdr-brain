package brained

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
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
	if b, err := os.ReadFile(path); err != nil || string(b) != "/tmp/none" {
		t.Fatalf("record after SetupPending(true) = %q, %v; want the socket", b, err)
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
	if err := os.WriteFile(path, []byte(h.d.env.socket), 0o600); err != nil {
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

// #462 review: the binding is read again right before the popup, so a session that setup bound
// after the check gets no popup.
func TestPendingRechecksBinding(t *testing.T) {
	h, popups := pendingHarness(t)
	h.f.addPane("w1:p1", "")
	path := markPending(t, h)
	l := newLoop(h.d)
	if err := l.check(context.Background()); err != nil {
		t.Fatal(err)
	}
	b := bindings.Binding{Session: "s1", Socket: h.f.path, Store: h.store, Kind: bindings.KindMain, Created: time.Now()}
	if err := bindings.Write(h.d.bindingsFile, []bindings.Binding{b}); err != nil {
		t.Fatal(err)
	}
	l.pendingSetup(context.Background())
	if len(popups) != 0 {
		t.Errorf("pendingSetup after the binding = %d popups, want 0", len(popups))
	}
	if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("record after the binding: %v, want none", err)
	}
}

// #462 review: the record of another socket, or one older than pendingLife, is of an old
// session: brained drops it with no popup.
func TestPendingFromPreviousSession(t *testing.T) {
	for _, c := range []struct {
		name   string
		socket string // "" for the socket of the brained
		age    time.Duration
	}{
		{"another socket", "old.sock", 0},
		{"old record", "", pendingLife + time.Hour},
	} {
		t.Run(c.name, func(t *testing.T) {
			h, popups := pendingHarness(t)
			h.f.addPane("w1:p1", "")
			socket := h.d.env.socket
			if c.socket != "" {
				socket = filepath.Join(t.TempDir(), c.socket)
			}
			environ := []string{"HERDR_SESSION=" + h.d.env.session, "HERDR_SOCKET_PATH=" + socket,
				"HERDR_PLUGIN_STATE_DIR=" + h.d.env.stateDir, "HERDR_PLUGIN_ID=brain"}
			if err := SetupPending(environ, true); err != nil {
				t.Fatal(err)
			}
			path := pendingPath(h.d.env.stateDir, h.d.env.session)
			at := time.Now().Add(-c.age)
			if err := os.Chtimes(path, at, at); err != nil {
				t.Fatal(err)
			}
			l := newLoop(h.d)
			if err := l.check(context.Background()); err != nil {
				t.Fatal(err)
			}
			l.pendingSetup(context.Background())
			if len(popups) != 0 {
				t.Errorf("popups = %d, want 0", len(popups))
			}
			if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("record of an old session: %v, want none", err)
			}
		})
	}
}

// #462 review: brained opens the popup with the setup lock held, and opens none while brain
// start holds it.
func TestPendingSetupLock(t *testing.T) {
	h, popups := pendingHarness(t)
	h.f.addPane("w1:p1", "")
	path := markPending(t, h)
	lockFile := setupLockPath(h.d.env.stateDir, h.d.env.session)
	var heldAtOpen bool
	h.d.setup = func(context.Context) error {
		u, err := flock(lockFile, false)
		if err == nil {
			u()
		}
		heldAtOpen = errors.Is(err, syscall.EWOULDBLOCK)
		popups <- struct{}{}
		return nil
	}
	l := newLoop(h.d)
	unlock, err := flock(lockFile, false) // brain start holds it
	if err != nil {
		t.Fatal(err)
	}
	l.pendingSetup(context.Background())
	if _, err := os.Stat(path); len(popups) != 0 || err != nil {
		t.Fatalf("while start holds the lock: %d popups, record %v; want 0 and the record", len(popups), err)
	}
	unlock()
	l.pendingSetup(context.Background())
	if len(popups) != 1 || !heldAtOpen {
		t.Errorf("popups = %d, lock held at the open = %v; want 1 and true", len(popups), heldAtOpen)
	}
}
