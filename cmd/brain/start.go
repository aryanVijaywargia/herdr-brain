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
	"github.com/aryanVijaywargia/herdr-brain/internal/paths"
)

// startHerdrDeadline is the deadline of the herdr call that opens the setup popup. [NEW] 5 s,
// as setupHerdrDeadline.
const startHerdrDeadline = 5 * time.Second

// startDeps are the parts of start that reach outside the process; tests replace them.
type startDeps struct {
	environ []string
	// running reports whether a brained holds the D7 lock of this session (brained.Running).
	running func(environ []string) (bool, error)
	// spawn starts `brain brained` with environ and does not wait for it.
	spawn func(environ []string) error
	// openSetup opens the setup popup in this session.
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
	return startDeps{environ: environ, running: brained.Running, spawn: spawn, openSetup: openSetup}
}

// start is `brain start`, the plugin `start` action (E19-S3). The build of `herdr plugin
// install` invokes it, because herdr runs [[startup]] only after a session restore. An action
// gets the same environment as [[startup]], so brained finds its session there. start starts
// brained when none holds the D7 lock, then opens the setup popup when the session has no
// binding. Two starts at once can both spawn brained; the D7 lock stops the second one.
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
	ok, err := bound(d.environ)
	if err != nil || ok {
		return err
	}
	return d.openSetup(ctx, d.environ)
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
	// The default session has no HERDR_SESSION (M0).
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

// openSetup opens the setup popup as the `setup` action does.
func openSetup(ctx context.Context, environ []string) error {
	bin := lookupEnv(environ, "HERDR_BIN_PATH")
	if bin == "" {
		return errors.New("HERDR_BIN_PATH is not set")
	}
	ctx, cancel := context.WithTimeout(ctx, startHerdrDeadline)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "plugin", "pane", "open", "--plugin", "brain", "--entrypoint", "setup")
	cmd.Env = environ
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("herdr plugin pane open: %w: %s", err, out)
	}
	return nil
}
