package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/aryanVijaywargia/herdr-brain/internal/herdr"
	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

// releaseFixture holds, in the fixture's store, the results of a "release" recall: a global
// fact, an approved skill and tool (revision 2: the draft is revision 1) of workspace w14 (ws_a14), and a global skill and tool, each
// promoted from w14. It also holds what recall never shows: a pending skill draft, a retired
// tool and a skill of workspace w15 (ws_a15).
type releaseFixture struct {
	*fixture
	fact, skill, tool, globalSkill, globalTool, draft, retired int64
}

func newReleaseFixture(t *testing.T) *releaseFixture {
	t.Helper()
	f := &releaseFixture{fixture: newFixture(t)}
	f.panes["w15:p1"] = herdr.Pane{ID: "w15:p1", WorkspaceID: "w15", TabID: "w15:t1"}
	f.fact = recallItem(t, f.fixture, "", "Tag a release after the changelog", store.StateActive, "")
	f.skill = f.approvedSkill(t, skillText("release", "R1"))
	f.tool = f.approvedTool(t, namedTool("release-check"))
	f.globalSkill = f.approvedSkill(t, skillText("release-notes", "R2"))
	f.changeSkill(t, f.globalSkill, "--scope", "global")
	f.globalTool = f.approvedTool(t, namedTool("release-tag"))
	f.changeSkill(t, f.globalTool, "--scope", "global")
	f.retired = f.approvedTool(t, namedTool("release-old"))
	f.changeSkill(t, f.retired, "--retire")
	p := f.skillDraft(t, skillText("release-draft", "R3"))
	f.draft = *f.rows(t).proposal(t, p).ItemID
	return f
}

// canon replaces the temp folder of the fixture's store in out with <tmp>.
func (f *releaseFixture) canon(t *testing.T, out string) string {
	t.Helper()
	dir := filepath.Dir(f.store)
	canon, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	return strings.ReplaceAll(strings.ReplaceAll(out, canon, "<tmp>"), dir, "<tmp>")
}

// recallGolden compares out with testdata/recall/<name>.txt; -update rewrites the file.
func recallGolden(t *testing.T, name, out string) {
	t.Helper()
	path := filepath.Join("testdata", "recall", name+".txt")
	if *update {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(out), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if out != string(want) {
		t.Errorf("recall output =\n%s\nwant (%s)\n%s", out, path, want)
	}
}

// #344 AC 1, 3 and 6: brain recall returns the matching skills and tools that the brief of the
// pane can deliver, global and of the pane's workspace, as the brief "Skills" and "Tools" parts
// show them: a skill with its description and the path of its SKILL.md, a tool with how to run
// it. A draft, a retired tool and a skill or tool of another workspace never show.
func TestRecallSkillsAndTools(t *testing.T) {
	f := newReleaseFixture(t)
	out, _, err := f.recall(t, "w14:p1", "w14", "release")
	if err != nil {
		t.Fatal(err)
	}
	recallGolden(t, "skills-and-tools", f.canon(t, out))
	for _, want := range []string{
		"- release: Do the release task. File: " + f.skillFile(f.skill, 2) + "\n",
		"- release-notes: Do the release-notes task. File: " + f.skillFile(f.globalSkill, 3) + "\n",
		"Run a tool with: brain run <name>",
		"- release-check: Tool release-check Inputs: package\n",
		"- release-tag: Tool release-tag Inputs: package\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("Recall(release) =\n%s\nwant %q", out, want)
		}
	}
	for _, not := range []string{"release-draft", "release-old"} {
		if strings.Contains(out, not) {
			t.Errorf("Recall(release) =\n%s\nwant no %q", out, not)
		}
	}

	// Another workspace and an unmapped one get the global skills and tools only.
	for _, pane := range []string{"w15", "w16"} {
		out, _, err := f.recall(t, pane+":p1", pane, "release")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out, "- release-notes: ") || !strings.Contains(out, "- release-tag: ") ||
			strings.Contains(out, "- release: ") || strings.Contains(out, "- release-check: ") {
			t.Errorf("Recall(release) in %s =\n%s\nwant the global skill and tool only", pane, out)
		}
	}
}

// #344 AC 2: a skill or a tool matches by the words of its name and its description, with the
// rule for items. Items, skills and tools have one order and one cap of 10: a tool with more
// points comes in before the items, and pushes the last item out.
func TestRecallOneOrderAndCap(t *testing.T) {
	f := newFixture(t)
	var items []int64
	for i := range maxRecall {
		items = append(items, recallItem(t, f, "ws_a14", fmt.Sprintf("deploy step %d", i), store.StateActive, ""))
	}
	tool := f.approvedTool(t, namedTool("deploy-check"))
	f.approvedSkill(t, skillText("gardening", "G1"))
	out, _, err := f.recall(t, "w14:p1", "w14", "deploy", "check")
	if err != nil {
		t.Fatal(err)
	}
	want := make([]string, 0, maxRecall-1)
	for _, id := range items[:maxRecall-1] {
		want = append(want, fmt.Sprint(id))
	}
	if got := printed(out); !slices.Equal(got, want) || !strings.Contains(out, "- deploy-check: ") ||
		strings.Contains(out, "gardening") {
		t.Errorf("Recall(deploy check) =\n%s\nwant the tool %d and the first %d items", out, tool, maxRecall-1)
	}
	// The skill matches by its name and by its description.
	for _, w := range []string{"garden", "task"} {
		if out, _, err := f.recall(t, "w14:p1", "w14", w); err != nil || !strings.Contains(out, "- gardening: ") {
			t.Errorf("Recall(%s) = %q, %v, want the skill", w, out, err)
		}
	}
}

// #344 AC 4: the recalls row names the printed skill and tool revisions, as it does for items,
// so outcomes count them as delivered.
func TestRecallRecordSkillsAndTools(t *testing.T) {
	f := newReleaseFixture(t)
	s := openStore(t, f.store)
	var receipt int64
	write(t, s, func(tx *sql.Tx) (err error) {
		session, agent := "default", "a"
		receipt, err = store.InsertDelivery(context.Background(), tx, store.Delivery{Profile: "claude", HerdrSession: &session,
			PaneID: "w14:p1", AgentSessionID: &agent, ItemRevisions: []store.RevisionRef{}, Result: store.ResultEmitted, At: testNow})
		return err
	})
	closeStore(t, s)
	if _, warn, err := f.recall(t, "w14:p1", "w14", "release"); err != nil || warn != "" {
		t.Fatalf("Recall() = %q, %v, want a recorded recall", warn, err)
	}
	refs, _ := json.Marshal([]store.RevisionRef{{Item: f.fact, Revision: 1}, {Item: f.skill, Revision: 2},
		{Item: f.tool, Revision: 2}, {Item: f.globalSkill, Revision: 3}, {Item: f.globalTool, Revision: 3}})
	if got, want := f.recalls(t), []string{fmt.Sprintf("%d: %s", receipt, refs)}; !slices.Equal(got, want) {
		t.Errorf("recalls = %v, want %v", got, want)
	}
}

// #344 AC 5: the recall popup lists the skills and the tools too, with their scope, in the
// order of brain recall, and Enter shows one as the brief shows it.
func TestRecallPopupSkillsAndTools(t *testing.T) {
	f := newReleaseFixture(t)
	screen, err := f.popup(t, f.popupEnviron("w14:p1", "w14"), "release", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		fmt.Sprintf("[%d] global: Tag a release after the changelog", f.fact),
		fmt.Sprintf("[%d] workspace w14 (shop): skill release: Do the release task.", f.skill),
		fmt.Sprintf("[%d] workspace w14 (shop): tool release-check: Tool release-check", f.tool),
		fmt.Sprintf("[%d] global: skill release-notes: Do the release-notes task.", f.globalSkill),
		fmt.Sprintf("[%d] global: tool release-tag: Tool release-tag", f.globalTool),
	} {
		if !strings.Contains(screen, want) {
			t.Errorf("popup screen =\n%s\nwant %q", screen, want)
		}
	}
	want := []string{fmt.Sprint(f.fact), fmt.Sprint(f.skill), fmt.Sprint(f.tool), fmt.Sprint(f.globalSkill), fmt.Sprint(f.globalTool)}
	if got := listed(screen); !slices.Equal(got, want) {
		t.Errorf("popup lists %v, want %v", got, want)
	}

	// Down to the skill, Enter: the view shows it as the brief shows it.
	view, err := f.popup(t, f.popupEnviron("w14:p1", "w14"), "release\x1b[B\r", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{fmt.Sprintf("item %d (skill, active), revision 2, workspace w14 (shop)", f.skill),
		"### Skills\n", "- release: Do the release task. File: " + f.skillFile(f.skill, 2) + "\n"} {
		if !strings.Contains(view, w) {
			t.Errorf("popup view =\n%s\nwant %q", view, w)
		}
	}
	// Down twice to the tool, Enter: the view shows how to run it.
	view, err = f.popup(t, f.popupEnviron("w14:p1", "w14"), "release\x1b[B\x1b[B\r", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{fmt.Sprintf("item %d (tool, active), revision 2, workspace w14 (shop)", f.tool),
		"### Tools\nRun a tool with: brain run <name>", "- release-check: Tool release-check Inputs: package\n"} {
		if !strings.Contains(view, w) {
			t.Errorf("popup view =\n%s\nwant %q", view, w)
		}
	}
}

// #344 AC 1: an automation never shows in a recall: an agent does not call it. Its tool shows.
func TestRecallNoAutomation(t *testing.T) {
	f := newAutomationFixture(t)
	toolID, id := f.bound(t)
	out, _, err := f.recall(t, "w14:p1", "w14", "touch", "ran", "write", "file", "claude", "automation")
	if err != nil || !strings.Contains(out, "- touch-ran: Write a file\n") {
		t.Errorf("Recall() = %q, %v, want the tool", out, err)
	}
	screen, err := f.popup(t, f.popupEnviron("w14:p1", "w14"), "touch ran write file claude automation", 0, 0)
	if got, want := listed(screen), []string{fmt.Sprint(toolID)}; err != nil || !slices.Equal(got, want) {
		t.Errorf("popup lists %v, %v, want only the tool %v, not the automation %d", got, err, want, id)
	}
}
