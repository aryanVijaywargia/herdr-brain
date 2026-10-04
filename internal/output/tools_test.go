package output

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aryanVijaywargia/herdr-brain/internal/store"
	"github.com/aryanVijaywargia/herdr-brain/internal/tool"
)

// toolToml is a tool.toml whose only step runs its run.sh, with marker in its description.
func toolToml(marker string) string {
	return "name = \"test-package\"\ndescription = \"Run the tests " + marker + "\"\ncwd = \"~/src/shop\"\n" +
		"timeout_s = 600\neffects = \"runs tests\"\nread_only = false\nargv = [\"run.sh\", \"{package}\"]\n\n" +
		"[inputs.package]\ntype = \"string\"\npattern = \"^[a-z]+$\"\n"
}

// toolText is the tool text of toolToml(marker) with its run.sh.
func toolText(marker string) string {
	return toolToml(marker) + "--- run.sh ---\npnpm test --filter \"$1\" | tail -n 20\n"
}

// addTool commits a new active tool item in workspace k1.
func addTool(t *testing.T, s *store.Store, text string) int64 {
	t.Helper()
	return addToolIn(t, s, "k1", store.StateActive, text)
}

// addToolIn commits a new tool item in state in workspace key; "" makes a global tool.
func addToolIn(t *testing.T, s *store.Store, key string, state store.State, text string) int64 {
	t.Helper()
	r := store.ItemRevision{Text: text, Scope: store.ScopeGlobal, State: state,
		Author: store.AuthorYou, Origin: store.OriginReview, Created: testNow}
	if key != "" {
		r.Scope, r.WorkspaceKey = store.ScopeWorkspace, &key
	}
	var id int64
	write(t, s, func(tx *sql.Tx) (err error) {
		id, _, err = store.CreateItem(context.Background(), tx,
			store.Item{ClaimID: text, Kind: store.KindTool, Created: testNow}, r)
		return err
	})
	return id
}

func mustMayRun(t *testing.T, s *store.Store, out string, id int64) store.Tool {
	t.Helper()
	tl, err := MayRun(context.Background(), s, out, id)
	if err != nil {
		t.Fatalf("MayRun(%d) = %v", id, err)
	}
	return tl
}

func wantMayNotRun(t *testing.T, s *store.Store, out string, id int64, want string) {
	t.Helper()
	_, err := MayRun(context.Background(), s, out, id)
	if !errors.Is(err, ErrMayNotRun) || !strings.Contains(err.Error(), want) {
		t.Errorf("MayRun(%d) = %v, want ErrMayNotRun with %q", id, err, want)
	}
}

// The build writes each tool revision as its files before the generation, and the manifest
// lists each file with its path, hash and pair. A tool is no brief item: only the catalog
// line names it (E17-S5). The approved tool may run.
func TestToolFiles(t *testing.T) {
	s, out := newStore(t)
	id := addTool(t, s, toolText("m1"))
	gen := mustBuild(t, s, out)
	dir := toolsDir(out)
	if want := filepath.Join(filepath.Dir(out), "tools", "1", "r1", "tool.toml"); revisionPath(dir, id, 1, tool.TomlName) != want {
		t.Fatalf("tool.toml path = %s, want %s", revisionPath(dir, id, 1, tool.TomlName), want)
	}
	tomlPath, scriptPath := revisionPath(dir, id, 1, tool.TomlName), revisionPath(dir, id, 1, tool.ScriptName)
	checkFile(t, tomlPath, toolToml("m1"))
	checkFile(t, scriptPath, "pnpm test --filter \"$1\" | tail -n 20\n")
	ref := store.RevisionRef{Item: id, Revision: 1}
	want := []RevisionFile{
		{Path: scriptPath, SHA256: hash([]byte("pnpm test --filter \"$1\" | tail -n 20\n")), RevisionRef: ref},
		{Path: tomlPath, SHA256: hash([]byte(toolToml("m1"))), RevisionRef: ref},
	}
	if m := mustManifest(t, out, gen); !reflect.DeepEqual(m.Tools, want) {
		t.Errorf("manifest tools = %+v, want %+v", m.Tools, want)
	}
	if b := mustRead(t, out, "k1"); !strings.HasSuffix(b.Workspace, "\n- test-package: Run the tests m1 Inputs: package\n") ||
		len(b.Revisions) != 0 {
		t.Errorf("brief = %q, %v, want the catalog line and no tool revision", b.Workspace, b.Revisions)
	}
	tl := mustMayRun(t, s, out, id)
	if tl.ContractHash != tool.Hash(toolText("m1"), "workspace", "k1") || *tl.ApprovedHash != tl.ContractHash {
		t.Errorf("MayRun() = %+v, want the approved contract hash", tl)
	}
}

// A tool file that no generation lists yet is no conflict: a stopped build leaves it, and the
// next build writes it again (§7.5 step 5).
func TestToolUnlistedFile(t *testing.T) {
	s, out := newStore(t)
	id := addTool(t, s, toolText("m1"))
	path := revisionPath(toolsDir(out), id, 1, tool.TomlName)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("half"), 0o600); err != nil {
		t.Fatal(err)
	}
	wantMayNotRun(t, s, out, id, "run.sh is missing")
	mustBuild(t, s, out)
	checkFile(t, path, toolToml("m1"))
	mustMayRun(t, s, out, id)
}

// AC S11: a hand-edited tool.toml is a conflict, and the tool does not run. It stays until you
// confirm; then repair writes it again, and the tool runs.
func TestToolHandEdit(t *testing.T) {
	s, out := newStore(t)
	id := addTool(t, s, toolText("m1"))
	gen := mustBuild(t, s, out)
	path := revisionPath(toolsDir(out), id, 1, tool.TomlName)
	if err := os.WriteFile(path, []byte(strings.Replace(toolToml("m1"), "600", "6000", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	wantMayNotRun(t, s, out, id, "changed outside Brain")
	want := []ConflictError{{Generation: gen, Files: []string{path}}}
	if cs := mustConflicts(t, out); !reflect.DeepEqual(cs, want) {
		t.Errorf("Conflicts() = %v, want %v", cs, want)
	}
	addItem(t, s, "k1", "use pnpm")
	_, err := Build(context.Background(), s, out, nil, at(testNow))
	wantConflict(t, err, gen, path)

	fakeSleep(t, func() {})
	mustConfirm(t, s, out, testNow)
	checkFile(t, path, toolToml("m1"))
	mustMayRun(t, s, out, id)

	// A deleted run.sh is a conflict too.
	script := revisionPath(toolsDir(out), id, 1, tool.ScriptName)
	if err := os.Remove(script); err != nil {
		t.Fatal(err)
	}
	wantMayNotRun(t, s, out, id, "missing")
}

// AC S21: a contract change that you did not approve stops the tool. A draft revision of the
// tool has no approval, and a tools row whose contract hash is not the approved one, or not
// the hash of the current revision, does not run. An approved change runs with its new hash.
func TestToolApproval(t *testing.T) {
	s, out := newStore(t)
	id := addTool(t, s, toolText("m1"))
	mustBuild(t, s, out)
	old := mustMayRun(t, s, out, id).ContractHash

	ctx := context.Background()
	write(t, s, func(tx *sql.Tx) error {
		r, err := store.ItemRevisionByKey(ctx, tx, id, 1)
		if err != nil {
			return err
		}
		r.Text, r.State = strings.Replace(toolText("m1"), "timeout_s = 600", "timeout_s = 900", 1), store.StateDraft
		_, err = store.WriteRevision(ctx, tx, 1, r)
		return err
	})
	mustBuild(t, s, out)
	wantMayNotRun(t, s, out, id, "draft")

	next := strings.Replace(toolText("m1"), "timeout_s = 600", "timeout_s = 900", 1)
	write(t, s, func(tx *sql.Tx) error {
		r, err := store.ItemRevisionByKey(ctx, tx, id, 2)
		if err != nil {
			return err
		}
		r.Text, r.State = next, store.StateActive
		_, err = store.WriteRevision(ctx, tx, 2, r)
		return err
	})
	mustBuild(t, s, out)
	tl := mustMayRun(t, s, out, id)
	if tl.ContractHash == old || tl.TimeoutS != 900 {
		t.Errorf("MayRun() after the approval = %+v, want the new contract", tl)
	}

	for name, q := range map[string]string{
		"approval of another hash": `UPDATE tools SET approved_hash = 'other' WHERE id = ?`,
		"row of another contract":  `UPDATE tools SET approved_hash = 'other', contract_hash = 'other' WHERE id = ?`,
	} {
		t.Run(name, func(t *testing.T) {
			var was store.Tool
			write(t, s, func(tx *sql.Tx) (err error) {
				if was, err = store.ToolByID(ctx, tx, id); err != nil {
					return err
				}
				_, err = tx.Exec(q, id)
				return err
			})
			wantMayNotRun(t, s, out, id, "not the one that you approved")
			write(t, s, func(tx *sql.Tx) error {
				_, err := tx.Exec(`UPDATE tools SET approved_hash = ?, contract_hash = ? WHERE id = ?`,
					*was.ApprovedHash, was.ContractHash, id)
				return err
			})
		})
	}
	mustMayRun(t, s, out, id)

	if _, err := MayRun(ctx, s, out, addItem(t, s, "k1", "use pnpm")); !errors.Is(err, ErrMayNotRun) {
		t.Errorf("MayRun(fact) = %v, want ErrMayNotRun", err)
	}
}

// §7.5 step 8: all revision files of an active tool stay; the files of a retired tool go when
// no current generation lists them and the pointer moved more than one hook cap ago. Holds
// counts them until then, so forget stays pending.
func TestToolRetireCleanup(t *testing.T) {
	s, out := newStore(t)
	id := addTool(t, s, toolText("r1"))
	mustBuild(t, s, out)
	mustRevise(t, s, id, 1, toolText("r2"))
	mustBuild(t, s, out)
	mustRepair(t, s, out, later)
	r1 := revisionPath(toolsDir(out), id, 1, tool.TomlName)
	checkFile(t, r1, toolToml("r1"))
	checkFile(t, revisionPath(toolsDir(out), id, 2, tool.TomlName), toolToml("r2"))

	retire(t, s, id, 2)
	mustBuild(t, s, out)
	wantMayNotRun(t, s, out, id, "retired")
	mustRepair(t, s, out, testNow)
	dir := filepath.Join(toolsDir(out), "1")
	if !exists(t, dir) || !mustHolds(t, out, id) {
		t.Errorf("tool files within one hook cap: exists %v, holds %v, want both", exists(t, dir), mustHolds(t, out, id))
	}
	mustRepair(t, s, out, later)
	if exists(t, dir) || mustHolds(t, out, id) {
		t.Errorf("tool files after one hook cap: exists %v, holds %v, want neither", exists(t, dir), mustHolds(t, out, id))
	}
}

// namedTool is toolText(name) with the tool name name.
func namedTool(name string) string {
	return strings.Replace(toolText(name), `name = "test-package"`, `name = "`+name+`"`, 1)
}

// AC E17-S5: with 12 approved tools that workspace k1 sees, its catalog lists 10, one line
// each, by approval time. A tool of another workspace, a draft and a stopped tool are not in it.
func TestCatalog(t *testing.T) {
	s, out := newStore(t)
	addWorkspace(t, s, "k2")
	var ids []int64
	for i := range 11 {
		ids = append(ids, addTool(t, s, namedTool(fmt.Sprintf("tool-%d", i+1))))
	}
	ids = append(ids, addToolIn(t, s, "", store.StateActive, namedTool("global-tool")))
	addToolIn(t, s, "k2", store.StateActive, namedTool("other-workspace"))
	addToolIn(t, s, "k1", store.StateDraft, namedTool("draft-tool"))
	stopped := addTool(t, s, namedTool("stopped-tool"))
	ctx := context.Background()
	write(t, s, func(tx *sql.Tx) error {
		// A new approval of tool-1 moves it to the end of the approval order.
		_, err := store.WriteRevision(ctx, tx, 1, store.ItemRevision{ItemID: ids[0], Text: namedTool("tool-1"),
			Scope: store.ScopeWorkspace, WorkspaceKey: ptr("k1"), State: store.StateActive, Author: store.AuthorYou,
			Origin: store.OriginReview, Created: testNow.Add(time.Hour)})
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE tools SET approved_hash = 'other' WHERE id = ?`, stopped)
		return err
	})
	mustBuild(t, s, out)
	_, catalog, _ := strings.Cut(mustRead(t, out, "k1").Workspace, "### Tools\n")
	lines := strings.Split(strings.TrimSuffix(catalog, "\n"), "\n")[1:]
	var want []string
	for i := 2; i <= 11; i++ {
		want = append(want, fmt.Sprintf("- tool-%d: Run the tests tool-%d Inputs: package", i, i))
	}
	if !slices.Equal(lines, want) {
		t.Errorf("catalog of k1 =\n%s\nwant\n%s", strings.Join(lines, "\n"), strings.Join(want, "\n"))
	}
	if w := mustRead(t, out, "k2").Workspace; !strings.Contains(w, "- global-tool:") || !strings.Contains(w, "- other-workspace:") ||
		strings.Contains(w, "tool-1") {
		t.Errorf("k2 workspace part = %q, want only global-tool and other-workspace", w)
	}
}

// AC E17-S5: a skill entry says so when a Brain tool that it requires is not approved or is
// stopped (§8.2). A required tool that is no Brain tool (gh) gets no note.
func TestSkillStoppedTool(t *testing.T) {
	s, out := newStore(t)
	run := addTool(t, s, namedTool("test-package"))
	addToolIn(t, s, "k1", store.StateDraft, namedTool("lint"))
	addSkill(t, s, "k1", "---\nname: release\ndescription: Cut a release.\nrequired-tools: test-package, gh, lint\n"+
		"example: \"Cut it.\"\n---\n\n1. Run the tests.\n")
	mustBuild(t, s, out)
	if w := mustRead(t, out, "k1").Workspace; !strings.Contains(w, "SKILL.md (Brain tool not approved or stopped: lint)\n") {
		t.Errorf("workspace part = %q, want the note on lint", w)
	}
	// A contract change that you did not approve yet stops test-package (S21).
	ctx := context.Background()
	write(t, s, func(tx *sql.Tx) error {
		r, err := store.ItemRevisionByKey(ctx, tx, run, 1)
		if err != nil {
			return err
		}
		r.Text, r.State = strings.Replace(r.Text, "timeout_s = 600", "timeout_s = 900", 1), store.StateDraft
		_, err = store.WriteRevision(ctx, tx, 1, r)
		return err
	})
	mustBuild(t, s, out)
	if w := mustRead(t, out, "k1").Workspace; !strings.Contains(w, "SKILL.md (Brain tool not approved or stopped: test-package, lint)\n") {
		t.Errorf("workspace part = %q, want the note on test-package and lint", w)
	}
}

// A skill's required tool resolves as brain run finds it: a stopped workspace tool hides an
// approved global tool of the same name, so the entry has the note (Sol review, PR #233).
func TestSkillRequiredToolPrecedence(t *testing.T) {
	s, out := newStore(t)
	addToolIn(t, s, "", store.StateActive, namedTool("lint"))
	addToolIn(t, s, "k1", store.StateDraft, namedTool("lint"))
	addSkill(t, s, "k1", "---\nname: check\ndescription: Check the code.\nrequired-tools: lint\n"+
		"example: \"Check it.\"\n---\n\n1. Run lint.\n")
	addSkill(t, s, "", "---\nname: check-all\ndescription: Check all code.\nrequired-tools: lint\n"+
		"example: \"Check it.\"\n---\n\n1. Run lint.\n")
	mustBuild(t, s, out)
	b := mustRead(t, out, "k1")
	if !strings.Contains(b.Workspace, "SKILL.md (Brain tool not approved or stopped: lint)\n") {
		t.Errorf("workspace part = %q, want the note on lint", b.Workspace)
	}
	if strings.Contains(b.Global, "not approved or stopped") {
		t.Errorf("global part = %q, want no note: the global lint is approved", b.Global)
	}
}
