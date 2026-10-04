package cli

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aryanVijaywargia/herdr-brain/internal/output"
	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

// toolText is a tool contract with name, two steps and the input package.
func toolText(name string) string {
	return "name = \"" + name + "\"\ndescription = \"Run the tests of one package\"\ncwd = \"~/src/shop\"\n" +
		"timeout_s = 600\neffects = \"runs tests; writes nothing outside the repo\"\nread_only = true\n" +
		"steps = [[\"pnpm\", \"install\"], [\"pnpm\", \"test\", \"--filter\", \"{package}\"]]\n\n" +
		"[inputs.package]\ntype = \"string\"\npattern = \"^[a-z0-9@/._-]{1,64}$\"\n"
}

// toolDraft saves a draft tool in workspace w14 (ws_a14) with one pending proposal, as E17-S6
// will, and returns the proposal id.
func (f *fixture) toolDraft(t *testing.T, text string) int64 {
	t.Helper()
	s := openStore(t, f.store)
	defer closeStore(t, s)
	key := "ws_a14"
	pid, err := saveDraft(context.Background(), s, draft{text: text, kind: store.KindTool, scope: store.ScopeWorkspace,
		workspaceKey: &key, target: "workspace w14 (shop)"}, testNow)
	if err != nil {
		t.Fatalf("saveDraft() = %v", err)
	}
	return pid
}

// approvedTool drafts text and approves it; it returns the item id.
func (f *fixture) approvedTool(t *testing.T, text string) int64 {
	t.Helper()
	p := f.toolDraft(t, text)
	f.mustReview(t, fmt.Sprint(p), "--approve")
	return *f.rows(t).proposal(t, p).ItemID
}

func (f *fixture) toolFile(id, rev int64) string {
	return filepath.Join(f.store, "tools", fmt.Sprint(id), fmt.Sprintf("r%d", rev), "tool.toml")
}

// mayRun runs output.MayRun on tool id in the fixture's store.
func (f *fixture) mayRun(t *testing.T, id int64) (store.Tool, error) {
	t.Helper()
	s := openStore(t, f.store)
	defer closeStore(t, s)
	return output.MayRun(context.Background(), s, filepath.Join(f.store, outDir), id)
}

func (f *fixture) mustMayRun(t *testing.T, id int64) store.Tool {
	t.Helper()
	tl, err := f.mayRun(t, id)
	if err != nil {
		t.Fatalf("MayRun(%d) = %v", id, err)
	}
	return tl
}

// The review shows the whole contract: the command bytes, the input schema, the scope, the
// cwd, the timeout, the stated effects and the restriction (§10.1). Approve writes the tool
// files, and the tool may run.
func TestReviewShowsToolContract(t *testing.T) {
	f := newFixture(t)
	p := f.toolDraft(t, toolText("test-package"))
	if out := f.mustReview(t); !strings.Contains(out, "tool test-package: Run the tests of one package") {
		t.Errorf("Review() list = %q, want the tool line", out)
	}
	out := f.mustReview(t, fmt.Sprint(p))
	for _, want := range []string{
		"kind:     tool\n",
		"  name = \"test-package\"\n",
		"  steps = [[\"pnpm\", \"install\"], [\"pnpm\", \"test\", \"--filter\", \"{package}\"]]\n",
		"contract:\n",
		"  step 1:    \"pnpm\" \"install\"\n",
		"  step 2:    \"pnpm\" \"test\" \"--filter\" \"{package}\"\n",
		"  input:     package: string, pattern ^[a-z0-9@/._-]{1,64}$\n",
		"scope:    workspace\n",
		"  cwd:       ~/src/shop\n",
		"  timeout:   600 s\n",
		"  effects:   runs tests; writes nothing outside the repo\n",
		"  read-only: yes\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("Review(%d) = %q, want %q", p, out, want)
		}
	}
	if strings.Contains(out, "warning:") {
		t.Errorf("Review(%d) = %q, want no permission warning", p, out)
	}
	f.mustReview(t, fmt.Sprint(p), "--approve")
	id := *f.rows(t).proposal(t, p).ItemID
	if b, err := os.ReadFile(f.toolFile(id, 2)); err != nil || string(b) != toolText("test-package") {
		t.Errorf("tool.toml = %q, %v, want the contract", b, err)
	}
	f.mustMayRun(t, id)
}

// The E4 permission warning applies to a tool: approve needs --confirm.
func TestToolPermissionWarning(t *testing.T) {
	f := newFixture(t)
	p := f.toolDraft(t, strings.Replace(toolText("deploy"), "writes nothing outside the repo",
		"needs the network outside the sandbox", 1))
	if out := f.mustReview(t, fmt.Sprint(p)); !strings.Contains(out, "warning:  this may change a permission") {
		t.Errorf("Review(%d) = %q, want the permission warning", p, out)
	}
	if _, err := f.review(t, fmt.Sprint(p), "--approve"); !errors.Is(err, errConfirm) {
		t.Fatalf("Review(--approve) = %v, want errConfirm", err)
	}
	f.mustReview(t, fmt.Sprint(p), "--approve", "--confirm")
}

// AC S21: a change to the command, the schema, the cwd, the timeout, the effects or the
// restriction does not run until you approve it: while the change waits, the tool keeps the
// contract hash that you approved. After the approval it runs with the new hash only.
func TestToolChangeNeedsApproval(t *testing.T) {
	for name, change := range map[string][2]string{
		"command":     {`"--filter", "{package}"`, `"--filter", "{package}", "--bail"`},
		"schema":      {"{1,64}", "{1,32}"},
		"cwd":         {"~/src/shop", "~/src/other"},
		"timeout":     {"timeout_s = 600", "timeout_s = 60"},
		"effects":     {"writes nothing outside the repo", "writes the coverage folder"},
		"restriction": {"read_only = true", "read_only = false"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			id := f.approvedTool(t, toolText("test-package"))
			old := f.mustMayRun(t, id).ContractHash
			next := strings.Replace(toolText("test-package"), change[0], change[1], 1)
			f.mustReview(t, "--item", fmt.Sprint(id), "--edit", next)
			r := f.rows(t)
			pid := r.proposals[len(r.proposals)-1].ID
			if tl := f.mustMayRun(t, id); tl.ContractHash != old || tl.Contract != toolText("test-package") {
				t.Errorf("MayRun() while the change waits = %+v, want the approved contract", tl)
			}
			approve := []string{fmt.Sprint(pid), "--approve"}
			if name == "restriction" {
				// The end of the read-only restriction changes a safety rule (§7.4).
				if out := f.mustReview(t, fmt.Sprint(pid)); !strings.Contains(out, "warning:  this may change a permission") {
					t.Errorf("Review(%d) = %q, want the permission warning", pid, out)
				}
				if _, err := f.review(t, approve...); !errors.Is(err, errConfirm) {
					t.Fatalf("Review(--approve) = %v, want errConfirm", err)
				}
				approve = append(approve, "--confirm")
			}
			f.mustReview(t, approve...)
			tl := f.mustMayRun(t, id)
			if tl.ContractHash == old || tl.Contract != next || *tl.ApprovedHash != tl.ContractHash {
				t.Errorf("MayRun() after the approval = %+v, want the new contract and hash", tl)
			}
		})
	}
}

// AC: a tool change that breaks a refusal rule is refused at once, also for an input in a
// later step, and nothing is written.
func TestToolEditRefused(t *testing.T) {
	f := newFixture(t)
	id := f.approvedTool(t, toolText("test-package"))
	n := len(f.rows(t).proposals)
	bad := strings.Replace(toolText("test-package"), `["pnpm", "test", "--filter", "{package}"]`,
		`["bash", "-c", "{package}"]`, 1)
	_, err := f.review(t, "--item", fmt.Sprint(id), "--edit", bad)
	if err == nil || !strings.Contains(err.Error(), "step 2") || !strings.Contains(err.Error(), "command position") {
		t.Fatalf("Review(--edit bad) = %v, want the step 2 command position refusal", err)
	}
	if got := len(f.rows(t).proposals); got != n {
		t.Errorf("proposals = %d, want %d", got, n)
	}
	// A draft whose contract breaks a rule shows why, and approve refuses it.
	p := f.toolDraft(t, bad)
	if out := f.mustReview(t, fmt.Sprint(p)); !strings.Contains(out, "contract: refused: ") {
		t.Errorf("Review(%d) = %q, want the refusal", p, out)
	}
	if _, err := f.review(t, fmt.Sprint(p), "--approve"); err == nil {
		t.Error("Review(--approve) of a refused contract = nil, want an error")
	}
}

// One active tool for each name in each scope: the second approval refuses and asks for a new
// name.
func TestApproveToolNameClash(t *testing.T) {
	f := newFixture(t)
	f.approvedTool(t, toolText("deploy"))
	second := f.toolDraft(t, toolText("deploy"))
	_, err := f.review(t, fmt.Sprint(second), "--approve")
	if !errors.Is(err, store.ErrToolName) || !strings.Contains(err.Error(), "new name") {
		t.Fatalf("Review(second --approve) = %v, want ErrToolName that asks for a new name", err)
	}
	f.mustReview(t, fmt.Sprint(second), "--approve", "--edit", toolText("deploy-docs"))
}

// AC S11: a hand-edited tool.toml is a conflict: status shows it, the tool does not run, and
// brain repair --confirm writes it again.
func TestToolConflictRepair(t *testing.T) {
	f := newStatusFixture(t)
	id := f.approvedTool(t, toolText("test-package"))
	path := f.toolFile(id, 2)
	if err := os.WriteFile(path, []byte(strings.Replace(toolText("test-package"), "pnpm", "npx", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if out := f.mustStatus(t); !strings.Contains(out, "output conflicts: 1") || !strings.Contains(out, path) {
		t.Errorf("Status() = %q, want the tool conflict", out)
	}
	if _, err := f.mayRun(t, id); !errors.Is(err, output.ErrMayNotRun) {
		t.Errorf("MayRun() = %v, want ErrMayNotRun", err)
	}
	var buf bytes.Buffer
	e := f.env(f.sock, "w14:p1", "w14", &buf)
	if err := Repair(context.Background(), e, []string{"--confirm"}); err != nil {
		t.Fatalf("Repair(--confirm) = %v", err)
	}
	if !strings.Contains(buf.String(), "tool file "+path+": written again") {
		t.Errorf("Repair(--confirm) printed %q, want the tool file written again", buf.String())
	}
	f.mustMayRun(t, id)
}

// Forget of a tool with two revisions clears its contract and removes all its files, and
// reports done (§7.5 step 8).
func TestForgetTool(t *testing.T) {
	f := newFixture(t)
	id := f.approvedTool(t, toolText("test-package"))
	f.mustReview(t, "--item", fmt.Sprint(id), "--edit", strings.Replace(toolText("test-package"), "600", "60", 1))
	r := f.rows(t)
	f.mustReview(t, fmt.Sprint(r.proposals[len(r.proposals)-1].ID), "--approve")
	if _, err := os.Stat(f.toolFile(id, 3)); err != nil {
		t.Fatal(err)
	}
	out := f.mustForget(t, clock(), fmt.Sprint(id))
	if !strings.HasSuffix(out, fmt.Sprintf("forget item %d: done\n", id)) {
		t.Errorf("Forget() printed %q, want done", out)
	}
	if files := fileTree(t, filepath.Join(f.store, "tools", fmt.Sprint(id))); len(files) != 0 {
		t.Errorf("tool files after forget: %v, want none", files)
	}
	if _, err := f.mayRun(t, id); !errors.Is(err, output.ErrMayNotRun) {
		t.Errorf("MayRun() after forget = %v, want ErrMayNotRun", err)
	}
}

// namedTool is toolText(name) with the description "Tool <name>".
func namedTool(name string) string {
	return strings.Replace(toolText(name), "Run the tests of one package", "Tool "+name, 1)
}

// addTool commits an active tool item text in workspace key (nil: global) at testNow, with no
// build.
func (f *fixture) addTool(t *testing.T, key *string, text string) int64 {
	t.Helper()
	s := openStore(t, f.store)
	defer closeStore(t, s)
	r := store.ItemRevision{Text: text, Scope: store.ScopeGlobal, WorkspaceKey: key, State: store.StateActive,
		Author: store.AuthorYou, Origin: store.OriginReview, Created: testNow}
	if key != nil {
		r.Scope = store.ScopeWorkspace
	}
	var id int64
	write(t, s, func(tx *sql.Tx) (err error) {
		id, _, err = store.CreateItem(context.Background(), tx, store.Item{ClaimID: text, Kind: store.KindTool, Created: testNow}, r)
		return err
	})
	return id
}

// addCalls adds n calls of tool id with result at at.
func (f *fixture) addCalls(t *testing.T, id int64, result store.CallResult, at time.Time, n int) {
	t.Helper()
	s := openStore(t, f.store)
	defer closeStore(t, s)
	write(t, s, func(tx *sql.Tx) error {
		for range n {
			code := 0
			c := store.ToolCall{ToolID: id, Origin: store.OriginAgent, Result: result, At: at}
			if result == store.CallRan {
				c.ExitCode = &code
			}
			if _, err := store.AddToolCall(context.Background(), tx, c); err != nil {
				return err
			}
		}
		return nil
	})
}

// tools runs brain tools from pane w14:p1.
func (f *fixture) tools(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := Tools(context.Background(), f.env(f.sock, "w14:p1", "w14", &out), args)
	return out.String(), err
}

// AC E17-S5: with 12 approved tools, the brief shows 10 and brain tools shows 12. brain tools
// shows only the tools in the caller's scope, with their input schemas: not a tool of another
// workspace, a draft or a retired tool.
func TestToolsList(t *testing.T) {
	f := newFixture(t)
	if out, err := f.tools(t); err != nil || out != "no approved tools\n" {
		t.Errorf("Tools() = %q, %v, want no approved tools", out, err)
	}
	for i := range 10 {
		f.addTool(t, ptr("ws_a14"), namedTool(fmt.Sprintf("tool-%d", i+1)))
	}
	f.addTool(t, nil, namedTool("global-tool"))
	f.addTool(t, ptr("ws_a15"), namedTool("blog-tool"))
	f.toolDraft(t, namedTool("draft-tool"))
	last := f.approvedTool(t, toolText("test-package"))

	out, err := f.tools(t)
	if err != nil {
		t.Fatalf("Tools() = %v", err)
	}
	if n := strings.Count(out, "\n  brain run "); n != 12 {
		t.Errorf("Tools() lists %d tools, want 12:\n%s", n, out)
	}
	want := fmt.Sprintf("test-package (item %d, workspace w14 (shop)): Run the tests of one package\n"+
		"  brain run test-package --package <value>\n  --package: string, pattern ^[a-z0-9@/._-]{1,64}$\n", last)
	if !strings.HasPrefix(out, "tool-1 (item ") || !strings.Contains(out, "global-tool (item ") ||
		!strings.Contains(out, "\nglobal-tool (item 12, global): Tool global-tool\n") || !strings.HasSuffix(out, want) {
		t.Errorf("Tools() =\n%s\nwant tool-1 first, global-tool, and test-package last:\n%s", out, want)
	}
	if strings.Contains(out, "blog-tool") || strings.Contains(out, "draft-tool") {
		t.Errorf("Tools() =\n%s\nwant no tool of another workspace and no draft", out)
	}
	_, catalog, _ := strings.Cut(f.brief(t), "### Tools\n")
	if n := strings.Count(catalog, "\n- "); n != 10 || !strings.Contains(catalog, "- tool-1: Tool tool-1 Inputs: package\n") ||
		strings.Contains(catalog, "test-package") {
		t.Errorf("catalog = %q, want the first 10 tools by approval time", catalog)
	}
	if _, err := f.tools(t, "extra"); err == nil || !strings.Contains(err.Error(), toolsUsage) {
		t.Errorf("Tools(extra) = %v, want the usage", err)
	}
}

// AC E17-S5: a tool with no ran call for 91 days is an archive candidate, and so is a tool
// with only refused calls; a pinned tool is not, and a ran call keeps a tool.
func TestToolsArchive(t *testing.T) {
	f := newFixture(t)
	idle := f.addTool(t, ptr("ws_a14"), namedTool("idle"))
	refused := f.addTool(t, ptr("ws_a14"), namedTool("refused"))
	ran := f.addTool(t, ptr("ws_a14"), namedTool("ran"))
	pinned := f.addTool(t, ptr("ws_a14"), namedTool("pinned"))
	if _, err := f.pin(t, fmt.Sprint(pinned)); err != nil {
		t.Fatal(err)
	}
	f.addCalls(t, refused, store.CallRefused, days(89), 4)
	f.addCalls(t, ran, store.CallRan, days(89), 1)
	out := f.mustArchive(t, 91)
	for _, id := range []int64{idle, refused} {
		if !strings.Contains(out, fmt.Sprintf("\nitem %d  workspace w14 (shop)  idle 91d  ", id)) {
			t.Errorf("Archive() at day 91 =\n%s\nwant tool item %d", out, id)
		}
	}
	for _, id := range []int64{ran, pinned} {
		if strings.Contains(out, fmt.Sprintf("\nitem %d ", id)) {
			t.Errorf("Archive() at day 91 =\n%s\nwant no tool item %d", out, id)
		}
	}
}

// brain status shows the ran calls of the last 4 weeks and the active tools with none, for the
// M4 review prompt "Tools get no calls in 4 weeks" (§13). Refused calls and older calls do
// not count.
func TestStatusToolCalls(t *testing.T) {
	f := newStatusFixture(t)
	used := f.addTool(t, ptr("ws_a14"), namedTool("used"))
	idle := f.addTool(t, nil, namedTool("idle"))
	now := testNow.Add(3 * time.Hour)
	f.addCalls(t, used, store.CallRan, now.Add(-27*24*time.Hour), 2)
	f.addCalls(t, used, store.CallRan, now.Add(-29*24*time.Hour), 1)
	f.addCalls(t, idle, store.CallRefused, now.Add(-time.Hour), 3)
	out := f.mustStatus(t)
	if !strings.Contains(out, "\n  calls in the last 4 weeks: 2 ran; no ran call: idle\n") {
		t.Errorf("Status() =\n%s\nwant 2 ran calls and idle with none", out)
	}
}
