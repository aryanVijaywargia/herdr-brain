package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
			f.mustReview(t, fmt.Sprint(pid), "--approve")
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
