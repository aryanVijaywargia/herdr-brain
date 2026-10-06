package signal

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path"
	"regexp"
	"slices"
	"strings"

	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

// The git anchors of the stale check (E16-S6, §9.1). Like the revert rule, they run only
// read-only git commands: rev-parse, ls-files, diff, log and hash-object without -w, which
// writes no object. An anchor holds the blob of a tracked file as it is in the work tree now [NEW]: a
// saved edit, a git mv or a deletion counts before a commit.

// wordStart and wordEnd hold the characters that a path in a text can stand between: quotes,
// brackets and sentence marks. A path can start with a dot (.gitignore), not end with one.
// [NEW] the path match rule: a word of the text, without these characters at its ends, a line
// suffix (":12" or ":12:3") and a leading "./", that names a file which git tracks under the
// workspace root, relative to that root.
const (
	wordStart = "`'\"()[]{}<>,;:!?*"
	wordEnd   = wordStart + "."
)

// lineSuffix is the line and column suffix of a path, as in "a.go:12:3".
var lineSuffix = regexp.MustCompile(`(:[0-9]+)+$`)

// shortBlob is the length of a blob hash in a dispute diff [NEW], as git's short hashes.
const shortBlob = 7

// FindAnchors returns an anchor for each path in text that git tracks under the workspace
// root root (§9.1), each path once, in text order. A root that is "", or
// outside git, or a repo with no commit, gives no anchor and no error.
func FindAnchors(ctx context.Context, root, text string) ([]store.Anchor, error) {
	if root == "" {
		return nil, nil
	}
	top, prefix, err := repo(ctx, root)
	if err != nil || top == "" {
		return nil, err
	}
	tree, err := workTree(ctx, top)
	if err != nil {
		return nil, err
	}
	var anchors []store.Anchor
	for _, w := range pathWords(text) {
		p := path.Clean(prefix + w)
		blob, ok := tree[p]
		if !ok || slices.ContainsFunc(anchors, func(a store.Anchor) bool { return a.Path == p }) {
			continue
		}
		anchors = append(anchors, store.Anchor{Root: top, Path: p, Blob: blob})
	}
	return anchors, nil
}

// pathWords returns the words of text that can be a relative path, as wordStart says.
func pathWords(text string) []string {
	var words []string
	for _, w := range strings.Fields(text) {
		w = strings.TrimLeft(w, wordStart)
		w = strings.TrimRight(lineSuffix.ReplaceAllString(strings.TrimRight(w, wordEnd), ""), wordEnd)
		w = strings.TrimPrefix(w, "./")
		if w == "" || strings.HasPrefix(w, "/") || strings.HasPrefix(w, "~") || strings.Contains(w, "://") {
			continue
		}
		if c := path.Clean(w); c == ".." || strings.HasPrefix(c, "../") {
			continue
		}
		words = append(words, w)
	}
	return words
}

// AnchorChange is one anchor that no longer matches git: its path holds another blob, or the
// path is gone: git no longer tracks it, or the file is deleted (§9.1).
type AnchorChange struct {
	Anchor store.Anchor
	// Blob is the blob of the path now; "" when the path is gone.
	Blob string
	// MovedTo is the path that the file moved to, when the path is gone; "" when git cannot
	// tell a move: the file is deleted.
	MovedTo string
}

// CheckAnchors compares anchors with the tracked files of their repos. It returns the anchors
// that changed, moved or were deleted, and the anchors as they are now: each path that is
// still there with its blob now, without the paths that are gone. A repo root that is no
// longer a repo, or has no commit, holds none of its paths.
func CheckAnchors(ctx context.Context, anchors []store.Anchor) (changes []AnchorChange, now []store.Anchor, err error) {
	trees := map[string]map[string]string{}
	for _, a := range anchors {
		tree, ok := trees[a.Root]
		if !ok {
			if tree, err = workTree(ctx, a.Root); err != nil {
				return nil, nil, err
			}
			trees[a.Root] = tree
		}
		blob, ok := tree[a.Path]
		if ok {
			now = append(now, store.Anchor{Root: a.Root, Path: a.Path, Blob: blob})
		}
		switch {
		case !ok:
			to, err := movedTo(ctx, a, tree)
			if err != nil {
				return nil, nil, err
			}
			changes = append(changes, AnchorChange{Anchor: a, MovedTo: to})
		case blob != a.Blob:
			changes = append(changes, AnchorChange{Anchor: a, Blob: blob})
		}
	}
	return changes, now, nil
}

// movedTo returns the path that the file of anchor a, whose path is gone from tree, moved to:
// a path that holds the old blob now, or else the last rename of the path that git finds since
// the stored blob, in the work tree, the index or the log, whose new path is in tree. It
// returns "" when git cannot tell a move.
func movedTo(ctx context.Context, a store.Anchor, tree map[string]string) (string, error) {
	if p := holder(tree, a.Blob); p != "" {
		return p, nil
	}
	// The work tree and the index against HEAD first, then the commits, newest first.
	for _, args := range [][]string{
		{"diff", "-M", "--raw", "-z", "--no-abbrev", "HEAD"},
		{"log", "-M", "--raw", "-z", "--no-abbrev", "--format=", fmt.Sprintf("--max-count=%d", maxLog)},
	} {
		out, err := git(ctx, a.Root, args...)
		if err != nil {
			if errors.As(err, new(*exec.ExitError)) {
				return "", nil // not a repo, or no commit
			}
			return "", err
		}
		to, done := renamed(string(out), a)
		if to != "" {
			if _, ok := tree[to]; ok {
				return to, nil
			}
		}
		if done {
			return "", nil
		}
	}
	return "", nil
}

// renamed reads raw diff records (git --raw -z) and returns the new path of the first rename of
// a.Path. done is true when a record made a.Path hold the stored blob first: the renames
// before it are older than the anchor, so they do not count.
func renamed(raw string, a store.Anchor) (to string, done bool) {
	f := strings.Split(raw, "\x00")
	for i := 0; i < len(f); i++ {
		meta := strings.Fields(strings.TrimLeft(f[i], "\n"))
		if len(meta) != 5 || !strings.HasPrefix(meta[0], ":") {
			continue
		}
		status, blob := meta[4], meta[3]
		src := ""
		if i+1 < len(f) {
			src = f[i+1]
		}
		dst := src
		if strings.HasPrefix(status, "R") || strings.HasPrefix(status, "C") {
			if i+2 < len(f) {
				dst = f[i+2]
			}
			i++
		}
		i++
		switch {
		case strings.HasPrefix(status, "R") && src == a.Path:
			return dst, true
		case dst == a.Path && blob == a.Blob:
			return "", true
		}
	}
	return "", false
}

// holder returns the first path, in path order, whose blob is blob in tree, or "".
func holder(tree map[string]string, blob string) string {
	var paths []string
	for p, b := range tree {
		if b == blob {
			paths = append(paths, p)
		}
	}
	if len(paths) == 0 {
		return ""
	}
	return slices.Min(paths)
}

// AnchorDiff is the anchor diff of a dispute, as the review queue shows it: for each change,
// the old anchor and the new one. [NEW] the form.
func AnchorDiff(changes []AnchorChange) string {
	lines := make([]string, 0, 2*len(changes))
	for _, c := range changes {
		lines = append(lines, "- "+c.Anchor.Path+" "+short(c.Anchor.Blob))
		switch {
		case c.Blob != "":
			lines = append(lines, "+ "+c.Anchor.Path+" "+short(c.Blob)+" (changed)")
		case c.MovedTo != "":
			lines = append(lines, "+ "+c.MovedTo+" "+short(c.Anchor.Blob)+" (moved)")
		default:
			lines = append(lines, "+ (deleted)")
		}
	}
	return strings.Join(lines, "\n")
}

// Moved returns the paths of the anchors that moved in diff, an AnchorDiff, in diff order.
func Moved(diff string) []string {
	var paths []string
	lines := strings.Split(diff, "\n")
	for i := 1; i < len(lines); i++ {
		if !strings.HasSuffix(lines[i], " (moved)") || !strings.HasPrefix(lines[i-1], "- ") {
			continue
		}
		if p, _, ok := strings.Cut(strings.TrimPrefix(lines[i-1], "- "), " "); ok {
			paths = append(paths, p)
		}
	}
	return paths
}

func short(blob string) string { return blob[:min(len(blob), shortBlob)] }

// repo returns the top folder of the repo of root, and the path of root in it with a final
// slash ("" at the top). top is "" when root is not in a repo.
func repo(ctx context.Context, root string) (top, prefix string, err error) {
	out, err := git(ctx, root, "rev-parse", "--show-toplevel", "--show-prefix")
	if err != nil {
		if errors.As(err, new(*exec.ExitError)) {
			return "", "", nil // not a repo, or root is gone (exit 128)
		}
		return "", "", err
	}
	lines := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	if len(lines) == 1 {
		lines = append(lines, "")
	}
	return lines[0], lines[1], nil
}

// workTree returns the blob of each file that git tracks in the repo at top, as the file is
// in the work tree now, by its path in the repo: the index blob, or the blob of the saved
// content when the file differs from the index. A deleted file is not in it. A folder that is
// not a repo has an empty tree.
func workTree(ctx context.Context, top string) (map[string]string, error) {
	out, err := git(ctx, top, "ls-files", "-s", "-z")
	if err != nil {
		if errors.As(err, new(*exec.ExitError)) {
			return map[string]string{}, nil // not a repo, or top is gone (exit 128)
		}
		return nil, err
	}
	tree := map[string]string{}
	for _, rec := range strings.Split(string(out), "\x00") {
		info, p, ok := strings.Cut(rec, "\t")
		f := strings.Fields(info)
		if !ok || len(f) != 3 || f[0] == gitlink {
			continue // a submodule is a commit, not a file
		}
		tree[p] = f[1]
	}
	deleted, err := lsFiles(ctx, top, "-d")
	if err != nil {
		return nil, err
	}
	for _, p := range deleted {
		delete(tree, p)
	}
	modified, err := lsFiles(ctx, top, "-m")
	if err != nil {
		return nil, err
	}
	modified = slices.DeleteFunc(modified, func(p string) bool { _, ok := tree[p]; return !ok })
	if len(modified) == 0 {
		return tree, nil
	}
	// hash-object without -w hashes the saved content and writes nothing.
	out, err = gitIn(ctx, top, strings.Join(modified, "\n")+"\n", "hash-object", "--stdin-paths")
	if err != nil {
		return nil, err
	}
	blobs := strings.Fields(string(out))
	if len(blobs) != len(modified) {
		return nil, fmt.Errorf("git hash-object: %d hashes for %d files", len(blobs), len(modified))
	}
	for i, p := range modified {
		tree[p] = blobs[i]
	}
	return tree, nil
}

// gitlink is the index mode of a submodule.
const gitlink = "160000"

// lsFiles returns the paths that git ls-files flag lists in the repo at top.
func lsFiles(ctx context.Context, top, flag string) ([]string, error) {
	out, err := git(ctx, top, "ls-files", "-z", flag)
	if err != nil {
		return nil, err
	}
	return slices.DeleteFunc(strings.Split(string(out), "\x00"), func(p string) bool { return p == "" }), nil
}
