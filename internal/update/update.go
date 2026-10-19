// Package update is `brain update` (E19-S4, HLD §4.1). herdr has no plugin update: a new
// `herdr plugin install` replaces a managed checkout, and a linked checkout is the user's own
// git clone. update finds the install kind and the repo in `herdr plugin list`, compares the
// installed version with the newest version of the repo (`git ls-remote`), and then reinstalls
// or pulls and builds, and runs the hook record step and `brain doctor --fix` of the new
// binary. A managed install at a release tag follows the release tags (E19-S6); any other
// install follows the default branch. It is the only Brain command that uses the network
// (§0 point 13).
package update

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"time"
)

// Usage is the usage text of brain update.
const Usage = `usage: brain update [--check | --records]
  brain update            install the newest version of the Brain repo, add the missing hook
                          record entries of its profiles (one question), then run brain doctor --fix;
                          an install with no --ref and a linked checkout follow the default branch,
                          an install at a release tag (--ref v<x.y.z>) follows the release tags, and
                          an install at any other --ref, such as main or a commit, is pinned
  brain update --check    compare the installed version with the newest version; change nothing
  brain update --records  add the missing hook record entries (one question), with no network call`

// pluginID is the id in herdr-plugin.toml.
const pluginID = "brain"

// herdrDeadline is the deadline of one herdr call that only asks or invokes: `herdr plugin
// list`, and the herdr calls of post-install. [NEW] 5 s, as the herdr deadline of the other
// commands. The install, git and the build get none: a build can take minutes, and the user
// can stop them.
const herdrDeadline = 5 * time.Second

// callWaitDelay is the part of the time of a herdr call that is kept for the kill of herdr and
// the wait for its output pipes. [NEW] 500 ms: the kill of the process group closes them at
// once in the usual case.
const callWaitDelay = 500 * time.Millisecond

// releaseTag is the form of a release tag, v<major>.<minor>.<patch> (scripts/check-tag.sh).
var releaseTag = regexp.MustCompile(`^v([0-9]+)\.([0-9]+)\.([0-9]+)$`)

// plugin is the Brain entry of `herdr plugin list --json` (herdr v0.9.1 and v0.9.5
// src/api/schema/plugins.rs, InstalledPluginInfo and PluginSourceInfo).
type plugin struct {
	ID      string `json:"plugin_id"`
	Version string `json:"version"`
	Root    string `json:"plugin_root"`
	Source  source `json:"source"`
}

// source is the registration of a plugin in herdr: PluginSourceInfo.
type source struct {
	Kind   string `json:"kind"` // "github" or "local"
	Owner  string `json:"owner"`
	Repo   string `json:"repo"`
	Subdir string `json:"subdir"`
	Ref    string `json:"requested_ref"`
	Commit string `json:"resolved_commit"`
	// Installed is the time of the install in Unix milliseconds, 0 when herdr gives none.
	Installed uint64 `json:"installed_unix_ms"`
}

// Records is the hook record step of brain update (#315, §8.5): it shows the hook record
// entries of the profiles of this binary that are missing, asks once on in, and adds them on
// yes, as brain setup does. Any other answer writes nothing.
type Records func(in io.Reader, stdout io.Writer) error

// updater runs the commands of one brain update.
type updater struct {
	environ        []string
	herdr          string
	stdin          io.Reader
	stdout, stderr io.Writer
}

// state is what an install kind compares and how it updates.
type state struct {
	commit            string // the installed commit, for the output
	installed, newest string // commit ids, or the release tags of a release install
	from, at          string // for the output: the install, and the newest version
	// pinned stops an install at a ref that is not a release tag: update follows no newer
	// version for it. "" for none.
	pinned string
	// guard stops an update before the compare: a linked checkout off main or with local
	// changes stops with its reason also when it is at the newest commit. nil for none.
	guard func(ctx context.Context) error
	apply func(ctx context.Context) error
}

// Main runs `brain update` with args in the process environment environ. It writes the
// output of each step to stdout and stderr, and reads the answer of the hook record step,
// records, from stdin. Up to date, it runs records; after an install, the new binary runs its
// own step (owner decision 2026-10-08, #315).
func Main(ctx context.Context, args, environ []string, stdin io.Reader, stdout, stderr io.Writer, records Records) error {
	check := len(args) == 1 && args[0] == "--check"
	if len(args) == 1 && args[0] == "--records" {
		return records(stdin, stdout)
	}
	if len(args) != 0 && !check {
		return fmt.Errorf("unexpected argument %q\n%s", args[0], Usage)
	}
	u := &updater{
		// No git question: a prompt for a password would wait forever with no terminal.
		environ: append(slices.Clone(environ), "GIT_TERMINAL_PROMPT=0"),
		herdr:   cmp.Or(lookup(environ, "HERDR_BIN_PATH"), "herdr"),
		stdin:   stdin, stdout: stdout, stderr: stderr,
	}
	p, err := u.plugin(ctx)
	if err != nil {
		return err
	}
	s, err := u.state(ctx, p)
	if err != nil {
		return err
	}
	if !check && s.guard != nil {
		if err := s.guard(ctx); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintf(stdout, "installed: brain %s, commit %s (%s)\n", p.Version, short(s.commit), s.from); err != nil {
		return err
	}
	if s.pinned != "" {
		if !check {
			return errors.New(s.pinned)
		}
		_, err := fmt.Fprintln(stdout, s.pinned)
		return err
	}
	verdict := "update available"
	if s.installed == s.newest {
		verdict = "up to date"
	}
	if _, err := fmt.Fprintf(stdout, "newest:    %s\n%s\n", s.at, verdict); err != nil {
		return err
	}
	switch {
	case check:
		return nil
	case s.installed == s.newest:
		return records(stdin, stdout)
	}
	if err := s.apply(ctx); err != nil {
		return err
	}
	return u.after(ctx, p.Root)
}

// plugin returns the Brain entry of `herdr plugin list`. herdr answers from its registry file
// when no server runs, so update works outside herdr too.
func (u *updater) plugin(ctx context.Context) (plugin, error) {
	p, ok, err := u.listed(ctx)
	if err != nil {
		return plugin{}, err
	}
	if !ok {
		return plugin{}, fmt.Errorf("herdr lists no %s plugin, so there is nothing to update", pluginID)
	}
	if !filepath.IsAbs(p.Root) {
		return plugin{}, fmt.Errorf("herdr plugin list: plugin root %q is not an absolute path", p.Root)
	}
	return p, nil
}

// listed returns the Brain entry of `herdr plugin list`; ok is false when herdr lists none.
func (u *updater) listed(ctx context.Context) (p plugin, ok bool, err error) {
	out, err := u.call(ctx, "plugin", "list", "--json", "--plugin", pluginID)
	if err != nil {
		return plugin{}, false, err
	}
	var r struct {
		Result struct {
			Type    string   `json:"type"`
			Plugins []plugin `json:"plugins"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		return plugin{}, false, fmt.Errorf("herdr plugin list: %w", err)
	}
	if r.Result.Type != "plugin_list" {
		return plugin{}, false, fmt.Errorf("herdr plugin list: reply type %q, want plugin_list", r.Result.Type)
	}
	i := slices.IndexFunc(r.Result.Plugins, func(p plugin) bool { return p.ID == pluginID })
	if i < 0 {
		return plugin{}, false, nil
	}
	return r.Result.Plugins[i], true, nil
}

// state is the state of the install kind of p.
func (u *updater) state(ctx context.Context, p plugin) (state, error) {
	switch p.Source.Kind {
	case "github":
		return u.managed(ctx, p)
	case "local":
		return u.linked(ctx, p)
	}
	return state{}, fmt.Errorf("herdr plugin list: unknown source kind %q", p.Source.Kind)
}

// managed is a GitHub install of herdr. herdr fetches https://github.com/<owner>/<repo>.git
// (src/cli/plugin.rs, remote_url), so update asks the same URL. An install with no ref follows
// the default branch, and an install at a release tag follows the release tags. An install at
// any other ref, such as a branch or a commit, is pinned: update asks no remote for it.
func (u *updater) managed(ctx context.Context, p plugin) (state, error) {
	src := p.Source
	if src.Owner == "" || src.Repo == "" || src.Commit == "" {
		return state{}, fmt.Errorf("herdr plugin list: the GitHub source of %s has no owner, repo or commit", pluginID)
	}
	repo := src.Owner + "/" + src.Repo
	if src.Subdir != "" {
		repo += "/" + src.Subdir
	}
	remote := "https://github.com/" + src.Owner + "/" + src.Repo + ".git"
	s := state{commit: src.Commit, from: "github:" + repo}
	if src.Ref != "" {
		s.from += " --ref " + src.Ref
	}
	switch _, release := version(src.Ref); {
	case src.Ref == "":
		newest, err := u.head(ctx, "", remote)
		if err != nil {
			return state{}, err
		}
		s.installed, s.newest = src.Commit, newest
		s.at = fmt.Sprintf("commit %s on the default branch of %s", short(newest), repo)
		s.apply = func(ctx context.Context) error { return u.install(ctx, src.Commit, repo) }
	case release:
		tag, err := u.newestTag(ctx, remote, src.Ref)
		if err != nil {
			return state{}, err
		}
		s.installed, s.newest = src.Ref, tag
		s.at = fmt.Sprintf("release tag %s of %s", tag, repo)
		s.apply = func(ctx context.Context) error { return u.install(ctx, src.Commit, repo, "--ref", tag) }
	default:
		s.pinned = fmt.Sprintf("this install is pinned to --ref %s, which is not a release tag, so brain update "+
			"changes nothing; to leave the ref, run: herdr plugin install %s --yes", src.Ref, repo)
	}
	return s, nil
}

// install runs `herdr plugin install` with args and --yes. commit is the installed commit, for
// the error.
func (u *updater) install(ctx context.Context, commit string, args ...string) error {
	args = append(append([]string{"plugin", "install"}, args...), "--yes")
	if err := u.run(ctx, "", nil, u.herdr, args...); err != nil {
		return fmt.Errorf("herdr plugin install (herdr keeps the old checkout, so Brain stays at commit %s): %w",
			short(commit), err)
	}
	return nil
}

// linked is a checkout that `herdr plugin link` added. update pulls and builds it only on main
// with no local changes; else it stops and changes nothing.
func (u *updater) linked(ctx context.Context, p plugin) (state, error) {
	root := p.Root
	installed, err := u.git(ctx, root, "rev-parse", "HEAD")
	if err != nil {
		return state{}, err
	}
	remote, err := u.git(ctx, root, "remote", "get-url", "origin")
	if err != nil {
		return state{}, err
	}
	newest, err := u.head(ctx, root, "origin")
	if err != nil {
		return state{}, err
	}
	guard := func(ctx context.Context) error {
		branch, err := u.git(ctx, root, "branch", "--show-current")
		if err != nil {
			return err
		}
		if branch != "main" {
			return fmt.Errorf("the linked checkout %s is on %s, not main; brain update changes nothing", root,
				cmp.Or(branch, "a detached HEAD"))
		}
		changes, err := u.git(ctx, root, "status", "--porcelain")
		if err != nil {
			return err
		}
		if changes != "" {
			return fmt.Errorf("the linked checkout %s has local changes; brain update changes nothing: "+
				"commit or remove them, then run brain update again", root)
		}
		return nil
	}
	apply := func(ctx context.Context) error {
		if err := u.run(ctx, root, nil, "git", "pull", "--ff-only"); err != nil {
			return err
		}
		return u.run(ctx, root, nil, "sh", filepath.Join(root, "scripts", "build.sh"))
	}
	return state{commit: installed, installed: installed, newest: newest, from: "linked: " + root,
		at: fmt.Sprintf("commit %s on the default branch of %s", short(newest), remote), guard: guard, apply: apply}, nil
}

// head is the commit of the default branch (HEAD) of remote.
func (u *updater) head(ctx context.Context, dir, remote string) (string, error) {
	out, err := u.git(ctx, dir, "ls-remote", remote, "HEAD")
	if err != nil {
		return "", err
	}
	commit, _, _ := strings.Cut(out, "\t")
	if commit == "" {
		return "", fmt.Errorf("git ls-remote %s: no HEAD", remote)
	}
	return commit, nil
}

// newestTag is the highest release tag of remote, or installed when remote has no higher one.
// It ignores the tags that are not release tags.
func (u *updater) newestTag(ctx context.Context, remote, installed string) (string, error) {
	out, err := u.git(ctx, "", "ls-remote", "--tags", "--refs", remote)
	if err != nil {
		return "", err
	}
	newest := installed
	high, _ := version(installed) // a release tag: managed checks it
	for _, line := range strings.Split(out, "\n") {
		_, ref, _ := strings.Cut(line, "\t")
		tag, ok := strings.CutPrefix(ref, "refs/tags/")
		if !ok {
			continue
		}
		if v, ok := version(tag); ok && slices.CompareFunc(v[:], high[:], compareNumber) > 0 {
			newest, high = tag, v
		}
	}
	return newest, nil
}

// version returns the numbers of a release tag as decimal text with no leading zeros. ok is
// false for any other ref. The numbers stay text, so a number of any size counts.
func version(ref string) (v [3]string, ok bool) {
	m := releaseTag.FindStringSubmatch(ref)
	if m == nil {
		return v, false
	}
	for i := range v {
		v[i] = strings.TrimLeft(m[i+1], "0")
	}
	return v, true
}

// compareNumber compares two numbers of version as numbers and not as text, so 10 is more
// than 9. A longer number is more; numbers of the same length compare as text.
func compareNumber(a, b string) int {
	return cmp.Or(cmp.Compare(len(a), len(b)), strings.Compare(a, b))
}

// after runs the new binary of the plugin root: its hook record step `brain update --records`,
// which gets the input of update for its one question, then `brain doctor --fix`. The new
// binary has the new profiles, so only it knows their hook record entries (#315), and a new
// binary can need new hook entries.
func (u *updater) after(ctx context.Context, root string) error {
	bin := filepath.Join(root, "bin", "brain")
	if err := u.run(ctx, "", u.stdin, bin, "update", "--records"); err != nil {
		return fmt.Errorf("the update is done, but its hook record step reports a problem (see above): %w", err)
	}
	if err := u.run(ctx, "", nil, bin, "doctor", "--fix"); err != nil {
		return fmt.Errorf("the update is done, but brain doctor --fix reports a problem (see above): %w", err)
	}
	return nil
}

// run runs name with args in dir and input stdin (nil for none), with its output on the output
// of update.
func (u *updater) run(ctx context.Context, dir string, stdin io.Reader, name string, args ...string) error {
	if _, err := fmt.Fprintf(u.stdout, "$ %s %s\n", name, strings.Join(args, " ")); err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir, cmd.Env, cmd.Stdin = dir, u.environ, stdin
	cmd.Stdout, cmd.Stderr = u.stdout, u.stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s %s: %w", filepath.Base(name), args[0], err)
	}
	return nil
}

// call runs herdr with args and returns its stdout. The call ends by herdrDeadline from now, or
// by the deadline of ctx when that is earlier: herdr gets that time less callWaitDelay, and the
// kill and the wait for the output pipes use the rest. herdr runs in its own process group. The
// deadline kills the group, and after each call a second kill of the group stops the children
// that herdr left, also after a good answer: a child that holds the output pipes would else keep
// the call open. A process that leaves the group on purpose (setsid) is out of scope; WaitDelay
// only closes the pipes that it holds, so the call still ends in time.
func (u *updater) call(ctx context.Context, args ...string) (string, error) {
	end := time.Now().Add(herdrDeadline)
	if d, ok := ctx.Deadline(); ok && d.Before(end) {
		end = d
	}
	ctx, cancel := context.WithDeadline(ctx, end.Add(-callWaitDelay))
	defer cancel()
	cmd := exec.CommandContext(ctx, u.herdr, args...)
	cmd.Env = u.environ
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = callWaitDelay
	out, err := capture(cmd)
	if cmd.Process != nil {
		// While a child is left in the group, the group id stays in use, so this kill reaches
		// only that group. With no child left, it finds no group (ESRCH): the error does not count.
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	return out, err
}

// git runs git with args in dir and returns its stdout with no space at the ends.
func (u *updater) git(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir, cmd.Env = dir, u.environ
	return capture(cmd)
}

// capture runs cmd and returns its stdout with no space at the ends. The error has the
// command line and the stderr of cmd.
func capture(cmd *exec.Cmd) (string, error) {
	name, args := filepath.Base(cmd.Args[0]), cmd.Args[1:]
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("%s %s: %s: %w", name, strings.Join(args, " "), bytes.TrimSpace(stderr.Bytes()), err)
	}
	return string(bytes.TrimSpace(out)), nil
}

// short is the 12-character form of a commit id.
func short(commit string) string {
	return commit[:min(len(commit), 12)]
}

// lookup returns the value of key in environ; the last entry wins, as in os/exec.
func lookup(environ []string, key string) string {
	var v string
	for _, kv := range environ {
		if k, val, ok := strings.Cut(kv, "="); ok && k == key {
			v = val
		}
	}
	return v
}
