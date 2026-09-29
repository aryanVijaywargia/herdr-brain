package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"time"

	"github.com/aryanVijaywargia/herdr-brain/internal/adapter"
	"github.com/aryanVijaywargia/herdr-brain/internal/bindings"
	"github.com/aryanVijaywargia/herdr-brain/internal/herdr"
	"github.com/aryanVijaywargia/herdr-brain/internal/install"
)

// session is the herdr session of every eval run (HLD §12.1).
const session = "brain-eval"

// Herdr is the herdr calls of the runner in its test session. *herdr.Client has them; tests
// replace it.
type Herdr interface {
	Panes(ctx context.Context) ([]herdr.Pane, error)
	Workspaces(ctx context.Context) ([]herdr.Workspace, error)
	PaneProcessInfo(ctx context.Context, id string) (herdr.ProcessInfo, error)
	CreateWorkspace(ctx context.Context, cwd, label string) (herdr.Pane, error)
	SplitPane(ctx context.Context, id, cwd string) (herdr.Pane, error)
	ClosePane(ctx context.Context, id string) error
	RunInPane(ctx context.Context, id, command string) error
	WaitOutput(ctx context.Context, id, re string, timeout time.Duration) (string, error)
	StartAgent(ctx context.Context, name, kind, id string) error
	PromptAgent(ctx context.Context, target, text string, timeout time.Duration) error
	ReadAgent(ctx context.Context, target string, lines int) (string, error)
	ReadAgentVisible(ctx context.Context, target string) (string, error)
}

// Server is a herdr server that the runner started. *herdr.Server is one.
type Server interface {
	Stop(ctx context.Context) error
}

// Timeouts of a run. Each value is [NEW]: no source in the HLD or the story.
type Timeouts struct {
	Call   time.Duration // one herdr call: 10 s
	Start  time.Duration // an agent start, which herdr bounds at 30 s itself: 60 s
	Prompt time.Duration // one probe prompt, until the agent is done: 5 min
	Step   time.Duration // one setup step: 60 s
	Exit   time.Duration // the exit of the last case's processes: 10 s
	Poll   time.Duration // the check interval of that wait: 100 ms
}

// defaultTimeouts are the Timeouts of a real run. [NEW]
var defaultTimeouts = Timeouts{
	Call:   10 * time.Second,
	Start:  60 * time.Second,
	Prompt: 5 * time.Minute,
	Step:   60 * time.Second,
	Exit:   10 * time.Second,
	Poll:   100 * time.Millisecond,
}

// readLines is how many screen lines `agent read` returns for one probe. It must hold the
// reply and the CLI footer (E10-S4 notes). [NEW] 400.
const readLines = 400

// Runner runs eval cases in the brain-eval test session (HLD §12.1). It holds the files,
// the herdr calls and the clock of one run.
type Runner struct {
	Bindings  string // the bindings file
	Install   string // the install record
	MainStore string // the main store folder, which a test store must stay apart from
	Lock      string // the eval lock file
	Scratch   string // the folder of the scratch git folders; "" is the temp folder
	Environ   []string

	Herdr       func(herdr.Target) (Herdr, error)
	Sessions    func(ctx context.Context) ([]herdr.Session, error)
	StartServer func(ctx context.Context, name string) (Server, error)
	Alive       func(pid int) bool // reports whether process pid still runs
	Timeouts    Timeouts
	Now         func() time.Time
	Version     func(ctx context.Context, harness string) (string, error) // the CLI version of harness
	Stdout      io.Writer
}

// errSetup is the error of a run that stops because Brain is not set up for it. The runner
// never runs setup itself.
var errSetup = errors.New("not set up for an eval run")

// Run runs cases one after the other and returns the outcome of each finished case. It
// writes nothing before the setup checks pass. It stops at the first failed case: the next
// case never starts on a state that the runner could not confirm. Once the session is up, it
// writes the result of each probe to the results file of the test store, also after a failed
// case: the probes of the cases that did not run are skipped there.
func (r *Runner) Run(ctx context.Context, cases []Case) (out []Outcome, err error) {
	b, skip, err := r.check(cases)
	if err != nil {
		return nil, err
	}
	unlock, err := lockFile(r.Lock)
	if err != nil {
		return nil, err
	}
	defer unlock()
	h, stop, err := r.session(ctx, b)
	if err != nil {
		return nil, err
	}
	defer func() {
		// The clean-up runs also when ctx is cancelled, so an interrupt does not leave the
		// session that this run started.
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), r.Timeouts.Call)
		defer cancel()
		err = errors.Join(err, stop(cctx))
	}()
	started := r.Now()
	versions := r.versions(ctx, cases, skip)
	defer func() {
		err = errors.Join(err, writeResults(b.Store, started, r.Now(), results(cases, out, versions)))
	}()
	for _, c := range cases {
		o, err := r.runCase(ctx, h, c, skip)
		if o.CaseID != "" {
			out = append(out, o) // also a partial outcome: its finished probes keep their results
		}
		if err != nil {
			return out, fmt.Errorf("case %s: %w", c.ID, err)
		}
	}
	return out, nil
}

// check runs the setup checks and returns the test binding, and the reason to skip each
// harness whose Brain hook is missing: its probes cannot show a delivery. It only reads.
func (r *Runner) check(cases []Case) (bindings.Binding, map[string]string, error) {
	rec, err := install.Read(r.Install)
	if err != nil {
		return bindings.Binding{}, nil, err
	}
	if len(rec.Entries) == 0 {
		return bindings.Binding{}, nil, fmt.Errorf("%w: no Brain hook is installed; run `brain setup` first", errSetup)
	}
	skip := map[string]string{}
	for _, c := range cases {
		for _, p := range c.Probes {
			if _, done := skip[p.Harness]; done {
				continue
			}
			if err := r.hookInstalled(p.Harness); errors.Is(err, errSetup) {
				skip[p.Harness] = err.Error()
			} else if err != nil {
				return bindings.Binding{}, nil, err
			}
		}
	}
	b, err := r.testBinding()
	return b, skip, err
}

// hookInstalled checks that the hook file of harness still holds the recorded Brain entry: a
// record without its entry is a missing install. An entry that you edited counts as there.
func (r *Runner) hookInstalled(harness string) error {
	p, err := adapter.Load(harness)
	if err != nil {
		return err
	}
	st, err := (&install.Installer{Record: r.Install}).WouldUninstall(p)
	if err != nil {
		return err
	}
	if st == install.NotInstalled || st == install.NotFound {
		return fmt.Errorf("%w: the Brain hook of %s is not installed; run `brain setup` first", errSetup, harness)
	}
	return nil
}

// testBinding reads the bindings file and returns the binding of the brain-eval session. It
// must be a test binding whose store stays apart from the main store (E2-S2, S76): the reset
// removes all that the store folder holds.
func (r *Runner) testBinding() (bindings.Binding, error) {
	fix := fmt.Sprintf("start `herdr --session %s server`, then run `brain setup --session %s --test <folder>`", session, session)
	kept, dropped, err := bindings.Read(r.Bindings)
	if err != nil {
		return bindings.Binding{}, err
	}
	if i := slices.IndexFunc(dropped, func(d bindings.Dropped) bool { return d.Binding.Session == session }); i >= 0 {
		return bindings.Binding{}, fmt.Errorf("%w: the binding of session %s is refused (%w); %s", errSetup, session, dropped[i].Err, fix)
	}
	i := slices.IndexFunc(kept, func(b bindings.Binding) bool { return b.Session == session })
	if i < 0 {
		return bindings.Binding{}, fmt.Errorf("%w: herdr session %s has no binding; %s", errSetup, session, fix)
	}
	b := kept[i]
	if b.Kind != bindings.KindTest {
		return bindings.Binding{}, fmt.Errorf("%w: session %s is bound to the %s store, not a test store; %s", errSetup, session, b.Kind, fix)
	}
	if _, err := os.Stat(r.MainStore); errors.Is(err, fs.ErrNotExist) {
		return b, nil // no main store yet: nothing to keep apart from
	}
	nested, err := bindings.Nested(r.MainStore, b.Store)
	if err != nil {
		return bindings.Binding{}, err
	}
	if nested {
		return bindings.Binding{}, fmt.Errorf("%w: test store %s is, holds or sits inside the main store %s", errSetup, b.Store, r.MainStore)
	}
	return b, nil
}

// lockFile takes the eval lock without waiting, so a second runner stops at once (HLD
// §12.1). The lock ends with the process, so a killed run leaves no stale lock. The file
// stays.
func lockFile(path string) (unlock func(), err error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600) // [NEW] 0o600, as the data home files
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close() // the lock error is the one to report
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("another eval run holds %s", path)
		}
		return nil, fmt.Errorf("lock %s: %w", path, err)
	}
	return func() { _ = f.Close() }, nil // the close drops the lock
}

// session reuses the running brain-eval session, or starts it. Its socket must be the one of
// the test binding. stop stops and deletes the session only when this run started it.
func (r *Runner) session(ctx context.Context, b bindings.Binding) (h Herdr, stop func(context.Context) error, err error) {
	find := func() (herdr.Session, bool, error) {
		cctx, cancel := context.WithTimeout(ctx, r.Timeouts.Call)
		defer cancel()
		ss, err := r.Sessions(cctx)
		if err != nil {
			return herdr.Session{}, false, err
		}
		i := slices.IndexFunc(ss, func(s herdr.Session) bool { return s.Name == session && s.Running })
		if i < 0 {
			return herdr.Session{}, false, nil
		}
		return ss[i], true, nil
	}
	stop = func(context.Context) error { return nil }
	s, ok, err := find()
	if err != nil {
		return nil, nil, err
	}
	if !ok {
		cctx, cancel := context.WithTimeout(ctx, r.Timeouts.Call)
		srv, err := r.StartServer(cctx, session)
		cancel()
		if err != nil {
			return nil, nil, err
		}
		fmt.Fprintf(r.Stdout, "started herdr session %s\n", session)
		stop = srv.Stop
		if s, ok, err = find(); err == nil && !ok {
			err = fmt.Errorf("herdr session %s does not run after its start", session)
		}
		if err != nil {
			return nil, nil, errors.Join(err, stopNow(ctx, stop, r.Timeouts.Call))
		}
	}
	if !samePath(s.Socket, b.Socket) {
		err := fmt.Errorf("herdr session %s has socket %s, but its binding has %s; run `brain setup --session %s --test %s` again",
			session, s.Socket, b.Socket, session, b.Store)
		return nil, nil, errors.Join(err, stopNow(ctx, stop, r.Timeouts.Call))
	}
	h, err = r.Herdr(herdr.Target{Session: s.Name, Socket: s.Socket})
	if err != nil {
		return nil, nil, errors.Join(err, stopNow(ctx, stop, r.Timeouts.Call))
	}
	return h, stop, nil
}

func stopNow(ctx context.Context, stop func(context.Context) error, d time.Duration) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), d)
	defer cancel()
	return stop(ctx)
}

func samePath(a, b string) bool { return filepath.Clean(a) == filepath.Clean(b) }
