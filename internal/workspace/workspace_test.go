package workspace

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aryanVijaywargia/herdr-brain/internal/herdr"
	"github.com/aryanVijaywargia/herdr-brain/internal/paths"
	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

func deadline(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// canon returns the canonical form of the existing path p.
func canon(t *testing.T, p string) string {
	t.Helper()
	c, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// gitRepo makes a git work tree with the folders sub and sub/deeper, and returns its path.
func gitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if out, err := exec.Command("git", "init", "-q", dir).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	mkdir(t, filepath.Join(dir, "sub", "deeper"))
	return dir
}

func mkdir(t *testing.T, p string) {
	t.Helper()
	if err := os.MkdirAll(p, 0o700); err != nil {
		t.Fatal(err)
	}
}

// testNow is the last seen time of the test calls.
var testNow = time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC)

// testSession is the workspace map of one herdr session, with the store of its binding.
type testSession struct {
	Session
	st *store.Store
}

// newSession returns the workspace map of herdr session name, with its own session_dir. It
// shares the store and the map file of share[0], as real sessions share the main store; with
// no share it gets a new store.
func newSession(t *testing.T, name string, share ...testSession) testSession {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "herdr.sock")
	if len(share) > 0 {
		return testSession{Session{Name: name, Socket: sock, MapFile: share[0].MapFile}, share[0].st}
	}
	dir := t.TempDir()
	st, err := store.Open(context.Background(), dir, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Error(err)
		}
	})
	return testSession{Session{Name: name, Socket: sock, MapFile: paths.WorkspaceMap(dir)}, st}
}

// row reads the store row of key.
func row(t *testing.T, s testSession, key string) store.Workspace {
	t.Helper()
	var w store.Workspace
	err := s.st.Write(context.Background(), func(tx *sql.Tx) error {
		var err error
		w, err = store.WorkspaceByKey(context.Background(), tx, key)
		return err
	})
	if err != nil {
		t.Fatalf("row %s: %v", key, err)
	}
	return w
}

// save writes session.json of s the way herdr 0.9.1 saves it, with the root of each id.
func save(t *testing.T, s testSession, rootByID map[string]string) {
	t.Helper()
	type ws struct {
		ID          string `json:"id"`
		CustomName  string `json:"custom_name"`
		IdentityCwd string `json:"identity_cwd"`
	}
	f := struct {
		Version    int  `json:"version"`
		Workspaces []ws `json:"workspaces"`
	}{Version: 3}
	for id, r := range rootByID {
		f.Workspaces = append(f.Workspaces, ws{ID: id, CustomName: id, IdentityCwd: r})
	}
	data, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, s.sessionFile(), string(data))
}

func writeFile(t *testing.T, path, data string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}

func refresh(t *testing.T, s testSession, ws ...herdr.Workspace) {
	t.Helper()
	if err := s.Refresh(deadline(t), os.Environ(), s.st, ws, testNow); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
}

func lookup(t *testing.T, s testSession, id string) Mapping {
	t.Helper()
	m, err := s.Lookup(id)
	if err != nil {
		t.Fatalf("Lookup(%s): %v", id, err)
	}
	return m
}

// mapped checks that id of s is Mapped and returns its entry.
func mapped(t *testing.T, s testSession, id string) Entry {
	t.Helper()
	m := lookup(t, s, id)
	if m.State != Mapped {
		t.Fatalf("Lookup(%s).State = %q (%v), want %q", id, m.State, m.Err, Mapped)
	}
	return m.Entry
}

func w(id, label string) herdr.Workspace { return herdr.Workspace{ID: id, Label: label, Number: 1} }

// AC: same session and id, root in the same project folder: same key after a herdr restart.
func TestRestartKeepsKey(t *testing.T) {
	repo := gitRepo(t)
	s := newSession(t, "default")
	save(t, s, map[string]string{"w1": repo})
	refresh(t, s, w("w1", "brain"))
	before := mapped(t, s, "w1")
	if before.Project != canon(t, repo) {
		t.Errorf("project = %q, want %q", before.Project, canon(t, repo))
	}

	// A restart keeps the ids and the last saved identity_cwd [M0]; herdr saves on stop.
	save(t, s, map[string]string{"w1": repo})
	refresh(t, s, w("w1", "brain"))
	if after := mapped(t, s, "w1"); after.Key != before.Key {
		t.Errorf("key after restart = %q, want %q", after.Key, before.Key)
	}
	if r := row(t, s, before.Key); r.HerdrSession != "default" || r.HerdrWorkspaceID != "w1" ||
		r.RootFolder == nil || *r.RootFolder != before.Project || r.Status != store.WorkspaceLive {
		t.Errorf("store row = %+v, want session default, w1, root folder %s, live", r, before.Project)
	}
}

// AC: a cd of the root pane to a subfolder of the project, before and after the herdr save:
// same key. The first mapping records the git top-level, also from a subfolder.
func TestCdInsideProjectKeepsKey(t *testing.T) {
	repo := gitRepo(t)
	s := newSession(t, "default")
	save(t, s, map[string]string{"w1": filepath.Join(repo, "sub")})
	refresh(t, s, w("w1", "brain"))
	first := mapped(t, s, "w1")
	if first.Project != canon(t, repo) {
		t.Fatalf("project = %q, want the git top-level %q", first.Project, canon(t, repo))
	}
	for _, r := range []string{
		filepath.Join(repo, "sub"),           // cd, not saved yet: session.json is unchanged
		filepath.Join(repo, "sub", "deeper"), // saved after a cd
		repo,                                 // cd back to the top
	} {
		save(t, s, map[string]string{"w1": r})
		refresh(t, s, w("w1", "brain"))
		if got := mapped(t, s, "w1"); got.Key != first.Key {
			t.Errorf("root %s: key = %q, want %q", r, got.Key, first.Key)
		}
	}
}

// AC: the root pane closes and the next root pane is inside the project: same key. A rename:
// same key, new label.
func TestRootPaneCloseAndRenameKeepKey(t *testing.T) {
	repo := gitRepo(t)
	s := newSession(t, "work")
	save(t, s, map[string]string{"w3": repo})
	refresh(t, s, w("w3", "brain"))
	first := mapped(t, s, "w3")

	save(t, s, map[string]string{"w3": filepath.Join(repo, "sub")}) // the next root pane
	refresh(t, s, w("w3", "brain-renamed"))
	got := mapped(t, s, "w3")
	if got.Key != first.Key || got.Label != "brain-renamed" {
		t.Errorf("after close and rename: key %q label %q, want key %q label %q",
			got.Key, got.Label, first.Key, "brain-renamed")
	}
	if r := row(t, s, first.Key); r.Label != "brain-renamed" {
		t.Errorf("store row label = %q, want %q", r.Label, "brain-renamed")
	}
}

// AC: root saved in another project folder: the workspace part is withheld ("withheld:
// identity"); status offers a remap; a remap gives a new key; the old items are listed for
// relink, not moved (S83).
func TestOtherProjectWithheldAndRemap(t *testing.T) {
	a, b := gitRepo(t), gitRepo(t)
	s := newSession(t, "default")
	save(t, s, map[string]string{"w1": a})
	refresh(t, s, w("w1", "brain"))
	old := mapped(t, s, "w1")
	if _, err := s.Remap(deadline(t), os.Environ(), s.st, "w1", testNow); err == nil {
		t.Error("Remap of a workspace inside its project: want an error")
	}

	save(t, s, map[string]string{"w1": filepath.Join(b, "sub")})
	refresh(t, s, w("w1", "brain")) // a brain call never remaps by itself
	m := lookup(t, s, "w1")
	if m.State != Withheld || !errors.Is(m.Err, ErrIdentity) || m.Entry.Key != old.Key {
		t.Fatalf("Lookup = %q %v key %q, want %q, ErrIdentity, key %q", m.State, m.Err, m.Entry.Key, Withheld, old.Key)
	}
	if !m.Remappable() || !strings.Contains(m.Status("w1"), "remap") {
		t.Errorf("Status = %q, Remappable = %v, want a remap offer", m.Status("w1"), m.Remappable())
	}

	r, err := s.Remap(deadline(t), os.Environ(), s.st, "w1", testNow)
	if err != nil {
		t.Fatalf("Remap: %v", err)
	}
	if r.New.Key == old.Key || r.New.Project != canon(t, b) {
		t.Errorf("Remap new = %+v, want a new key for project %s", r.New, canon(t, b))
	}
	if len(r.Old) != 1 || r.Old[0].Key != old.Key || r.Old[0].Project != old.Project {
		t.Errorf("Remap old = %+v, want the old entry %+v for relink", r.Old, old)
	}
	if got := mapped(t, s, "w1"); got.Key != r.New.Key {
		t.Errorf("key after remap = %q, want %q", got.Key, r.New.Key)
	}
	// The old key stays in the map with its project folder, so its items wait for a relink.
	m2, err := readMap(s.MapFile)
	if err != nil {
		t.Fatal(err)
	}
	if len(m2.Entries) != 2 || m2.Entries[0].Key != old.Key || m2.Entries[0].ReplacedBy != r.New.Key {
		t.Errorf("map entries = %+v, want the old entry kept and replaced by %q", m2.Entries, r.New.Key)
	}
	if o, n := row(t, s, old.Key), row(t, s, r.New.Key); *o.RootFolder != old.Project || *n.RootFolder != r.New.Project {
		t.Errorf("store rows: old %+v, new %+v, want both kept with their project folders", o, n)
	}
}

// AC: a missing or unreadable session.json, an unknown format, or an unmapped id: no
// workspace part. The E2-S5 reader rules: withhold, never guess.
func TestNoWorkspacePart(t *testing.T) {
	repo := gitRepo(t)
	good := `{"version":3,"workspaces":[{"id":"w1","identity_cwd":"` + repo + `"}]}`
	tests := []struct {
		name string
		file string // session.json after the mapping; "" removes it
		id   string
		want State
	}{
		{"good", good, "w1", Mapped},
		{"extra fields ignored", `{"version":3,"x":1,"workspaces":[{"id":"w1","identity_cwd":"` + repo + `","y":2}]}`, "w1", Mapped},
		{"missing file", "", "w1", Withheld},
		{"not JSON", `{"version":3,`, "w1", Withheld},
		{"unknown version", `{"version":4,"workspaces":[{"id":"w1","identity_cwd":"` + repo + `"}]}`, "w1", Withheld},
		{"no version", `{"workspaces":[{"id":"w1","identity_cwd":"` + repo + `"}]}`, "w1", Withheld},
		{"no workspaces", `{"version":3}`, "w1", Withheld},
		{"other entry missing a field", `{"version":3,"workspaces":[{"id":"w2"},{"id":"w1","identity_cwd":"` + repo + `"}]}`, "w1", Withheld},
		{"wrong type", `{"version":3,"workspaces":[{"id":"w1","identity_cwd":7}]}`, "w1", Withheld},
		{"relative root", `{"version":3,"workspaces":[{"id":"w1","identity_cwd":"tmp/a"}]}`, "w1", Withheld},
		{"root does not exist", `{"version":3,"workspaces":[{"id":"w1","identity_cwd":"` + repo + `/gone"}]}`, "w1", Withheld},
		{"duplicate id", `{"version":3,"workspaces":[{"id":"w1","identity_cwd":"` + repo + `"},{"id":"w1","identity_cwd":"` + repo + `"}]}`, "w1", Withheld},
		{"not in the file", `{"version":3,"workspaces":[{"id":"w2","identity_cwd":"` + repo + `"}]}`, "w1", Withheld},
		{"unmapped id", good, "w9", Unmapped},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newSession(t, "default")
			writeFile(t, s.sessionFile(), good)
			refresh(t, s, w("w1", "brain"))
			if tt.file == "" {
				if err := os.Remove(s.sessionFile()); err != nil {
					t.Fatal(err)
				}
			} else {
				writeFile(t, s.sessionFile(), tt.file)
			}
			m := lookup(t, s, tt.id)
			if m.State != tt.want {
				t.Errorf("Lookup(%s).State = %q (%v), want %q", tt.id, m.State, m.Err, tt.want)
			}
			if tt.want == Withheld && !errors.Is(m.Err, ErrIdentity) {
				t.Errorf("Lookup(%s).Err = %v, want ErrIdentity", tt.id, m.Err)
			}
		})
	}
}

// An unreadable session.json at the first mapping leaves the workspace unmapped.
func TestRefreshUnreadableRootStaysUnmapped(t *testing.T) {
	s := newSession(t, "default")
	writeFile(t, s.sessionFile(), `{"version":4,"workspaces":[]}`)
	refresh(t, s, w("w1", "brain"))
	if m := lookup(t, s, "w1"); m.State != Unmapped {
		t.Errorf("Lookup(w1).State = %q, want %q", m.State, Unmapped)
	}
}

// AC: w14 in session A and w14 in session B: two keys (S79). Both sessions share the main
// store, so they share one map file.
func TestSameIDInTwoSessions(t *testing.T) {
	repo := gitRepo(t)
	a := newSession(t, "A")
	b := newSession(t, "B", a)
	save(t, a, map[string]string{"w14": repo})
	save(t, b, map[string]string{"w14": repo})
	refresh(t, a, w("w14", "brain"))
	refresh(t, b, w("w14", "brain"))
	if ka, kb := mapped(t, a, "w14").Key, mapped(t, b, "w14").Key; ka == kb {
		t.Errorf("w14 in A and in B: both key %q, want two keys", ka)
	}
}

// AC: a new workspace not yet mapped: the hook sees "unmapped" (S48; the hook itself is E7).
func TestNewWorkspaceUnmapped(t *testing.T) {
	repo := gitRepo(t)
	s := newSession(t, "default")
	save(t, s, map[string]string{"w1": repo})
	refresh(t, s, w("w1", "brain"))
	save(t, s, map[string]string{"w1": repo, "w2": repo}) // w2 was made after the last brain call
	if m := lookup(t, s, "w2"); m.State != Unmapped {
		t.Errorf("Lookup(w2).State = %q, want %q", m.State, Unmapped)
	}
}

// AC: a recreated session whose w1 has a root in another project, and the first Brain call is
// the hook: no workspace part from the old w1. A later brain call does not remap it either.
func TestRecreatedSessionHookFirst(t *testing.T) {
	repo := gitRepo(t)
	other := t.TempDir() // a folder outside git
	s := newSession(t, "work")
	save(t, s, map[string]string{"w1": repo})
	refresh(t, s, w("w1", "brain"))
	old := mapped(t, s, "w1")

	// herdr state deleted and the session recreated: w1 is reused for another folder [M0].
	save(t, s, map[string]string{"w1": other})
	if m := lookup(t, s, "w1"); m.State != Withheld {
		t.Fatalf("hook Lookup(w1).State = %q, want %q", m.State, Withheld)
	}
	refresh(t, s, w("w1", "scratch"))
	if m := lookup(t, s, "w1"); m.State != Withheld || m.Entry.Key != old.Key {
		t.Errorf("after a brain call: %q key %q, want %q key %q", m.State, m.Entry.Key, Withheld, old.Key)
	}
}

// Outside git, the project folder is the canonical root, and its subfolders keep the key.
func TestProjectOutsideGit(t *testing.T) {
	dir := t.TempDir()
	mkdir(t, filepath.Join(dir, "sub"))
	s := newSession(t, "default")
	save(t, s, map[string]string{"w1": dir})
	refresh(t, s, w("w1", "notes"))
	first := mapped(t, s, "w1")
	if first.Project != canon(t, dir) {
		t.Errorf("project = %q, want %q", first.Project, canon(t, dir))
	}
	save(t, s, map[string]string{"w1": filepath.Join(dir, "sub")})
	if got := mapped(t, s, "w1"); got.Key != first.Key {
		t.Errorf("key in a subfolder = %q, want %q", got.Key, first.Key)
	}
}

// The caller's GIT_DIR and GIT_WORK_TREE do not choose the project folder: the saved root does.
func TestGitVarsIgnored(t *testing.T) {
	parent := t.TempDir()
	a, b := filepath.Join(parent, "a"), filepath.Join(parent, "b")
	for _, dir := range []string{a, b} {
		if out, err := exec.Command("git", "init", "-q", dir).CombinedOutput(); err != nil {
			t.Fatalf("git init: %v: %s", err, out)
		}
	}
	s := newSession(t, "default")
	save(t, s, map[string]string{"w1": a})
	env := append(os.Environ(), "GIT_DIR="+filepath.Join(a, ".git"), "GIT_WORK_TREE="+parent)
	if err := s.Refresh(deadline(t), env, s.st, []herdr.Workspace{w("w1", "a")}, testNow); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if got := mapped(t, s, "w1"); got.Project != canon(t, a) {
		t.Errorf("project = %q, want %q", got.Project, canon(t, a))
	}
	save(t, s, map[string]string{"w1": b})
	if m := lookup(t, s, "w1"); m.State != Withheld {
		t.Errorf("state in a sibling repository = %q, want %q", m.State, Withheld)
	}
}

// A broken repository is not a folder outside git: the mapping fails and waits for a later
// call, which records the real top-level once the repository works.
func TestBrokenGitFailsMapping(t *testing.T) {
	dir := t.TempDir()
	mkdir(t, filepath.Join(dir, "sub"))
	gitFile := filepath.Join(dir, ".git")
	writeFile(t, gitFile, "gitdir: "+filepath.Join(dir, "missing")+"\n")
	s := newSession(t, "default")
	save(t, s, map[string]string{"w1": filepath.Join(dir, "sub")})
	if err := s.Refresh(deadline(t), os.Environ(), s.st, []herdr.Workspace{w("w1", "broken")}, testNow); err == nil {
		t.Fatal("Refresh with a broken .git: no error")
	}
	if m := lookup(t, s, "w1"); m.State != Unmapped {
		t.Fatalf("state = %q, want %q", m.State, Unmapped)
	}
	if err := os.Remove(gitFile); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "init", "-q", dir).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	refresh(t, s, w("w1", "repaired"))
	if got := mapped(t, s, "w1"); got.Project != canon(t, dir) {
		t.Errorf("project = %q, want %q", got.Project, canon(t, dir))
	}
}

// Git runs only at the first mapping: a known pair needs no git, even for a rename.
func TestGitOnlyAtFirstMapping(t *testing.T) {
	repo := gitRepo(t)
	s := newSession(t, "default")
	save(t, s, map[string]string{"w1": repo})
	refresh(t, s, w("w1", "brain"))
	t.Setenv("PATH", t.TempDir()) // no git on PATH now
	refresh(t, s, w("w1", "renamed"))
	if got := mapped(t, s, "w1"); got.Label != "renamed" {
		t.Errorf("label = %q, want %q", got.Label, "renamed")
	}
}

// AC: on each brain call, `herdr workspace list` of the bound session feeds the map.
func TestSync(t *testing.T) {
	repo := gitRepo(t)
	s := newSession(t, "brain-test")
	save(t, s, map[string]string{"w1": repo})
	fixture, err := os.ReadFile(filepath.Join("..", "herdr", "testdata", "workspace-list.json"))
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(t.TempDir(), "herdr")
	writeFile(t, bin, "#!/bin/sh\ncat <<'EOF'\n"+string(fixture)+"\nEOF\n")
	if err := os.Chmod(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := s.Sync(deadline(t), append(os.Environ(), "HERDR_BIN_PATH="+bin), s.st, testNow); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if got := mapped(t, s, "w1"); got.Label != "brain-test" {
		t.Errorf("label = %q, want %q (from workspace list)", got.Label, "brain-test")
	}
}

// A map file that Brain cannot parse is an error, not an empty map.
func TestLookupBadMapFile(t *testing.T) {
	s := newSession(t, "default")
	writeFile(t, s.MapFile, `{"version":2,"entries":[]}`)
	if _, err := s.Lookup("w1"); err == nil {
		t.Error("Lookup with a map file of version 2: want an error")
	}
}
