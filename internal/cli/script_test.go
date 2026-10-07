package cli

import (
	"context"
	"database/sql"
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aryanVijaywargia/herdr-brain/internal/extract"
	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

// chainSession adds ended agent session id in workspace key ws: you ask to check the code, and
// the agent runs go vet and go test in folder /src/shop.
func (f *fixture) chainSession(t *testing.T, ws, id string) {
	t.Helper()
	f.storeWrite(t, func(ctx context.Context, tx *sql.Tx) error {
		key := store.SessionKey{HerdrSession: "default", Harness: "claude", Kind: store.KeyAgent, ID: id}
		sid, err := store.OpenSession(ctx, tx, key, "w14:p1", ws, testNow)
		if err != nil {
			return err
		}
		seg, err := store.StartSegment(ctx, tx, sid, "w14:p1", ws, "recent_unwrapped", testNow)
		if err != nil {
			return err
		}
		add := func(i int, o store.Observation) (int64, error) {
			o.CaptureIdentity, o.SegmentID, o.Status, o.Counts = fmt.Sprintf("%s-%d", id, i), seg, store.ObservationComplete, true
			o.At = testNow.Add(time.Duration(i) * time.Minute)
			return store.AddObservation(ctx, tx, o)
		}
		if _, err := add(0, store.Observation{Source: store.SourceHerdr, Event: "working", Excerpt: "check the code",
			Sender: store.SenderYou}); err != nil {
			return err
		}
		exit := 0
		for i, argv := range []string{"go vet ./...", "go test ./..."} {
			obs, err := add(i+1, store.Observation{Source: store.SourceHook, Event: "tool", Excerpt: argv, Sender: store.SenderTool})
			if err != nil {
				return err
			}
			if err := store.AddCommandRecord(ctx, tx, store.CommandRecord{ObservationID: obs, Argv: argv, CWD: "/src/shop",
				ExitStatus: &exit}); err != nil {
				return err
			}
		}
		return store.EndSession(ctx, tx, key, testNow.Add(3*time.Minute))
	})
}

// extractChains runs one extraction pass, with no model claim, an hour after testNow.
func (f *fixture) extractChains(t *testing.T) {
	t.Helper()
	s := openStore(t, f.store)
	err := extract.Pass{Store: s, Runner: &skillCLI{}, HerdrSession: "default", DefaultOn: true,
		Now: func() time.Time { return testNow.Add(time.Hour) }}.Run(context.Background())
	closeStore(t, s)
	if err != nil {
		t.Fatalf("Run() = %v", err)
	}
}

// scriptAndTool returns the script proposal and the tool draft of the one command chain.
func scriptAndTool(t *testing.T, r rows) (script, tool store.Proposal) {
	t.Helper()
	for _, p := range r.proposals {
		if p.Script() {
			script = p
		} else {
			tool = p
		}
	}
	if len(r.proposals) != 2 || script.ID == 0 || tool.ItemID == nil {
		t.Fatalf("proposals = %+v, want a script and a tool draft", r.proposals)
	}
	return script, tool
}

// files returns the files under the fixture folder but the store database, which approve
// writes, by path, with their content.
func (f *fixture) files(t *testing.T) map[string]string {
	t.Helper()
	files := readTree(t, filepath.Dir(f.store))
	maps.DeleteFunc(files, func(path, _ string) bool { return strings.HasPrefix(filepath.Base(path), store.FileName) })
	return files
}

// AC: a chain in 3 sessions gives one script proposal. Approve writes nothing outside the store.
// The review offers the script and the tool draft of the chain, and you pick one: approving the
// script supersedes the tool draft.
func TestScriptReview(t *testing.T) {
	f := newFixture(t)
	for i := range 3 {
		f.chainSession(t, "ws_a14", fmt.Sprintf("s%d", i))
	}
	f.extractChains(t)
	script, tool := scriptAndTool(t, f.rows(t))
	if out := f.mustReview(t); !strings.Contains(out, fmt.Sprintf("%d  %s  script go-vet: run go vet ./..., then go test ./...",
		script.ID, store.ScriptTarget)) {
		t.Errorf("review =\n%s\nwant the script line", out)
	}
	show := f.mustReview(t, fmt.Sprint(script.ID))
	for _, want := range []string{"kind:     script\n", "\n  set -e\n  cd '/src/shop'\n  go vet ./...\n  go test ./...\n",
		"install:  Brain writes no file", "diff:\n  @@ line 1\n  + #!/bin/sh\n",
		fmt.Sprintf("pick one: proposal %d is a tool draft of the same command chain", tool.ID),
		fmt.Sprintf("actions:  brain review %d --approve | --reject | --later\n", script.ID)} {
		if !strings.Contains(show, want) {
			t.Errorf("review show =\n%s\nwant %q", show, want)
		}
	}
	if strings.Contains(show, "revision:") || strings.Contains(show, "claim:") || strings.Contains(show, "--change") {
		t.Errorf("review show =\n%s\nwant no revision line, no claim and no change action", show)
	}
	if show := f.mustReview(t, fmt.Sprint(script.ID), "--detail"); !strings.Contains(show, "\nclaim:    cl_") || strings.Contains(show, "revision:") {
		t.Errorf("review show --detail =\n%s\nwant the claim and no revision line", show)
	}
	if show := f.mustReview(t, fmt.Sprint(tool.ID)); !strings.Contains(show,
		fmt.Sprintf("pick one: proposal %d is a script of the same command chain", script.ID)) {
		t.Errorf("review show of the tool draft =\n%s\nwant the script offered", show)
	}
	for _, args := range [][]string{{"--change", "use a loop"}, {"--approve", "--edit", "echo hi"}, {"--approve", "--pin"}} {
		if _, err := f.review(t, append([]string{fmt.Sprint(script.ID)}, args...)...); err == nil {
			t.Errorf("review %d %q = nil, want a refusal", script.ID, args)
		}
	}

	before := struct {
		files map[string]string
		items []store.Item
	}{f.files(t), f.rows(t).items}
	out := f.mustReview(t, fmt.Sprint(script.ID), "--approve")
	if want := fmt.Sprintf("proposal %d approved: Brain wrote no file", script.ID); !strings.HasPrefix(out, want) ||
		!strings.Contains(out, fmt.Sprintf("proposal %d of the same command chain superseded", tool.ID)) {
		t.Errorf("approve = %q, want %q and the tool draft superseded", out, want)
	}
	if after := f.files(t); !maps.Equal(before.files, after) {
		t.Errorf("files after approve = %v, want %v: approve writes nothing outside the store", after, before.files)
	}
	r := f.rows(t)
	script, tool = scriptAndTool(t, r)
	if script.Status != store.ProposalApproved || tool.Status != store.ProposalSuperseded {
		t.Errorf("script %s, tool draft %s, want approved and superseded", script.Status, tool.Status)
	}
	n := len(r.items)
	if it, _ := r.item(t, *tool.ItemID); it.State != store.StateDraft || n != len(before.items) {
		t.Errorf("items = %+v, want no new item and the tool draft still a draft", r.items)
	}
	f.extractChains(t)
	if n := len(f.rows(t).proposals); n != 2 {
		t.Errorf("proposals after a new pass = %d, want 2: you picked the script", n)
	}
}

// Approving the tool draft supersedes the script of the same chain: you pick one.
func TestScriptPickTool(t *testing.T) {
	f := newFixture(t)
	for i := range 3 {
		f.chainSession(t, "ws_a14", fmt.Sprintf("s%d", i))
	}
	f.extractChains(t)
	script, tool := scriptAndTool(t, f.rows(t))
	out := f.mustReview(t, fmt.Sprint(tool.ID), "--approve")
	if !strings.Contains(out, fmt.Sprintf("proposal %d of the same command chain superseded", script.ID)) {
		t.Errorf("approve = %q, want the script superseded", out)
	}
	script, tool = scriptAndTool(t, f.rows(t))
	if script.Status != store.ProposalSuperseded || tool.Status != store.ProposalApproved {
		t.Errorf("script %s, tool draft %s, want superseded and approved", script.Status, tool.Status)
	}
}

// A rejection of a script rejects its claim in its scope, with the tool draft of the same chain
// (E12-S3); the same chain in another workspace still gets its script.
func TestScriptRejectScope(t *testing.T) {
	f := newFixture(t)
	for i := range 3 {
		f.chainSession(t, "ws_a14", fmt.Sprintf("s%d", i))
	}
	f.extractChains(t)
	script, _ := scriptAndTool(t, f.rows(t))
	out := f.mustReview(t, fmt.Sprint(script.ID), "--reject", "--reason", "I run them by hand")
	if !strings.Contains(out, "stays rejected") || !strings.Contains(out, "of the same claim rejected") {
		t.Errorf("reject = %q, want the claim rejected with the tool draft", out)
	}
	for i := range 3 {
		f.chainSession(t, "ws_a15", fmt.Sprintf("b%d", i))
	}
	f.extractChains(t)
	var a14, a15 int
	for _, p := range f.rows(t).proposals {
		switch {
		case !p.Script():
		case p.Status == store.ProposalRejected:
			a14++
		case p.Status == store.ProposalPending:
			a15++
		}
	}
	if a14 != 1 || a15 != 1 {
		t.Errorf("rejected scripts = %d, pending scripts = %d, want 1 and 1", a14, a15)
	}
	if out, err := f.reviewBlog(t); err != nil || !strings.Contains(out, "script go-vet") {
		t.Errorf("review in blog = %q, %v, want its script", out, err)
	}
	if out := f.mustReview(t); strings.Contains(out, "script go-vet") {
		t.Errorf("review in shop =\n%s\nwant no script: the shop claim is rejected", out)
	}
}

// Approving a script closes the open review pane of its tool draft, as the tool draft is
// superseded.
func TestScriptClosesSiblingPane(t *testing.T) {
	f := newFixture(t)
	for i := range 3 {
		f.chainSession(t, "ws_a14", fmt.Sprintf("s%d", i))
	}
	f.extractChains(t)
	script, tool := scriptAndTool(t, f.rows(t))
	f.addReviewPane(t, store.ReviewPane{HerdrSession: "default", SessionIdentity: ptr("ws_a14"), PaneID: "w14:p2",
		BesidePaneID: "w14:p1", ProposalID: tool.ID, Opened: testNow})
	f.herdr.procs["w14:p2"] = reviewProc(tool.ID)
	f.mustReview(t, fmt.Sprint(script.ID), "--approve")
	if !slices.Equal(f.herdr.closed, []string{"w14:p2"}) {
		t.Errorf("closed %v, want [w14:p2]: the pane of the superseded tool draft", f.herdr.closed)
	}
}

// The review pane of a script offers approve, reject and later only: the other keys do
// nothing, so a pin toggle cannot make the approve fail.
func TestScriptPane(t *testing.T) {
	f := newFixture(t)
	for i := range 3 {
		f.chainSession(t, "ws_a14", fmt.Sprintf("s%d", i))
	}
	f.extractChains(t)
	script, _ := scriptAndTool(t, f.rows(t))
	screen, err := f.pane(t, script.ID, typed("pseca"))
	if err != nil {
		t.Fatalf("pane = %v\n%s", err, screen)
	}
	if !strings.Contains(screen, "a approve  r reject  l later  v evidence  d detail  q close\n") || strings.Contains(screen, "p pin") {
		t.Errorf("pane =\n%s\nwant the script keys only", screen)
	}
	if script, _ = scriptAndTool(t, f.rows(t)); script.Status != store.ProposalApproved {
		t.Errorf("script %s, want approved", script.Status)
	}
}
