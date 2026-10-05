package main

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/aryanVijaywargia/herdr-brain/internal/adapter"
	"github.com/aryanVijaywargia/herdr-brain/internal/bindings"
	"github.com/aryanVijaywargia/herdr-brain/internal/harness"
	"github.com/aryanVijaywargia/herdr-brain/internal/herdr"
	"github.com/aryanVijaywargia/herdr-brain/internal/install"
	"github.com/aryanVijaywargia/herdr-brain/internal/paths"
	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

const setupUsage = `usage: brain setup [--session <name> [--test <folder>]] [--extractor]`

// setupHerdrDeadline is the deadline of each herdr call of setup. [NEW] 5 s, as the store
// commands: a person waits for setup, not an agent start.
const setupHerdrDeadline = 5 * time.Second

// setupBusyTimeout is how long setup waits for the store lock of another Brain process. [NEW]
// 5 s, as the store commands.
const setupBusyTimeout = 5 * time.Second

// codexTrust are the trust steps of spike/m0/FINDINGS.md "E8-S3: trust check", for a profile
// with hook_trust "codex".
const codexTrust = `trust the hook in Codex.
Codex runs a new hook only after you trust it:
  1. Start codex in a terminal. Codex shows "Hooks need review" and "1 hook is new or changed."
  2. Choose "Review hooks". Find the SessionStart hook with the Brain command, from source ~/.codex/hooks.json.
  3. Press t to trust it. Press t on the event list to trust all hooks that need review.
  4. Or choose "Trust all and continue" on the first screen. This trusts every new or changed hook, not only Brain's.
  5. Do not choose "Continue without trusting (hooks won't run)". Brain then gets no context.
  6. Later, type /hooks in Codex to see the trust state or to trust the hook.
  7. Trust again after a Brain update that changes the hook command or timeout.
     Each new Brain hook entry (a hook record entry too) needs trust again.
  codex exec does not show the review screen. It skips an untrusted hook with no warning.
  Run ` + "`brain doctor`" + ` to check.
`

// locality explains where the excerpts of an extractor call go (§7.3 D9, D4).
const locality = `Brain learns lessons from your finished sessions with one headless call of an official CLI
that you choose: claude -p or codex exec, with no tools. Each call sends short excerpts of your
sessions, after the secret filter, to the provider that this CLI is set up for, with the CLI's own
sign-in. Brain never reads its tokens. Your agents already send their sessions to their provider
the same way. Extraction is on after this step. To stop the calls for one workspace or for all:
brain extract off [--workspace <id>]. Capture and the signal rules keep running then.
Brain never picks or switches the extractor CLI by itself.
`

const firstUse = `First use:
  1. In a Claude or Codex session in herdr, have the agent run: brain remember "<one lesson>" --global
     Approve it in the review pane.
  2. Start a new session of the other CLI in herdr. It gets the item at its start
     (Codex at its first prompt).
  3. Run: brain status
`

// setupDeps are the parts of setup that reach outside the process; tests replace them.
type setupDeps struct {
	environ []string
	// sessions lists the herdr sessions (herdr.Sessions).
	sessions func(ctx context.Context) ([]herdr.Session, error)
	// server checks the server version of herdr session t (herdr.Client.CheckServer).
	server func(ctx context.Context, t herdr.Target) (version string, untested bool, err error)
	// lookPath finds a CLI on PATH. A CLI counts as installed when it is there, as in
	// harness.Check.
	lookPath func(file string) (string, error)
	now      func() time.Time
	// profiles holds the profiles of the CLIs that setup offers to hook (adapter.LoadAll); nil
	// is the embedded set.
	profiles fs.FS
}

func osSetupDeps(environ []string) setupDeps {
	return setupDeps{
		environ:  environ,
		sessions: func(ctx context.Context) ([]herdr.Session, error) { return herdr.Sessions(ctx, environ) },
		server: func(ctx context.Context, t herdr.Target) (string, bool, error) {
			c, err := herdr.New(environ, t)
			if err != nil {
				return "", false, err
			}
			return c.CheckServer(ctx)
		},
		lookPath: exec.LookPath,
		now:      time.Now,
	}
}

// setup is `brain setup` (E9-S1), the first command a new user runs, from the herdr `setup`
// action or by its path. It is the only command that makes a binding, so it needs none (HLD
// §8.3). It checks herdr and asks the extractor choice, then shows every write in one plan and
// asks once (E19-S2, D3): yes writes every step, and any other answer writes nothing. A step
// that is already right shows as "already done"; with nothing to write, setup asks nothing.
// It never edits a shell config file.
//
// herdr 0.9.1 has no persistent session identity (§4.2, [M0]), so no binding is ever marked
// "rebind needed", and setup has no rebind path for it (§8.3 continuity).
func setup(ctx context.Context, d setupDeps, args []string, in *bufio.Reader, stdout io.Writer) error {
	name, test, again, err := setupFlags(d, args)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "Step 1: check the herdr version of session %q.\n", name)
	sess, err := herdrSession(ctx, d, name, stdout)
	if err != nil {
		return err
	}

	// The whole plan comes before the first write, so that a refusal or "no" writes nothing.
	var p plan
	storeDir, err := planBind(d, sess, test, &p, stdout)
	if err != nil {
		return err
	}
	bin, err := paths.BinaryPath()
	if err != nil {
		return fmt.Errorf("binary path: %w", err)
	}
	link, err := paths.LinkPath()
	if err != nil {
		return fmt.Errorf("link path: %w", err)
	}
	if err := planLink(bin, link, &p, stdout); err != nil {
		return err
	}
	trustNow, trustAdded, err := planHooks(ctx, d, bin, &p, stdout)
	if err != nil {
		return err
	}

	fmt.Fprintln(stdout, "\nStep 2: the extractor CLI.")
	if err := chooseExtractor(ctx, d, storeDir, again, &p, in, stdout); err != nil {
		return err
	}

	fmt.Fprintln(stdout, "\nStep 3: the changes.")
	fmt.Fprint(stdout, p.text.String())
	applied, err := p.apply(in, stdout, bin)
	if err != nil {
		return err
	}
	if trustNow || applied && trustAdded {
		fmt.Fprint(stdout, "\nStep 4: "+codexTrust)
	}
	fmt.Fprint(stdout, "\n"+firstUse)
	return nil
}

// setupFlags parses the arguments of setup. The session name defaults to this herdr session.
func setupFlags(d setupDeps, args []string) (name, test string, again bool, err error) {
	flags := flag.NewFlagSet("setup", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.StringVar(&name, "session", "", "")
	flags.StringVar(&test, "test", "", "")
	flags.BoolVar(&again, "extractor", false, "")
	if err := flags.Parse(args); err != nil {
		return "", "", false, fmt.Errorf("%w\n%s", err, setupUsage)
	}
	if flags.NArg() != 0 {
		return "", "", false, fmt.Errorf("unexpected argument %q\n%s", flags.Arg(0), setupUsage)
	}
	if test != "" && name == "" {
		return "", "", false, fmt.Errorf("--test needs --session <name>\n%s", setupUsage)
	}
	// This herdr session: a pane of the default session has no HERDR_SESSION (§4.2).
	return cmp.Or(name, lookupEnv(d.environ, "HERDR_SESSION"), "default"), test, again, nil
}

// plan holds the writes of setup and the text that shows them, so that setup asks once for
// all of them (E19-S2).
type plan struct {
	text  strings.Builder // one line or block for each step, done or not
	steps []func() error  // the writes, in order
}

func (p *plan) say(format string, a ...any) { fmt.Fprintf(&p.text, format, a...) }

func (p *plan) add(step func() error) { p.steps = append(p.steps, step) }

// apply asks once and runs every step on yes. It reports whether it ran them. A failed write
// stops setup; a second setup plans the rest again.
func (p *plan) apply(in *bufio.Reader, stdout io.Writer, bin string) (bool, error) {
	if len(p.steps) == 0 {
		fmt.Fprintln(stdout, "Every step is already done. Setup changes nothing.")
		return false, nil
	}
	ok, err := ask(in, stdout, "Apply all of this? [y/N] ")
	if err != nil || !ok {
		if err == nil {
			fmt.Fprintf(stdout, "Setup wrote nothing. Run brain setup again to apply the changes. Run Brain with: %s\n", bin)
		}
		return false, err
	}
	for _, step := range p.steps {
		if err := step(); err != nil {
			return false, err
		}
	}
	return true, nil
}

// chooseExtractor asks which CLI runs the extractor of the store in folder dir, and its model,
// and adds the write of the answer (D4) to p. It first explains where the excerpts go (D9). A
// store with a recorded choice keeps it with no question, unless again. The choice none is
// recorded too, as a store row with no CLI, so that a second setup asks nothing.
func chooseExtractor(ctx context.Context, d setupDeps, dir string, again bool, p *plan, in *bufio.Reader, stdout io.Writer) error {
	fmt.Fprint(stdout, locality)
	cur, err := readExtractor(ctx, dir)
	if err != nil {
		return err
	}
	// A store row has a change time; a store with no row has no choice yet.
	chosen := !cur.Changed.IsZero()
	switch {
	case chosen && !again && cur.CLI == "":
		p.say("- Extractor: none. Already done. To choose one: brain setup --extractor\n")
		return nil
	case chosen && !again:
		p.say("- Extractor: %s, %s. Already done. To change it: brain setup --extractor\n", cur.CLI,
			cmp.Or(cur.Model, "its default model"))
		return nil
	}
	answer, err := askLine(in, stdout, "Which CLI runs the extractor: claude, codex or none? [none] ")
	if err != nil {
		return err
	}
	cli := store.ExtractorCLI(strings.ToLower(answer))
	switch cli {
	case store.ExtractorClaude, store.ExtractorCodex:
	case "", "none":
		planNone(ctx, d, dir, cur, p, stdout)
		return nil
	default:
		fmt.Fprintf(stdout, "Unknown answer %q: the extractor choice stays as it was. To choose one: brain setup --extractor\n", answer)
		return nil
	}
	if _, err := d.lookPath(string(cli)); err != nil {
		fmt.Fprintf(stdout, "%s is not on PATH: each call fails, and brain doctor says why, until it is.\n", cli)
	}
	model, err := askLine(in, stdout, "Model (empty for the default model of the CLI): ")
	if err != nil {
		return err
	}
	if cli == cur.CLI && model == cur.Model {
		p.say("- Extractor: %s, %s. Already done.\n", cli, cmp.Or(model, "its default model"))
		return nil
	}
	p.say("- Extractor: record %s, %s in the store %s.\n", cli, cmp.Or(model, "its default model"), dir)
	p.say("  Each extractor call sends short excerpts, after the secret filter, to the provider of %s, as Step 2 says.\n", cli)
	p.add(func() error {
		if err := writeExtractor(ctx, dir, cli, model, d.now()); err != nil {
			return err
		}
		x, err := readExtractor(ctx, dir)
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "Extractor: %s, %s.\n", cli, cmp.Or(model, "its default model"))
		if !x.On {
			fmt.Fprintln(stdout, "Extraction stays off for the whole store, as you set it: brain extract on turns it on.")
		}
		return nil
	})
	return nil
}

// planNone plans the extractor choice none for the store in folder dir, whose setting is cur.
func planNone(ctx context.Context, d setupDeps, dir string, cur store.ExtractorSetting, p *plan, stdout io.Writer) {
	switch {
	case !cur.Changed.IsZero() && cur.CLI == "":
		p.say("- Extractor: none. Already done.\n")
		return
	case cur.CLI == "":
		p.say("- Extractor: record none in the store %s. Brain makes no extractor call.\n", dir)
	default:
		// A stored choice is cleared: none means no call (D4, D9).
		p.say("- Extractor: clear the choice %s. Brain then makes no extractor call.\n", cur.CLI)
	}
	p.add(func() error {
		if err := writeExtractor(ctx, dir, "", "", d.now()); err != nil {
			return err
		}
		fmt.Fprintln(stdout, "No extractor CLI chosen: Brain makes no extractor call, and brain status says so. "+
			"To choose one later: brain setup --extractor")
		return nil
	})
}

// writeExtractor records cli and model in the store in folder dir; cli "" clears the choice.
func writeExtractor(ctx context.Context, dir string, cli store.ExtractorCLI, model string, now time.Time) error {
	s, err := store.Open(ctx, dir, setupBusyTimeout)
	if err != nil {
		return err
	}
	err = s.Write(ctx, func(tx *sql.Tx) error { return store.SetExtractorCLI(ctx, tx, cli, model, now) })
	return errors.Join(err, s.Close())
}

// readExtractor returns the extractor setting of the store in folder dir. A folder with no
// store file has no chosen CLI: setup does not make the store only to read it.
func readExtractor(ctx context.Context, dir string) (store.ExtractorSetting, error) {
	if _, err := os.Stat(filepath.Join(dir, store.FileName)); errors.Is(err, fs.ErrNotExist) {
		return store.ExtractorSetting{}, nil
	}
	s, err := store.Open(ctx, dir, setupBusyTimeout)
	if err != nil {
		return store.ExtractorSetting{}, err
	}
	var x store.ExtractorSetting
	err = s.Write(ctx, func(tx *sql.Tx) (err error) {
		x, err = store.Extractor(ctx, tx)
		return err
	})
	return x, errors.Join(err, s.Close())
}

// herdrSession finds the herdr session name and checks the version of its server (D6). An
// older herdr than 0.9.1 stops setup with the version and the fix (S81).
func herdrSession(ctx context.Context, d setupDeps, name string, stdout io.Writer) (herdr.Session, error) {
	lctx, cancel := context.WithTimeout(ctx, setupHerdrDeadline)
	ss, err := d.sessions(lctx)
	cancel()
	if err != nil {
		return herdr.Session{}, err
	}
	i := slices.IndexFunc(ss, func(s herdr.Session) bool { return s.Name == name })
	switch {
	case i < 0:
		return herdr.Session{}, fmt.Errorf("herdr has no session %q: start it with `herdr --session %s`, then run brain setup again", name, name)
	case !ss[i].Running:
		return herdr.Session{}, fmt.Errorf("herdr session %q is not running: start it, then run brain setup again", name)
	}
	vctx, cancel := context.WithTimeout(ctx, setupHerdrDeadline)
	v, untested, err := d.server(vctx, herdr.Target{Session: ss[i].Name, Socket: ss[i].Socket})
	cancel()
	if err != nil {
		return herdr.Session{}, fmt.Errorf("herdr session %q: %w", name, err)
	}
	if untested {
		fmt.Fprintf(stdout, "herdr %s: not tested with Brain yet. Brain works, and brain status warns.\n", v)
	} else {
		fmt.Fprintf(stdout, "herdr %s: OK.\n", v)
	}
	return ss[i], nil
}

// planBind plans the binding of sess to the main store, or to the test store folder test
// (§8.3). It refuses a test store that is, holds or sits inside the main store or another test
// store, after links and aliases (S76), and a real session on a test store. A binding that is
// already right stays as it is. It returns the store folder of the binding.
func planBind(d setupDeps, sess herdr.Session, test string, p *plan, stdout io.Writer) (string, error) {
	file, err := paths.BindingsFile()
	if err != nil {
		return "", err
	}
	want, err := wantBinding(sess, test)
	if err != nil {
		return "", err
	}
	// Another setup can write the file while this one waits for the answer. writeBinding
	// compares the file with these bytes, so it never drops that change.
	before, err := readBindingsFile(file)
	if err != nil {
		return "", err
	}
	bs, dropped, err := bindings.Read(file)
	if err != nil {
		return "", fmt.Errorf("%w; fix or remove the bindings file, then run brain setup again", err)
	}
	var cur *bindings.Binding
	for i, b := range bs {
		if b.Session == sess.Name {
			cur = &bs[i]
		}
	}
	switch {
	case cur != nil && cur.Socket == want.Socket && cur.Store == want.Store && cur.Kind == want.Kind:
		p.say("- Binding: session %q is bound to %s. Already done.\n", sess.Name, want.Store)
		return want.Store, nil
	case want.Kind == bindings.KindTest && (sess.Default || cur != nil && cur.Kind == bindings.KindMain):
		return "", fmt.Errorf("refused: session %q is a real herdr session, and a real session cannot use a test store", sess.Name)
	}

	// The new file: every kept binding of another session, and want. A bad test binding that
	// Read dropped leaves the file (owner decision 2026-09-28: it does not stop the others).
	next := slices.DeleteFunc(slices.Clone(bs), func(b bindings.Binding) bool { return b.Session == sess.Name })
	want.Created = d.now().UTC()
	next = append(next, want)
	// The checks compare folders by identity, so they need the store folder. A new folder
	// is made only after the answer yes, and the checks run again then.
	if _, err := os.Stat(want.Store); err == nil {
		if err := checkBinding(file, next, want); err != nil {
			return "", err
		}
	}

	p.say("- Binding: write %s: session %q, socket %s, store %s (%s).\n",
		file, want.Session, want.Socket, want.Store, want.Kind)
	showReplaced(&p.text, sess.Name, cur, dropped)
	p.add(func() error {
		if err := writeBinding(file, before, next, want); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "Bound session %q to %s.\n", sess.Name, want.Store)
		return nil
	})
	return want.Store, nil
}

// showReplaced shows the binding of session that setup replaces, and the bad test bindings of
// other sessions that it removes.
func showReplaced(w io.Writer, session string, cur *bindings.Binding, dropped []bindings.Dropped) {
	if cur != nil {
		fmt.Fprintf(w, "  Session %q is bound to %s (%s). Setup replaces this binding.\n", cur.Session, cur.Store, cur.Kind)
	}
	for _, x := range dropped {
		if x.Binding.Session != session {
			fmt.Fprintf(w, "  Setup also removes the bad test binding of session %q: %v\n", x.Binding.Session, x.Err)
		}
	}
}

// writeBinding makes the store folder of want, checks next again and writes it to file. It
// holds a lock on the bindings folder, and refuses when the file is no longer before. On a
// failure it removes the folders that it made.
func writeBinding(file string, before []byte, next []bindings.Binding, want bindings.Binding) error {
	unlock, err := lockDir(filepath.Dir(file))
	if err != nil {
		return err
	}
	defer unlock()
	now, err := readBindingsFile(file)
	if err != nil {
		return err
	}
	if !bytes.Equal(before, now) {
		return fmt.Errorf("refused: %s changed while setup waited for your answer; run brain setup again", file)
	}
	made, err := mkdirs(want.Store)
	if err != nil {
		return err
	}
	if err := checkBinding(file, next, want); err != nil {
		return errors.Join(err, rmdirs(made))
	}
	if err := bindings.Write(file, next); err != nil {
		return errors.Join(err, rmdirs(made))
	}
	return nil
}

// readBindingsFile returns the bytes of the bindings file, or nil when it does not exist.
func readBindingsFile(file string) ([]byte, error) {
	b, err := os.ReadFile(file)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	return b, err
}

// lockDir makes dir and takes an exclusive lock on it, so two setups write the bindings file
// one after the other. The lock is on the folder, so it leaves no lock file.
func lockDir(dir string) (unlock func(), err error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	f, err := os.Open(dir)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close() // the lock error is the one to report
		return nil, fmt.Errorf("lock %s: %w", dir, err)
	}
	return func() { _ = f.Close() }, nil // the close drops the lock
}

// wantBinding is the binding of sess to the main store, or to the test store folder test.
func wantBinding(sess herdr.Session, test string) (bindings.Binding, error) {
	want := bindings.Binding{Session: sess.Name, Socket: sess.Socket, Kind: bindings.KindMain}
	var err error
	if test == "" {
		want.Store, err = paths.MainStore()
	} else {
		want.Store, err = filepath.Abs(test)
		want.Kind = bindings.KindTest
	}
	return want, err
}

// checkBinding runs the checks of the bindings file on next. A test store must also stay
// apart from the main store folder when no session is bound to it yet (S76).
func checkBinding(file string, next []bindings.Binding, want bindings.Binding) error {
	if err := bindings.Check(file, next); err != nil {
		return fmt.Errorf("refused: %w", err)
	}
	if want.Kind != bindings.KindTest {
		return nil
	}
	main, err := paths.MainStore()
	if err != nil {
		return err
	}
	if _, err := os.Stat(main); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	nested, err := bindings.Nested(main, want.Store)
	if err != nil {
		return err
	}
	if nested {
		return fmt.Errorf("refused: test store %s is, holds or sits inside the main store %s", want.Store, main)
	}
	return nil
}

// mkdirs makes dir and its missing parents with mode 0o700, as the bindings file, and
// returns the folders that it made, the deepest first.
func mkdirs(dir string) ([]string, error) {
	var made []string
	for p := dir; ; p = filepath.Dir(p) {
		if _, err := os.Lstat(p); err == nil {
			break
		} else if !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
		made = append(made, p)
		if filepath.Dir(p) == p {
			break
		}
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return made, nil
}

// rmdirs removes the empty folders that mkdirs made, the deepest first.
func rmdirs(made []string) error {
	var errs []error
	for _, p := range made {
		errs = append(errs, os.Remove(p))
	}
	return errors.Join(errs...)
}

// planHooks plans the Brain hook entry for the CLI of each profile that is installed (E8-S2,
// E16-S10), and its hook record entries. It shows each entry and the uninstall steps. It
// reports whether the entry of a profile with hook_trust "codex" is in place now, and whether
// the plan adds one, so that setup shows the trust steps.
func planHooks(ctx context.Context, d setupDeps, bin string, p *plan, stdout io.Writer) (trustNow, trustAdded bool, err error) {
	inst, err := installer(bin)
	if err != nil {
		return false, false, err
	}
	ps, err := adapter.LoadAll(d.profiles)
	if err != nil {
		return false, false, err
	}
	var hooked bool
	for _, pr := range ps {
		kind := pr.Kind
		if _, err := d.lookPath(kind); err != nil {
			p.say("- %s is not installed. Setup skips it.\n", kind)
			continue
		}
		e, st, err := inst.WouldInstall(pr)
		if err != nil {
			return false, false, err
		}
		start := false
		switch st {
		case install.Present:
			p.say("- The Brain hook for %s is in %s. Already done.\n", kind, e.Target)
		case install.Edited:
			p.say("- %s holds a Brain hook entry for %s that you edited. Brain keeps it.\n", e.Target, kind)
			continue
		default:
			p.say("- Add this entry to the %s array of %s:\n    %s\n", pr.Events.Start, e.Target, e.Text)
			start = true
		}
		records, err := planRecords(ctx, inst, pr, p)
		if err != nil {
			return false, false, err
		}
		codex := pr.HookTrust == adapter.TrustCodex
		trustNow = trustNow || codex && !start
		trustAdded = trustAdded || codex && start
		if start || len(records) > 0 {
			hooked = true
			p.add(func() error { return applyHooks(inst, pr, e.Target, start, records, stdout) })
		}
	}
	if hooked {
		p.say("- Brain records each hook entry it adds in %s, with a copy of each hook file before its first change.\n", inst.Record)
		p.say("- To remove the hook entries later: brain uninstall (a dry run), then brain uninstall --apply.\n" +
			"  It removes only Brain's entries, and keeps every other byte of each file.\n")
	}
	return trustNow, trustAdded, nil
}

// installer is the hook installer of the user's home and install record, for binary bin.
func installer(bin string) (*install.Installer, error) {
	home, err := paths.Home()
	if err != nil {
		return nil, err
	}
	record, err := paths.InstallFile()
	if err != nil {
		return nil, err
	}
	return &install.Installer{Home: home, Record: record, Binary: bin}, nil
}

// planRecords plans the hook record entries of p (E11-S5, §6.1), which setup adds after its
// start entry. It plans them only when the profile turns them on, after the E11-S1 test, and
// when the E8 guard does not say retest: a CLI version that no test passed, or hook input of
// an unknown shape. It returns the hooks to add.
func planRecords(ctx context.Context, inst *install.Installer, p *adapter.Profile, pl *plan) ([]adapter.Hook, error) {
	hs := p.Hooks()[1:]
	if len(hs) == 0 {
		return nil, nil
	}
	var add []adapter.Hook
	var texts []string
	var target string
	for _, h := range hs {
		e, st, err := inst.WouldInstallHook(p, h)
		if err != nil {
			return nil, err
		}
		switch st {
		case install.Present:
		case install.Edited:
			pl.say("  %s holds a Brain %s entry for %s that you edited. Brain keeps it.\n", e.Target, h.CLIEvent, p.Kind)
		default:
			add, target = append(add, h), e.Target
			texts = append(texts, fmt.Sprintf("    %s: %s\n", h.CLIEvent, e.Text))
		}
	}
	if len(add) == 0 {
		return nil, nil
	}
	ok, err := guard(ctx, inst, p)
	if err != nil {
		return nil, err
	}
	if !ok {
		pl.say("  %s needs a retest first (brain doctor). Setup adds no hook record entry.\n", p.Kind)
		return nil, nil
	}
	pl.say("  Brain also stores hook records of %s: prompts, commands and replies. Add these entries to %s:\n", p.Kind, target)
	for _, t := range texts {
		pl.say("%s", t)
	}
	return add, nil
}

// applyHooks adds the start entry of p when start is true, then the hook record entries
// records. A start entry that you edited since the plan stays, with no record entry.
func applyHooks(inst *install.Installer, p *adapter.Profile, target string, start bool, records []adapter.Hook, stdout io.Writer) error {
	if start {
		// A CLI can be installed before it made its config folder.
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return err
		}
		st, err := inst.Install(p)
		if err != nil {
			return err
		}
		if st == install.Edited {
			fmt.Fprintf(stdout, "%s holds a Brain hook entry for %s that you edited. Brain keeps it.\n", target, p.Kind)
			return nil
		}
		fmt.Fprintf(stdout, "Added the Brain hook for %s.\n", p.Kind)
	}
	if len(records) == 0 {
		return nil
	}
	for _, h := range records {
		if _, err := inst.InstallHook(p, h); err != nil {
			return err
		}
	}
	fmt.Fprintf(stdout, "Added the hook record entries for %s.\n", p.Kind)
	if p.HookTrust == adapter.TrustCodex {
		fmt.Fprint(stdout, "Codex needs trust again for each new hook entry: type /hooks and press t on each.\n")
	}
	return nil
}

// guard is the E8 guard of p (§8.5): false when harness.Check says retest.
func guard(ctx context.Context, inst *install.Installer, p *adapter.Profile) (bool, error) {
	conf, err := paths.ConformanceFile()
	if err != nil {
		return false, err
	}
	data, err := paths.DataHome()
	if err != nil {
		return false, err
	}
	// The guard needs no panes: detection does not change retest.
	ss, err := harness.Check(ctx, noPanes{}, harness.Files{Install: inst.Record, Conformance: conf,
		Retest: filepath.Join(data, "retest")}, []*adapter.Profile{p})
	if err != nil {
		return false, err
	}
	return !ss[0].Retest, nil
}

// noPanes is a pane list with no pane.
type noPanes struct{}

func (noPanes) Panes(context.Context) ([]herdr.Pane, error) { return nil, nil }

// planLink shows the binary path and plans the link -> bin.
// It never replaces a file that it did not make (D3).
func planLink(bin, link string, p *plan, stdout io.Writer) error {
	p.say("- Brain binary: %s\n", bin)
	target, err := filepath.EvalSymlinks(link)
	switch {
	case err == nil && target == bin:
		p.say("  %s already points to it. Already done.\n", link)
		return nil
	case err == nil:
		p.say("  %s exists and points elsewhere. Brain does not change it.\n  Run Brain with: %s\n", link, bin)
		return nil
	case !errors.Is(err, os.ErrNotExist):
		return fmt.Errorf("check link %s: %w", link, err)
	}
	if _, err := os.Lstat(link); err == nil {
		p.say("  %s exists but is broken. Brain does not change it.\n  Run Brain with: %s\n", link, bin)
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("check link %s: %w", link, err)
	}

	p.say("  Make the symlink %s -> %s.\n", link, bin)
	p.add(func() error {
		if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
			return fmt.Errorf("create link dir: %w", err)
		}
		if err := os.Symlink(bin, link); err != nil {
			return fmt.Errorf("create link: %w", err)
		}
		fmt.Fprintf(stdout, "Linked %s.\n", link)
		if !onPath(filepath.Dir(link)) {
			fmt.Fprintf(stdout, "%s is not on your PATH. Add it yourself, or run Brain with: %s\n", filepath.Dir(link), bin)
		}
		return nil
	})
	return nil
}

// ask prints question and reads one answer line. Only "y" or "yes" is yes; EOF with no answer
// means "no". It ends the line after the answer, as a terminal shows it.
func ask(in *bufio.Reader, stdout io.Writer, question string) (bool, error) {
	answer, err := askLine(in, stdout, question)
	a := strings.ToLower(answer)
	return a == "y" || a == "yes", err
}

// askLine asks question and returns the answer line with no spaces around it. No answer is "".
func askLine(in *bufio.Reader, stdout io.Writer, question string) (string, error) {
	fmt.Fprint(stdout, question)
	answer, err := in.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", fmt.Errorf("read answer: %w", err)
	}
	fmt.Fprintln(stdout)
	return strings.TrimSpace(answer), nil
}

func onPath(dir string) bool {
	for _, d := range filepath.SplitList(os.Getenv("PATH")) {
		if filepath.Clean(d) == dir {
			return true
		}
	}
	return false
}

// lookupEnv returns the value of key in environ; the last entry wins, as in os/exec.
func lookupEnv(environ []string, key string) string {
	var v string
	for _, kv := range environ {
		if k, val, ok := strings.Cut(kv, "="); ok && k == key {
			v = val
		}
	}
	return v
}
