package workspace

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// sessionVersion is the session.json version of herdr 0.9.1 [M0] (spike/m0/FINDINGS.md, E2-S5).
const sessionVersion = 3

// ErrIdentity means that the saved root of a workspace cannot be read, or lies outside the
// project folder of its map entry. The call gets no workspace part ("withheld: identity").
var ErrIdentity = errors.New("withheld: identity")

// roots reads the saved root (identity_cwd) of each workspace from herdr's session.json. It
// withholds, never guesses (E2-S5): a missing or unparsable file, another version, or an entry
// with a missing field or a wrong type fails the whole file, so the result does not depend on
// entry order. Unknown extra fields are ignored. An id that appears twice maps to nil.
func roots(path string) (map[string][]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrIdentity, err)
	}
	var f struct {
		Version    *int `json:"version"`
		Workspaces *[]struct {
			ID          *string `json:"id"`
			IdentityCwd *string `json:"identity_cwd"`
		} `json:"workspaces"`
	}
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("%w: parse %s: %w", ErrIdentity, path, err)
	}
	if f.Version == nil || *f.Version != sessionVersion {
		return nil, fmt.Errorf("%w: %s: unknown version", ErrIdentity, path)
	}
	if f.Workspaces == nil {
		return nil, fmt.Errorf("%w: %s: no workspaces", ErrIdentity, path)
	}
	rs := make(map[string][]string, len(*f.Workspaces))
	for i, w := range *f.Workspaces {
		if w.ID == nil || w.IdentityCwd == nil {
			return nil, fmt.Errorf("%w: %s: workspace %d: missing id or identity_cwd", ErrIdentity, path, i)
		}
		rs[*w.ID] = append(rs[*w.ID], *w.IdentityCwd)
	}
	return rs, nil
}

// root returns the canonical saved root of workspace id from the session.json at path.
func root(path, id string) (string, error) {
	rs, err := roots(path)
	if err != nil {
		return "", err
	}
	return rootOf(rs, id)
}

// rootOf returns the canonical saved root of workspace id in rs.
func rootOf(rs map[string][]string, id string) (string, error) {
	r := rs[id]
	switch {
	case len(r) == 0:
		return "", fmt.Errorf("%w: workspace %s is not in session.json", ErrIdentity, id)
	case len(r) > 1:
		return "", fmt.Errorf("%w: workspace %s is in session.json %d times", ErrIdentity, id, len(r))
	case !filepath.IsAbs(r[0]):
		return "", fmt.Errorf("%w: root %q of workspace %s is not absolute", ErrIdentity, r[0], id)
	}
	c, err := filepath.EvalSymlinks(r[0])
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrIdentity, err)
	}
	return c, nil
}

// within reports whether path is the existing folder parent or below it. It compares parent
// with each folder on the way up from path by identity, not by text, so an alias such as
// another letter case on a case-insensitive disk counts as the same folder (as in bindings).
// path is canonical, so there are no links on the way up.
func within(parent, path string) (bool, error) {
	fp, err := os.Stat(parent)
	if err != nil {
		return false, err
	}
	for p := path; ; p = filepath.Dir(p) {
		fi, err := os.Stat(p)
		if err != nil {
			return false, err
		}
		if os.SameFile(fp, fi) {
			return true, nil
		}
		if filepath.Dir(p) == p {
			return false, nil
		}
	}
}

// projectFolder returns the project folder of the canonical root: the git top-level of root,
// or root itself outside git. Only a first mapping or a remap runs it, never the hook.
//
// root is outside git when neither root nor a folder above it holds a .git entry. Git then
// does not run. Inside git, any git error fails the mapping: a broken repository is not a
// folder outside git, and a known mapping never runs git again.
func projectFolder(ctx context.Context, environ []string, root string) (string, error) {
	inGit, err := hasGitAbove(root)
	if err != nil || !inGit {
		return root, err
	}
	cmd := exec.CommandContext(ctx, "git", "-C", root, "rev-parse", "--show-toplevel")
	cmd.Env = withoutGitVars(environ)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git top-level of %s: %w: %s", root, err, strings.TrimSpace(stderr.String()))
	}
	return filepath.EvalSymlinks(strings.TrimSpace(string(out)))
}

// hasGitAbove reports whether root or a folder above it holds a .git entry (a folder, or the
// file of a worktree).
func hasGitAbove(root string) (bool, error) {
	for p := root; ; p = filepath.Dir(p) {
		_, err := os.Lstat(filepath.Join(p, ".git"))
		if err == nil {
			return true, nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return false, err
		}
		if filepath.Dir(p) == p {
			return false, nil
		}
	}
}

// withoutGitVars returns environ without the GIT_ variables. The caller's GIT_DIR or
// GIT_WORK_TREE would otherwise choose the repository, not the saved root.
func withoutGitVars(environ []string) []string {
	var env []string
	for _, kv := range environ {
		if !strings.HasPrefix(kv, "GIT_") {
			env = append(env, kv)
		}
	}
	return env
}
