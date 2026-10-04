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

	"github.com/aryanVijaywargia/herdr-brain/internal/extract"
	"github.com/aryanVijaywargia/herdr-brain/internal/output"
	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

// bindTool is a tool contract named touch-ran with no input that runs in folder dir: it writes
// the file "ran" there, so a test sees whether it ran.
func bindTool(dir string) string {
	return "name = \"touch-ran\"\ndescription = \"Write a file\"\ncwd = \"" + dir + "\"\ntimeout_s = 10\n" +
		"effects = \"writes the file ran in a temp folder\"\nread_only = false\nargv = [\"/usr/bin/touch\", \"ran\"]\n"
}

// automationFixture is a status fixture with a temp folder that its tools run in.
type automationFixture struct {
	*statusFixture
	dir string
}

func newAutomationFixture(t *testing.T) *automationFixture {
	t.Helper()
	return &automationFixture{statusFixture: newStatusFixture(t), dir: t.TempDir()}
}

// automation runs brain automation args from pane w14:p1 of workspace w14.
func (f *automationFixture) automation(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := Automation(context.Background(), f.env(f.sock, "w14:p1", "w14", &out), args)
	return out.String(), err
}

func (f *automationFixture) mustAutomation(t *testing.T, args ...string) string {
	t.Helper()
	out, err := f.automation(t, args...)
	if err != nil {
		t.Fatalf("Automation(%q) = %v", args, err)
	}
	return out
}

// add proposes a binding of tool touch-ran in the fixture's folder for claude, and returns the
// proposal id.
func (f *automationFixture) add(t *testing.T) int64 {
	t.Helper()
	f.mustAutomation(t, "add", "touch-ran", "--folder", f.dir, "--cli", "claude")
	r := f.rows(t)
	return r.proposals[len(r.proposals)-1].ID
}

// bound approves a tool and a binding of it; it returns the tool and the automation item ids.
func (f *automationFixture) bound(t *testing.T) (toolID, id int64) {
	t.Helper()
	toolID = f.approvedTool(t, bindTool(f.dir))
	p := f.add(t)
	f.mustReview(t, fmt.Sprint(p), "--approve")
	return toolID, *f.rows(t).proposal(t, p).ItemID
}

// mayRun runs output.AutomationMayRun on automation id.
func (f *automationFixture) mayRun(t *testing.T, id int64) error {
	t.Helper()
	s := openStore(t, f.store)
	defer closeStore(t, s)
	return s.Write(context.Background(), func(tx *sql.Tx) error {
		_, err := output.AutomationMayRun(context.Background(), tx, filepath.Join(f.store, outDir), id)
		return err
	})
}

// ran reports whether a tool ran in the fixture's folder.
func (f *automationFixture) ran(t *testing.T) bool {
	t.Helper()
	_, err := os.Stat(filepath.Join(f.dir, "ran"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	return err == nil
}

// AC: approve on a temp store. The review shows the exact tool, trigger and folder, says that
// your approval is the permission and that undo cannot reverse effects that already happened.
// Approve makes the binding runnable, and nothing runs at the approval.
func TestAutomationApprove(t *testing.T) {
	f := newAutomationFixture(t)
	toolID := f.approvedTool(t, bindTool(f.dir))
	p := f.add(t)
	show := f.mustReview(t, fmt.Sprint(p))
	for _, want := range []string{
		"kind:     automation\n",
		fmt.Sprintf("  tool:     touch-ran (item %d), contract hash ", toolID),
		"    step 1:    \"/usr/bin/touch\" \"ran\"\n",
		"  trigger:  working → idle or working → done of a claude agent in this workspace\n",
		"  folder:   " + f.dir + "\n",
		"is the permission",
		"they cannot reverse effects that already happened\n",
	} {
		if !strings.Contains(show, want) {
			t.Errorf("review show = %q, want %q", show, want)
		}
	}
	if err := f.mayRun(t, *f.rows(t).proposal(t, p).ItemID); err == nil {
		t.Error("AutomationMayRun() of a draft = nil, want a refusal")
	}
	f.mustReview(t, fmt.Sprint(p), "--approve")
	id := *f.rows(t).proposal(t, p).ItemID
	if err := f.mayRun(t, id); err != nil {
		t.Errorf("AutomationMayRun(%d) = %v, want nil after the approval", id, err)
	}
	if f.ran(t) {
		t.Error("the tool ran at the approval")
	}
	if out := f.mustAutomation(t, "list"); !strings.Contains(out, fmt.Sprintf("automation %d: on\n", id)) {
		t.Errorf("list = %q, want the binding on", out)
	}
	// The catalog names the tool (E17-S5); the binding text is in no part.
	if b := f.brief(t); strings.Contains(b, "contract_hash") || !strings.Contains(b, "\n- touch-ran: ") {
		t.Errorf("brief = %q, want no automation in it, and the tool in the catalog", b)
	}
	if out := f.mustStatus(t); !strings.Contains(out, "\nautomations: 1 active, 0 paused, 0 stopped\n") {
		t.Errorf("Status() = %q, want one active automation", out)
	}
}

// AC: pause and resume on a temp store. A paused binding may not run; resume turns it on.
func TestAutomationPause(t *testing.T) {
	f := newAutomationFixture(t)
	_, id := f.bound(t)
	f.mustAutomation(t, "pause", fmt.Sprint(id))
	if err := f.mayRun(t, id); !errors.Is(err, output.ErrMayNotRun) || !strings.Contains(err.Error(), "paused") {
		t.Errorf("AutomationMayRun() = %v, want paused", err)
	}
	if out := f.mustAutomation(t, "list"); !strings.Contains(out, fmt.Sprintf("automation %d: paused\n", id)) {
		t.Errorf("list = %q, want the binding paused", out)
	}
	if out := f.mustStatus(t); !strings.Contains(out, "\nautomations: 1 active, 1 paused, 0 stopped\n") {
		t.Errorf("Status() = %q, want one paused automation", out)
	}
	f.mustAutomation(t, "resume", fmt.Sprint(id))
	if err := f.mayRun(t, id); err != nil {
		t.Errorf("AutomationMayRun() = %v, want nil after resume", err)
	}
}

// AC: removal on a temp store. A removed binding may not run, and pause or resume cannot turn
// it on again.
func TestAutomationRemove(t *testing.T) {
	f := newAutomationFixture(t)
	_, id := f.bound(t)
	out := f.mustAutomation(t, "remove", fmt.Sprint(id))
	if !strings.Contains(out, "starts no run again") {
		t.Errorf("remove = %q", out)
	}
	if err := f.mayRun(t, id); !errors.Is(err, output.ErrMayNotRun) {
		t.Errorf("AutomationMayRun() = %v, want a refusal", err)
	}
	if _, err := f.automation(t, "resume", fmt.Sprint(id)); err == nil {
		t.Error("resume of a removed automation = nil, want an error")
	}
	if it, _ := f.rows(t).item(t, id); it.State != store.StateRetired {
		t.Errorf("state = %s, want retired", it.State)
	}
	if out := f.mustAutomation(t, "list"); !strings.Contains(out, "no automations in") {
		t.Errorf("list = %q, want none", out)
	}
}

// AC: undo on a temp store. Undo disables future runs, also the undo of a second approval: the
// older binding does not come back. A second undo does nothing.
func TestAutomationUndo(t *testing.T) {
	f := newAutomationFixture(t)
	toolID, id := f.bound(t)
	// A changed tool, and the binding approved again with the new hash: revision 3.
	f.mustReview(t, "--item", fmt.Sprint(toolID), "--edit", strings.Replace(bindTool(f.dir), "timeout_s = 10", "timeout_s = 20", 1))
	r := f.rows(t)
	f.mustReview(t, fmt.Sprint(r.proposals[len(r.proposals)-1].ID), "--approve")
	p := f.add(t)
	f.mustReview(t, fmt.Sprint(p), "--approve")
	if it, _ := f.rows(t).item(t, id); it.CurrentRevision != 3 {
		t.Fatalf("revision = %d, want 3", it.CurrentRevision)
	}
	out := f.mustUndo(t, fmt.Sprint(id))
	if !strings.Contains(out, "the item is retired at revision 4") ||
		!strings.Contains(out, "undo cannot reverse effects that already happened") {
		t.Errorf("Undo() = %q", out)
	}
	if err := f.mayRun(t, id); !errors.Is(err, output.ErrMayNotRun) {
		t.Errorf("AutomationMayRun() = %v, want a refusal after undo", err)
	}
	if out := f.mustUndo(t, fmt.Sprint(id)); !strings.Contains(out, "undone already") {
		t.Errorf("second Undo() = %q, want nothing to do", out)
	}
}

// AC: a changed contract hash of the tool stops the binding, and status says why. A new
// proposal pins the new hash, and its approval starts the binding again.
func TestAutomationContractChange(t *testing.T) {
	f := newAutomationFixture(t)
	toolID, id := f.bound(t)
	f.mustReview(t, "--item", fmt.Sprint(toolID), "--edit", strings.Replace(bindTool(f.dir), "timeout_s = 10", "timeout_s = 20", 1))
	r := f.rows(t)
	f.mustReview(t, fmt.Sprint(r.proposals[len(r.proposals)-1].ID), "--approve")
	err := f.mayRun(t, id)
	if !errors.Is(err, output.ErrMayNotRun) || !strings.Contains(err.Error(), "changed after your approval") {
		t.Errorf("AutomationMayRun() = %v, want the changed contract", err)
	}
	out := f.mustStatus(t)
	if !strings.Contains(out, "\nautomations: 1 active, 0 paused, 1 stopped\n") ||
		!strings.Contains(out, fmt.Sprintf("  stopped: automation %d (%s): ", id, f.dir)) ||
		!strings.Contains(out, "the contract of tool touch-ran changed after your approval of the binding") {
		t.Errorf("Status() = %q, want the stopped automation and why", out)
	}
	p := f.add(t)
	if got := *f.rows(t).proposal(t, p).ItemID; got != id {
		t.Errorf("proposal item = %d, want the binding %d", got, id)
	}
	f.mustReview(t, fmt.Sprint(p), "--approve")
	if err := f.mayRun(t, id); err != nil {
		t.Errorf("AutomationMayRun() = %v, want nil after the new approval", err)
	}
}

// AC: the approval refuses a tool with a required input, and a tool whose cwd is not the
// pinned folder. add refuses them at once; a tool that changed after the proposal is refused
// at the approval, and nothing is written. A CLI with no tested stop condition gets none.
func TestAutomationRefused(t *testing.T) {
	f := newAutomationFixture(t)
	withInput := runTool("write-word", f.dir, "10", writeWord)
	f.approvedTool(t, withInput)
	if _, err := f.automation(t, "add", "write-word", "--folder", f.dir, "--cli", "claude"); !errors.Is(err, store.ErrBindingRefused) ||
		!strings.Contains(err.Error(), "required input") {
		t.Errorf("add of a tool with an input = %v, want the refusal", err)
	}
	toolID := f.approvedTool(t, bindTool(f.dir))
	if _, err := f.automation(t, "add", "touch-ran", "--folder", t.TempDir(), "--cli", "claude"); !errors.Is(err, store.ErrBindingRefused) ||
		!strings.Contains(err.Error(), "not the pinned folder") {
		t.Errorf("add in another folder = %v, want the refusal", err)
	}
	if _, err := f.automation(t, "add", "touch-ran", "--folder", f.dir, "--cli", "agy"); !errors.Is(err, store.ErrBindingRefused) {
		t.Errorf("add for agy = %v, want the refusal", err)
	}

	for _, tc := range []struct {
		name, text, want string
	}{
		{"input", strings.Replace(withInput, "write-word", "touch-ran", 1), "required input"},
		{"cwd", bindTool(t.TempDir()), "not the pinned folder"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := f.add(t)
			f.mustReview(t, "--item", fmt.Sprint(toolID), "--edit", tc.text)
			r := f.rows(t)
			f.mustReview(t, fmt.Sprint(r.proposals[len(r.proposals)-1].ID), "--approve")
			_, err := f.review(t, fmt.Sprint(p), "--approve")
			if !errors.Is(err, store.ErrBindingRefused) || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("approve = %v, want the refusal %q", err, tc.want)
			}
			if got := f.rows(t).proposal(t, p).Status; got != store.ProposalPending {
				t.Errorf("proposal status = %s, want pending: nothing written", got)
			}
			// The tool goes back to the contract that a binding can take.
			f.mustReview(t, "--item", fmt.Sprint(toolID), "--edit", bindTool(f.dir))
			r = f.rows(t)
			f.mustReview(t, fmt.Sprint(r.proposals[len(r.proposals)-1].ID), "--approve")
			f.mustReview(t, fmt.Sprint(p), "--reject")
		})
	}
}

func TestParseAdd(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want addArgs
		ok   bool
	}{
		{[]string{"t", "--folder", "/x", "--cli", "codex"}, addArgs{tool: "t", folder: "/x", cli: "codex"}, true},
		{[]string{"--folder", "/x", "t", "--cli", "codex"}, addArgs{}, false},
		{[]string{"t", "--folder", "/x"}, addArgs{}, false},
		{[]string{"t", "--cli", "codex"}, addArgs{}, false},
		{[]string{"t", "--folder", "/x", "--cli", "codex", "more"}, addArgs{}, false},
		{nil, addArgs{}, false},
	} {
		got, err := parseAdd(tc.args)
		if (err == nil) != tc.ok || got != tc.want {
			t.Errorf("parseAdd(%q) = %+v, %v, want %+v, ok %t", tc.args, got, err, tc.want, tc.ok)
		}
	}
}

// stopSession records one session of the learned case in workspace w14: the claude agent in
// pane w14:p1 works, its Stop hook records the stop, and then brain run runs tool touch-ran from
// the pane, through the fake herdr. The session ends after the call.
func (f *automationFixture) stopSession(t *testing.T, id string) {
	t.Helper()
	key := store.SessionKey{HerdrSession: "default", Harness: "claude", Kind: store.KeyAgent, ID: id}
	f.storeWrite(t, func(ctx context.Context, tx *sql.Tx) error {
		sid, err := store.OpenSession(ctx, tx, key, "w14:p1", "ws_a14", testNow.Add(-time.Hour))
		if err != nil {
			return err
		}
		seg, err := store.StartSegment(ctx, tx, sid, "w14:p1", "ws_a14", "recent_unwrapped", testNow.Add(-time.Hour))
		if err != nil {
			return err
		}
		for i, o := range []store.Observation{
			{Source: store.SourceHerdr, Event: "working", Excerpt: "Editing main.go", Sender: store.SenderAssistant},
			{Source: store.SourceHook, Event: "stop", Excerpt: "Done.", Sender: store.SenderAssistant},
		} {
			o.CaptureIdentity, o.SegmentID, o.Status, o.Counts = fmt.Sprintf("%s-%d", id, i), seg, store.ObservationComplete, true
			o.At = testNow.Add(time.Duration(i-2) * time.Minute)
			if _, err := store.AddObservation(ctx, tx, o); err != nil {
				return err
			}
		}
		return nil
	})
	rf := &runFixture{fixture: f.fixture, dir: f.dir}
	if _, err := rf.run(t, "w14:p1", "w14", "touch-ran"); err != nil {
		t.Fatalf("Run(touch-ran) = %v", err)
	}
	if err := os.Remove(filepath.Join(f.dir, "ran")); err != nil {
		t.Fatal(err)
	}
	f.storeWrite(t, func(ctx context.Context, tx *sql.Tx) error { return store.EndSession(ctx, tx, key, testNow) })
}

// pass runs one extraction pass with a fake extractor CLI, and returns its calls.
func (f *automationFixture) pass(t *testing.T) int {
	t.Helper()
	s := openStore(t, f.store)
	defer closeStore(t, s)
	cli := &skillCLI{}
	err := extract.Pass{Store: s, Runner: cli, HerdrSession: "default", DefaultOn: true,
		Now: func() time.Time { return testNow.Add(time.Hour) }}.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() = %v", err)
	}
	return cli.calls
}

// automationProposals returns the pending proposals of automation items.
func (f *automationFixture) automationProposals(t *testing.T) []store.Proposal {
	t.Helper()
	r := f.rows(t)
	var ps []store.Proposal
	for _, p := range r.proposals {
		if it, _ := r.item(t, *p.ItemID); it.Kind == store.KindAutomation && p.Status == store.ProposalPending {
			ps = append(ps, p)
		}
	}
	return ps
}

// AC: offline, with the fake herdr and a fake extractor: the recorded learned case (the same
// approved tool runs in its folder after a stop of the claude agent, in 3 sessions) gives one
// scoped automation proposal, with no model call. Nothing runs before the approval. A
// rejection stops a new proposal in that scope.
func TestLearnedAutomation(t *testing.T) {
	f := newAutomationFixture(t)
	toolID := f.approvedTool(t, bindTool(f.dir))
	for i, id := range []string{"s1", "s2"} {
		f.stopSession(t, id)
		if calls := f.pass(t); calls != 0 || len(f.automationProposals(t)) != 0 {
			t.Fatalf("after session %d: %d extractor calls, proposals %+v; want none below the gate", i+1, calls,
				f.automationProposals(t))
		}
	}
	f.stopSession(t, "s3")
	calls := f.pass(t)
	f.pass(t) // a second pass adds nothing
	ps := f.automationProposals(t)
	if calls != 0 || len(ps) != 1 {
		t.Fatalf("%d extractor calls, proposals %+v; want one proposal and no call", calls, ps)
	}
	p := ps[0]
	if p.Scope != store.ScopeWorkspace || p.CandidateID == nil {
		t.Errorf("proposal = %+v, want a workspace proposal of a candidate", p)
	}
	it, _ := f.rows(t).item(t, *p.ItemID)
	if it.WorkspaceKey == nil || *it.WorkspaceKey != "ws_a14" {
		t.Errorf("item workspace = %v, want ws_a14", it.WorkspaceKey)
	}
	b, err := store.ParseBinding(p.Text)
	if err != nil || b.ToolID != toolID || b.Folder != f.dir || b.CLI != "claude" || b.Trigger != "working → idle or working → done" {
		t.Errorf("binding = %+v, %v; want tool %d in %s on the claude stop", b, err, toolID, f.dir)
	}
	if !strings.Contains(f.mustReview(t, fmt.Sprint(p.ID)), "  folder:   "+f.dir+"\n") {
		t.Error("review show has no folder line")
	}
	if err := f.mayRun(t, it.ID); !errors.Is(err, output.ErrMayNotRun) {
		t.Errorf("AutomationMayRun() before approval = %v, want a refusal", err)
	}
	if f.ran(t) {
		t.Error("the tool ran before the approval")
	}
	for _, c := range f.calls(t) {
		if c.Origin != store.OriginAgent {
			t.Errorf("tool call %+v, want only the recorded agent calls", c)
		}
	}

	f.mustReview(t, fmt.Sprint(p.ID), "--reject")
	f.stopSession(t, "s4")
	f.pass(t)
	if ps := f.automationProposals(t); len(ps) != 0 {
		t.Errorf("proposals after the rejection = %+v, want none in that scope", ps)
	}
}

// A call of an older contract of the tool is no evidence: the tool got a new contract after
// the 3 sessions, so its cwd may be another folder, and no proposal comes.
func TestLearnedAutomationOldContract(t *testing.T) {
	f := newAutomationFixture(t)
	toolID := f.approvedTool(t, bindTool(f.dir))
	for _, id := range []string{"s1", "s2", "s3"} {
		f.stopSession(t, id)
	}
	f.mustReview(t, "--item", fmt.Sprint(toolID), "--edit", strings.Replace(bindTool(f.dir), "timeout_s = 10", "timeout_s = 20", 1))
	r := f.rows(t)
	f.mustReview(t, fmt.Sprint(r.proposals[len(r.proposals)-1].ID), "--approve")
	if calls := f.pass(t); calls != 0 || len(f.automationProposals(t)) != 0 {
		t.Errorf("%d calls, proposals %+v; want none from calls of an older contract", calls, f.automationProposals(t))
	}
}

// calls returns the tool calls of the store.
func (f *automationFixture) calls(t *testing.T) []store.ToolCall {
	t.Helper()
	return (&runFixture{fixture: f.fixture}).calls(t)
}
