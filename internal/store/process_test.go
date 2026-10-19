package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Exit codes of the helper process.
const (
	exitOK   = 0
	exitBusy = 3
)

// TestHelperProcess is not a test. The tests below run the test binary again as a second
// process that opens the same store; the helper does the work that BRAIN_STORE_HELPER names.
func TestHelperProcess(t *testing.T) {
	action := os.Getenv("BRAIN_STORE_HELPER")
	if action == "" {
		t.Skip("helper process only")
	}
	out, err := helper(action)
	fmt.Print(out)
	if errors.Is(err, ErrBusy) {
		os.Exit(exitBusy)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(exitOK)
}

// helper opens the store in BRAIN_STORE_DIR with the busy timeout BRAIN_STORE_TIMEOUT_MS.
// "write" commits BRAIN_STORE_ROWS rows, one per transaction, and prints how many it
// committed. "measure" does open, one insert and close, and prints the time in microseconds.
func helper(action string) (string, error) {
	ms, err := strconv.Atoi(os.Getenv("BRAIN_STORE_TIMEOUT_MS"))
	if err != nil {
		return "", err
	}
	timeout := time.Duration(ms) * time.Millisecond
	dir := os.Getenv("BRAIN_STORE_DIR")
	ctx := context.Background()
	start := time.Now()
	s, err := Open(ctx, dir, timeout)
	if err != nil {
		return "", err
	}
	if action == "measure" {
		err := errors.Join(insert(ctx, s, 1), s.Close())
		return strconv.FormatInt(time.Since(start).Microseconds(), 10), err
	}
	rows, err := strconv.Atoi(os.Getenv("BRAIN_STORE_ROWS"))
	if err != nil {
		return "", errors.Join(err, s.Close())
	}
	n := 0
	for ; n < rows; n++ {
		if err = insert(ctx, s, n); err != nil {
			break
		}
	}
	return strconv.Itoa(n), errors.Join(err, s.Close())
}

func insert(ctx context.Context, s *Store, n int) error {
	return s.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, "INSERT INTO t (pid, n) VALUES (?, ?)", os.Getpid(), n)
		return err
	})
}

// helperCmd returns a helper process for the store in dir.
func helperCmd(t *testing.T, action, dir string, timeout time.Duration, rows int) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperProcess$")
	cmd.Env = append(os.Environ(),
		"BRAIN_STORE_HELPER="+action,
		"BRAIN_STORE_DIR="+dir,
		"BRAIN_STORE_TIMEOUT_MS="+strconv.FormatInt(timeout.Milliseconds(), 10),
		"BRAIN_STORE_ROWS="+strconv.Itoa(rows),
	)
	cmd.Stderr = os.Stderr
	return cmd
}

// exitCode returns the exit code of a finished helper, or fails the test when it did not run.
func exitCode(t *testing.T, err error) int {
	t.Helper()
	var ee *exec.ExitError
	if err != nil && !errors.As(err, &ee) {
		t.Fatalf("run helper: %v", err)
	}
	if ee != nil {
		return ee.ExitCode()
	}
	return exitOK
}

// newTestStore makes a store with the embedded migrations and the test table t.
func newTestStore(t *testing.T) (dir string, s *Store) {
	t.Helper()
	dir = t.TempDir()
	s, err := Open(context.Background(), dir, testTimeout)
	if err != nil {
		t.Fatalf("Open() = %v", err)
	}
	err = s.Write(context.Background(), func(tx *sql.Tx) error {
		_, err := tx.Exec("CREATE TABLE t (pid INTEGER NOT NULL, n INTEGER NOT NULL)")
		return err
	})
	if err != nil {
		t.Fatalf("create table: %v", err)
	}
	return dir, s
}

func TestWriteBusyFromOtherProcess(t *testing.T) {
	dir, s := newTestStore(t)
	defer closeTest(t, s)

	// This process holds the write lock while the helper tries to write, so the helper
	// must give up after its busy timeout.
	var code int
	var out []byte
	err := s.Write(context.Background(), func(tx *sql.Tx) error {
		if _, err := tx.Exec("INSERT INTO t VALUES (0, 0)"); err != nil {
			return err
		}
		var err error
		out, err = helperCmd(t, "write", dir, 100*time.Millisecond, 1).Output() // 100 ms
		code = exitCode(t, err)
		return nil
	})
	if err != nil {
		t.Fatalf("Write() = %v", err)
	}
	if code != exitBusy {
		t.Errorf("helper exit code = %d, want %d (busy)", code, exitBusy)
	}
	if got := strings.TrimSpace(string(out)); got != "0" {
		t.Errorf("helper committed %s rows, want 0", got)
	}
	if got := queryInt(t, s, "SELECT count(*) FROM t"); got != 1 {
		t.Errorf("rows = %d, want 1", got)
	}
}

func TestTwoProcessesWrite(t *testing.T) {
	dir, s := newTestStore(t)
	defer closeTest(t, s)

	const rows = 200 // enough transactions for the two writers to overlap
	cmds := make([]*exec.Cmd, 2)
	outs := make([]strings.Builder, 2)
	for i := range cmds {
		cmds[i] = helperCmd(t, "write", dir, testTimeout, rows)
		cmds[i].Stdout = &outs[i]
		if err := cmds[i].Start(); err != nil {
			t.Fatalf("start helper: %v", err)
		}
	}
	committed := 0
	for i, cmd := range cmds {
		code := exitCode(t, cmd.Wait())
		if !slices.Contains([]int{exitOK, exitBusy}, code) {
			t.Errorf("helper %d exit code = %d, want %d or %d (busy)", i, code, exitOK, exitBusy)
		}
		n, err := strconv.Atoi(outs[i].String())
		if err != nil {
			t.Fatalf("helper %d output %q: %v", i, outs[i].String(), err)
		}
		if code == exitOK && n != rows {
			t.Errorf("helper %d committed %d rows, want %d", i, n, rows)
		}
		committed += n
	}

	var check string
	if err := s.db.QueryRow("PRAGMA integrity_check").Scan(&check); err != nil {
		t.Fatal(err)
	}
	if check != "ok" {
		t.Errorf("integrity_check = %q, want ok", check)
	}
	if got := queryInt(t, s, "SELECT count(*) FROM t"); got != committed {
		t.Errorf("rows = %d, want %d (the rows that the helpers committed)", got, committed)
	}
}

// TestMeasureOpenInsertClose measures open, one insert and close in a new process, on a store
// that exists. It runs only with BRAIN_STORE_MEASURE=<runs>, and it logs the numbers:
//
//	BRAIN_STORE_MEASURE=50 go test ./internal/store -run Measure -v
func TestMeasureOpenInsertClose(t *testing.T) {
	runs, err := strconv.Atoi(os.Getenv("BRAIN_STORE_MEASURE"))
	if err != nil {
		t.Skip("set BRAIN_STORE_MEASURE=<runs> to measure")
	}
	dir, s := newTestStore(t)
	closeTest(t, s)

	inside := make([]time.Duration, runs)
	whole := make([]time.Duration, runs)
	for i := range runs {
		start := time.Now()
		out, err := helperCmd(t, "measure", dir, testTimeout, 0).Output()
		whole[i] = time.Since(start)
		if code := exitCode(t, err); code != exitOK {
			t.Fatalf("helper exit code = %d, want %d", code, exitOK)
		}
		us, err := strconv.Atoi(string(out))
		if err != nil {
			t.Fatalf("helper output %q: %v", out, err)
		}
		inside[i] = time.Duration(us) * time.Microsecond
	}
	t.Logf("open+insert+close in the process: %s", stats(inside))
	t.Logf("whole process (start to exit):    %s", stats(whole))
}

func stats(ds []time.Duration) string {
	slices.Sort(ds)
	pct := func(p int) time.Duration { return ds[(len(ds)-1)*p/100] }
	return fmt.Sprintf("n=%d min=%s p50=%s p95=%s max=%s", len(ds), ds[0], pct(50), pct(95), ds[len(ds)-1])
}
