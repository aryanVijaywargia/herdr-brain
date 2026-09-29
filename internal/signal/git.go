package signal

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
)

// maxLog is the number of commits that the revert rule reads [NEW].
const maxLog = 1000

// reverts matches the line that git revert writes in the message of a revert commit.
var reverts = regexp.MustCompile(`This reverts commit ([0-9a-f]{7,64})`)

// Reverts finds the reverts in the git log of the workspace root root (§7.1): a revert commit,
// or a commit that puts a file back to an earlier content. The log does not say which commits
// an agent made, so the rule is wide: every commit counts. It runs only read-only git
// commands: rev-parse and log. A root outside git, or a repo with no commit, has no reverts.
func Reverts(ctx context.Context, root string) ([]Signal, error) {
	if _, err := git(ctx, root, "rev-parse", "-q", "--verify", "HEAD"); err != nil {
		if errors.As(err, new(*exec.ExitError)) {
			return nil, nil // not a repo (exit 128), or no commit yet (exit 1)
		}
		return nil, err
	}
	// --reverse applies after --max-count: the newest commits, oldest first.
	out, err := git(ctx, root, "log", "--reverse", fmt.Sprintf("--max-count=%d", maxLog), "--raw", "--no-abbrev",
		"--no-renames", "--format=%x1e%H%x00%s%x00%b%x00")
	if err != nil {
		return nil, err
	}
	var sigs []Signal
	blobs := map[string]map[string]string{} // path → content → the commit that made it; "" before the log
	for _, rec := range strings.Split(string(out), "\x1e")[1:] {
		f := strings.SplitN(rec, "\x00", 4)
		if len(f) < 4 {
			return nil, fmt.Errorf("read git log: bad record %q", rec)
		}
		back := goesBack(f[0], f[3], blobs)
		switch m := reverts.FindStringSubmatch(f[2]); {
		case m != nil:
			sigs = append(sigs, Signal{Kind: Revert, Commits: []string{f[0], m[1]}})
		case strings.HasPrefix(f[1], `Revert "`) || back != nil:
			sigs = append(sigs, Signal{Kind: Revert, Commits: append([]string{f[0]}, back...)})
		}
	}
	return sigs, nil
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
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir, "--no-pager"}, args...)...)
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}
