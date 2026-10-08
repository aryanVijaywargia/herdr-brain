package cli

import (
	"context"
	"database/sql"
	"encoding/json"
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

// skillCLI is an extractor CLI that answers each prompt with one release skill claim, with
// each ref of the prompt as evidence.
type skillCLI struct{ calls int }

func (c *skillCLI) Run(_ context.Context, cmd extract.Command) ([]byte, error) {
	c.calls++
	refs := []string{}
	for _, m := range promptRef.FindAllStringSubmatch(cmd.Argv[len(cmd.Argv)-1], -1) {
		refs = append(refs, m[1])
	}
	return json.Marshal(map[string]any{"is_error": false, "structured_output": map[string]any{"claims": []any{
		map[string]any{"kind": "skill", "situation": "releasing", "rule": "follow the release steps",
			"scope_guess": "workspace", "evidence_refs": refs, "same_as": nil, "conflicts_with": nil,
			"touches_permission": false, "expected_benefit": "no missed release step", "name": "release",
			"description": "Release a new version.", "required_tools": "git, go", "example": "Release v1.2.0.",
			"steps": "1. Run go test ./...\n2. Update the changelog.\n3. Tag and push."}}}})
}

// releaseLines are one release procedure in three wordings (S8).
var releaseLines = []string{
	"to release: 1. run the tests with go test ./... 2. update the changelog for the new version 3. tag the release with git tag and push the tag to origin",
	"release checklist, 1. run all the tests with go test ./... first 2. update the changelog entry for this version 3. tag the release with git tag and push it",
	"how we release: 1. run the tests with go test ./... 2. write the changelog for the new version 3. make a git tag for the release and push the tag to origin",
}

// releaseSession adds an ended agent session in workspace key ws where you explain the release
// procedure three times.
func (f *fixture) releaseSession(t *testing.T, ws string) {
	t.Helper()
	f.storeWrite(t, func(ctx context.Context, tx *sql.Tx) error {
		key := store.SessionKey{HerdrSession: "default", Harness: "claude", Kind: store.KeyAgent, ID: "release"}
		id, err := store.OpenSession(ctx, tx, key, "w14:p1", ws, testNow)
		if err != nil {
			return err
		}
		seg, err := store.StartSegment(ctx, tx, id, "w14:p1", ws, "recent_unwrapped", testNow)
		if err != nil {
			return err
		}
		for i, text := range releaseLines {
			if _, err := store.AddObservation(ctx, tx, store.Observation{CaptureIdentity: fmt.Sprintf("release-%d", i),
				Source: store.SourceHerdr, SegmentID: seg, Event: "working", Excerpt: text, Status: store.ObservationComplete,
				Sender: store.SenderYou, Counts: true, At: testNow.Add(time.Duration(i) * time.Minute)}); err != nil {
				return err
			}
		}
		return store.EndSession(ctx, tx, key, testNow.Add(3*time.Minute))
	})
}

// AC S8: a release procedure explained 3 times in "shop" gives one workspace skill draft. The
// review shows the whole file. After approval the "shop" brief lists it; "blog" does not.
func TestSkillDraftS8(t *testing.T) {
	f := newFixture(t)
	f.setClaude(t)
	f.releaseSession(t, "ws_a14")
	s := openStore(t, f.store)
	cli := &skillCLI{}
	err := extract.Pass{Store: s, Runner: cli, HerdrSession: "default", DefaultOn: true,
		Now: func() time.Time { return testNow.Add(time.Hour) }}.Run(context.Background())
	closeStore(t, s)
	if err != nil || cli.calls != 1 {
		t.Fatalf("Run() = %v with %d calls, want one call", err, cli.calls)
	}
	ps := f.rows(t).proposals
	if len(ps) != 1 {
		t.Fatalf("proposals = %+v, want one skill draft", ps)
	}
	id := fmt.Sprint(ps[0].ID)
	if out := f.mustReview(t); !strings.Contains(out, id+"  workspace w14 ()  skill release: Release a new version.") {
		t.Errorf("brain review:\n%s\nwant the skill draft in w14", out)
	}
	out := f.mustReview(t, id)
	for _, want := range []string{skillLine, "text:\n  ---\n  name: release\n", "  example: Release v1.2.0.\n",
		"  3. Tag and push.\n", "scope:    workspace\n"} {
		if !strings.Contains(out, want) {
			t.Errorf("brain review %s:\n%s\nwant %q", id, out, want)
		}
	}

	f.mustReview(t, id, "--approve")
	item := *f.rows(t).proposal(t, ps[0].ID).ItemID
	if b := f.brief(t); !strings.Contains(b, "- release: Release a new version.") || !strings.Contains(b, f.skillFile(item, 2)) {
		t.Errorf("shop brief = %q, want the release skill", b)
	}
	blog, err := output.Read(filepath.Join(f.store, outDir), "ws_a15")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(blog.Global+blog.Workspace, "release") {
		t.Errorf("blog brief = %q, want no release skill", blog.Global+blog.Workspace)
	}
}

// releaseTelling is release telling n of the M2 eval rerun (2026-10-08), with a new version
// each time: the procedure signal finds two of them similar.
func releaseTelling(n int) string {
	v := fmt.Sprintf("0.%d.0", n+1)
	return fmt.Sprintf("Release %s: update the version in package.json to %s, then run npm test, then commit as "+
		"'Release %s' and tag that commit 'v%s'. Push the tag when it is done.", v, v, v, v)
}

// releaseSessions adds the ended agent sessions of release tellings 1 to n in workspace key
// ws, one telling in each, codex and claude in turn, and runs an extraction pass with cli after
// each session.
func (f *fixture) releaseSessions(t *testing.T, ws string, n int, cli extract.Runner) {
	t.Helper()
	for i := 1; i <= n; i++ {
		at, pane := testNow.Add(time.Duration(i)*time.Minute), fmt.Sprintf("w14:p%d", i)
		f.storeWrite(t, func(ctx context.Context, tx *sql.Tx) error {
			key := store.SessionKey{HerdrSession: "default", Harness: []string{"codex", "claude"}[(i-1)%2],
				Kind: store.KeyAgent, ID: fmt.Sprintf("release-%d", i)}
			id, err := store.OpenSession(ctx, tx, key, pane, ws, at)
			if err != nil {
				return err
			}
			seg, err := store.StartSegment(ctx, tx, id, pane, ws, "recent_unwrapped", at)
			if err != nil {
				return err
			}
			if _, err := store.AddObservation(ctx, tx, store.Observation{CaptureIdentity: fmt.Sprintf("release-%d", i),
				Source: store.SourceHerdr, SegmentID: seg, Event: "working", Excerpt: releaseTelling(i),
				Status: store.ObservationComplete, Sender: store.SenderYou, Counts: true, At: at}); err != nil {
				return err
			}
			return store.EndSession(ctx, tx, key, at)
		})
		s := openStore(t, f.store)
		err := extract.Pass{Store: s, Runner: cli, HerdrSession: "default", DefaultOn: true,
			Now: func() time.Time { return testNow.Add(time.Hour) }}.Run(context.Background())
		closeStore(t, s)
		if err != nil {
			t.Fatalf("Run() after telling %d = %v", i, err)
		}
	}
}

// AC 2 (#313, S8): the release of the M2 eval rerun, one telling in each of four sessions, codex
// and claude in turn, gives one workspace skill draft, not a preference. The review shows the
// whole file. After approval the "shop" brief lists the skill with the path of its file (the
// input of the #165 kept-skill check); "blog" does not.
func TestSkillDraftAcrossSessions(t *testing.T) {
	f := newFixture(t)
	f.setClaude(t)
	cli := &skillCLI{}
	f.releaseSessions(t, "ws_a14", 4, cli)
	if cli.calls != 3 {
		t.Errorf("calls = %d, want 3: the first telling has no signal", cli.calls)
	}
	ps := f.rows(t).proposals
	if len(ps) != 1 {
		t.Fatalf("proposals = %+v, want one skill draft", ps)
	}
	id := fmt.Sprint(ps[0].ID)
	out := f.mustReview(t, id)
	for _, want := range []string{skillLine, "text:\n  ---\n  name: release\n", "scope:    workspace\n"} {
		if !strings.Contains(out, want) {
			t.Errorf("brain review %s:\n%s\nwant %q", id, out, want)
		}
	}

	f.mustReview(t, id, "--approve")
	item := *f.rows(t).proposal(t, ps[0].ID).ItemID
	if b := f.brief(t); !strings.Contains(b, "- release: Release a new version.") || !strings.Contains(b, f.skillFile(item, 2)) {
		t.Errorf("shop brief = %q, want the release skill", b)
	}
	blog, err := output.Read(filepath.Join(f.store, outDir), "ws_a15")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(blog.Global+blog.Workspace, "release") {
		t.Errorf("blog brief = %q, want no release skill", blog.Global+blog.Workspace)
	}
}

// AC: a patch proposal shows the diff of the changed lines only. Approve makes a new revision,
// and the next build writes its file and points the brief at it.
func TestSkillPatch(t *testing.T) {
	f := newFixture(t)
	id := f.approvedSkill(t, skillText("deploy", "one"))
	f.mustReview(t, "--item", fmt.Sprint(id), "--edit", skillText("deploy", "two"))
	r := f.rows(t)
	p := r.proposals[len(r.proposals)-1]
	if want := "@@ line 10\n- 1. Say one.\n+ 1. Say two."; p.Diff != want {
		t.Errorf("patch diff = %q, want %q", p.Diff, want)
	}
	if out := f.mustReview(t); !strings.Contains(out, fmt.Sprintf("patch of skill deploy (item %d)", id)) {
		t.Errorf("brain review:\n%s\nwant the patch line", out)
	}
	out := f.mustReview(t, fmt.Sprint(p.ID))
	if !strings.Contains(out, "diff:\n  @@ line 10\n  - 1. Say one.\n  + 1. Say two.\n") ||
		!strings.Contains(out, "(a skill patch: the diff shows the changed lines)") {
		t.Errorf("brain review %d:\n%s\nwant the diff", p.ID, out)
	}
	for _, same := range []string{"name: deploy", "description:", "# Steps"} {
		if strings.Contains(out, same) {
			t.Errorf("brain review %d shows the unchanged line %q:\n%s", p.ID, same, out)
		}
	}
	old, err := os.ReadFile(f.skillFile(id, 2))
	if err != nil {
		t.Fatal(err)
	}

	f.mustReview(t, fmt.Sprint(p.ID), "--approve")
	it, cur := f.rows(t).item(t, id)
	if it.CurrentRevision != 3 || cur.Text != skillText("deploy", "two") {
		t.Fatalf("item %+v, text %q, want revision 3 with the patch", it, cur.Text)
	}
	b, err := os.ReadFile(f.skillFile(id, 3))
	if err != nil || string(b) != skillText("deploy", "two") {
		t.Errorf("revision 3 file = %q, %v, want the new text", b, err)
	}
	if again, err := os.ReadFile(f.skillFile(id, 2)); err != nil || string(again) != string(old) {
		t.Errorf("revision 2 file = %q, %v, want it unchanged", again, err)
	}
	if brief := f.brief(t); !strings.Contains(brief, f.skillFile(id, 3)) || strings.Contains(brief, f.skillFile(id, 2)) {
		t.Errorf("brief = %q, want the revision 3 path only", brief)
	}
}

// An edit of a skill that is not a skill text is refused: it could never be approved.
func TestSkillPatchRefused(t *testing.T) {
	f := newFixture(t)
	id := f.approvedSkill(t, skillText("deploy", "one"))
	n := len(f.rows(t).proposals)
	_, err := f.review(t, "--item", fmt.Sprint(id), "--edit", "deploy with care")
	if err == nil || !strings.Contains(err.Error(), "is a skill") {
		t.Fatalf("Review(--item --edit) = %v, want a refusal", err)
	}
	if got := len(f.rows(t).proposals); got != n {
		t.Errorf("proposals = %d, want %d", got, n)
	}
}

// The pane edit is one line, so on a skill it sends you to brain review, and writes nothing.
func TestPaneSkillEdit(t *testing.T) {
	f := newFixture(t)
	pid := f.skillDraft(t, skillText("deploy", "one"))
	screen, err := f.pane(t, pid, typed("e"))
	if err != nil || !strings.Contains(screen, fmt.Sprintf("edit it with brain review %d --approve --edit", pid)) {
		t.Fatalf("pane = %v, screen:\n%s", err, screen)
	}
	if p := f.rows(t).proposal(t, pid); p.Status != store.ProposalPending {
		t.Errorf("proposal %s, want pending", p.Status)
	}
}
