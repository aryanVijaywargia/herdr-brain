package update

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"time"
)

// postInstallUsage is the usage of the hidden command post-install.
const postInstallUsage = "usage: brain post-install --commit <sha> [--before <ms>]"

// postInstallWait is how long the child of post-install waits for herdr to list the install.
// [NEW] 60 s: after the build, an install only moves or registers the checkout.
// BRAIN_AUTO_SETUP_WAIT (seconds) is for tests only.
const postInstallWait = 60 * time.Second

// PostInstall is the hidden command `brain post-install --commit <sha>` (#427). scripts/build.sh
// runs it at the end of a good build: herdr runs [[startup]] only after a session restore, so
// after an install nothing would start brained or setup (E19-S3). It uses only the output of
// the herdr CLI, and no herdr folder layout, so it works with each herdr version that keeps
// that output.
//
// herdr runs the build "after confirmation and before Herdr registers the plugin" (herdr
// plugin docs), and gives the build no HERDR_ variable. So the command first reads the Brain
// entry of `herdr plugin list` that is there before the install, and starts itself again with
// --before <the install time of that entry>, detached. That child polls `herdr plugin list`
// until herdr lists a GitHub install of this commit with a later install time, and then
// invokes brain.start in each running herdr session: the build cannot know the session of the
// install. A linked checkout (source kind local), or a plain build of a commit that herdr
// does not install, never fits, so the child stops after postInstallWait and invokes nothing.
func PostInstall(ctx context.Context, args, environ []string) error {
	fs := flag.NewFlagSet("post-install", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	commit := fs.String("commit", "", "")
	before := fs.String("before", "", "")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("%w\n%s", err, postInstallUsage)
	}
	if *commit == "" || fs.NArg() != 0 {
		return errors.New(postInstallUsage)
	}
	u := &updater{environ: environ, herdr: cmp.Or(lookup(environ, "HERDR_BIN_PATH"), "herdr")}
	if *before == "" {
		p, ok, err := u.listed(ctx)
		if err != nil {
			return err
		}
		var ms uint64 // no entry yet: any install time is later
		if ok {
			ms = p.Source.Installed
		}
		return detach("post-install", "--commit", *commit, "--before", strconv.FormatUint(ms, 10))
	}
	ms, err := strconv.ParseUint(*before, 10, 64)
	if err != nil {
		return fmt.Errorf("--before: %w", err)
	}
	wait := postInstallWait
	if s := lookup(environ, "BRAIN_AUTO_SETUP_WAIT"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil {
			return fmt.Errorf("BRAIN_AUTO_SETUP_WAIT: %w", err)
		}
		wait = time.Duration(n) * time.Second
	}
	if !u.awaitInstall(ctx, *commit, ms, wait) {
		return nil
	}
	return u.startSessions(ctx)
}

// fresh reports whether p is the install of this build: a GitHub install of commit whose
// install time is after before, the install time of the entry before the build. An entry with
// no install time never fits, so a herdr that does not give one starts nothing.
func fresh(p plugin, commit string, before uint64) bool {
	return p.Source.Kind == "github" && p.Source.Commit == commit && p.Source.Installed > before
}

// awaitInstall polls `herdr plugin list` each second until herdr lists the install of this
// build, and reports whether it did before wait ends.
func (u *updater) awaitInstall(ctx context.Context, commit string, before uint64, wait time.Duration) bool {
	ctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		// An error is no answer yet: the next poll asks again, until wait ends.
		if p, ok, err := u.listed(ctx); err == nil && ok && fresh(p, commit, before) {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-tick.C:
		}
	}
}

// startSessions invokes brain.start in each running herdr session of `herdr session list`.
// A failed invoke does not stop the invokes of the other sessions.
func (u *updater) startSessions(ctx context.Context) error {
	out, err := u.call(ctx, "session", "list", "--json")
	if err != nil {
		return err
	}
	var r struct {
		Sessions []struct {
			Name    string `json:"name"`
			Running bool   `json:"running"`
			Error   string `json:"connection_error"`
		} `json:"sessions"`
	}
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		return fmt.Errorf("herdr session list: %w", err)
	}
	var errs []error
	for _, s := range r.Sessions {
		if !s.Running || s.Error != "" {
			continue
		}
		if _, err := u.call(ctx, "--session", s.Name, "plugin", "action", "invoke", "brain.start"); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// detach starts this binary with args in its own session, in /, with stdin, stdout and stderr
// on /dev/null, and does not wait for it. herdr reads the build output until EOF, so a child
// that kept those pipes would keep the install running.
func detach(args ...string) error {
	bin, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(bin, args...)
	cmd.Dir = "/" // nil Stdin, Stdout and Stderr are /dev/null (os/exec)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}
