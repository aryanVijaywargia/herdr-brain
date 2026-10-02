package output

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

// skillText is a skill text with name and a marker in its steps.
func skillText(name, marker string) string {
	return "---\nname: " + name + "\ndescription: Do the " + name + " task.\nrequired-tools: none\n" +
		"example: \"Do it.\"\n---\n\n# Steps\n\n1. Say " + marker + ".\n"
}

// addSkill commits a new active skill item. key "" makes a global skill.
func addSkill(t *testing.T, s *store.Store, key, text string) int64 {
	t.Helper()
	r := store.ItemRevision{Text: text, Scope: store.ScopeGlobal, State: store.StateActive,
		Author: store.AuthorYou, Origin: store.OriginReview, Created: testNow}
	if key != "" {
		r.Scope, r.WorkspaceKey = store.ScopeWorkspace, &key
	}
	var id int64
	write(t, s, func(tx *sql.Tx) (err error) {
		id, _, err = store.CreateItem(context.Background(), tx,
			store.Item{ClaimID: text, Kind: store.KindSkill, Created: testNow}, r)
		return err
	})
	return id
}

func addWorkspace(t *testing.T, s *store.Store, key string) {
	t.Helper()
	write(t, s, func(tx *sql.Tx) error {
		return store.InsertWorkspace(context.Background(), tx, store.Workspace{Key: key, HerdrSession: "default",
			HerdrWorkspaceID: key, Label: "brain", Status: store.WorkspaceLive, LastSeen: testNow})
	})
}

// checkFile checks that path holds text.
func checkFile(t *testing.T, path, text string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil || string(b) != text {
		t.Errorf("ReadFile(%s) = %q, %v, want %q", path, b, err, text)
	}
}

func mustManifest(t *testing.T, out string, gen int64) Manifest {
	t.Helper()
	m, _, err := readManifest(filepath.Join(out, folder(gen)))
	if err != nil {
		t.Fatalf("readManifest(%d) = %v", gen, err)
	}
	return m
}

// AC S27 and §8.2: each scope part lists its own skills with the absolute path of the revision
// file; a second workspace on the same checkout lists none. The manifest lists each file with
// its path, hash and pair, and the parts name the skill revisions for the receipt.
func TestSkillList(t *testing.T) {
	s, out := newStore(t)
	addWorkspace(t, s, "k2")
	g := addSkill(t, s, "", skillText("release-notes", "m1"))
	w := addSkill(t, s, "k1", skillText("deploy", "m2"))
	fact := addItem(t, s, "k1", "use pnpm")
	gen := mustBuild(t, s, out)

	dir := skillsDir(out)
	gPath, wPath := skillPath(dir, g, 1), skillPath(dir, w, 1)
	if !filepath.IsAbs(gPath) || gPath != filepath.Join(filepath.Dir(out), "skills", "1", "r1", "SKILL.md") {
		t.Fatalf("skillPath(global) = %s, want <store>/skills/1/r1/SKILL.md", gPath)
	}
	checkFile(t, gPath, skillText("release-notes", "m1"))
	checkFile(t, wPath, skillText("deploy", "m2"))

	b := mustRead(t, out, "k1")
	if !strings.Contains(b.Global, "- release-notes: Do the release-notes task. File: "+gPath+"\n") {
		t.Errorf("global part = %q, want the release-notes entry", b.Global)
	}
	if !strings.Contains(b.Workspace, "- deploy: Do the deploy task. File: "+wPath+"\n") ||
		strings.Contains(b.Workspace, "[2]") {
		t.Errorf("workspace part = %q, want the deploy entry and no item line for it", b.Workspace)
	}
	wantRevs := []store.RevisionRef{{Item: g, Revision: 1}, {Item: fact, Revision: 1}, {Item: w, Revision: 1}}
	if !reflect.DeepEqual(b.Revisions, wantRevs) {
		t.Errorf("Read(k1) revisions = %v, want %v", b.Revisions, wantRevs)
	}
	other := mustRead(t, out, "k2")
	if strings.Contains(other.Workspace, "deploy") || strings.Contains(other.Workspace, "Skills") {
		t.Errorf("workspace k2 part = %q, want no skill of k1", other.Workspace)
	}

	m := mustManifest(t, out, gen)
	want := []SkillFile{
		{Path: gPath, SHA256: hash([]byte(skillText("release-notes", "m1"))), RevisionRef: store.RevisionRef{Item: g, Revision: 1}},
		{Path: wPath, SHA256: hash([]byte(skillText("deploy", "m2"))), RevisionRef: store.RevisionRef{Item: w, Revision: 1}},
	}
	if !reflect.DeepEqual(m.Skills, want) {
		t.Errorf("manifest skills = %+v, want %+v", m.Skills, want)
	}
}

// AC: the same skill name in the main store and in a test store gives two files, each in its
// own store.
func TestSkillTwoStores(t *testing.T) {
	var paths []string
	for range 2 {
		s, out := newStore(t)
		id := addSkill(t, s, "", skillText("deploy", "m"))
		mustBuild(t, s, out)
		p := skillPath(skillsDir(out), id, 1)
		checkFile(t, p, skillText("deploy", "m"))
		if !strings.HasPrefix(p, filepath.Dir(out)+string(filepath.Separator)) {
			t.Errorf("skill file %s is outside its store %s", p, filepath.Dir(out))
		}
		paths = append(paths, p)
	}
	if paths[0] == paths[1] {
		t.Errorf("both stores use %s, want two files", paths[0])
	}
}

// AC: a build that stops after the skill file write and before the pointer switch, and a
// stale build, leave only unlisted files: the next build reports no conflict, and the brief
// lists the right revision.
func TestSkillStoppedAndStaleBuild(t *testing.T) {
	s, out := newStore(t)
	id := addSkill(t, s, "k1", skillText("deploy", "r1"))
	mustBuild(t, s, out)
	mustRevise(t, s, id, 1, skillText("deploy", "r2"))
	r2 := skillPath(skillsDir(out), id, 2)

	stop := errors.New("stopped")
	b := builder{out: out, now: at(testNow), afterRender: func() {}, writeFile: func(path string, data []byte) error {
		if filepath.Base(path) == globalName {
			return stop
		}
		return writeFile(path, data)
	}}
	if _, err := b.build(context.Background(), s, nil); !errors.Is(err, stop) {
		t.Fatalf("build() = %v, want the injected stop", err)
	}
	checkFile(t, r2, skillText("deploy", "r2"))
	// An unlisted file that does not match is written again, not reported.
	if err := os.WriteFile(r2, []byte("half"), 0o600); err != nil {
		t.Fatal(err)
	}
	if cs := mustConflicts(t, out); len(cs) != 0 {
		t.Errorf("Conflicts() after the stop = %v, want none", cs)
	}

	stale := builder{out: out, now: at(testNow), writeFile: writeFile, afterRender: func() {
		mustRevise(t, s, id, 2, skillText("deploy", "r3"))
	}}
	if _, err := stale.build(context.Background(), s, nil); !errors.Is(err, ErrStale) {
		t.Fatalf("build() = %v, want ErrStale", err)
	}
	gen := mustBuild(t, s, out)
	if cs := mustConflicts(t, out); len(cs) != 0 {
		t.Errorf("Conflicts() after the stale build = %v, want none", cs)
	}
	checkFile(t, r2, skillText("deploy", "r2"))
	r3 := skillPath(skillsDir(out), id, 3)
	checkFile(t, r3, skillText("deploy", "r3"))
	br := mustRead(t, out, "k1")
	if br.Generation != gen || !strings.Contains(br.Workspace, "File: "+r3+"\n") ||
		!slices.Contains(br.Revisions, store.RevisionRef{Item: id, Revision: 3}) {
		t.Errorf("Read() = %d %q %v, want generation %d with revision 3", br.Generation, br.Workspace, br.Revisions, gen)
	}
}

// AC §5.5: a session that got revision 1 can still read its file after revision 2; its brief
// named revision 1. Cleanup keeps every revision file of an active skill.
func TestSkillOldRevisionStays(t *testing.T) {
	s, out := newStore(t)
	id := addSkill(t, s, "k1", skillText("deploy", "r1"))
	mustBuild(t, s, out)
	got := mustRead(t, out, "k1")
	r1 := skillPath(skillsDir(out), id, 1)
	if !strings.Contains(got.Workspace, r1) || !slices.Contains(got.Revisions, store.RevisionRef{Item: id, Revision: 1}) {
		t.Fatalf("Read() = %q %v, want revision 1", got.Workspace, got.Revisions)
	}
	mustRevise(t, s, id, 1, skillText("deploy", "r2"))
	mustBuild(t, s, out)
	mustRepair(t, s, out, later)
	checkFile(t, r1, skillText("deploy", "r1"))
	if b := mustRead(t, out, "k1"); !strings.Contains(b.Workspace, skillPath(skillsDir(out), id, 2)) {
		t.Errorf("Read() after revision 2 = %q, want the revision 2 file", b.Workspace)
	}
}

// AC S11: a hand-edited listed skill file stays and is a conflict; the hook still reads the
// brief (§8.3 step 5); Confirm writes the file again.
func TestSkillHandEdit(t *testing.T) {
	s, out := newStore(t)
	id := addSkill(t, s, "k1", skillText("deploy", "r1"))
	gen := mustBuild(t, s, out)
	path := skillPath(skillsDir(out), id, 1)
	if err := os.WriteFile(path, []byte("mine"), 0o600); err != nil {
		t.Fatal(err)
	}
	addItem(t, s, "k1", "use pnpm")
	_, err := Build(context.Background(), s, out, nil, at(testNow))
	wantConflict(t, err, gen, path)
	checkFile(t, path, "mine")
	if b := mustRead(t, out, "k1"); b.Generation != gen {
		t.Errorf("Read() generation = %d, want %d: a skill edit does not withhold", b.Generation, gen)
	}
	want := []ConflictError{{Generation: gen, Files: []string{path}}}
	if cs := mustConflicts(t, out); !reflect.DeepEqual(cs, want) {
		t.Errorf("Conflicts() = %v, want %v", cs, want)
	}

	fakeSleep(t, func() {})
	if cs := mustConfirm(t, s, out, testNow); !reflect.DeepEqual(cs, want) {
		t.Errorf("Confirm() = %v, want %v", cs, want)
	}
	checkFile(t, path, skillText("deploy", "r1"))
	if cs := mustConflicts(t, out); len(cs) != 0 {
		t.Errorf("Conflicts() after Confirm = %v, want none", cs)
	}
	if b := mustRead(t, out, "k1"); !strings.Contains(b.Workspace, "use pnpm") {
		t.Errorf("Read() after Confirm = %q, want the new item", b.Workspace)
	}
}

// AC §7.5 step 8: the files of a retired skill go when no current generation lists them and
// the pointer moved more than one hook cap ago; Holds counts them until then.
func TestSkillRetireCleanup(t *testing.T) {
	s, out := newStore(t)
	id := addSkill(t, s, "k1", skillText("deploy", "r1"))
	mustBuild(t, s, out)
	mustRevise(t, s, id, 1, skillText("deploy", "r2"))
	mustBuild(t, s, out)
	retire(t, s, id, 2)
	mustBuild(t, s, out)
	mustRepair(t, s, out, testNow)
	dir := filepath.Join(skillsDir(out), "1")
	if !exists(t, dir) || !mustHolds(t, out, id) {
		t.Errorf("skill files within one hook cap: exists %v, holds %v, want both", exists(t, dir), mustHolds(t, out, id))
	}
	mustRepair(t, s, out, later)
	if exists(t, dir) || mustHolds(t, out, id) {
		t.Errorf("skill files after one hook cap: exists %v, holds %v, want neither", exists(t, dir), mustHolds(t, out, id))
	}
}

// AC: a retire while a listed file is hand-edited keeps the file and the conflict, so a
// forget stays pending; Confirm builds without the skill, and cleanup then removes the file.
func TestSkillConflictKeepsFiles(t *testing.T) {
	s, out := newStore(t)
	id := addSkill(t, s, "k1", skillText("deploy", "r1"))
	gen := mustBuild(t, s, out)
	path := skillPath(skillsDir(out), id, 1)
	if err := os.WriteFile(path, []byte("mine"), 0o600); err != nil {
		t.Fatal(err)
	}
	retire(t, s, id, 1)
	_, err := Build(context.Background(), s, out, nil, at(testNow))
	wantConflict(t, err, gen, path)
	wantConflict(t, Repair(context.Background(), s, out, at(later)), gen, path)
	checkFile(t, path, "mine")
	if !mustHolds(t, out, id) {
		t.Error("Holds() with the kept conflict file = false, want true")
	}
	fakeSleep(t, func() {})
	mustConfirm(t, s, out, testNow)
	mustRepair(t, s, out, later)
	if exists(t, filepath.Join(skillsDir(out), "1")) || mustHolds(t, out, id) {
		t.Error("skill files after Confirm and cleanup are still there, want them gone")
	}
}

// Review fix: a hand edit of a listed skill file after the first source check of a build
// stays, as a conflict; the build does not write over it (S11).
func TestSkillEditDuringBuild(t *testing.T) {
	s, out := newStore(t)
	id := addSkill(t, s, "k1", skillText("deploy", "r1"))
	gen := mustBuild(t, s, out)
	path := skillPath(skillsDir(out), id, 1)
	addItem(t, s, "k1", "use pnpm")
	b := builder{out: out, now: at(testNow), writeFile: writeFile, afterRender: func() {
		if err := os.WriteFile(path, []byte("mine"), 0o600); err != nil {
			t.Fatal(err)
		}
	}}
	_, err := b.build(context.Background(), s, nil)
	wantConflict(t, err, gen, path)
	checkFile(t, path, "mine")
	checkPointer(t, out, gen)
}

// Review fix: Confirm writes a confirmed older revision again, not only the newest one, so a
// session that got revision 1 reads the approved text.
func TestSkillConfirmOldRevision(t *testing.T) {
	s, out := newStore(t)
	id := addSkill(t, s, "k1", skillText("deploy", "r1"))
	mustBuild(t, s, out)
	r1 := skillPath(skillsDir(out), id, 1)
	if err := os.WriteFile(r1, []byte("mine"), 0o600); err != nil {
		t.Fatal(err)
	}
	mustRevise(t, s, id, 1, skillText("deploy", "r2"))
	if _, err := Build(context.Background(), s, out, nil, at(testNow)); err == nil {
		t.Fatal("Build() = nil, want the conflict on revision 1")
	}
	fakeSleep(t, func() {})
	mustConfirm(t, s, out, testNow)
	checkFile(t, r1, skillText("deploy", "r1"))
	checkFile(t, skillPath(skillsDir(out), id, 2), skillText("deploy", "r2"))
	if cs := mustConflicts(t, out); len(cs) != 0 {
		t.Errorf("Conflicts() after Confirm = %v, want none", cs)
	}
}

// AC E16-S6 (S72): a disputed skill and a disputed item stay delivered with the mark: the
// skill keeps its entry in the skill list, its file and its manifest pair. A dispute writes no
// revision, and the build that follows it marks them.
func TestDisputedSkill(t *testing.T) {
	s, out := newStore(t)
	w := addSkill(t, s, "k1", skillText("deploy", "m2"))
	fact := addItem(t, s, "k1", "use pnpm, see package.json")
	mustBuild(t, s, out)
	write(t, s, func(tx *sql.Tx) error {
		for _, id := range []int64{w, fact} {
			base := int64(1)
			p := store.Proposal{ItemID: &id, BaseRevision: &base, Text: "t", Scope: store.ScopeWorkspace, Target: "k1",
				Diff: "- package.json abc\n+ (deleted)", Dispute: []string{"package.json"}, Status: store.ProposalPending, Created: testNow}
			if pid, err := store.Dispute(context.Background(), tx, p); err != nil || pid == 0 {
				return errors.Join(err, errors.New("no dispute"))
			}
		}
		return nil
	})
	gen := mustBuild(t, s, out)
	b := mustRead(t, out, "k1")
	wPath := skillPath(skillsDir(out), w, 1)
	mark := " (disputed: package.json changed on " + testNow.UTC().Format("2006-01-02") + ")\n"
	if !strings.Contains(b.Workspace, "- deploy: Do the deploy task. File: "+wPath+mark) ||
		!strings.Contains(b.Workspace, "use pnpm, see package.json"+mark) {
		t.Errorf("workspace part = %q, want the skill entry and the item, each with the mark", b.Workspace)
	}
	if want := []store.RevisionRef{{Item: fact, Revision: 1}, {Item: w, Revision: 1}}; !reflect.DeepEqual(b.Revisions, want) {
		t.Errorf("revisions = %v, want %v", b.Revisions, want)
	}
	if m := mustManifest(t, out, gen); len(m.Skills) != 1 || m.Skills[0].Path != wPath {
		t.Errorf("manifest skills = %+v, want the deploy file", m.Skills)
	}
	checkFile(t, wPath, skillText("deploy", "m2"))
}
