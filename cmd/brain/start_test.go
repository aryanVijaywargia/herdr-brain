package main

import (
	"bufio"
	"context"
	"os"
	"path/filepath"
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

// fakeStart returns start deps with the real D7 lock check. Its spawn takes the lock as
// brained does and counts the starts; its openSetup counts the popups.
func fakeStart(t *testing.T, environ []string, stateDir string) (d startDeps, spawns, popups *int) {
	t.Helper()
	spawns, popups = new(int), new(int)
	d = startDeps{
		environ: environ,
		running: brained.Running,
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
