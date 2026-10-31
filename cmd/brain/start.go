package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"

	"github.com/aryanVijaywargia/herdr-brain/internal/bindings"
	"github.com/aryanVijaywargia/herdr-brain/internal/brained"
	"github.com/aryanVijaywargia/herdr-brain/internal/herdr"
	"github.com/aryanVijaywargia/herdr-brain/internal/install"
	"github.com/aryanVijaywargia/herdr-brain/internal/paths"
)

// startHerdrDeadline is the deadline of the herdr call that lists the workspaces. 5 s, as
// setupHerdrDeadline.
const startHerdrDeadline = 5 * time.Second

// startDeps are the parts of start that reach outside the process; tests replace them.
type startDeps struct {
	environ []string
	// running reports whether a brained holds the brained lock of this session (brained.Running).
	running func(environ []string) (bool, error)
	// spawn starts `brain brained` with environ and does not wait for it.
	spawn func(environ []string) error
	// relink points the ~/.local/bin/brain link that setup made to this binary when it is
	// stale (#459).
	relink func() error
	// workspaces returns the number of workspaces of this session.
	workspaces func(ctx context.Context, environ []string) (int, error)
	// lockSetup runs fn with the setup lock of this session (brained.SetupLocked).
	lockSetup func(environ []string, fn func() error) error
	// pending records, or with false removes, the setup popup for brained (brained.SetupPending).
	pending func(environ []string, pending bool) error
	// openSetup opens the setup popup in this session (brained.OpenSetup).
	openSetup func(ctx context.Context, environ []string) error
}

func osStartDeps(environ []string) startDeps {
	spawn := func(environ []string) error {
		bin, err := os.Executable()
		if err != nil {
			return err
		}
		return spawnBrained(bin, environ)
	}
	relink := func() error {
		link, err := paths.LinkPath()
		if err != nil {
			return err
		}
		bin, err := paths.BinaryPath()
		if err != nil {
			return err
		}
		return relinkStale(link, bin)
	}
	return startDeps{environ: environ, running: brained.Running, spawn: spawn, relink: relink, workspaces: workspaces,
		lockSetup: brained.SetupLocked, pending: brained.SetupPending, openSetup: brained.OpenSetup}
}

// start is `brain start`, the plugin `start` action (E19-S3). The build of `herdr plugin
// install` invokes it, because herdr runs [[startup]] only after a session restore. An action
// gets the same environment as [[startup]], so brained finds its session there. start starts
// brained when none holds the brained lock, points a stale ~/.local/bin/brain link of setup to
// this binary, then opens the setup popup when the session has no binding. Two starts at once
// can both spawn brained; the brained lock stops the second one.
func start(ctx context.Context, d startDeps) error {
	running, err := d.running(d.environ)
	if err != nil {
		return fmt.Errorf("brained lock: %w", err)
	}
	if !running {
		if err := d.spawn(d.environ); err != nil {
			return fmt.Errorf("start brained: %w", err)
		}
	}
	// Each install runs start from its new checkout, so the link follows the install here. A
	// failed relink does not stop the popup.
	var linkErr error
	if err := d.relink(); err != nil {
		linkErr = fmt.Errorf("relink: %w", err)
	}
	return errors.Join(linkErr, d.setup(ctx))
}

// setup opens the setup popup when the session has no binding, with the setup lock held. herdr
// opens a popup only in a workspace, so in a session with none it leaves the popup to brained,
// which opens it once a pane exists (#462). When it opens the popup itself, it first removes
// such a record, so brained does not open a second one. A bound session drops the record. The
// binding is read again after the workspace call: setup can bind the session during it.
func (d startDeps) setup(ctx context.Context) error {
	return d.lockSetup(d.environ, func() error {
		ok, err := bound(d.environ)
		if err != nil {
			return err
		}
		if ok {
			return d.pending(d.environ, false)
		}
		n, err := d.workspaces(ctx, d.environ)
		if err != nil {
			return err
		}
		if n == 0 {
			return d.pending(d.environ, true)
		}
		if err := d.pending(d.environ, false); err != nil {
			return err
		}
		if ok, err := bound(d.environ); err != nil || ok {
			return err
		}
		return d.openSetup(ctx, d.environ)
	})
}

// relinkStale points the link at link to the brain binary bin when it is a stale link of setup
// (install.LinkStale), and changes nothing in every other case.
func relinkStale(link, bin string) error {
	l, err := install.CheckLink(link, bin)
	if err != nil || l.State != install.LinkStale {
		return err
	}
	return install.Relink(link, bin)
}

// workspaces returns the number of workspaces of the herdr session of environ.
func workspaces(ctx context.Context, environ []string) (int, error) {
	c, err := herdr.New(environ, herdr.Target{Session: cmp.Or(lookupEnv(environ, "HERDR_SESSION"), "default"),
		Socket: lookupEnv(environ, "HERDR_SOCKET_PATH")})
	if err != nil {
		return 0, err
	}
	ctx, cancel := context.WithTimeout(ctx, startHerdrDeadline)
	defer cancel()
	ws, err := c.Workspaces(ctx)
	return len(ws), err
}

// bound reports whether this herdr session has a binding, as brained finds it: by socket,
// then by session name.
func bound(environ []string) (bool, error) {
	file, err := paths.BindingsFile()
	if err != nil {
		return false, err
	}
	bs, _, err := bindings.Read(file)
	if err != nil {
		return false, err
	}
	b, ok, err := bindings.BySocket(bs, lookupEnv(environ, "HERDR_SOCKET_PATH"))
	if err != nil {
		return false, err
	}
	// The default session has no HERDR_SESSION.
	return ok && b.Session == cmp.Or(lookupEnv(environ, "HERDR_SESSION"), "default"), nil
}

// spawnBrained starts `bin brained` in its own session, with stdin, stdout and stderr on
// /dev/null, and does not wait for it. herdr reads an action's output until EOF, so a brained
// that kept those pipes would keep the action running.
func spawnBrained(bin string, environ []string) error {
	null, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer null.Close() //nolint:errcheck // the child has its own copy; nothing is written here
	cmd := exec.Command(bin, "brained")
	cmd.Env = environ
	cmd.Stdin, cmd.Stdout, cmd.Stderr = null, null, null
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}
