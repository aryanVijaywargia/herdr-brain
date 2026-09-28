package main

import (
	"bufio"
	"cmp"
	"context"
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
	"time"

	"github.com/aryanVijaywargia/herdr-brain/internal/adapter"
	"github.com/aryanVijaywargia/herdr-brain/internal/bindings"
	"github.com/aryanVijaywargia/herdr-brain/internal/herdr"
	"github.com/aryanVijaywargia/herdr-brain/internal/install"
	"github.com/aryanVijaywargia/herdr-brain/internal/paths"
)

const setupUsage = `usage: brain setup [--session <name> [--test <folder>]]`

// setupHerdrDeadline is the deadline of each herdr call of setup. [NEW] 5 s, as the store
// commands: a person waits for setup, not an agent start.
const setupHerdrDeadline = 5 * time.Second

// harnesses are the CLIs that setup offers to hook, in order (M1 profiles, HLD §8.2).
var harnesses = []string{"claude", "codex"}

// codexTrust are the trust steps of spike/m0/FINDINGS.md "E8-S3: trust check".
const codexTrust = `Codex runs a new hook only after you trust it:
  1. Start codex in a terminal. Codex shows "Hooks need review" and "1 hook is new or changed."
  2. Choose "Review hooks". Find the SessionStart hook with the Brain command, from source ~/.codex/hooks.json.
  3. Press t to trust it. Press t on the event list to trust all hooks that need review.
  4. Or choose "Trust all and continue" on the first screen. This trusts every new or changed hook, not only Brain's.
  5. Do not choose "Continue without trusting (hooks won't run)". Brain then gets no context.
  6. Later, type /hooks in Codex to see the trust state or to trust the hook.
  7. Trust again after a Brain update that changes the hook command or timeout.
  codex exec does not show the review screen. It skips an untrusted hook with no warning.
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
// §8.3). Each step says what it will do, and each write asks first; there is no --yes in M1.
// A step that is already right changes nothing and asks nothing. It never edits a shell
// config file.
//
// herdr 0.9.1 has no persistent session identity (§4.2, [M0]), so no binding is ever marked
// "rebind needed", and setup has no rebind path for it (§8.3 continuity).
func setup(ctx context.Context, d setupDeps, args []string, in *bufio.Reader, stdout io.Writer) error {
	flags := flag.NewFlagSet("setup", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	name := flags.String("session", "", "")
	test := flags.String("test", "", "")
	if err := flags.Parse(args); err != nil {
		return fmt.Errorf("%w\n%s", err, setupUsage)
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected argument %q\n%s", flags.Arg(0), setupUsage)
	}
	if *test != "" && *name == "" {
		return fmt.Errorf("--test needs --session <name>\n%s", setupUsage)
	}
	// This herdr session: a pane of the default session has no HERDR_SESSION (§4.2).
	*name = cmp.Or(*name, lookupEnv(d.environ, "HERDR_SESSION"), "default")

	fmt.Fprintf(stdout, "Step 1: check the herdr version of session %q.\n", *name)
	sess, err := herdrSession(ctx, d, *name, stdout)
	if err != nil {
		return err
	}

	if *test == "" {
		fmt.Fprintf(stdout, "\nStep 2: bind herdr session %q to the main store.\n", *name)
	} else {
		fmt.Fprintf(stdout, "\nStep 2: bind herdr session %q to the test store %s.\n", *name, *test)
	}
	if err := bind(d, sess, *test, in, stdout); err != nil {
		return err
	}

	fmt.Fprintln(stdout, "\nStep 3: the brain command on your PATH.")
	bin, err := paths.BinaryPath()
	if err != nil {
		return fmt.Errorf("binary path: %w", err)
	}
	link, err := paths.LinkPath()
	if err != nil {
		return fmt.Errorf("link path: %w", err)
	}
	if err := offerLink(bin, link, in, stdout); err != nil {
		return err
	}

	fmt.Fprintln(stdout, "\nStep 4: the Brain hook in each agent CLI.")
	codex, err := hooks(d, bin, in, stdout)
	if err != nil {
		return err
	}
	if codex {
		fmt.Fprint(stdout, "\nStep 5: trust the hook in Codex.\n"+codexTrust)
	}
	fmt.Fprint(stdout, "\n"+firstUse)
	return nil
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

// bind binds sess to the main store, or to the test store folder test (§8.3). It refuses a
// test store that is, holds or sits inside the main store or another test store, after links
// and aliases (S76), and a real session on a test store. A binding that is already right
// stays as it is.
func bind(d setupDeps, sess herdr.Session, test string, in *bufio.Reader, stdout io.Writer) error {
	file, err := paths.BindingsFile()
	if err != nil {
		return err
	}
	want, err := wantBinding(sess, test)
	if err != nil {
		return err
	}
	bs, dropped, err := bindings.Read(file)
	if err != nil {
		return fmt.Errorf("%w; fix or remove the bindings file, then run brain setup again", err)
	}
	var cur *bindings.Binding
	for i, b := range bs {
		if b.Session == sess.Name {
			cur = &bs[i]
		}
	}
	switch {
	case cur != nil && cur.Socket == want.Socket && cur.Store == want.Store && cur.Kind == want.Kind:
		fmt.Fprintf(stdout, "Session %q is already bound to %s.\n", sess.Name, want.Store)
		return nil
	case want.Kind == bindings.KindTest && (sess.Default || cur != nil && cur.Kind == bindings.KindMain):
		return fmt.Errorf("refused: session %q is a real herdr session, and a real session cannot use a test store", sess.Name)
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
			return err
		}
	}

	if cur != nil {
		fmt.Fprintf(stdout, "Session %q is bound to %s (%s). Setup replaces this binding.\n", cur.Session, cur.Store, cur.Kind)
	}
	for _, x := range dropped {
		if x.Binding.Session != sess.Name {
			fmt.Fprintf(stdout, "Setup also removes the bad test binding of session %q: %v\n", x.Binding.Session, x.Err)
		}
	}
	ok, err := ask(in, stdout, fmt.Sprintf("Write %s: session %q, socket %s, store %s (%s)? [y/N] ",
		file, want.Session, want.Socket, want.Store, want.Kind))
	if err != nil || !ok {
		if err == nil {
			fmt.Fprintf(stdout, "No binding written. Brain gives session %q nothing until it is bound.\n", sess.Name)
		}
		return err
	}
	if err := writeBinding(file, next, want); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "Bound session %q to %s.\n", sess.Name, want.Store)
	return nil
}

// writeBinding makes the store folder of want, checks next again and writes it to file. On a
// failure it removes the folders that it made.
func writeBinding(file string, next []bindings.Binding, want bindings.Binding) error {
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

// hooks offers the Brain hook entry to each installed CLI (E8-S2). It shows the entry and the
// uninstall steps before it asks. It reports whether the Codex entry is in place, so that
// setup shows the trust steps.
func hooks(d setupDeps, bin string, in *bufio.Reader, stdout io.Writer) (codex bool, err error) {
	home, err := paths.Home()
	if err != nil {
		return false, err
	}
	record, err := paths.InstallFile()
	if err != nil {
		return false, err
	}
	inst := &install.Installer{Home: home, Record: record, Binary: bin}
	for _, kind := range harnesses {
		if _, err := d.lookPath(kind); err != nil {
			fmt.Fprintf(stdout, "%s is not installed. Setup skips it.\n", kind)
			continue
		}
		p, err := adapter.Load(kind)
		if err != nil {
			return false, err
		}
		e, st, err := inst.WouldInstall(p)
		if err != nil {
			return false, err
		}
		switch st {
		case install.Present:
			fmt.Fprintf(stdout, "The Brain hook for %s is already in %s.\n", kind, e.Target)
		case install.Edited:
			fmt.Fprintf(stdout, "%s holds a Brain hook entry for %s that you edited. Brain keeps it.\n", e.Target, kind)
			continue
		default:
			fmt.Fprintf(stdout, "Brain adds this entry to the %s array of %s:\n  %s\n"+
				"To remove it later: brain uninstall (a dry run), then brain uninstall --apply.\n"+
				"It removes only this entry, and keeps every other byte of the file.\n",
				p.Events.Start, e.Target, e.Text)
			ok, err := ask(in, stdout, fmt.Sprintf("Add the Brain hook for %s? [y/N] ", kind))
			if err != nil {
				return false, err
			}
			if !ok {
				fmt.Fprintf(stdout, "No change to %s. %s sessions get no Brain items.\n", e.Target, kind)
				continue
			}
			if st, err = inst.Install(p); err != nil {
				return false, err
			}
			if st == install.Edited {
				fmt.Fprintf(stdout, "%s holds a Brain hook entry for %s that you edited. Brain keeps it.\n", e.Target, kind)
				continue
			}
			fmt.Fprintf(stdout, "Added the Brain hook for %s.\n", kind)
		}
		codex = codex || kind == "codex"
	}
	return codex, nil
}

// offerLink shows the binary path and asks before it makes link -> bin.
// It never replaces a file that it did not make.
func offerLink(bin, link string, in *bufio.Reader, stdout io.Writer) error {
	fmt.Fprintf(stdout, "Brain binary: %s\n", bin)
	target, err := filepath.EvalSymlinks(link)
	switch {
	case err == nil && target == bin:
		fmt.Fprintf(stdout, "%s already points to it.\n", link)
		return nil
	case err == nil:
		fmt.Fprintf(stdout, "%s exists and points elsewhere. Brain did not change it.\nRun Brain with: %s\n", link, bin)
		return nil
	case !errors.Is(err, os.ErrNotExist):
		return fmt.Errorf("check link %s: %w", link, err)
	}
	if _, err := os.Lstat(link); err == nil {
		fmt.Fprintf(stdout, "%s exists but is broken. Brain did not change it.\nRun Brain with: %s\n", link, bin)
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("check link %s: %w", link, err)
	}

	ok, err := ask(in, stdout, fmt.Sprintf("Make the symlink %s -> %s? [y/N] ", link, bin))
	if err != nil {
		return err
	}
	if !ok {
		fmt.Fprintf(stdout, "No link made. Run Brain with: %s\n", bin)
		return nil
	}
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
}

// ask prints question and reads one answer line. Only "y" or "yes" is yes; EOF with no answer
// means "no". It ends the line after the answer, as a terminal shows it.
func ask(in *bufio.Reader, stdout io.Writer, question string) (bool, error) {
	fmt.Fprint(stdout, question)
	answer, err := in.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return false, fmt.Errorf("read answer: %w", err)
	}
	fmt.Fprintln(stdout)
	a := strings.ToLower(strings.TrimSpace(answer))
	return a == "y" || a == "yes", nil
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
