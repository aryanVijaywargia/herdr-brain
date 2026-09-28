// Command brain-eval is the eval runner of Brain (HLD §12.1, E10). It is a separate tool, not
// a brain command, and scripts/build.sh does not build it. It runs eval cases in the
// brain-eval test herdr session against the test store, and never touches the real store,
// a real pane, the CLI config files or the install record.
//
// This story (E10-S1) gives the runner frame and its isolation rules. The probe files and the
// results come in E10-S2, the M1 stages and conformance.json in E10-S3. Until then one ad-hoc
// case comes from the flags, for a hand test.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/aryanVijaywargia/herdr-brain/internal/herdr"
	"github.com/aryanVijaywargia/herdr-brain/internal/paths"
)

const usage = `usage: brain-eval [--step <shell command>]... [--probe claude|codex]... [--prompt <text>]

Runs one ad-hoc case in the brain-eval test session: each --step in a shell pane of a fresh
scratch git folder, then --prompt to a fresh CLI of each --probe. "{{marker}}" in a step or the
prompt is the fresh marker of the case. With no step and no probe, it only runs the checks.
Set up first: brain setup, and brain setup --session brain-eval --test <folder>.
`

// repeated is a flag that can be given more than once.
type repeated []string

func (r *repeated) String() string     { return fmt.Sprint(*r) }
func (r *repeated) Set(v string) error { *r = append(*r, v); return nil }

func main() {
	os.Exit(run(os.Args[1:], os.Environ(), os.Stdout, os.Stderr))
}

func run(args, environ []string, stdout, stderr io.Writer) int {
	cases, err := parse(args)
	if errors.Is(err, flag.ErrHelp) {
		fmt.Fprint(stdout, usage)
		return 0
	}
	if err != nil {
		fmt.Fprintf(stderr, "brain-eval: %v\n\n%s", err, usage)
		return 2
	}
	r, err := newRunner(environ, stdout)
	if err != nil {
		fmt.Fprintf(stderr, "brain-eval: %v\n", err)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	out, err := r.Run(ctx, cases)
	for _, o := range out {
		fmt.Fprintf(stdout, "case %s: marker %s\n", o.CaseID, o.Marker)
		for _, rep := range o.Replies {
			fmt.Fprintf(stdout, "--- %s\n%s\n", rep.Harness, rep.Text)
		}
	}
	if err != nil {
		fmt.Fprintf(stderr, "brain-eval: %v\n", err)
		return 1
	}
	return 0
}

func parse(args []string) ([]Case, error) {
	flags := flag.NewFlagSet("brain-eval", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	var steps, probes repeated
	flags.Var(&steps, "step", "")
	flags.Var(&probes, "probe", "")
	prompt := flags.String("prompt", "", "")
	if err := flags.Parse(args); err != nil {
		return nil, err
	}
	if flags.NArg() != 0 {
		return nil, fmt.Errorf("unexpected argument %q", flags.Arg(0))
	}
	if (len(probes) == 0) != (*prompt == "") {
		return nil, errors.New("--probe and --prompt go together")
	}
	if len(steps) == 0 && len(probes) == 0 {
		return nil, nil
	}
	c := Case{ID: "adhoc", Setup: steps}
	for _, p := range probes {
		if p != "claude" && p != "codex" {
			return nil, fmt.Errorf("--probe %q: want claude or codex", p)
		}
		c.Probes = append(c.Probes, Probe{Harness: p, Prompt: *prompt})
	}
	return []Case{c}, nil
}

// newRunner returns the Runner of a real run with the process environment environ.
func newRunner(environ []string, stdout io.Writer) (*Runner, error) {
	r := &Runner{
		Environ: environ,
		Herdr: func(t herdr.Target) (Herdr, error) {
			c, err := herdr.New(environ, t)
			if err != nil {
				return nil, err
			}
			return c, nil
		},
		Sessions: func(ctx context.Context) ([]herdr.Session, error) { return herdr.Sessions(ctx, environ) },
		StartServer: func(ctx context.Context, name string) (Server, error) {
			s, err := herdr.StartServer(ctx, environ, name)
			if err != nil {
				return nil, err
			}
			return s, nil
		},
		Alive:    alive,
		Timeouts: defaultTimeouts,
		Stdout:   stdout,
	}
	for _, f := range []struct {
		dst *string
		fn  func() (string, error)
	}{{&r.Bindings, paths.BindingsFile}, {&r.Install, paths.InstallFile}, {&r.MainStore, paths.MainStore}, {&r.Lock, paths.EvalLock}} {
		p, err := f.fn()
		if err != nil {
			return nil, err
		}
		*f.dst = p
	}
	return r, nil
}

// alive reports whether process pid still runs. EPERM means that it runs as another user.
func alive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
