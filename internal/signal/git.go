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

// Reverts finds the revert commits in the git log of the workspace root root (§7.1). It runs
// only read-only git commands: rev-parse and log. A root outside git, or a repo with no
// commit, has no reverts.
func Reverts(ctx context.Context, root string) ([]Signal, error) {
	if _, err := git(ctx, root, "rev-parse", "-q", "--verify", "HEAD"); err != nil {
		if errors.As(err, new(*exec.ExitError)) {
			return nil, nil // not a repo (exit 128), or no commit yet (exit 1)
		}
		return nil, err
	}
	out, err := git(ctx, root, "log", fmt.Sprintf("--max-count=%d", maxLog), "--format=%H%x00%s%x00%b%x1e")
	if err != nil {
		return nil, err
	}
	var sigs []Signal
	for _, rec := range strings.Split(string(out), "\x1e") {
		hash, rest, ok := strings.Cut(strings.TrimSpace(rec), "\x00")
		if !ok {
			continue
		}
		subject, body, _ := strings.Cut(rest, "\x00")
		m := reverts.FindStringSubmatch(body)
		if m == nil && !strings.HasPrefix(subject, `Revert "`) {
			continue
		}
		s := Signal{Kind: Revert, Commits: []string{hash}}
		if m != nil {
			s.Commits = append(s.Commits, m[1])
		}
		sigs = append(sigs, s)
	}
	return sigs, nil
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
