package main

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"maps"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/aryanVijaywargia/herdr-brain/internal/adapter"
	"github.com/aryanVijaywargia/herdr-brain/internal/bindings"
	"github.com/aryanVijaywargia/herdr-brain/internal/herdr"
	"github.com/aryanVijaywargia/herdr-brain/internal/paths"
	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

// Case is one eval case: a probe of a probe file, or its control.
type Case struct {
	ID      string
	Stage   string // one of stages; "" for an ad-hoc case
	Source  string
	Control bool     // the control: the same case with no setup steps (§12.1)
	Setup   []string // shell steps, run in order in the scratch folder
	Probes  []Probe
	// Check is what each reply is checked against. An ad-hoc case has none and gets no label.
	Check          Check
	ExpectRevision int64    // the revision that a right answer comes from; 0 when not set
	Hook           string   // an exclusion case: the hook mode of hookModes; "" for a CLI probe
	FullBrief      bool     // a delivery case whose brief must be full size, with {{marker}} in its middle
	Forgotten      []string // a forget case: the answers that the store must not hold after the setup
	// Skills are the fixture skills of a skill choice case: the runner seeds them all, and
	// each gets a result. SkillFor is the skill whose example the prompt is; "" for a
	// negative prompt.
	Skills   []FixtureSkill
	SkillFor string
}

// Probe is one prompt to a fresh official CLI.
type Probe struct {
	Harness string // claude or codex: the herdr agent kind and the install entry
	Prompt  string
}

// Outcome is what one case returned. E10-S2 labels it; E10-S3 scores it.
type Outcome struct {
	CaseID  string
	Marker  string            // the value of {{marker}}
	Markers map[string]string // each marker placeholder name to its value in this case
	Session string            // the store binding of the case: its herdr session
	Store   string            // and its store folder
	Replies []Reply
	// BriefCheck is why the brief of a full-brief case is not full size with its marker in the
	// middle; "" when it is.
	BriefCheck string
	// Leak is where the store still holds a forgotten answer of a forget case; "" when nowhere.
	Leak string
}

// Reply is what one probe returned: the reply text with its run status and receipt.
type Reply struct {
	Harness string
	Pane    string
	Screen  string // the whole text of `agent read`
	Text    string // the reply: the screen text after the ask
	Status  Status
	Why     string          // why the status is not delivered, or skipped
	Receipt *store.Delivery // the newest receipt of the pane; nil when there is none
	Record  string          // a usefulness case: what the probe changed in the scratch folder
}

// caseLabel starts the label of the workspace of each case.
const caseLabel = "brain-eval-case-"

// brainProcess is the process name of Brain itself, a live writer of a store.
const brainProcess = "brain"

// errExitUnconfirmed stops a run: a process of the last case may still write to the store,
// so the runner must not reset it (HLD §12.1).
var errExitUnconfirmed = errors.New("could not confirm the exit of the last case's processes; no reset")

// runCase runs one case in a fresh state: it checks that nothing else writes to the test
// store, resets the store, makes a fresh scratch git folder and markers, runs the setup steps
// in a shell pane and each probe in a fresh CLI pane. A probe of a harness in skip is skipped
// with the reason. At the end it closes the panes that it opened and waits until their
// processes exit.
func (r *Runner) runCase(ctx context.Context, h Herdr, c Case, skip map[string]string) (o Outcome, err error) {
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
	markers, err := newMarkers(c)
	if err != nil {
		return Outcome{}, err
	}
	marker := markers["marker"]
	if err := r.seedSkills(ctx, b, c, markers); err != nil {
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
		return h.CreateWorkspace(ctx, dir, workspaceLabel(c))
	})
	if err != nil {
		return Outcome{}, err
	}
	opened = append(opened, shell.ID)
	o = Outcome{CaseID: c.ID, Marker: marker, Markers: markers, Session: b.Session, Store: b.Store}
	skipCase, err := r.setup(ctx, h, shell, dir, c, markers, &opened) // why no probe of the case can run
	if err != nil {
		return Outcome{}, err
	}
	want, err := r.checkOutput(ctx, b, c, shell.WorkspaceID, &o)
	if err != nil {
		return Outcome{}, err
	}
	for i, p := range c.Probes {
		why, ok := skip[p.Harness]
		if skipCase != "" {
			why, ok = skipCase, true
		}
		if ok {
			o.Replies = append(o.Replies, Reply{Harness: p.Harness, Status: Skipped, Why: why})
			continue
		}
		rep, err := r.runProbe(ctx, h, shell, dir, c, p, b, markers, want, i, &opened)
		if err != nil {
			return o, fmt.Errorf("probe %d (%s): %w", i+1, p.Harness, err)
		}
		o.Replies = append(o.Replies, rep)
	}
	return o, nil
}

// checkOutput returns the hash of the brief that the setup built for workspace wsID. It checks
// the brief of a full-brief case and the store of a forget case into o.
func (r *Runner) checkOutput(ctx context.Context, b bindings.Binding, c Case, wsID string, o *Outcome) (want string, err error) {
	want, text, err := briefHash(b, wsID)
	if err != nil {
		return "", err
	}
	if c.FullBrief && !c.Control {
		o.BriefCheck = briefCheck(text, o.Marker)
	}
	if len(c.Forgotten) > 0 && !c.Control {
		if o.Leak, err = r.leak(ctx, b, fillAll(c.Forgotten, o.Markers)); err != nil {
			return "", err
		}
	}
	return want, nil
}

// runProbe runs probe i of case c: a hook call for an exclusion case, else a fresh CLI. The
// reply of a usefulness case gets its record.
func (r *Runner) runProbe(ctx context.Context, h Herdr, shell herdr.Pane, dir string, c Case, p Probe, b bindings.Binding, markers map[string]string, want string, i int, opened *[]string) (rep Reply, err error) {
	marker := markers["marker"]
	p.Prompt = fill(p.Prompt, markers)
	if c.Hook != "" {
		rep, err = r.hookProbe(ctx, h, shell, dir, c, p, b, marker, i, opened)
	} else {
		rep, err = r.probe(ctx, h, shell, dir, fmt.Sprintf("probe-%s-%d", marker, i+1), p, b, want, opened)
	}
	if err == nil && c.Stage == "usefulness" && rep.Status != Skipped {
		rep.Record = r.record(ctx, dir)
	}
	return rep, err
}

// setup runs the setup steps of case c in order. skip is why no probe of the case can run: a
// setup CLI that did not get ready. The steps after it do not run then.
func (r *Runner) setup(ctx context.Context, h Herdr, shell herdr.Pane, dir string, c Case, markers map[string]string, opened *[]string) (skip string, err error) {
	marker := markers["marker"]
	for i, step := range c.Setup {
		step = fill(step, markers)
		if harness, prompt, ok := parseAgentStep(step, adapter.Kinds(r.Profiles)); ok {
			skip, err = r.runAgentStep(ctx, h, shell, dir, fmt.Sprintf("setup-%s-%d", marker, i+1), harness, prompt, opened)
		} else {
			err = r.step(ctx, h, shell.ID, step, marker, i)
		}
		if err != nil {
			return "", fmt.Errorf("setup step %d: %w", i+1, err)
		}
		if skip != "" {
			return skip, nil
		}
	}
	return "", nil
}

// workspaceLabel is the label of the workspace of case c.
func workspaceLabel(c Case) string {
	if c.Control {
		return caseLabel + c.ID + "-control"
	}
	return caseLabel + c.ID
}

// newMarkers returns a fresh marker for {{marker}}, for each other marker placeholder of c and
// for each fixture skill of c (§12.1 "fresh opaque markers").
func newMarkers(c Case) (map[string]string, error) {
	names := []string{"marker"}
	for _, sk := range c.Skills {
		names = append(names, skillMarker(sk.Name))
	}
	texts := slices.Concat(c.Setup, c.Check.Expect, c.Check.Stale, c.Forgotten)
	for _, p := range c.Probes {
		texts = append(texts, p.Prompt)
	}
	for _, t := range texts {
		for _, m := range slot.FindAllStringSubmatch(t, -1) {
			if markerName.MatchString(m[1]) && !slices.Contains(names, m[1]) {
				names = append(names, m[1])
			}
		}
	}
	markers := map[string]string{}
	for _, n := range names {
		for {
			m, err := newMarker()
			if err != nil {
				return nil, err
			}
			if !slices.Contains(slices.Collect(maps.Values(markers)), m) {
				markers[n] = m
				break
			}
		}
	}
	return markers, nil
}

// fill puts the value of each marker placeholder of s in its place.
func fill(s string, markers map[string]string) string {
	return slot.ReplaceAllStringFunc(s, func(m string) string {
		if v, ok := markers[m[2:len(m)-2]]; ok {
			return v
		}
		return m
	})
}

func fillAll(ss []string, markers map[string]string) []string {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = fill(s, markers)
	}
	return out
}

// guard fails when a case workspace of an earlier run is still open, or when a pane that this
// run did not open holds an agent, or a foreground process of the CLI of a profile or of Brain: it could write to
// the test store during or after the reset (HLD §12.1). The runner closes its own panes at
// the end of each case, so any such pane is foreign here, for example one of an interrupted
// run.
func (r *Runner) guard(ctx context.Context, h Herdr) error {
	kinds := adapter.Kinds(r.Profiles) // the process name of each profile's CLI is its kind
	ws, err := call(ctx, r.Timeouts.Call, h.Workspaces)
	if err != nil {
		return err
	}
	// A case workspace outlives its case only when a run did not reach its clean-up. A
	// background process in it can still write, and herdr does not show it; so any such
	// workspace stops the reset.
	if i := slices.IndexFunc(ws, func(w herdr.Workspace) bool { return strings.HasPrefix(w.Label, caseLabel) }); i >= 0 {
		return fmt.Errorf("workspace %s (%s) is left from an earlier run; no reset (close it, then run again)", ws[i].ID, ws[i].Label)
	}
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
			if proc.Name == brainProcess || slices.Contains(kinds, proc.Name) {
				return fmt.Errorf("pane %s runs %s that this run did not start; no reset (close it, then run again)", p.ID, proc.Name)
			}
		}
	}
	return nil
}

// reset empties the test store folder: the database with its WAL files, the workspace map,
// and the compiled generations with their pointer (HLD §12.1). The folder stays: the
// binding needs it. The results file and the report stay too: they hold only the markers of
// old cases.
func reset(store string) error {
	entries, err := os.ReadDir(store)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if p := filepath.Join(store, e.Name()); p == paths.EvalResults(store) || p == paths.EvalReport(store) {
			continue
		}
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
// prompt with herdr, reads the reply and its receipt in the test store of binding b, and
// checks the receipt against want, the expected brief hash. The
// runner never touches the CLI's credentials or native memory. A CLI that herdr cannot start
// because it is not ready, such as Codex at its folder-trust dialog, is skipped: the runner
// never answers that dialog.
func (r *Runner) probe(ctx context.Context, h Herdr, shell herdr.Pane, dir, name string, p Probe, b bindings.Binding, want string, opened *[]string) (Reply, error) {
	rep := Reply{Harness: p.Harness}
	pane, err := call(ctx, r.Timeouts.Call, func(ctx context.Context) (herdr.Pane, error) {
		return h.SplitPane(ctx, shell.ID, dir)
	})
	if err != nil {
		return Reply{}, err
	}
	*opened = append(*opened, pane.ID)
	rep.Pane = pane.ID
	prof, err := adapter.LoadFS(r.Profiles, p.Harness)
	if err != nil {
		return Reply{}, err
	}
	args, err := startArgs(prof, dir, false)
	if err != nil {
		return Reply{}, err
	}
	if _, err := call(ctx, r.Timeouts.Start, func(ctx context.Context) (struct{}, error) {
		return struct{}{}, h.StartAgent(ctx, name, p.Harness, pane.ID, args)
	}); err != nil {
		if hasCode(err, "agent_not_ready") {
			rep.Status, rep.Why = Skipped, fmt.Sprintf("the CLI did not get ready, for example at its folder-trust dialog, which the runner does not answer: %v", err)
			return rep, nil
		}
		return Reply{}, err
	}
	if _, err := call(ctx, r.Timeouts.Prompt+r.Timeouts.Call, func(ctx context.Context) (struct{}, error) {
		return struct{}{}, h.PromptAgent(ctx, name, p.Prompt, r.Timeouts.Prompt)
	}); err != nil {
		return Reply{}, err
	}
	rep.Screen, err = call(ctx, r.Timeouts.Call, func(ctx context.Context) (string, error) {
		// An alternate-screen CLI has only its screen; a longer read scrolls it (HLD §6.2).
		if prof.AltScreen {
			return h.ReadAgentVisible(ctx, name)
		}
		return h.ReadAgent(ctx, name, readLines)
	})
	if err != nil && !hasCode(err, "agent_not_idle") {
		return Reply{}, err
	}
	d, ok, derr := r.receipt(ctx, b, pane.ID, p.Harness)
	if derr != nil {
		return Reply{}, derr
	}
	if ok {
		rep.Receipt = &d
	}
	rep.Status, rep.Why = deliveryStatus(d, ok, want)
	text, found := replyAfter(rep.Screen, p.Prompt)
	if !found {
		text = strings.TrimSpace(rep.Screen) // what the runner saw, for your review of the UNSURE
	}
	rep.Text = text
	switch {
	case err != nil:
		rep.Status, rep.Why = Incomplete, err.Error()
	case !found:
		rep.Status, rep.Why = Incomplete, "the read does not show the ask with a reply after it"
	}
	return rep, nil
}

// hasCode reports whether err is a herdr refusal with code.
func hasCode(err error, code string) bool {
	return err != nil && strings.Contains(err.Error(), "("+code+")")
}

// replyAfter returns the text after the last place where screen shows ask. found is false
// when the screen does not show the ask, or shows nothing after it: then the runner cannot see
// the whole reply. Line breaks and runs of spaces in the screen's copy of ask do not matter,
// because a CLI wraps a long prompt.
func replyAfter(screen, ask string) (text string, found bool) {
	words := strings.Fields(ask)
	if len(words) == 0 {
		return "", false
	}
	for i, w := range words {
		words[i] = regexp.QuoteMeta(w)
	}
	ms := regexp.MustCompile(strings.Join(words, `\s+`)).FindAllStringIndex(screen, -1)
	if len(ms) == 0 {
		return "", false
	}
	text = strings.TrimSpace(screen[ms[len(ms)-1][1]:])
	return text, text != ""
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
