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
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/aryanVijaywargia/herdr-brain/internal/bindings"
)

// TestMain lets the test binary run as `brain brained` for TestRestartRunsNewBinary and
// TestReinstallRunsNewRoot. Each start writes "<pid> <inode of its binary> <HERDR_PLUGIN_ROOT>"
// to BRAINED_TEST_LOG, then runs Main.
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
		err = Main(context.Background(), os.Environ(), os.Stderr, nil)
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
	if _, err := fmt.Fprintf(f, "%d %d %s\n", os.Getpid(), ino, os.Getenv("HERDR_PLUGIN_ROOT")); err != nil {
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
	bin := filepath.Join(dir, "bin", "brain")
	log, environ := filepath.Join(dir, "starts"), helperEnv(dir, dir, f.path)
	pid, done := startHelper(t, bin, log, environ)

	first := waitStarts(t, log, 1, done)
	waitLock(t, environ, done)
	copyFile(t, self(t), bin) // the new binary: same content, new file
	ino, err := inode(bin)
	if err != nil {
		t.Fatal(err)
	}
	got := waitStarts(t, log, 2, done)
	want := fmt.Sprintf("%d %d %s", pid, ino, dir)
	if got[0] != first[0] || got[1] != want {
		t.Errorf("brained starts = %q, want a second start %q in the same process", got, want)
	}
	waitLock(t, environ, done)
	if err := Main(context.Background(), environ, io.Discard, nil); !errors.Is(err, ErrRunning) {
		t.Errorf("second Main after the restart = %v, want ErrRunning", err)
	}
}

// #468: herdr 0.9.5 puts a reinstall in a new checkout folder and keeps the old one. When herdr
// lists Brain in the new plugin root, brained runs the binary there in the same process, with
// that root in HERDR_PLUGIN_ROOT, and holds the session lock again.
func TestReinstallRunsNewRoot(t *testing.T) {
	f := newFakeHerdr(t)
	dir := t.TempDir()
	oldRoot, newRoot := filepath.Join(dir, "1-0", "checkout"), filepath.Join(dir, "2-0", "checkout")
	f.setPlugins(fmt.Sprintf(`[{"plugin_id":"brain","enabled":true,"plugin_root":%q}]`, oldRoot))
	log, environ := filepath.Join(dir, "starts"), helperEnv(dir, oldRoot, f.path)
	pid, done := startHelper(t, filepath.Join(oldRoot, "bin", "brain"), log, environ)

	first := waitStarts(t, log, 1, done)
	waitLock(t, environ, done)
	bin := filepath.Join(newRoot, "bin", "brain")
	if err := os.MkdirAll(filepath.Dir(bin), 0o700); err != nil {
		t.Fatal(err)
	}
	copyFile(t, self(t), bin)
	ino, err := inode(bin)
	if err != nil {
		t.Fatal(err)
	}
	f.setPlugins(fmt.Sprintf(`[{"plugin_id":"brain","enabled":true,"plugin_root":%q}]`, newRoot))
	got := waitStarts(t, log, 2, done)
	want := fmt.Sprintf("%d %d %s", pid, ino, newRoot)
	if got[0] != first[0] || got[1] != want {
		t.Errorf("brained starts = %q, want a second start %q in the same process", got, want)
	}
	waitLock(t, environ, done)
}

// helperEnv is the [[startup]] environment of a brained with the plugin root root, on the herdr
// socket sock, with its home and state folder in dir.
func helperEnv(dir, root, sock string) []string {
	return []string{"HOME=" + filepath.Join(dir, "home"), "PATH=/usr/bin:/bin", "HERDR_SESSION=s1",
		"HERDR_SOCKET_PATH=" + sock, "HERDR_PLUGIN_STATE_DIR=" + filepath.Join(dir, "state"), "HERDR_PLUGIN_ID=brain",
		"HERDR_PLUGIN_ROOT=" + root}
}

// self is the path of this test binary.
func self(t *testing.T) string {
	t.Helper()
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return bin
}

// startHelper copies this test binary to bin and runs it as `bin brained` with environ, as the
// helper of TestMain that logs its starts to log. It returns the pid and the end of the process.
func startHelper(t *testing.T, bin, log string, environ []string) (int, chan error) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(bin), 0o700); err != nil {
		t.Fatal(err)
	}
	copyFile(t, self(t), bin)
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
	return cmd.Process.Pid, done
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

// #468: a check asks installed about the plugin root that herdr lists, and ends the run with a
// reinstallError for a root with another binary. A root with the same binary changes nothing,
// and a herdr that lists no root asks nothing.
func TestRunRestartsOnReinstall(t *testing.T) {
	h := newHarness(t, bindings.KindMain)
	var mu sync.Mutex
	var asked []string
	h.d.installed = func(root string) bool {
		mu.Lock()
		defer mu.Unlock()
		asked = append(asked, root)
		return root == "/new"
	}
	plugins := func(root string) {
		h.f.setPlugins(fmt.Sprintf(`[{"plugin_id":"brain","enabled":true,"plugin_root":%q}]`, root))
	}
	h.start(t)
	h.check(t) // the check of the start is over: it found no root
	plugins("/same")
	h.check(t)
	h.check(t) // the check of the tick before is over: it found /same
	plugins("/new")
	err := tickUntilEnd(t, h)
	var re *reinstallError
	if !errors.As(err, &re) || re.root != "/new" {
		t.Errorf("run = %v, want a reinstallError for /new", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if !slices.Contains(asked, "/same") || slices.Contains(asked, "") || asked[len(asked)-1] != "/new" {
		t.Errorf("installed asked about %q, want /same, then /new last, and no empty root", asked)
	}
}

// tickUntilEnd gives the run of h ticks until it ends, and returns its error.
func tickUntilEnd(t *testing.T, h *harness) error {
	t.Helper()
	deadline := time.After(wait)
	for {
		select {
		case h.tick <- time.Now():
		case err := <-h.done:
			h.done <- err // for the cleanup
			return err
		case <-deadline:
			t.Fatal("brained did not end")
		}
	}
}

func TestInstalled(t *testing.T) {
	dir := t.TempDir()
	own := putBinary(t, filepath.Join(dir, "1-0", "checkout", "bin", "brain"), "v1")
	fi, err := os.Stat(own)
	if err != nil {
		t.Fatal(err)
	}
	putBinary(t, filepath.Join(dir, "2-0", "checkout", "bin", "brain"), "v2")
	if err := os.MkdirAll(filepath.Join(dir, "3-0", "checkout", "bin"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(own, filepath.Join(dir, "3-0", "checkout", "bin", "brain")); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		root string
		want bool
	}{
		{"1-0", false}, // the binary that runs
		{"2-0", true},  // a new install
		{"3-0", false}, // a link to the binary that runs
		{"4-0", false}, // no binary yet
	} {
		if got := installed(filepath.Join(dir, c.root, "checkout"), fi); got != c.want {
			t.Errorf("installed(%s) = %t, want %t", c.root, got, c.want)
		}
	}
}

// putBinary writes data at path, with its folders, and returns path.
func putBinary(t *testing.T, path, data string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
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
