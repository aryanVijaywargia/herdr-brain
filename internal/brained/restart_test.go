package brained

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/aryanVijaywargia/herdr-brain/internal/bindings"
)

// TestMain lets the test binary run as `brain brained` for TestRestartRunsNewBinary. Each
// start writes "<pid> <inode of its binary>" to BRAINED_TEST_LOG, then runs Main.
func TestMain(m *testing.M) {
	if log := os.Getenv("BRAINED_TEST_LOG"); log != "" {
		os.Exit(helper(log))
	}
	os.Exit(m.Run())
}

func helper(log string) int {
	bin, err := os.Executable()
	if err == nil {
		err = appendStart(log, bin)
	}
	if err == nil {
		err = Main(context.Background(), os.Environ(), os.Stderr)
	}
	fmt.Fprintf(os.Stderr, "brained helper: %v\n", err)
	return 1
}

func appendStart(log, bin string) error {
	ino, err := inode(bin)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(log, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(f, "%d %d\n", os.Getpid(), ino); err != nil {
		_ = f.Close() // the write error is the one to report
		return err
	}
	return f.Close()
}

func inode(path string) (uint64, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, fmt.Errorf("stat %s: no inode", path)
	}
	return uint64(st.Ino), nil //nolint:unconvert // Ino is uint64 on darwin and linux, but not on every GOARCH
}

// copyFile writes a copy of src at dst through a temp file and a rename, as an install does:
// dst gets a new inode.
func copyFile(t *testing.T, src, dst string) {
	t.Helper()
	in, err := os.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close() //nolint:errcheck // read-only: Close cannot lose data
	tmp := dst + ".new"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o700) //nolint:gosec // a test binary must be executable
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(out, in); err != nil {
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, dst); err != nil {
		t.Fatal(err)
	}
}

// starts returns the start lines of the helper log.
func starts(t *testing.T, log string) []string {
	t.Helper()
	b, err := os.ReadFile(log)
	if errors.Is(err, os.ErrNotExist) || len(b) == 0 {
		return nil // no start yet
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
}

// waitStarts waits until the helper log holds n starts.
func waitStarts(t *testing.T, log string, n int, done chan error) []string {
	t.Helper()
	tk := time.NewTicker(20 * time.Millisecond)
	defer tk.Stop()
	deadline := time.After(3 * checkEvery)
	for {
		if s := starts(t, log); len(s) >= n {
			return s
		}
		select {
		case <-tk.C:
		case err := <-done:
			done <- err // for the cleanup
			t.Fatalf("brained ended: %v", err)
		case <-deadline:
			t.Fatalf("brained starts = %q, want %d", starts(t, log), n)
		}
	}
}

// AC (E19-S4): after its binary file changes, brained runs the new binary in the same process
// and holds the session lock again. A second brained still stops with ErrRunning.
func TestRestartRunsNewBinary(t *testing.T) {
	f := newFakeHerdr(t)
	dir := t.TempDir()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "bin", "brain")
	if err := os.MkdirAll(filepath.Dir(bin), 0o700); err != nil {
		t.Fatal(err)
	}
	copyFile(t, self, bin)
	log, state := filepath.Join(dir, "starts"), filepath.Join(dir, "state")
	environ := []string{"HOME=" + filepath.Join(dir, "home"), "PATH=/usr/bin:/bin", "HERDR_SESSION=s1",
		"HERDR_SOCKET_PATH=" + f.path, "HERDR_PLUGIN_STATE_DIR=" + state, "HERDR_PLUGIN_ID=brain"}
	cmd := exec.Command(bin, "brained")
	cmd.Env = append(slices.Clone(environ), "BRAINED_TEST_LOG="+log)
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	done := make(chan error, 1)
	go func() {
		_, _ = io.Copy(&out, bufio.NewReader(stderr)) // ends at the exit of the helper
		done <- cmd.Wait()
	}()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		<-done
		t.Logf("brained stderr:\n%s", out.String())
	})

	first := waitStarts(t, log, 1, done)
	waitLock(t, environ, done)
	copyFile(t, self, bin) // the new binary: same content, new file
	ino, err := inode(bin)
	if err != nil {
		t.Fatal(err)
	}
	got := waitStarts(t, log, 2, done)
	want := fmt.Sprintf("%d %d", cmd.Process.Pid, ino)
	if got[0] != first[0] || got[1] != want {
		t.Errorf("brained starts = %q, want a second start %q in the same process", got, want)
	}
	waitLock(t, environ, done)
	if err := Main(context.Background(), environ, io.Discard); !errors.Is(err, ErrRunning) {
		t.Errorf("second Main after the restart = %v, want ErrRunning", err)
	}
}

// waitLock waits until a brained holds the lock of the session of environ.
func waitLock(t *testing.T, environ []string, done chan error) {
	t.Helper()
	tk := time.NewTicker(20 * time.Millisecond)
	defer tk.Stop()
	deadline := time.After(3 * checkEvery)
	for {
		ok, err := lockHeld(environ)
		if err != nil {
			t.Fatal(err)
		}
		if ok {
			return
		}
		select {
		case <-tk.C:
		case err := <-done:
			done <- err // for the cleanup
			t.Fatalf("brained ended: %v", err)
		case <-deadline:
			t.Fatal("no brained holds the session lock")
		}
	}
}

// lockHeld is Running with no window: a test lock that a starting brained would see.
func lockHeld(environ []string) (bool, error) {
	e, err := parseEnv(environ)
	if err != nil {
		return false, err
	}
	unlock, err := lock(e.stateDir, e.session)
	if errors.Is(err, ErrRunning) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	unlock()
	return false, nil
}

// A tick after a binary change ends the run with errRestart, before any other check.
func TestRunRestartsOnMove(t *testing.T) {
	h := newHarness(t, bindings.KindMain)
	moved := make(chan bool, 1)
	moved <- false
	h.d.moved = func() bool { return <-moved }
	h.start(t)
	h.check(t) // no change
	moved <- true
	h.check(t)
	if err := h.end(t); !errors.Is(err, errRestart) {
		t.Errorf("run = %v, want errRestart", err)
	}
}

func TestChanged(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "brain")
	write := func(s string) {
		if err := os.WriteFile(bin, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("v1")
	fi, err := os.Stat(bin)
	if err != nil {
		t.Fatal(err)
	}
	if changed(bin, fi) {
		t.Error("changed(same file) = true, want false")
	}
	if err := os.Remove(bin); err != nil {
		t.Fatal(err)
	}
	if changed(bin, fi) {
		t.Error("changed(missing file) = true, want false: a reinstall swaps the folder")
	}
	write("v2")
	if !changed(bin, fi) {
		t.Error("changed(new file) = false, want true")
	}
}
