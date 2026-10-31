package main

import (
	"bufio"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/aryanVijaywargia/herdr-brain/internal/bindings"
	"github.com/aryanVijaywargia/herdr-brain/internal/brained"
	"github.com/aryanVijaywargia/herdr-brain/internal/paths"
)

// startEnv is the environment of the start action in herdr session s1, with a scratch HOME.
func startEnv(t *testing.T) (environ []string, stateDir, socket string) {
	t.Helper()
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	stateDir = filepath.Join(home, "state")
	socket = filepath.Join(home, "s1.sock")
	return []string{"HERDR_SESSION=s1", "HERDR_SOCKET_PATH=" + socket,
		"HERDR_PLUGIN_STATE_DIR=" + stateDir, "HERDR_PLUGIN_ID=brain"}, stateDir, socket
}

// fakeStart returns start deps with the real brained lock check and the real record of a
// pending popup. Its spawn takes the lock as brained does and counts the starts; the session
// has one workspace and no link to repair; its openSetup counts the popups.
func fakeStart(t *testing.T, environ []string, stateDir string) (d startDeps, spawns, popups *int) {
	t.Helper()
	spawns, popups = new(int), new(int)
	d = startDeps{
		environ:    environ,
		running:    brained.Running,
		relink:     func() error { return nil },
		workspaces: func(context.Context, []string) (int, error) { return 1, nil },
		lockSetup:  brained.SetupLocked,
		pending:    brained.SetupPending,
		spawn: func([]string) error {
			*spawns++
			f, err := os.OpenFile(filepath.Join(stateDir, "brained-s1.lock"), os.O_CREATE|os.O_RDWR, 0o600)
			if err != nil {
				return err
			}
			t.Cleanup(func() { _ = f.Close() }) // the close drops the lock
			return syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		},
		openSetup: func(context.Context, []string) error { *popups++; return nil },
	}
	return d, spawns, popups
}

// #239 AC: brain.start run twice starts one brained.
func TestStartTwiceStartsOneBrained(t *testing.T) {
	environ, stateDir, _ := startEnv(t)
	d, spawns, _ := fakeStart(t, environ, stateDir)
	for i := range 2 {
		if err := start(context.Background(), d); err != nil {
			t.Fatalf("start %d: %v", i+1, err)
		}
	}
	if *spawns != 1 {
		t.Errorf("brained starts = %d, want 1", *spawns)
	}
}

// #239 AC: start opens the setup popup only when this session has no binding.
func TestStartPopupOnlyWithoutBinding(t *testing.T) {
	for _, c := range []struct {
		name    string
		session string // the session of the binding; "" for no bindings file
		popups  int
	}{
		{"no binding", "", 1},
		{"this session bound", "s1", 0},
		{"another session bound", "s2", 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			environ, stateDir, socket := startEnv(t)
			if c.session != "" {
				writeTestBinding(t, c.session, socket)
			}
			d, _, popups := fakeStart(t, environ, stateDir)
			if err := start(context.Background(), d); err != nil {
				t.Fatal(err)
			}
			if *popups != c.popups {
				t.Errorf("popups = %d, want %d", *popups, c.popups)
			}
		})
	}
}

// #462: with no workspace, start opens no popup and leaves it to brained. A later start in a
// session with a workspace opens it and takes the record back, so brained opens no second one.
func TestStartNoWorkspace(t *testing.T) {
	environ, stateDir, _ := startEnv(t)
	d, _, popups := fakeStart(t, environ, stateDir)
	d.workspaces = func(context.Context, []string) (int, error) { return 0, nil }
	if err := start(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	record := filepath.Join(stateDir, "setup-s1.pending")
	if _, err := os.Stat(record); *popups != 0 || err != nil {
		t.Fatalf("start with no workspace: %d popups, record %v; want 0 popups and a record", *popups, err)
	}
	d.workspaces = func(context.Context, []string) (int, error) { return 1, nil }
	if err := start(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(record); *popups != 1 || !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("start with a workspace: %d popups, record %v; want 1 popup and no record", *popups, err)
	}
}

// #462: a bound session gets no popup and no record, and start asks herdr for no workspace.
func TestStartBoundNoRecord(t *testing.T) {
	environ, stateDir, socket := startEnv(t)
	writeTestBinding(t, "s1", socket)
	d, _, popups := fakeStart(t, environ, stateDir)
	d.workspaces = func(context.Context, []string) (int, error) {
		t.Error("start listed the workspaces of a bound session")
		return 0, nil
	}
	if err := start(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(stateDir, "setup-s1.pending")); *popups != 0 || !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("bound session: %d popups, record %v; want none", *popups, err)
	}
}

// #459: each start repairs the link, and a failed repair is reported but does not stop the popup.
func TestStartRelink(t *testing.T) {
	environ, stateDir, _ := startEnv(t)
	d, _, popups := fakeStart(t, environ, stateDir)
	cause := errors.New("read-only")
	relinks := 0
	d.relink = func() error { relinks++; return cause }
	err := start(context.Background(), d)
	if !errors.Is(err, cause) || !strings.HasPrefix(err.Error(), "relink: ") {
		t.Errorf("start = %v, want the relink error", err)
	}
	if relinks != 1 || *popups != 1 {
		t.Errorf("relinks = %d, popups = %d; want 1 and 1", relinks, *popups)
	}
}

// #459: relinkStale points a link of setup to an old plugin checkout to this binary, and
// leaves a link that Brain did not make.
func TestRelinkStale(t *testing.T) {
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(home, ".local", "bin", "brain")
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	install := func(n string) string {
		return filepath.Join(home, ".config", "herdr", "plugins", "github-installations", "brain-bbbf7a6412d6", n, "checkout", "bin", "brain")
	}
	bin := install("2-0")
	for _, c := range []struct {
		target, want string
	}{
		{install("1-0"), bin},
		{filepath.Join(home, "src", "brain"), filepath.Join(home, "src", "brain")},
	} {
		if err := os.RemoveAll(link); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(c.target, link); err != nil {
			t.Fatal(err)
		}
		if err := relinkStale(link, bin); err != nil {
			t.Fatalf("relinkStale() = %v", err)
		}
		if got, err := os.Readlink(link); err != nil || got != c.want {
			t.Errorf("link to %s after relinkStale = %q, %v; want %q", c.target, got, err, c.want)
		}
	}
}

// heldElsewhere reports whether another process could not take the setup lock of s1 now.
func heldElsewhere(t *testing.T, stateDir string) bool {
	t.Helper()
	f, err := os.OpenFile(filepath.Join(stateDir, "setup-s1.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close() //nolint:errcheck // a lock file: nothing is written
	err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	return errors.Is(err, syscall.EWOULDBLOCK)
}

// #462 review: start holds the setup lock over the record and the popup, so a second start or
// brained cannot open the popup at the same time. flock locks of one process conflict too, so
// a second open file in this test stands for the other process.
func TestStartSetupHoldsLock(t *testing.T) {
	for _, n := range []int{0, 1} {
		environ, stateDir, _ := startEnv(t)
		d, _, popups := fakeStart(t, environ, stateDir)
		var held []bool
		d.pending = func(environ []string, pending bool) error {
			held = append(held, heldElsewhere(t, stateDir))
			return brained.SetupPending(environ, pending)
		}
		d.openSetup = func(context.Context, []string) error {
			held = append(held, heldElsewhere(t, stateDir))
			*popups++
			return nil
		}
		d.workspaces = func(context.Context, []string) (int, error) { return n, nil }
		if err := start(context.Background(), d); err != nil {
			t.Fatal(err)
		}
		if len(held) != 1+n || slices.Contains(held, false) {
			t.Errorf("%d workspaces: lock held at each step = %v, want true at %d steps", n, held, 1+n)
		}
		if heldElsewhere(t, stateDir) {
			t.Errorf("%d workspaces: setup lock held after start", n)
		}
	}
}

// #462 review: a start that found no workspace writes its record before a second start can
// look, so the second start takes the record back and opens the popup: one popup, no record.
func TestStartLatePendingWrite(t *testing.T) {
	environ, stateDir, _ := startEnv(t)
	d, _, popups := fakeStart(t, environ, stateDir)
	d.running = func([]string) (bool, error) { return true, nil }
	second := d
	second.workspaces = func(context.Context, []string) (int, error) { return 1, nil }
	done := make(chan error, 1)
	d.workspaces = func(context.Context, []string) (int, error) {
		// The workspace appears, and the second start runs while this one holds the lock.
		go func() { done <- start(context.Background(), second) }()
		return 0, nil
	}
	if err := start(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(stateDir, "setup-s1.pending")); *popups != 1 || !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("popups = %d, record %v; want 1 popup and no record", *popups, err)
	}
}

// #462 review: setup can bind the session during the workspace call; then start opens no popup
// and leaves no record.
func TestStartBindingDuringWorkspaceCall(t *testing.T) {
	environ, stateDir, socket := startEnv(t)
	d, _, popups := fakeStart(t, environ, stateDir)
	d.workspaces = func(context.Context, []string) (int, error) {
		writeTestBinding(t, "s1", socket)
		return 1, nil
	}
	if err := start(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(stateDir, "setup-s1.pending")); *popups != 0 || !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("start after the session got its binding: %d popups, record %v; want none", *popups, err)
	}
}

func writeTestBinding(t *testing.T, session, socket string) {
	t.Helper()
	file, err := paths.BindingsFile()
	if err != nil {
		t.Fatal(err)
	}
	store, err := paths.MainStore()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(store, 0o700); err != nil {
		t.Fatal(err)
	}
	b := bindings.Binding{Session: session, Socket: socket, Store: store, Kind: bindings.KindMain, Created: time.Now()}
	if err := bindings.Write(file, []bindings.Binding{b}); err != nil {
		t.Fatal(err)
	}
}

// spawnBrained returns while `bin brained` still runs, with the environment it was given.
func TestSpawnBrainedDetached(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "brain")
	script := "#!/bin/sh\necho \"$$ $* $BRAIN_TEST\" > \"$FIFO\"\nexec sleep 30\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := spawnBrained(bin, append(os.Environ(), "FIFO="+fifo, "BRAIN_TEST=env")); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(fifo) // blocks until the child opens it to write
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close() //nolint:errcheck // read-only: Close cannot lose data
	line, err := bufio.NewReader(f).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	pid, rest, _ := strings.Cut(strings.TrimSpace(line), " ")
	if rest != "brained env" {
		t.Errorf("child args and env = %q, want %q", rest, "brained env")
	}
	n, err := strconv.Atoi(pid)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(n, 0); err != nil {
		t.Errorf("child %d after spawnBrained returned: %v, want it running", n, err)
	}
	_ = syscall.Kill(n, syscall.SIGKILL) // only the cleanup of the test child
}
