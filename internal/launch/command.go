package launch

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/aryanVijaywargia/herdr-brain/internal/resolver"
)

// callDeadline is the deadline of the herdr call of the resolve, and of `<cli> --version`.
const callDeadline = 5 * time.Second

// Deps are the parts of a launch command that reach outside the process; tests replace them.
type Deps struct {
	Environ  []string
	Self     string // the running brain binary: a launch never runs it as the CLI
	Bindings string // the bindings file
	Pane     resolver.PaneFunc
	Now      func() time.Time
	Stderr   io.Writer
	// Exec replaces the process with bin, with the args argv and the env environ
	// (syscall.Exec): the signals, the terminal and the exit code are then the CLI's.
	Exec func(bin string, argv, environ []string) error
}

// Main is `brain <kind> [args]`: it starts the CLI of kind in place of Brain. In a live pane of
// a bound herdr session, a form that gets Brain flags starts with the Brain flags of a new
// launch, then args with no change. Every other call starts the plain CLI with args, and
// prints one line on stderr with the reason. A refuse conflict starts nothing. Main writes
// nothing to a CLI config file. It returns only on an error.
func Main(ctx context.Context, d Deps, kind string, args []string) error {
	data, err := load(kind)
	if err != nil {
		return err
	}
	bin, err := findCLI(d.Environ, kind, d.Self)
	if err != nil {
		return err
	}
	argv, notes, plain, err := brainArgv(ctx, d, data, bin, args)
	if err != nil {
		return err
	}
	if plain != "" {
		argv = args
		notes = []string{fmt.Sprintf("starting the plain %s with no Brain flags: %s", kind, plain)}
	}
	for _, n := range notes {
		if _, err := fmt.Fprintf(d.Stderr, "brain %s: %s\n", kind, n); err != nil {
			return err
		}
	}
	return d.Exec(bin, append([]string{kind}, argv...), d.Environ)
}

// brainArgv returns the args of a Brain launch of the CLI bin of d with the user's args, and
// the notes to print. plain is the reason when the call gets no Brain flags.
func brainArgv(ctx context.Context, d Deps, data *cliData, bin string, args []string) (argv, notes []string, plain string, err error) {
	a := parse(data, args)
	if r := a.plain(); r != "" {
		return nil, nil, r, nil
	}
	r, plain := resolve(ctx, d)
	if plain != "" {
		return nil, nil, plain, nil
	}
	if err := a.refused(); err != nil {
		return nil, nil, "", err
	}
	if plain := supported(ctx, data, bin, d.Environ); plain != "" {
		return nil, nil, plain, nil
	}
	dir, err := Create(ctx, r.Binding, r.WorkspaceID, r.PaneID, data.kind, d.Now())
	if err != nil {
		return nil, nil, "", err
	}
	argv, notes, err = a.argv(dir, Plan{})
	return argv, notes, "", err
}

// resolve resolves the pane of the call with the resolver. plain is why the call gets no
// Brain flags: it is not from a live pane of a bound herdr session, or Brain cannot read the
// bindings file.
func resolve(ctx context.Context, d Deps) (r *resolver.Resolved, plain string) {
	ctx, cancel := context.WithTimeout(ctx, callDeadline)
	defer cancel()
	res, err := resolver.New(d.Bindings, openNoStore, d.Pane).Resolve(ctx, d.Environ, "")
	if err != nil {
		return nil, err.Error()
	}
	switch v := res.(type) {
	case *resolver.NoStore:
		if v.Err != nil {
			return nil, fmt.Sprintf("%s (%v)", v.Reason, v.Err)
		}
		return nil, string(v.Reason)
	case *resolver.Denied:
		return nil, fmt.Sprintf("herdr does not confirm pane %s in workspace %s (%s)", v.PaneID, v.WorkspaceID, v.Reason)
	case *resolver.Resolved:
		_ = v.Store.Close() // openNoStore opened nothing
		return v, ""
	}
	panic(fmt.Sprintf("launch: unknown resolver result %T", res))
}

// supported returns why the CLI bin of d gets no Brain flags: its version is older than the
// min_version of d, or Brain cannot read its version. "" is a supported CLI.
func supported(ctx context.Context, d *cliData, bin string, environ []string) string {
	v, err := cliVersion(ctx, bin, environ)
	if err != nil {
		return err.Error()
	}
	if d.older(v) {
		return fmt.Sprintf("%s %s is older than %s", d.kind, v, d.MinVersion)
	}
	return ""
}

// noStore is the store of the resolve of a launch. The resolve only checks the pane: the
// launch writes its own record with Create, writes no receipt, and leaves the agent checks of
// the pane to the next Brain call.
type noStore struct{}

func openNoStore(context.Context, string) (resolver.Store, error) { return noStore{}, nil }

func (noStore) WriteWithheld(context.Context, resolver.Withheld) error {
	return errors.New("a launch writes no receipts")
}

func (noStore) RecordAgentSession(context.Context, string, string, string) error {
	return errors.New("a launch writes no receipts")
}

func (noStore) UncheckedAgentSessions(context.Context, string, string) ([]string, error) {
	return nil, nil
}

func (noStore) SetAgentCheck(context.Context, string, string, string, resolver.AgentCheck) error {
	return nil
}

func (noStore) Close() error { return nil }

// cliVersion runs `bin --version` and returns the first version word of its output.
func cliVersion(ctx context.Context, bin string, environ []string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, callDeadline)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "--version")
	cmd.Env = environ
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("%s --version: %w", bin, err)
	}
	for _, w := range strings.Fields(string(out)) {
		if versionRe.MatchString(w) {
			return w, nil
		}
	}
	return "", fmt.Errorf("%s --version: no version in %q", bin, out)
}

// findCLI returns the first executable file named kind in an absolute folder of the PATH of
// environ that is not the brain binary self: not self, not a link to it and not a copy of it.
// So `brain claude` never runs itself, also with a brain link named claude earlier on PATH.
func findCLI(environ []string, kind, self string) (string, error) {
	me, err := os.Stat(self)
	if err != nil {
		return "", fmt.Errorf("brain binary: %w", err)
	}
	for _, dir := range filepath.SplitList(lookup(environ, "PATH")) {
		if !filepath.IsAbs(dir) {
			continue // as exec.LookPath: the working folder must not supply the CLI
		}
		p := filepath.Join(dir, kind)
		fi, err := os.Stat(p)
		// As exec.LookPath, a PATH entry that is not an executable file is skipped.
		if err != nil || !fi.Mode().IsRegular() || fi.Mode().Perm()&0o111 == 0 {
			continue
		}
		if os.SameFile(me, fi) {
			continue
		}
		if fi.Size() == me.Size() {
			same, err := sameContent(self, p)
			if err != nil {
				return "", err
			}
			if same {
				continue
			}
		}
		return p, nil
	}
	return "", fmt.Errorf("%s not found on PATH (Brain skips its own binary and its copies)", kind)
}

// sameContent reports whether the files a and b hold the same bytes.
func sameContent(a, b string) (bool, error) {
	ha, err := fileHash(a)
	if err != nil {
		return false, err
	}
	hb, err := fileHash(b)
	if err != nil {
		return false, err
	}
	return bytes.Equal(ha, hb), nil
}

// fileHash returns the SHA-256 of the file at path.
func fileHash(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close() //nolint:errcheck // read-only: Close cannot lose data
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return nil, err
	}
	return h.Sum(nil), nil
}
