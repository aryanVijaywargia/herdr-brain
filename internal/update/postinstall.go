package update

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"time"
)

// postInstallUsage is the usage of the hidden command post-install.
const postInstallUsage = "usage: brain post-install --commit <sha> [--nonce <hex> --deadline <unix ms> [--old <json>]]"

// nonceFile is the file in the bin folder of a build that holds the nonce of that build. /bin/
// is in .gitignore, so the file does not change the clean check of build.sh.
const nonceFile = ".install-nonce"

// postInstallWait is the time from the end of the build to the deadline of post-install and
// its child. [NEW] 60 s: after the build, an install only moves or registers the checkout.
// BRAIN_AUTO_SETUP_WAIT (seconds) is for tests only.
const postInstallWait = 60 * time.Second

// PostInstall is the hidden command `brain post-install --commit <sha>` (#427). scripts/build.sh
// runs it in the checkout at the end of a good build: herdr runs [[startup]] only after a
// session restore, so after an install nothing would start brained or setup (E19-S3). It uses
// only the output of the herdr CLI and no herdr folder layout, so it works with each herdr
// version that keeps that output.
//
// The command writes a random nonce to bin/.install-nonce and starts itself again, detached,
// with that nonce and one deadline, postInstallWait from now. The child polls `herdr plugin
// list` until herdr lists a GitHub install of this commit whose plugin root holds the same
// nonce: the install of this build. herdr 0.9.1 moves the checkout with the file, and herdr
// 0.9.5 builds in place. When herdr already listed this checkout as the plugin root before
// the child started, the child also needs a change in the registration of the entry (--old):
// a build by hand in the plugin root changes none, and an install in place changes at least
// the install time or the commit. Then the child invokes brain.start in each running herdr
// session, because the build cannot know the session of the install. Each herdr call ends by
// the deadline, also when its own 5 s are not over.
func PostInstall(ctx context.Context, args, environ []string) error {
	fs := flag.NewFlagSet("post-install", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	commit := fs.String("commit", "", "")
	nonce := fs.String("nonce", "", "")
	deadline := fs.String("deadline", "", "")
	old := fs.String("old", "", "")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("%w\n%s", err, postInstallUsage)
	}
	if *commit == "" || fs.NArg() != 0 || (*nonce == "") != (*deadline == "") || (*old != "" && *nonce == "") {
		return errors.New(postInstallUsage)
	}
	u := &updater{environ: environ, herdr: cmp.Or(lookup(environ, "HERDR_BIN_PATH"), "herdr")}
	if *deadline == "" {
		return u.watch(ctx, *commit)
	}
	return u.child(ctx, *commit, *nonce, *deadline, *old)
}

// watch is the part of post-install in the build: it writes the nonce and starts the child
// that waits for the install of this build.
func (u *updater) watch(ctx context.Context, commit string) error {
	wait := postInstallWait
	if s := lookup(u.environ, "BRAIN_AUTO_SETUP_WAIT"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil {
			return fmt.Errorf("BRAIN_AUTO_SETUP_WAIT: %w", err)
		}
		wait = time.Duration(n) * time.Second
	}
	deadline := time.Now().Add(wait)
	ctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	root, err := os.Getwd()
	if err != nil {
		return err
	}
	p, ok, err := u.listed(ctx)
	if err != nil {
		return err
	}
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return fmt.Errorf("nonce: %w", err)
	}
	nonce := hex.EncodeToString(b)
	if err := os.WriteFile(filepath.Join(root, "bin", nonceFile), []byte(nonce), 0o600); err != nil {
		return err
	}
	args := []string{"post-install", "--commit", commit, "--nonce", nonce, "--deadline", strconv.FormatInt(deadline.UnixMilli(), 10)}
	if ok && sameDir(p.Root, root) {
		old, err := json.Marshal(p.Source)
		if err != nil {
			return err
		}
		args = append(args, "--old", string(old))
	}
	return detach(args...)
}

// child is the detached part of post-install: it waits for the install of this build until
// deadline (Unix milliseconds), then invokes brain.start. old is the JSON of the registration
// that herdr listed for this checkout before the child started, "" for none.
func (u *updater) child(ctx context.Context, commit, nonce, deadline, old string) error {
	ms, err := strconv.ParseInt(deadline, 10, 64)
	if err != nil {
		return fmt.Errorf("--deadline: %w", err)
	}
	var before *source
	if old != "" {
		before = new(source)
		if err := json.Unmarshal([]byte(old), before); err != nil {
			return fmt.Errorf("--old: %w", err)
		}
	}
	ctx, cancel := context.WithDeadline(ctx, time.UnixMilli(ms))
	defer cancel()
	if !u.awaitInstall(ctx, commit, nonce, before) {
		return nil
	}
	return u.startSessions(ctx)
}

// sameDir reports whether the paths a and b name the same folder. A path that does not exist
// names no folder.
func sameDir(a, b string) bool {
	fa, err := os.Stat(a)
	if err != nil {
		return false
	}
	fb, err := os.Stat(b)
	return err == nil && os.SameFile(fa, fb)
}

// ours reports whether p is the install of this build: a GitHub install of commit whose plugin
// root holds nonce in bin/.install-nonce. A root with no such file, or with the nonce of
// another build, is another install, so a read error means no. With old, the registration that
// herdr listed for this checkout before, p must also differ from old in a field: the install
// time (by inequality only, never by order), the ref or the commit, for example.
func ours(p plugin, commit, nonce string, old *source) bool {
	if p.Source.Kind != "github" || p.Source.Commit != commit || !filepath.IsAbs(p.Root) {
		return false
	}
	if old != nil && p.Source == *old {
		return false
	}
	b, err := os.ReadFile(filepath.Join(p.Root, "bin", nonceFile))
	return err == nil && string(b) == nonce
}

// awaitInstall polls `herdr plugin list` each second until herdr lists the install of this
// build, and reports whether it did before the deadline of ctx.
func (u *updater) awaitInstall(ctx context.Context, commit, nonce string, old *source) bool {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		// An error is no answer yet: the next poll asks again, until the deadline.
		if p, ok, err := u.listed(ctx); err == nil && ok && ours(p, commit, nonce, old) {
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
// A failed invoke does not stop the invokes of the other sessions. After the deadline of ctx,
// each call fails at once.
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
