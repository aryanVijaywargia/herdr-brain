package signal

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// maxLog is the number of commits that the revert rule reads [NEW].
const maxLog = 1000

// reverts matches the line that git revert writes in the message of a revert commit.
var reverts = regexp.MustCompile(`This reverts commit ([0-9a-f]{7,64})`)

// RevertCommit is one revert in the git log: a revert commit, or a commit that puts a file back
// to an earlier content.
type RevertCommit struct {
	Commit string
	// Reverted holds the commit that the revert undoes, when the message names it or the log
	// holds the commit that made the earlier content; else it is empty.
	Reverted []string
	At       time.Time // the commit time
	Subject  string
	Paths    []string // the files that the commit changes, in log order
}

// Reverts finds the reverts in the git log of the workspace root root: a revert commit, or a
// commit that puts a file back to an earlier content. The log does not say which commits an
// agent made, so the rule is wide: every commit counts. It runs only read-only git commands:
// rev-parse and log. A root outside git, or a repo with no commit, has no reverts.
func Reverts(ctx context.Context, root string) ([]RevertCommit, error) {
	if _, err := git(ctx, root, "rev-parse", "-q", "--verify", "HEAD"); err != nil {
		if errors.As(err, new(*exec.ExitError)) {
			return nil, nil // not a repo (exit 128), or no commit yet (exit 1)
		}
		return nil, err
	}
	// --reverse applies after --max-count: the newest commits, oldest first.
	out, err := git(ctx, root, "log", "--reverse", fmt.Sprintf("--max-count=%d", maxLog), "--raw", "--no-abbrev",
		"--no-renames", "--format=%x1e%H%x00%ct%x00%s%x00%b%x00")
	if err != nil {
		return nil, err
	}
	var rs []RevertCommit
	blobs := map[string]map[string]string{} // path → content → the commit that made it; "" before the log
	for _, rec := range strings.Split(string(out), "\x1e")[1:] {
		f := strings.SplitN(rec, "\x00", 5)
		if len(f) < 5 {
			return nil, fmt.Errorf("read git log: bad record %q", rec)
		}
		secs, err := strconv.ParseInt(f[1], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("read git log: commit time %q: %w", f[1], err)
		}
		r := RevertCommit{Commit: f[0], At: time.Unix(secs, 0).UTC(), Subject: f[2], Paths: paths(f[4])}
		back := goesBack(f[0], f[4], blobs)
		switch m := reverts.FindStringSubmatch(f[3]); {
		case m != nil:
			r.Reverted = []string{m[1]}
		case strings.HasPrefix(f[2], `Revert "`) || back != nil:
			r.Reverted = back
		default:
			continue
		}
		rs = append(rs, r)
	}
	return rs, nil
}

// paths returns the paths of the raw changes raw of one commit, in order.
func paths(raw string) []string {
	var ps []string
	for _, l := range strings.Split(raw, "\n") {
		if _, p, ok := strings.Cut(l, "\t"); ok && strings.HasPrefix(l, ":") {
			ps = append(ps, p)
		}
	}
	return ps
}

// Changes holds the tracked files under a workspace root whose content in the work tree is
// not their HEAD content: the HEAD blob of each, by its path in the repo; "" when HEAD has no
// such file.
type Changes map[string]string

// WorkChanges returns the changes under the workspace root root now, and the paths of before,
// the changes of an earlier call, whose content is back to their HEAD content of that call:
// `git checkout -- <file>`, `git restore` or `git stash` with no commit. A commit of the
// change is not a revert: the file then holds the changed content. Untracked files do not
// count. It runs only read-only git commands, as FindAnchors. A root that is "", or outside
// git, has no changes.
func WorkChanges(ctx context.Context, root string, before Changes) (now Changes, back []string, err error) {
	if root == "" {
		return nil, nil, nil
	}
	top, prefix, err := repo(ctx, root)
	if err != nil || top == "" {
		return nil, nil, err
	}
	tree, err := workTree(ctx, top)
	if err != nil {
		return nil, nil, err
	}
	head, err := headTree(ctx, top)
	if err != nil {
		return nil, nil, err
	}
	now = Changes{}
	for _, files := range []map[string]string{tree, head} {
		for p := range files {
			if strings.HasPrefix(p, prefix) && tree[p] != head[p] {
				now[p] = head[p]
			}
		}
	}
	for p, h := range before {
		if _, changed := now[p]; !changed && tree[p] == h {
			back = append(back, p)
		}
	}
	slices.Sort(back)
	return now, back, nil
}

// headTree returns the blob of each file of the HEAD commit of the repo at top, by its path in
// the repo. A repo with no commit has an empty tree.
func headTree(ctx context.Context, top string) (map[string]string, error) {
	tree := map[string]string{}
	if _, err := git(ctx, top, "rev-parse", "-q", "--verify", "HEAD"); err != nil {
		if errors.As(err, new(*exec.ExitError)) {
			return tree, nil // no commit yet (exit 1)
		}
		return nil, err
	}
	out, err := git(ctx, top, "ls-tree", "-r", "-z", "--full-tree", "HEAD")
	if err != nil {
		return nil, err
	}
	for _, rec := range strings.Split(string(out), "\x00") {
		info, p, ok := strings.Cut(rec, "\t")
		f := strings.Fields(info)
		if !ok || len(f) != 3 || f[0] == gitlink {
			continue // a submodule is a commit, not a file
		}
		tree[p] = f[2]
	}
	return tree, nil
}

// goesBack reads the raw changes of commit hash and records them in blobs. It returns a
// non-nil list when the commit puts a file back to a content that it had before: the commit
// that made that content, or no commit when it was older than the log. A deleted file does
// not count: a revert commit of an added file is found by its message.
func goesBack(hash, raw string, blobs map[string]map[string]string) []string {
	var back []string
	for _, l := range strings.Split(raw, "\n") {
		change, path, ok := strings.Cut(strings.TrimPrefix(l, ":"), "\t")
		f := strings.Fields(change)
		if !ok || len(f) < 4 {
			continue
		}
		before, after := f[2], f[3]
		if blobs[path] == nil {
			blobs[path] = map[string]string{}
		}
		if _, seen := blobs[path][before]; !seen {
			blobs[path][before] = ""
		}
		if by, seen := blobs[path][after]; seen && back == nil && after != before && strings.Trim(after, "0") != "" {
			back = []string{}
			if by != "" {
				back = append(back, by)
			}
		}
		blobs[path][after] = hash
	}
	return back
}

// git runs the read-only git command args in dir and returns its output.
func git(ctx context.Context, dir string, args ...string) ([]byte, error) {
	return gitIn(ctx, dir, "", args...)
}

// gitIn runs the read-only git command args in dir with stdin as its input, and returns its
// output.
func gitIn(ctx context.Context, dir, stdin string, args ...string) ([]byte, error) {
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir, "--no-pager"}, args...)...)
	cmd.Stdin = strings.NewReader(stdin)
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}
