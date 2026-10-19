package hook

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aryanVijaywargia/herdr-brain/internal/paths"
)

// The fake herdr scripts: one answers `pane get` for w14:p1, one never answers.
const (
	answer = `echo '{"id":"cli:pane:get","result":{"pane":{"agent_status":"idle","cwd":"/tmp","focused":true,` +
		`"pane_id":"w14:p1","revision":0,"tab_id":"w14:t1","terminal_id":"term_1","workspace_id":"w14"},"type":"pane_info"}}'`
	stall = "exec sleep 5"
)

// startSlack is the time to start the brain process, which is before the hook clock.
const startSlack = 300 * time.Millisecond

// processSetup puts the bindings of a new fixture in the data home of a scratch HOME. It
// returns the fixture and a hook environment whose herdr is a shell script that runs script,
// so each lookup starts a process as the real one does.
func processSetup(t *testing.T, script string) (*fixture, []string) {
	t.Helper()
	f := newFixture(t)
	home := t.TempDir()
	t.Setenv("HOME", home) // for Main in this process
	file, err := paths.BindingsFile()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(f.file)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, data, 0o600); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(home, "herdr")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"+script+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return f, append(environ(f.mainSock, "", "w14:p1", "w14"), "HOME="+home, "PATH=/usr/bin:/bin", "HERDR_BIN_PATH="+bin)
}

// brainBinary builds cmd/brain, the command that an agent runs as its hook. It runs the new
// binary once: macOS checks a binary at its first run, which took 140-570 ms on the dev Mac
// and more under load (#335). An installed hook pays that once, not at each run, so the
// timed runs must not.
func brainBinary(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "brain")
	if out, err := exec.Command("go", "build", "-o", bin, "../../cmd/brain").CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	if out, err := exec.Command(bin, "version").CombinedOutput(); err != nil {
		t.Fatalf("brain version: %v\n%s", err, out)
	}
	return bin
}

// hookCommand is the brain hook as an agent starts it.
func hookCommand(ctx context.Context, bin string, env []string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, bin, "hook", "--profile", "claude", "--event", "start")
	cmd.Env = env
	return cmd
}

// fullPipe returns a pipe whose buffer is full, so the next write to w stalls.
func fullPipe(t *testing.T) (r, w *os.File) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close(); _ = w.Close() })
	if err := w.SetWriteDeadline(time.Now().Add(100 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	fill := bytes.Repeat([]byte("x"), 4096)
	for {
		if _, err := w.Write(fill); err != nil {
			if !errors.Is(err, os.ErrDeadlineExceeded) {
				t.Fatal(err)
			}
			break
		}
	}
	return r, w
}

// TestProcessCap (E7-S4): the brain process ends by the cap when stdin stays open,
// herdr does not answer or stdout is full. It exits 0, prints nothing (also after the
// blockage ends) and writes no receipt. The exit ends the stalled step.
func TestProcessCap(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the brain binary")
	}
	bin := brainBinary(t)
	tests := []struct {
		name   string
		script string
		// setup sets stdin and stdout; it returns what the process printed, read after the
		// process exits and the blockage ends.
		setup func(t *testing.T, cmd *exec.Cmd) (printed func() string)
	}{
		{"stdin held open", answer, func(t *testing.T, cmd *exec.Cmd) func() string {
			r, w, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = r.Close(); _ = w.Close() })
			var out bytes.Buffer
			cmd.Stdin, cmd.Stdout = r, &out
			return func() string {
				_, _ = w.Write([]byte(input))
				_ = w.Close()
				return out.String()
			}
		}},
		{"stalled lookup", stall, func(_ *testing.T, cmd *exec.Cmd) func() string {
			var out bytes.Buffer
			cmd.Stdin, cmd.Stdout = strings.NewReader(input), &out
			return out.String
		}},
		{"full stdout", answer, func(t *testing.T, cmd *exec.Cmd) func() string {
			r, w := fullPipe(t)
			cmd.Stdin, cmd.Stdout = strings.NewReader(input), w
			return func() string {
				_ = w.Close()
				all, err := io.ReadAll(r)
				if err != nil {
					t.Fatal(err)
				}
				return strings.Trim(string(all), "x")
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, env := processSetup(t, tt.script)
			ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
			defer cancel()
			cmd := hookCommand(ctx, bin, env)
			printed := tt.setup(t, cmd)
			start := time.Now()
			err := cmd.Run()
			if took := time.Since(start); err != nil || took > hookCap+capSlack+startSlack {
				t.Fatalf("brain hook = %v after %v, want exit 0 by %v", err, took, hookCap+capSlack+startSlack)
			}
			if out := printed(); out != "" {
				t.Errorf("printed %q, want nothing", out)
			}
			if ds := deliveries(t, f.mainStore); len(ds) != 0 {
				t.Errorf("receipts = %s, want none", show(ds[0]))
			}
		})
	}
}

// TestProcessTiming times the brain hook from process start to exit: the Go runtime start,
// the hook as TestTiming times it and the exit. It logs p50 and p99; run it with -v to see
// them. The target is < 100 ms. The lookup is a fake herdr script, not the herdr CLI.
func TestProcessTiming(t *testing.T) {
	if testing.Short() {
		t.Skip("timing runs")
	}
	bin := brainBinary(t)
	f, env := processSetup(t, answer)
	run := func() (string, time.Duration) {
		var out bytes.Buffer
		cmd := hookCommand(context.Background(), bin, env)
		cmd.Stdin, cmd.Stdout = strings.NewReader(input), &out
		start := time.Now()
		if err := cmd.Run(); err != nil {
			t.Fatalf("brain hook = %v", err)
		}
		return out.String(), time.Since(start)
	}
	run() // the first start of a new executable can be slow on macOS
	times := make([]time.Duration, timingRuns)
	emitted := 0
	for i := range times {
		var out string
		out, times[i] = run()
		if strings.Contains(briefOf(t, out), mainA14) {
			emitted++
		}
	}
	if emitted == 0 {
		t.Fatal("no run printed the brief")
	}
	receipts := 0
	for _, d := range deliveries(t, f.mainStore) {
		if d.Result == "emitted" {
			receipts++
		}
	}
	slices.Sort(times)
	t.Logf("brain process, %d runs (%d emitted, %d emitted receipts with the warm-up), %s/%s: p50 %v, p99 %v, max %v",
		timingRuns, emitted, receipts, runtime.GOOS, runtime.GOARCH, rank(times, 50), rank(times, 99), times[len(times)-1])
}

// rank is the nearest-rank percentile p of the sorted times, as in the E7-S4 test.
func rank(times []time.Duration, p int) time.Duration {
	return times[(p*len(times)+99)/100-1]
}
