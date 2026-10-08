// Package update is `brain update` (E19-S4, HLD §4.1). herdr has no plugin update: a new
// `herdr plugin install` replaces a managed checkout, and a linked checkout is the user's own
// git clone. update finds the install kind and the repo in `herdr plugin list`, compares the
// installed commit with the newest commit of the repo's default branch (`git ls-remote`), and
// then reinstalls or pulls and builds, and runs the hook record step and `brain doctor --fix`
// of the new binary. It is the only Brain command that uses the network (§0 point 13).
package update

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// Usage is the usage text of brain update.
const Usage = `usage: brain update [--check | --records]
  brain update            install the newest commit of the Brain repo, add the missing hook
                          record entries of its profiles (one question), then run brain doctor --fix
  brain update --check    compare the installed commit with the newest commit; change nothing
  brain update --records  add the missing hook record entries (one question), with no network call`

// pluginID is the id in herdr-plugin.toml.
const pluginID = "brain"

// listDeadline is the deadline of `herdr plugin list`. [NEW] 5 s, as the herdr deadline of
// the other commands. The install, git and the build get none: a build can take minutes, and
// the user can stop them.
const listDeadline = 5 * time.Second

// plugin is the Brain entry of `herdr plugin list --json` (herdr v0.9.1
// src/api/schema/plugins.rs, InstalledPluginInfo and PluginSourceInfo).
type plugin struct {
	ID      string `json:"plugin_id"`
	Version string `json:"version"`
	Root    string `json:"plugin_root"`
	Source  struct {
		Kind   string `json:"kind"` // "github" or "local"
		Owner  string `json:"owner"`
		Repo   string `json:"repo"`
		Subdir string `json:"subdir"`
		Ref    string `json:"requested_ref"`
		Commit string `json:"resolved_commit"`
	} `json:"source"`
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
	installed, newest string // commit ids
	from, remote      string // for the output
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
	var s state
	switch p.Source.Kind {
	case "github":
		s, err = u.managed(ctx, p)
	case "local":
		s, err = u.linked(ctx, p)
	default:
		err = fmt.Errorf("herdr plugin list: unknown source kind %q", p.Source.Kind)
	}
	if err != nil {
		return err
	}
	if !check && s.guard != nil {
		if err := s.guard(ctx); err != nil {
			return err
		}
	}
	verdict := "update available"
	if s.installed == s.newest {
		verdict = "up to date"
	}
	if _, err := fmt.Fprintf(stdout, "installed: brain %s, commit %s (%s)\nnewest:    commit %s on the default branch of %s\n%s\n",
		p.Version, short(s.installed), s.from, short(s.newest), s.remote, verdict); err != nil {
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
	ctx, cancel := context.WithTimeout(ctx, listDeadline)
	defer cancel()
	out, err := u.output(ctx, "", u.herdr, "plugin", "list", "--json", "--plugin", pluginID)
	if err != nil {
		return plugin{}, err
	}
	var r struct {
		Result struct {
			Type    string   `json:"type"`
			Plugins []plugin `json:"plugins"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		return plugin{}, fmt.Errorf("herdr plugin list: %w", err)
	}
	if r.Result.Type != "plugin_list" {
		return plugin{}, fmt.Errorf("herdr plugin list: reply type %q, want plugin_list", r.Result.Type)
	}
	i := slices.IndexFunc(r.Result.Plugins, func(p plugin) bool { return p.ID == pluginID })
	if i < 0 {
		return plugin{}, fmt.Errorf("herdr lists no %s plugin, so there is nothing to update", pluginID)
	}
	p := r.Result.Plugins[i]
	if !filepath.IsAbs(p.Root) {
		return plugin{}, fmt.Errorf("herdr plugin list: plugin root %q is not an absolute path", p.Root)
	}
	return p, nil
}

// managed is a GitHub install of herdr. herdr fetches https://github.com/<owner>/<repo>.git
// (src/cli/plugin.rs, remote_url), so update asks the same URL.
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
	newest, err := u.head(ctx, "", remote)
	if err != nil {
		return state{}, err
	}
	from := "github:" + repo
	if src.Ref != "" {
		from += " --ref " + src.Ref
	}
	apply := func(ctx context.Context) error {
		if src.Ref != "" {
			return fmt.Errorf("brain update follows the default branch, but this install is at --ref %s; "+
				"to leave the ref, run: herdr plugin install %s --yes", src.Ref, repo)
		}
		if err := u.run(ctx, "", nil, u.herdr, "plugin", "install", repo, "--yes"); err != nil {
			return fmt.Errorf("herdr plugin install (herdr keeps the old checkout, so Brain stays at commit %s): %w",
				short(src.Commit), err)
		}
		return nil
	}
	return state{installed: src.Commit, newest: newest, from: from, remote: repo, apply: apply}, nil
}

// linked is a checkout that `herdr plugin link` added. update pulls and builds it only on main
// with no local changes; else it stops and changes nothing.
func (u *updater) linked(ctx context.Context, p plugin) (state, error) {
	root := p.Root
	installed, err := u.output(ctx, root, "git", "rev-parse", "HEAD")
	if err != nil {
		return state{}, err
	}
	remote, err := u.output(ctx, root, "git", "remote", "get-url", "origin")
	if err != nil {
		return state{}, err
	}
	newest, err := u.head(ctx, root, "origin")
	if err != nil {
		return state{}, err
	}
	guard := func(ctx context.Context) error {
		branch, err := u.output(ctx, root, "git", "branch", "--show-current")
		if err != nil {
			return err
		}
		if branch != "main" {
			return fmt.Errorf("the linked checkout %s is on %s, not main; brain update changes nothing", root,
				cmp.Or(branch, "a detached HEAD"))
		}
		changes, err := u.output(ctx, root, "git", "status", "--porcelain")
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
	return state{installed: installed, newest: newest, from: "linked: " + root, remote: remote, guard: guard, apply: apply}, nil
}

// head is the commit of the default branch (HEAD) of remote.
func (u *updater) head(ctx context.Context, dir, remote string) (string, error) {
	out, err := u.output(ctx, dir, "git", "ls-remote", remote, "HEAD")
	if err != nil {
		return "", err
	}
	commit, _, _ := strings.Cut(out, "\t")
	if commit == "" {
		return "", fmt.Errorf("git ls-remote %s: no HEAD", remote)
	}
	return commit, nil
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

// output runs name with args in dir and returns its stdout with no space at the ends.
func (u *updater) output(ctx context.Context, dir, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir, cmd.Env = dir, u.environ
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("%s %s: %s: %w", filepath.Base(name), strings.Join(args, " "), bytes.TrimSpace(stderr.Bytes()), err)
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
