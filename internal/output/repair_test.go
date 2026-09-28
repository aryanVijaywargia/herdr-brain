package output

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

// later is past one hook cap after testNow, the time of the test builds.
var later = testNow.Add(time.Second)

func mustRepair(t *testing.T, s *store.Store, out string, now time.Time) {
	t.Helper()
	if err := Repair(context.Background(), s, out, at(now)); err != nil {
		t.Fatalf("Repair() = %v", err)
	}
}

// wantConflict checks that err is a conflict on file name of generation gen.
func wantConflict(t *testing.T, err error, gen int64, name string) {
	t.Helper()
	var ce *ConflictError
	if !errors.As(err, &ce) || ce.Generation != gen || !slices.Equal(ce.Files, []string{name}) {
		t.Fatalf("error = %v, want a conflict on generation %d, file %s", err, gen, name)
	}
}

func mustConflicts(t *testing.T, out string) []ConflictError {
	t.Helper()
	cs, err := Conflicts(out)
	if err != nil {
		t.Fatalf("Conflicts() = %v", err)
	}
	return cs
}

func mustHolds(t *testing.T, out string, item int64) bool {
	t.Helper()
	h, err := Holds(out, item)
	if err != nil {
		t.Fatalf("Holds(%d) = %v", item, err)
	}
	return h
}

// handEdit writes "mine" over file name of generation gen.
func handEdit(t *testing.T, out string, gen int64, name string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(out, folder(gen), filepath.FromSlash(name)), []byte("mine"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// checkBytes checks that file name of generation gen still holds text.
func checkBytes(t *testing.T, out string, gen int64, name, text string) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(out, folder(gen), filepath.FromSlash(name)))
	if err != nil || string(b) != text {
		t.Errorf("%s of generation %d = %q, %v, want your edit %q", name, gen, b, err, text)
	}
}

func checkPointer(t *testing.T, out string, want int64) {
	t.Helper()
	if gen, err := readPointer(out); err != nil || gen != want {
		t.Errorf("pointer = %d, %v, want %d", gen, err, want)
	}
}

func exists(t *testing.T, path string) bool {
	t.Helper()
	_, err := os.Stat(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Fatal(err)
	}
	return err == nil
}

func TestRepairBuildsWhenBehind(t *testing.T) {
	s, out := newStore(t)
	mustRepair(t, s, out, testNow)
	if exists(t, filepath.Join(out, pointerName)) {
		t.Errorf("Repair() with no generation made a pointer")
	}

	addItem(t, s, "", "use npm")
	mustRepair(t, s, out, testNow)
	want := newest(t, s)
	checkPointer(t, out, want)
	if n := len(appliedEdits(t, s, want)); n != 2 {
		t.Errorf("applied edits of generation %d = %d, want 2", want, n)
	}

	mustRepair(t, s, out, later)
	checkPointer(t, out, want)
	if n := len(appliedEdits(t, s, want)); n != 2 {
		t.Errorf("applied edits after a repair with nothing to build = %d, want 2", n)
	}
}

// AC S44: a hand edit during repair is kept and reported.
func TestRepairKeepsHandEdit(t *testing.T) {
	s, out := newStore(t)
	id := addItem(t, s, "", "one")
	old := mustBuild(t, s, out)
	handEdit(t, out, old, globalName)
	gen := mustRevise(t, s, id, 1, "two")

	wantConflict(t, Repair(context.Background(), s, out, at(testNow)), old, globalName)
	checkBytes(t, out, old, globalName, "mine")
	checkPointer(t, out, old)
	// The first check stops the build before it writes anything.
	if exists(t, filepath.Join(out, folder(gen))) || exists(t, filepath.Join(out, stagingPrefix+"2")) {
		t.Errorf("Repair() with a conflict wrote generation %d", gen)
	}
	if cs := mustConflicts(t, out); len(cs) != 1 || cs[0].Generation != old {
		t.Errorf("Conflicts() = %v, want generation %d", cs, old)
	}
}

// AC: a hand edit to the source generation after the first check and before the switch stops
// the switch, is a conflict, and stays.
func TestHandEditBeforeSwitch(t *testing.T) {
	s, out := newStore(t)
	id := addItem(t, s, "k1", "one")
	old := mustBuild(t, s, out)
	mustRevise(t, s, id, 1, "two")

	name := workspaceName("k1")
	b := builder{out: out, now: at(testNow), writeFile: writeFile, afterRender: func() { handEdit(t, out, old, name) }}
	_, err := b.build(context.Background(), s, nil)
	wantConflict(t, err, old, name)
	checkBytes(t, out, old, name, "mine")
	checkPointer(t, out, old)
	if cs := mustConflicts(t, out); len(cs) != 1 || cs[0].Generation != old {
		t.Errorf("Conflicts() = %v, want generation %d", cs, old)
	}
}

// AC: a hand edit to a retired generation before cleanup keeps the whole folder, and it shows
// as a conflict. A clean retired generation goes.
func TestHandEditToRetiredGeneration(t *testing.T) {
	s, out := newStore(t)
	id := addItem(t, s, "", "one")
	edited := mustBuild(t, s, out)
	mustRevise(t, s, id, 1, "two")
	clean := mustBuild(t, s, out)
	mustRevise(t, s, id, 2, "three")
	cur := mustBuild(t, s, out)
	handEdit(t, out, edited, globalName)

	mustRepair(t, s, out, later)
	checkBytes(t, out, edited, globalName, "mine")
	if !exists(t, filepath.Join(out, folder(edited), manifestName)) {
		t.Errorf("the manifest of the edited generation %d is gone, want the whole folder kept", edited)
	}
	if exists(t, filepath.Join(out, folder(clean))) {
		t.Errorf("clean retired generation %d is still there", clean)
	}
	checkPointer(t, out, cur)
	want := []ConflictError{{Generation: edited, Files: []string{globalName}}}
	if cs := mustConflicts(t, out); !reflect.DeepEqual(cs, want) {
		t.Errorf("Conflicts() = %v, want %v", cs, want)
	}
}

// §7.5 step 8: a generation goes only when the pointer left it more than one hook cap ago.
func TestCleanupWaitsOneHookCap(t *testing.T) {
	s, out := newStore(t)
	id := addItem(t, s, "", "one")
	old := mustBuild(t, s, out)
	mustRevise(t, s, id, 1, "two")
	mustBuild(t, s, out)
	dir := filepath.Join(out, folder(old))

	mustRepair(t, s, out, testNow.Add(hookCap))
	if !exists(t, dir) {
		t.Fatalf("generation %d went at one hook cap, want it kept", old)
	}
	mustRepair(t, s, out, testNow.Add(hookCap+time.Millisecond))
	if exists(t, dir) {
		t.Errorf("generation %d is still there after one hook cap", old)
	}
}

// rewriteManifest changes the manifest in dir with fn. The result is still valid JSON.
func rewriteManifest(dir string, fn func(*Manifest)) error {
	path := filepath.Join(dir, manifestName)
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return err
	}
	fn(&m)
	if b, err = json.Marshal(m); err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
}

// damage lists changes to a generation folder that no hash of a named file shows: file is the
// name that the conflict gives.
var damage = []struct {
	name, file string
	edit       func(dir string) error
}{
	{"manifest with no files", manifestName, func(dir string) error {
		return rewriteManifest(dir, func(m *Manifest) { m.Files = map[string]File{} })
	}},
	{"manifest with no revision list", manifestName, func(dir string) error {
		return rewriteManifest(dir, func(m *Manifest) {
			f := m.Files[globalName]
			f.Revisions = nil
			m.Files[globalName] = f
		})
	}},
	{"manifest with a bad hash", manifestName, func(dir string) error {
		return rewriteManifest(dir, func(m *Manifest) {
			f := m.Files[globalName]
			f.SHA256 = "x"
			m.Files[globalName] = f
		})
	}},
	{"file the manifest does not name", "notes.md", func(dir string) error {
		return os.WriteFile(filepath.Join(dir, "notes.md"), []byte("item 99"), 0o600)
	}},
}

// Astra finding 1: a damaged retired generation is a conflict, and cleanup keeps it.
func TestCleanupKeepsDamagedGeneration(t *testing.T) {
	for _, c := range damage {
		t.Run(c.name, func(t *testing.T) {
			s, out := newStore(t)
			id := addItem(t, s, "", "one")
			old := mustBuild(t, s, out)
			mustRevise(t, s, id, 1, "two")
			mustBuild(t, s, out)
			if err := c.edit(filepath.Join(out, folder(old))); err != nil {
				t.Fatal(err)
			}

			mustRepair(t, s, out, later)
			if !exists(t, filepath.Join(out, folder(old), globalName)) {
				t.Errorf("damaged generation %d is gone, want it kept", old)
			}
			want := []ConflictError{{Generation: old, Files: []string{c.file}}}
			if cs := mustConflicts(t, out); !reflect.DeepEqual(cs, want) {
				t.Errorf("Conflicts() = %v, want %v", cs, want)
			}
			if !mustHolds(t, out, 99) {
				t.Errorf("Holds(99) = false, want true: the damaged folder may hold anything")
			}
		})
	}
}

// Astra finding 2: cleanup counts the reader grace period from the switch, not from the start
// of a slow build.
func TestSlowBuildKeepsOldGeneration(t *testing.T) {
	s, out := newStore(t)
	id := addItem(t, s, "", "one")
	old := mustBuild(t, s, out)
	mustRevise(t, s, id, 1, "two")

	clock := testNow
	b := builder{out: out, now: func() time.Time { return clock }, writeFile: writeFile,
		afterRender: func() { clock = clock.Add(time.Second) }}
	if _, err := b.build(context.Background(), s, nil); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(out, folder(old))
	mustRepair(t, s, out, clock.Add(hookCap))
	if !exists(t, dir) {
		t.Fatalf("generation %d went one hook cap after the build started, want it kept", old)
	}
	mustRepair(t, s, out, clock.Add(hookCap+time.Millisecond))
	if exists(t, dir) {
		t.Errorf("generation %d is still there one hook cap after the switch", old)
	}
}

// AC S66: a current file that does not match its manifest is a conflict.
func TestConflictsCurrentGeneration(t *testing.T) {
	cases := append(slices.Clone(damage), []struct {
		name, file string
		edit       func(dir string) error
	}{
		{"changed part", globalName, func(dir string) error {
			return os.WriteFile(filepath.Join(dir, globalName), []byte("mine"), 0o600)
		}},
		{"missing part", workspaceName("k1"), func(dir string) error {
			return os.Remove(filepath.Join(dir, filepath.FromSlash(workspaceName("k1"))))
		}},
		{"missing manifest", manifestName, func(dir string) error {
			return os.Remove(filepath.Join(dir, manifestName))
		}},
	}...)
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, out := newStore(t)
			addItem(t, s, "k1", "use pnpm")
			gen := mustBuild(t, s, out)
			if cs := mustConflicts(t, out); len(cs) != 0 {
				t.Fatalf("Conflicts() before the edit = %v, want none", cs)
			}
			if err := c.edit(filepath.Join(out, folder(gen))); err != nil {
				t.Fatal(err)
			}
			want := []ConflictError{{Generation: gen, Files: []string{c.file}}}
			if cs := mustConflicts(t, out); !reflect.DeepEqual(cs, want) {
				t.Errorf("Conflicts() = %v, want %v", cs, want)
			}
		})
	}
}

// AC S77: with a conflict open, forget stays pending.
func TestForgetPendingWithConflict(t *testing.T) {
	s, out := newStore(t)
	id := addItem(t, s, "", "secret plan")
	gen := mustBuild(t, s, out)
	handEdit(t, out, gen, globalName)
	markForget(t, s, gen)
	retire(t, s, id, 1)

	for _, now := range []time.Time{testNow, later} {
		wantConflict(t, Repair(context.Background(), s, out, at(now)), gen, globalName)
		if !mustHolds(t, out, id) {
			t.Errorf("Holds(%d) with the conflict open = false, want true (forget pending)", id)
		}
	}
}

// AC: a crash between two files, then forget: cleanup removes the staging folder, and no Brain
// output or staging file holds the forgotten text.
func TestCrashThenForget(t *testing.T) {
	s, out := newStore(t)
	id := addItem(t, s, "", "secret plan")
	addItem(t, s, "k1", "use pnpm")
	first := mustBuild(t, s, out)
	second := mustRevise(t, s, id, 1, "secret plan v2")
	stop := errors.New("stopped")
	n := 0
	b := builder{out: out, now: at(testNow), afterRender: func() {}, writeFile: func(path string, data []byte) error {
		if n++; n == 2 {
			return stop
		}
		return writeFile(path, data)
	}}
	if _, err := b.build(context.Background(), s, nil); !errors.Is(err, stop) {
		t.Fatalf("build() = %v, want the injected stop", err)
	}

	markForget(t, s, first, second)
	retire(t, s, id, 2)
	mustRepair(t, s, out, testNow)
	if exists(t, filepath.Join(out, stagingPrefix+"2")) {
		t.Errorf("staging folder of generation %d is still there", second)
	}
	// The pointer left the first generation just now, so a hook can still read it.
	if !mustHolds(t, out, id) {
		t.Errorf("Holds(%d) right after the switch = false, want true (forget pending)", id)
	}

	mustRepair(t, s, out, later)
	if mustHolds(t, out, id) {
		t.Errorf("Holds(%d) after cleanup = true, want false (forget done)", id)
	}
	if files := filesWith(t, out, "secret plan"); len(files) != 0 {
		t.Errorf("files that hold the forgotten text: %v, want none", files)
	}
}

// filesWith lists the files under out whose bytes hold text.
func filesWith(t *testing.T, out, text string) []string {
	t.Helper()
	var files []string
	err := filepath.WalkDir(out, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(path)
		if err == nil && strings.Contains(string(b), text) {
			files = append(files, path)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

// Holds counts a generation by its manifest pairs, and counts every folder that cannot prove
// absence.
func TestHolds(t *testing.T) {
	for _, c := range []struct {
		name string
		edit func(out string, gen int64) error
		want bool
	}{
		{"manifest without the item", func(string, int64) error { return nil }, false},
		{"text without a manifest pair", func(out string, gen int64) error {
			return os.WriteFile(filepath.Join(out, folder(gen), globalName), []byte("item 99"), 0o600)
		}, false},
		{"staging folder", func(out string, _ int64) error {
			return os.Mkdir(filepath.Join(out, stagingPrefix+"9"), 0o700)
		}, true},
		{"missing manifest", func(out string, gen int64) error {
			return os.Remove(filepath.Join(out, folder(gen), manifestName))
		}, true},
		{"invalid manifest", func(out string, gen int64) error {
			return os.WriteFile(filepath.Join(out, folder(gen), manifestName), []byte("{"), 0o600)
		}, true},
		{"manifest of another generation", func(out string, gen int64) error {
			return os.Rename(filepath.Join(out, folder(gen)), filepath.Join(out, folder(gen+5)))
		}, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			s, out := newStore(t)
			addItem(t, s, "", "use npm")
			gen := mustBuild(t, s, out)
			if err := c.edit(out, gen); err != nil {
				t.Fatal(err)
			}
			if got := mustHolds(t, out, 99); got != c.want {
				t.Errorf("Holds(99) = %v, want %v", got, c.want)
			}
		})
	}
	s, out := newStore(t)
	id := addItem(t, s, "k1", "use pnpm")
	mustBuild(t, s, out)
	if !mustHolds(t, out, id) {
		t.Errorf("Holds(%d) of an item in a workspace part = false, want true", id)
	}
}

// AC: approve A, approve B, generation cleanup, forget A: A's text is gone from the audit rows
// that B made, and B stays active and delivered.
func TestForgetClearsLaterAuditRows(t *testing.T) {
	s, out := newStore(t)
	ctx := context.Background()
	a := addItem(t, s, "", "alpha secret")
	mustBuild(t, s, out)
	bID := addItem(t, s, "", "use pnpm")
	bGen := mustBuild(t, s, out)
	mustRepair(t, s, out, later)
	if n := textAfter(t, s, 0, "alpha secret"); n == 0 {
		t.Fatal("no applied edit holds A's text before the forget")
	}

	// What the forget flow (E4-S6) does: clear the rows that held A, mark the generations that
	// hold A, and retire A.
	var ids []int64
	write(t, s, func(tx *sql.Tx) (err error) {
		ids, err = store.AppliedEditsWithItem(ctx, tx, a)
		return err
	})
	var fromB bool
	for _, e := range appliedEdits(t, s, bGen) {
		fromB = fromB || slices.Contains(ids, e.ID)
	}
	if !fromB {
		t.Errorf("AppliedEditsWithItem(%d) = %v, want the rows of generation %d too", a, ids, bGen)
	}
	write(t, s, func(tx *sql.Tx) error {
		for _, id := range ids {
			if _, err := tx.Exec("UPDATE applied_edits SET content_before = NULL, content_after = NULL WHERE id = ?",
				id); err != nil {
				return err
			}
		}
		return nil
	})
	markForget(t, s, bGen)
	retire(t, s, a, 1)
	mustRepair(t, s, out, later)
	mustRepair(t, s, out, later.Add(time.Second))

	if n := textAfter(t, s, 0, "alpha secret"); n != 0 {
		t.Errorf("%d applied edits hold A's text, want 0", n)
	}
	if mustHolds(t, out, a) {
		t.Errorf("Holds(%d) = true, want false (forget done)", a)
	}
	br := mustRead(t, out, "k1")
	want := []store.RevisionRef{{Item: bID, Revision: 1}}
	if !strings.Contains(br.Global, "use pnpm") || strings.Contains(br.Global, "alpha secret") ||
		!reflect.DeepEqual(br.Revisions, want) {
		t.Errorf("Read() = %q, revisions %v, want B only (%v)", br.Global, br.Revisions, want)
	}
}
