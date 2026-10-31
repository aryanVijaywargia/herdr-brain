package brained

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/aryanVijaywargia/herdr-brain/internal/bindings"
)

// helperLog is the brained log of the TestMain helper: each line goes to stderr and to the file
// path. With pause set, the first line about a handoff ("brained runs") creates the file pause,
// then waits until the test opens the FIFO pause+".fifo" for writing and closes it (resume).
type helperLog struct{ path, pause string }

func (l *helperLog) Write(b []byte) (int, error) {
	if l.pause != "" && bytes.Contains(b, []byte("brained runs")) {
		if err := l.wait(); err != nil {
			return 0, err
		}
	}
	f, err := os.OpenFile(l.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return 0, err
	}
	if _, err := f.Write(b); err != nil {
		_ = f.Close() // the write error is the one to report
		return 0, err
	}
	if err := f.Close(); err != nil {
		return 0, err
	}
	return os.Stderr.Write(b)
}

// wait pauses once: after a restart, the new binary finds the file pause and goes on.
func (l *helperLog) wait() error {
	if _, err := os.Stat(l.pause); err == nil {
		return nil
	}
	if err := os.WriteFile(l.pause, nil, 0o600); err != nil {
		return err
	}
	fifo, err := os.Open(l.pause + ".fifo") // blocks until the test opens it to write
	if err != nil {
		return err
	}
	defer fifo.Close()                 //nolint:errcheck // read-only: Close cannot lose data
	_, err = io.Copy(io.Discard, fifo) // ends when the test closes it
	return err
}

// resume ends the pause of helperLog.
func resume(t *testing.T, pause string) {
	t.Helper()
	fifo, err := os.OpenFile(pause+".fifo", os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := fifo.Close(); err != nil {
		t.Fatal(err)
	}
}

// waitFor waits until ok reports true.
func waitFor(t *testing.T, what string, ok func() bool, done chan error) {
	t.Helper()
	tk := time.NewTicker(20 * time.Millisecond)
	defer tk.Stop()
	deadline := time.After(3 * checkEvery)
	for !ok() {
		select {
		case <-tk.C:
		case err := <-done:
			done <- err // for the cleanup
			t.Fatalf("brained ended before %s: %v", what, err)
		case <-deadline:
			t.Fatalf("no %s", what)
		}
	}
}

// logLines returns the lines of the helper log of log that hold text.
func logLines(t *testing.T, log, text string) []string {
	t.Helper()
	b, err := os.ReadFile(log + ".log")
	if errors.Is(err, os.ErrNotExist) {
		return nil // no line yet
	}
	if err != nil {
		t.Fatal(err)
	}
	return slices.DeleteFunc(strings.Split(string(b), "\n"), func(l string) bool { return !strings.Contains(l, text) })
}

// pluginRoot is the checkout of GitHub install n of Brain in the herdr folder in dir.
func pluginRoot(dir, n string) string {
	return filepath.Join(dir, "config", "herdr", "plugins", "github-installations", "brain-bbbf7a6412d6", n, "checkout")
}

// pluginList is the plugin.list JSON of the fake herdr with Brain in root.
func pluginList(root string) string {
	return fmt.Sprintf(`[{"plugin_id":"brain","enabled":true,"plugin_root":%q}]`, root)
}

// putSelf puts a copy of this test binary, which prints a brain version, at path.
func putSelf(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	copyFile(t, self(t), path)
}

// #468: herdr 0.9.5 puts a reinstall in a new checkout folder and keeps the old one. When herdr
// lists Brain in the new plugin root, brained runs the binary there in the same process, with
// that root in HERDR_PLUGIN_ROOT. The lock is never free during the handoff: a lock check of
// brain doctor at that moment finds brained, and the new binary keeps the lock.
func TestReinstallRunsNewRoot(t *testing.T) {
	f := newFakeHerdr(t)
	dir := t.TempDir()
	oldRoot, newRoot := pluginRoot(dir, "1-0"), pluginRoot(dir, "2-0")
	f.setPlugins(pluginList(oldRoot))
	log, environ := filepath.Join(dir, "starts"), helperEnv(dir, oldRoot, f.path)
	pause := filepath.Join(dir, "pause")
	if err := syscall.Mkfifo(pause+".fifo", 0o600); err != nil {
		t.Fatal(err)
	}
	pid, done := startHelper(t, rootBinary(oldRoot), log, append(slices.Clone(environ), "BRAINED_TEST_PAUSE="+pause))

	first := waitStarts(t, log, 1, done)
	waitLock(t, environ, done)
	putSelf(t, rootBinary(newRoot))
	ino, err := inode(rootBinary(newRoot))
	if err != nil {
		t.Fatal(err)
	}
	f.setPlugins(pluginList(newRoot))
	waitFor(t, "pause before the exec", func() bool { _, err := os.Stat(pause); return err == nil }, done)
	e, err := parseEnv(environ)
	if err != nil {
		t.Fatal(err)
	}
	if unlock, err := lock(e.stateDir, e.session); !errors.Is(err, ErrRunning) {
		t.Errorf("lock during the handoff = %v, want ErrRunning", err)
		if err == nil {
			defer unlock() // as brain doctor at that moment: the new binary would find the lock taken
		}
	}
	resume(t, pause)
	got := waitStarts(t, log, 2, done)
	want := fmt.Sprintf("%d %d %s", pid, ino, newRoot)
	if got[0] != first[0] || got[1] != want {
		t.Errorf("brained starts = %q, want a second start %q in the same process", got, want)
	}
	waitLock(t, environ, done)
	if err := Main(context.Background(), environ, io.Discard, nil); !errors.Is(err, ErrRunning) {
		t.Errorf("second Main after the handoff = %v, want ErrRunning", err)
	}
}

// #468: a new install whose binary does not run (a truncated file) gets one log line, and
// brained keeps running with its lock. When the file changes to a good binary, brained runs it.
func TestReinstallBadBinary(t *testing.T) {
	f := newFakeHerdr(t)
	dir := t.TempDir()
	oldRoot, newRoot := pluginRoot(dir, "1-0"), pluginRoot(dir, "2-0")
	f.setPlugins(pluginList(oldRoot))
	log, environ := filepath.Join(dir, "starts"), helperEnv(dir, oldRoot, f.path)
	pid, done := startHelper(t, rootBinary(oldRoot), log, environ)

	waitStarts(t, log, 1, done)
	waitLock(t, environ, done)
	bin := rootBinary(newRoot)
	if err := os.MkdirAll(filepath.Dir(bin), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin, []byte("not ready"), 0o700); err != nil { //nolint:gosec // a test binary must be executable
		t.Fatal(err)
	}
	f.setPlugins(pluginList(newRoot))
	waitFor(t, "log line of the bad binary", func() bool { return len(logLines(t, log, "keeps running")) > 0 }, done)
	waitLock(t, environ, done)
	if s := starts(t, log); len(s) != 1 {
		t.Errorf("brained starts = %q, want one", s)
	}

	putSelf(t, bin)
	ino, err := inode(bin)
	if err != nil {
		t.Fatal(err)
	}
	got := waitStarts(t, log, 2, done)
	if want := fmt.Sprintf("%d %d %s", pid, ino, newRoot); got[1] != want {
		t.Errorf("brained starts = %q, want a second start %q", got, want)
	}
	if lines := logLines(t, log, "keeps running"); len(lines) != 1 {
		t.Errorf("log lines of the bad binary = %q, want one", lines)
	}
}

// newHandoff returns a handoff of the brain binary of GitHub install 1-0 in dir, a path with no
// symlink, with its log.
func newHandoff(t *testing.T, dir string) (*handoff, *bytes.Buffer) {
	t.Helper()
	bin := rootBinary(pluginRoot(dir, "1-0"))
	putSelf(t, bin)
	fi, err := os.Stat(bin)
	if err != nil {
		t.Fatal(err)
	}
	var log bytes.Buffer
	return &handoff{bin: bin, fi: fi, environ: []string{"PATH=/usr/bin:/bin"}, log: &log,
		exec: func(string, []string, []string) error { return errors.New("no exec in this test") }}, &log
}

// #468: only a binary of another install of this plugin that only you can change, and that
// prints its version, replaces brained. A bad one gets one log line and no new check until its
// file changes.
func TestReinstalled(t *testing.T) {
	for _, c := range []struct {
		name string
		make func(t *testing.T, dir, bin string) // bin is rootBinary of the new root
		want bool
		line bool // a log line
	}{
		{"good binary", func(t *testing.T, _, bin string) { putSelf(t, bin) }, true, false},
		{"no binary yet", func(*testing.T, string, string) {}, false, false},
		{"the binary that runs", func(t *testing.T, dir, bin string) { symlink(t, rootBinary(pluginRoot(dir, "1-0")), bin) }, false, false},
		{"truncated binary", func(t *testing.T, _, bin string) { writeExe(t, bin, "not ready", 0o700) }, false, true},
		{"a folder", func(t *testing.T, _, bin string) { mkdir(t, bin) }, false, true},
		{"not executable", func(t *testing.T, _, bin string) { putSelf(t, bin); chmod(t, bin, 0o600) }, false, true},
		{"others can write it", func(t *testing.T, _, bin string) { putSelf(t, bin); chmod(t, bin, 0o777) }, false, true},
		{"others can write its folder", func(t *testing.T, _, bin string) { putSelf(t, bin); chmod(t, filepath.Dir(bin), 0o777) }, false, true},
		{"another plugin", func(t *testing.T, dir, bin string) {
			other := filepath.Join(dir, "config", "herdr", "plugins", "github-installations", "other-0a1b", "1-0", "checkout", "bin", "brain")
			putSelf(t, other)
			symlink(t, other, bin)
		}, false, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			h, log := newHandoff(t, dir)
			root := pluginRoot(dir, "2-0")
			mkdir(t, filepath.Join(root, "bin"))
			c.make(t, dir, rootBinary(root))
			for range 2 { // the second check of a bad binary logs nothing new
				err := h.reinstalled(context.Background(), root)
				var re *reinstallError
				if got := errors.As(err, &re); got != c.want || (got && (re.root != root || re.bin != rootBinary(root))) {
					t.Fatalf("reinstalled(%s) = %v, want a reinstallError: %t", root, err, c.want)
				}
			}
			want := 0
			if c.line {
				want = 1
			}
			if lines := strings.Count(log.String(), "\n"); lines != want {
				t.Errorf("log = %q, want %d lines", log.String(), want)
			}
		})
	}
}

// #468: when the exec of a new install fails, brained keeps its lock and serves again, and that
// binary gets no new try until its file changes. A binary that changed since its check is not
// run.
func TestNextExecFails(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h, log := newHandoff(t, dir)
	var calls [][]string
	h.exec = func(argv0 string, _ []string, envv []string) error {
		calls = append(calls, append([]string{argv0}, envv...))
		return syscall.ENOEXEC
	}
	state := filepath.Join(dir, "state")
	f, err := lockSession(state, "s1")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close() //nolint:errcheck // the close drops the lock; nothing is written
	root := pluginRoot(dir, "2-0")
	putSelf(t, rootBinary(root))
	ctx := context.Background()

	if err := h.next(f, h.reinstalled(ctx, root)); err != nil {
		t.Fatalf("next(failed exec) = %v, want nil: brained serves again", err)
	}
	if len(calls) != 1 || calls[0][0] != rootBinary(root) || !slices.Contains(calls[0], "HERDR_PLUGIN_ROOT="+root) ||
		!slices.ContainsFunc(calls[0], func(kv string) bool { return strings.HasPrefix(kv, lockFDEnv+"=") }) {
		t.Errorf("exec calls = %q, want one of the new binary with %s and the new root", calls, lockFDEnv)
	}
	if !strings.Contains(log.String(), "brained keeps running") {
		t.Errorf("log = %q, want the failed exec", log.String())
	}
	if _, err := lock(state, "s1"); !errors.Is(err, ErrRunning) {
		t.Errorf("lock after a failed exec = %v, want ErrRunning: f keeps it", err)
	}
	if err := h.reinstalled(ctx, root); err != nil {
		t.Errorf("reinstalled after a failed exec = %v, want nil until the file changes", err)
	}

	putSelf(t, rootBinary(root)) // a new file: a new try
	re := h.reinstalled(ctx, root)
	putSelf(t, rootBinary(root)) // and it changes again before the exec
	if err := h.next(f, re); err != nil || len(calls) != 1 {
		t.Errorf("next(changed binary) = %v with %d exec calls, want nil and no new exec", err, len(calls))
	}
}

// adopt takes over only the lock file of the session, from a descriptor that is open.
func TestAdopt(t *testing.T) {
	dir := t.TempDir()
	path := lockPath(dir, "s1")
	f, err := lockFile(path, false)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close() //nolint:errcheck // the close drops the lock; nothing is written
	fd, err := syscall.Dup(int(f.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	other, err := os.CreateTemp(dir, "other")
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close() //nolint:errcheck // nothing is written
	for _, bad := range []string{"x", "-1", "999999", strconv.Itoa(int(other.Fd()))} {
		if _, err := adopt(bad, path); err == nil {
			t.Errorf("adopt(%s) = nil error, want an error", bad)
		}
	}
	got, err := adopt(strconv.Itoa(fd), path)
	if err != nil {
		t.Fatalf("adopt(lock descriptor) = %v", err)
	}
	if err := got.Close(); err != nil {
		t.Fatal(err)
	}
}

// #468: a check asks reinstalled about the plugin root that herdr lists, and ends the run with
// its reinstallError. A herdr that lists no root asks nothing.
func TestRunRestartsOnReinstall(t *testing.T) {
	h := newHarness(t, bindings.KindMain)
	var mu sync.Mutex
	var asked []string
	h.d.reinstalled = func(_ context.Context, root string) error {
		mu.Lock()
		defer mu.Unlock()
		asked = append(asked, root)
		if root == "/new" {
			return &reinstallError{root: root}
		}
		return nil
	}
	h.start(t)
	h.check(t) // the check of the start is over: it found no root
	h.f.setPlugins(pluginList("/same"))
	h.check(t)
	h.check(t) // the check of the tick before is over: it found /same
	h.f.setPlugins(pluginList("/new"))
	err := tickUntilEnd(t, h)
	var re *reinstallError
	if !errors.As(err, &re) || re.root != "/new" {
		t.Errorf("run = %v, want a reinstallError for /new", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if !slices.Contains(asked, "/same") || slices.Contains(asked, "") || asked[len(asked)-1] != "/new" {
		t.Errorf("reinstalled asked about %q, want /same, then /new last, and no empty root", asked)
	}
}

func symlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}

func mkdir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
}

func chmod(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

func writeExe(t *testing.T, path, data string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(data), mode); err != nil {
		t.Fatal(err)
	}
}
