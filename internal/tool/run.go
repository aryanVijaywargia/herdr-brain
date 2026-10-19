package tool

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// The exit codes of a run that no step gave. [NEW] the shell's numbers.
const (
	ExitTimeout  = 124 // the timeout stopped the run
	ExitNotStart = 127 // a step did not start
	exitSignal   = 128 // plus the signal number: a signal stopped a step
)

// waitDelay is how long a run waits for the output of a child that keeps the pipes open
// after its step ended. [NEW] 2 s.
const waitDelay = 2 * time.Second

// ErrTimeout means that the timeout of the contract stopped the run.
var ErrTimeout = errors.New("the tool timed out")

// Fill checks the values against the input schema of c and returns the commands of c with
// each input filled in. Each input of the schema needs one value; a value of an input that
// the schema does not have, a value of the wrong type and a value that fails the pattern stop
// the call. A value fills one whole argv element, so it never becomes command text (S58).
func (c Contract) Fill(values map[string]string) ([][]string, error) {
	for _, n := range slices.Sorted(maps.Keys(values)) {
		in, ok := c.Inputs[n]
		if !ok {
			return nil, fmt.Errorf("tool %s has no input %s", c.Name, n)
		}
		if err := in.value(n, values[n]); err != nil {
			return nil, err
		}
	}
	for _, n := range slices.Sorted(maps.Keys(c.Inputs)) {
		if _, ok := values[n]; !ok {
			return nil, fmt.Errorf("input %s is missing: add --%s <value>", n, n)
		}
	}
	steps := c.Commands()
	out := make([][]string, len(steps))
	for i, argv := range steps {
		out[i] = make([]string, len(argv))
		for j, el := range argv {
			if m := placeholder.FindStringSubmatch(el); m != nil {
				// Parse allows only an input that fills the whole element.
				el = values[m[1]]
			}
			out[i][j] = el
		}
	}
	return out, nil
}

// value checks v against the type and the pattern of input n.
func (in Input) value(n, v string) error {
	if in.Type == "integer" {
		if _, err := strconv.ParseInt(v, 10, 64); err != nil {
			return fmt.Errorf("input %s: %q is not an integer", n, v)
		}
	}
	// Parse checked the pattern.
	if !regexp.MustCompile(in.Pattern).MatchString(v) {
		return fmt.Errorf("input %s: %q does not match the pattern %s", n, v, in.Pattern)
	}
	return nil
}

// Dir is the folder that the tool runs in: its cwd, with a leading ~ as home.
func (c Contract) Dir(home string) string { return Dir(c.Cwd, home) }

// Dir is folder with a leading ~ as home, as a tool cwd or a pinned automation folder names it.
func Dir(folder, home string) string {
	if folder == "~" {
		return home
	}
	if rest, ok := strings.CutPrefix(folder, "~/"); ok {
		return filepath.Join(home, rest)
	}
	return folder
}

// Run is one run of the filled commands of a contract.
type Run struct {
	Commands [][]string
	Dir      string // the cwd
	// ScriptDir is the folder of the checked revision; a step that runs run.sh runs the run.sh there.
	ScriptDir string
	Env       []string
	Timeout   time.Duration
	Stdout    io.Writer
	Stderr    io.Writer
}

// Exec runs the steps in order with no shell, and stops at the first step that fails. It
// returns the exit code of the last step that ran, and an error when a step failed. When the
// timeout ends, it stops the step and its children (their process group) and returns
// ExitTimeout and ErrTimeout. Standard input is /dev/null: a tool takes its inputs as
// arguments. [NEW] the null input.
func (r Run) Exec(ctx context.Context) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, r.Timeout)
	defer cancel()
	for i, argv := range r.Commands {
		code, err := r.step(ctx, argv)
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return ExitTimeout, fmt.Errorf("step %d: %w after %s; the run and its children were stopped", i+1, ErrTimeout, r.Timeout)
		}
		if err != nil {
			if len(r.Commands) > 1 {
				err = fmt.Errorf("step %d: %w", i+1, err)
			}
			return code, err
		}
	}
	return 0, nil
}

// step runs one argv list. A run.sh step runs the reviewed run.sh with sh, and the inputs
// reach it as "$1", "$2".
func (r Run) step(ctx context.Context, argv []string) (int, error) {
	prog, args := argv[0], argv[1:]
	if prog == ScriptName {
		prog, args = "/bin/sh", append([]string{filepath.Join(r.ScriptDir, ScriptName)}, args...)
	}
	cmd := exec.Command(prog, args...)
	cmd.Dir, cmd.Env = r.Dir, r.Env
	cmd.Stdout, cmd.Stderr = r.Stdout, r.Stderr
	// A process group of its own, so the timeout stops the step's children too.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = waitDelay
	if err := cmd.Start(); err != nil {
		return ExitNotStart, fmt.Errorf("%s did not start: %w", argv[0], err)
	}
	err := waitGroup(ctx, cmd)
	var exit *exec.ExitError
	switch {
	case err == nil:
		return 0, nil
	case errors.As(err, &exit):
		if ws, ok := exit.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return exitSignal + int(ws.Signal()), fmt.Errorf("%s: %w", argv[0], err)
		}
		return exit.ExitCode(), fmt.Errorf("%s: %w", argv[0], err)
	case errors.Is(err, exec.ErrWaitDelay):
		// The step ended well; a child kept its output open after it.
		return 0, nil
	default:
		// The step started, but its output could not be copied. [NEW] exit code 1.
		return 1, fmt.Errorf("%s: %w", argv[0], err)
	}
}

// waitGroup waits for the started cmd and its output. When ctx ends first, it kills the
// process group of cmd, also after the leader has ended while a child still holds the output
// open: exec.CommandContext stops its watch when the leader ends.
func waitGroup(ctx context.Context, cmd *exec.Cmd) error {
	done, stopped := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(stopped)
		select {
		case <-ctx.Done():
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) // ESRCH: the group has ended already
		case <-done:
		}
	}()
	err := cmd.Wait()
	close(done)
	<-stopped
	return err
}
