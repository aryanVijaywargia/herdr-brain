package main

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/aryanVijaywargia/herdr-brain/internal/herdr"
)

// Case is one eval case. The probe files of E10-S2 fill it; this story needs only its frame.
type Case struct {
	ID     string
	Setup  []string // shell steps, run in order in the scratch folder; "{{marker}}" is the case marker
	Probes []Probe
}

// Probe is one prompt to a fresh official CLI.
type Probe struct {
	Harness string // claude or codex: the herdr agent kind and the install entry
	Prompt  string // "{{marker}}" is the case marker
}

// Outcome is what one case returned. E10-S2 labels it; E10-S3 scores it.
type Outcome struct {
	CaseID  string
	Marker  string
	Replies []Reply
}

// Reply is the screen text of one probe after its prompt, from `agent read`.
type Reply struct {
	Harness string
	Text    string
}

// markerSlot is the placeholder of the case marker in steps and prompts.
const markerSlot = "{{marker}}"

// writers are the process names of a live writer of a store: an agent CLI or Brain itself.
var writers = []string{"claude", "codex", "brain"}

// errExitUnconfirmed stops a run: a process of the last case may still write to the store,
// so the runner must not reset it (HLD §12.1).
var errExitUnconfirmed = errors.New("could not confirm the exit of the last case's processes; no reset")

// runCase runs one case in a fresh state: it checks that nothing else writes to the test
// store, resets the store, makes a fresh scratch git folder and marker, runs the setup steps
// in a shell pane and each probe in a fresh CLI pane. At the end it closes the panes that it
// opened and waits until their processes exit.
func (r *Runner) runCase(ctx context.Context, h Herdr, c Case) (o Outcome, err error) {
	b, err := r.testBinding() // the binding can change between cases
	if err != nil {
		return Outcome{}, err
	}
	if err := r.guard(ctx, h); err != nil {
		return Outcome{}, err
	}
	if err := reset(b.Store); err != nil {
		return Outcome{}, fmt.Errorf("reset test store: %w", err)
	}
	marker, err := newMarker()
	if err != nil {
		return Outcome{}, err
	}
	dir, err := r.scratch(ctx)
	if err != nil {
		return Outcome{}, err
	}
	var opened []string
	defer func() {
		err = errors.Join(err, r.closeAll(ctx, h, opened), os.RemoveAll(dir))
	}()
	shell, err := call(ctx, r.Timeouts.Call, func(ctx context.Context) (herdr.Pane, error) {
		return h.CreateWorkspace(ctx, dir, "brain-eval-"+c.ID)
	})
	if err != nil {
		return Outcome{}, err
	}
	opened = append(opened, shell.ID)
	for i, step := range c.Setup {
		if err := r.step(ctx, h, shell.ID, strings.ReplaceAll(step, markerSlot, marker), marker, i); err != nil {
			return Outcome{}, fmt.Errorf("setup step %d: %w", i+1, err)
		}
	}
	o = Outcome{CaseID: c.ID, Marker: marker}
	for i, p := range c.Probes {
		text, err := r.probe(ctx, h, shell.ID, dir, fmt.Sprintf("probe-%s-%d", marker, i+1), p, marker, &opened)
		if err != nil {
			return Outcome{}, fmt.Errorf("probe %d (%s): %w", i+1, p.Harness, err)
		}
		o.Replies = append(o.Replies, Reply{Harness: p.Harness, Text: text})
	}
	return o, nil
}

// guard fails when a pane that this run did not open holds an agent, or a foreground CLI or
// Brain process: it could write to the test store during or after the reset (HLD §12.1). The
// runner closes its own panes at the end of each case, so any such pane is foreign here,
// for example one of an interrupted run.
func (r *Runner) guard(ctx context.Context, h Herdr) error {
	panes, err := call(ctx, r.Timeouts.Call, h.Panes)
	if err != nil {
		return err
	}
	for _, p := range panes {
		if p.Agent != nil {
			return fmt.Errorf("pane %s runs agent %s that this run did not start; no reset (close it, then run again)", p.ID, *p.Agent)
		}
		pi, err := call(ctx, r.Timeouts.Call, func(ctx context.Context) (herdr.ProcessInfo, error) {
			return h.PaneProcessInfo(ctx, p.ID)
		})
		if errors.Is(err, herdr.ErrPaneNotFound) {
			continue // it closed since the list
		}
		if err != nil {
			return err
		}
		for _, proc := range pi.Processes {
			if slices.Contains(writers, proc.Name) {
				return fmt.Errorf("pane %s runs %s that this run did not start; no reset (close it, then run again)", p.ID, proc.Name)
			}
		}
	}
	return nil
}

// reset empties the test store folder: the database with its WAL files, the workspace map,
// and the compiled generations with their pointer (HLD §12.1). The folder stays: the
// binding needs it.
func reset(store string) error {
	entries, err := os.ReadDir(store)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := os.RemoveAll(filepath.Join(store, e.Name())); err != nil {
			return err
		}
	}
	return nil
}

// newMarker returns a fresh opaque marker: four letters, a dash and four digits, for example
// kest-1234. [NEW] shape; it only has to be new in each case and absent from real text.
func newMarker() (string, error) {
	const letters = "abcdefghijklmnopqrstuvwxyz"
	b := make([]byte, 0, 9)
	for i := range 8 {
		set := letters
		if i >= 4 {
			set = "0123456789"
		}
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(set))))
		if err != nil {
			return "", fmt.Errorf("marker: %w", err)
		}
		if i == 4 {
			b = append(b, '-')
		}
		b = append(b, set[n.Int64()])
	}
	return string(b), nil
}

// scratch makes a fresh scratch git folder for one case.
func (r *Runner) scratch(ctx context.Context) (string, error) {
	dir, err := os.MkdirTemp(r.Scratch, "brain-eval-case-")
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, r.Timeouts.Step)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "init", "-q", dir)
	cmd.Env = r.Environ
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", errors.Join(fmt.Errorf("git init %s: %w: %s", dir, err, strings.TrimSpace(string(out))), os.RemoveAll(dir))
	}
	return dir, nil
}

// step runs one setup step in the shell pane and waits for its exit code. The done line
// carries the marker and the step number, so it matches neither an earlier step nor the
// typed command line, which shows `$?` and not a number.
func (r *Runner) step(ctx context.Context, h Herdr, pane, command, marker string, i int) error {
	done := fmt.Sprintf("brain-eval-done-%s-%d", marker, i+1)
	if _, err := call(ctx, r.Timeouts.Call, func(ctx context.Context) (struct{}, error) {
		return struct{}{}, h.RunInPane(ctx, pane, command+"; echo "+done+" $?")
	}); err != nil {
		return err
	}
	line, err := call(ctx, r.Timeouts.Step+r.Timeouts.Call, func(ctx context.Context) (string, error) {
		return h.WaitOutput(ctx, pane, regexp.QuoteMeta(done)+" [0-9]+$", r.Timeouts.Step)
	})
	if err != nil {
		return err
	}
	code, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), done)))
	if err != nil {
		return fmt.Errorf("done line %q has no exit code", line)
	}
	if code != 0 {
		return fmt.Errorf("exit code %d", code)
	}
	return nil
}

// probe opens a pane beside the shell pane, starts a fresh official CLI in it, sends the
// prompt with herdr and returns the screen text from `agent read`. The runner never touches
// the CLI's credentials or native memory.
func (r *Runner) probe(ctx context.Context, h Herdr, shell, dir, name string, p Probe, marker string, opened *[]string) (string, error) {
	pane, err := call(ctx, r.Timeouts.Call, func(ctx context.Context) (herdr.Pane, error) {
		return h.SplitPane(ctx, shell, dir)
	})
	if err != nil {
		return "", err
	}
	*opened = append(*opened, pane.ID)
	if _, err := call(ctx, r.Timeouts.Start, func(ctx context.Context) (struct{}, error) {
		return struct{}{}, h.StartAgent(ctx, name, p.Harness, pane.ID)
	}); err != nil {
		return "", err
	}
	if _, err := call(ctx, r.Timeouts.Prompt+r.Timeouts.Call, func(ctx context.Context) (struct{}, error) {
		return struct{}{}, h.PromptAgent(ctx, name, strings.ReplaceAll(p.Prompt, markerSlot, marker), r.Timeouts.Prompt)
	}); err != nil {
		return "", err
	}
	return call(ctx, r.Timeouts.Call, func(ctx context.Context) (string, error) {
		return h.ReadAgent(ctx, name, readLines)
	})
}

// closeAll closes the panes that the case opened, the last first, and waits until their
// shell and foreground processes exit. herdr ends the whole process group of a closed pane.
// It runs also when ctx is cancelled. It returns errExitUnconfirmed when a process still
// runs after the wait.
func (r *Runner) closeAll(ctx context.Context, h Herdr, opened []string) error {
	ctx = context.WithoutCancel(ctx)
	var pids []int
	var errs []error
	for _, id := range slices.Backward(opened) {
		pi, err := call(ctx, r.Timeouts.Call, func(ctx context.Context) (herdr.ProcessInfo, error) {
			return h.PaneProcessInfo(ctx, id)
		})
		if errors.Is(err, herdr.ErrPaneNotFound) {
			continue // closed with its workspace, or by hand
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("pane %s: %w (%w)", id, err, errExitUnconfirmed))
			continue
		}
		pids = append(pids, pi.ShellPID)
		for _, p := range pi.Processes {
			pids = append(pids, p.PID)
		}
		_, err = call(ctx, r.Timeouts.Call, func(ctx context.Context) (struct{}, error) {
			return struct{}{}, h.ClosePane(ctx, id)
		})
		if err != nil && !errors.Is(err, herdr.ErrPaneNotFound) {
			errs = append(errs, fmt.Errorf("close pane %s: %w (%w)", id, err, errExitUnconfirmed))
		}
	}
	if err := r.waitExit(ctx, pids); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// waitExit waits until no process in pids runs, for at most the exit timeout.
func (r *Runner) waitExit(ctx context.Context, pids []int) error {
	deadline := time.NewTimer(r.Timeouts.Exit)
	defer deadline.Stop()
	tick := time.NewTicker(r.Timeouts.Poll)
	defer tick.Stop()
	for {
		pids = slices.DeleteFunc(pids, func(pid int) bool { return !r.Alive(pid) })
		if len(pids) == 0 {
			return nil
		}
		select {
		case <-deadline.C:
			return fmt.Errorf("%w: pids %v still run", errExitUnconfirmed, pids)
		case <-ctx.Done():
			return fmt.Errorf("%w: %w", errExitUnconfirmed, ctx.Err())
		case <-tick.C:
		}
	}
}

// call runs fn with a deadline of d, as every herdr call needs one.
func call[T any](ctx context.Context, d time.Duration, fn func(context.Context) (T, error)) (T, error) {
	ctx, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	return fn(ctx)
}
