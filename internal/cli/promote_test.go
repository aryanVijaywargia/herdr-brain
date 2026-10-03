package cli

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aryanVijaywargia/herdr-brain/internal/herdr"
	"github.com/aryanVijaywargia/herdr-brain/internal/output"
	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

// promote runs `brain promote args` from pane w14:p1 of workspace w14 ("shop").
func (f *fixture) promote(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := Promote(context.Background(), f.env(f.sock, "w14:p1", "w14", &out), args)
	return out.String(), err
}

// mustPromote promotes item id and returns the id of its promotion proposal.
func (f *fixture) mustPromote(t *testing.T, id int64) int64 {
	t.Helper()
	if out, err := f.promote(t, fmt.Sprint(id)); err != nil {
		t.Fatalf("Promote(%d) = %q, %v", id, out, err)
	}
	r := f.rows(t)
	return r.proposals[len(r.proposals)-1].ID
}

// brainItem commits an active Brain item "use pnpm" in shop (ws_a14) that the extractor drafted,
// with evidence corrections of its claim in shop and one in each workspace key of elsewhere.
// It returns the item id.
func (f *fixture) brainItem(t *testing.T, evidence int, elsewhere ...string) int64 {
	t.Helper()
	claim := f.claim(t)
	for i := range evidence {
		f.correct(t, claim, "ws_a14", fmt.Sprint("shop-", i))
	}
	for _, ws := range elsewhere {
		f.correct(t, claim, ws, "other-"+ws)
	}
	ctx := context.Background()
	s := openStore(t, f.store)
	defer closeStore(t, s)
	var id int64
	write(t, s, func(tx *sql.Tx) (err error) {
		id, _, err = store.CreateItem(ctx, tx, store.Item{ClaimID: claim, Kind: store.KindPreference, Created: testNow},
			store.ItemRevision{Text: "use pnpm", Scope: store.ScopeWorkspace, WorkspaceKey: ptr("ws_a14"), State: store.StateActive,
				Author: store.AuthorBrain, Origin: store.OriginExtractor, Created: testNow})
		return err
	})
	if _, err := output.Build(ctx, s, filepath.Join(f.store, outDir), nil, now); err != nil {
		t.Fatalf("Build() = %v", err)
	}
	return id
}

// parts are the global part and the workspace part of shop that a hook reads now.
func (f *fixture) parts(t *testing.T) (global, workspace string) {
	t.Helper()
	b, err := output.Read(filepath.Join(f.store, outDir), "ws_a14")
	if err != nil {
		t.Fatalf("output.Read() = %v", err)
	}
	return b.Global, b.Workspace
}

// AC: each bar check refuses with its reason (§9.3), and writes nothing. An item that you
// wrote, and a Brain item with 2 pieces of evidence, pass.
func TestPromoteBar(t *testing.T) {
	cases := []struct {
		name string
		item func(f *fixture) int64
		want string // "" when the promotion passes
	}{
		{name: "a handoff", item: func(f *fixture) int64 {
			p := f.remember(t, "next: port the cart page", "--kind", "handoff")
			f.mustReview(t, fmt.Sprint(p), "--approve")
			return *f.rows(t).proposal(t, p).ItemID
		}, want: "is a handoff: a plan for one workspace, not a lasting rule"},
		{name: "a disputed item", item: func(f *fixture) int64 {
			item, _ := f.disputed(t)
			return item
		}, want: "has an open dispute"},
		{name: "a Brain item with 1 piece of evidence", item: func(f *fixture) int64 { return f.brainItem(t, 1) },
			want: "is a Brain item with 1 piece(s) of evidence"},
		{name: "a Brain item with 2 pieces of evidence", item: func(f *fixture) int64 { return f.brainItem(t, 2) }},
		{name: "an item that you wrote", item: func(f *fixture) int64 { return f.addActive(t, ptr("ws_a14"), "use pnpm") }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			id := c.item(f)
			before := len(f.rows(t).proposals)
			out, err := f.promote(t, fmt.Sprint(id))
			if c.want != "" {
				if err == nil || !strings.Contains(err.Error(), c.want) || !strings.Contains(err.Error(), "nothing was written") {
					t.Fatalf("Promote(%d) = %v, want refused: %s", id, err, c.want)
				}
				if n := len(f.rows(t).proposals); n != before {
					t.Errorf("proposals = %d after the refusal, want %d", n, before)
				}
				return
			}
			if err != nil {
				t.Fatalf("Promote(%d) = %v, want a proposal", id, err)
			}
			r := f.rows(t)
			p := r.proposals[len(r.proposals)-1]
			it, _ := r.item(t, id)
			if !strings.Contains(out, fmt.Sprintf("proposal %d: promote item %d to global", p.ID, id)) ||
				p.Status != store.ProposalPending || p.Scope != store.ScopeGlobal || *p.BaseRevision != it.CurrentRevision ||
				it.Scope != store.ScopeWorkspace {
				t.Errorf("Promote(%d) printed %q, proposal %+v, item %+v; want a pending global proposal and the item in its workspace",
					id, out, p, it)
			}
		})
	}
}

// AC: the bar runs on the "change scope" of approve too: a Brain draft with 1 piece of
// evidence cannot go global, and a handoff draft cannot either.
func TestChangeScopeBar(t *testing.T) {
	f := newFixture(t)
	claim := f.claim(t)
	pid, err := f.propose(t, f.correct(t, claim, "ws_a14", "a1"), "workspace w14 (shop)")
	if err != nil {
		t.Fatal(err)
	}
	f.storeWrite(t, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE item_revisions SET origin = ?`, store.OriginExtractor)
		return err
	})
	handoff := f.remember(t, "next: port the cart page", "--kind", "handoff")
	for p, want := range map[int64]string{pid: "1 piece(s) of evidence", handoff: "is a handoff"} {
		_, err := f.review(t, fmt.Sprint(p), "--approve", "--scope", "global")
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("Review(%d --approve --scope global) = %v, want refused: %s", p, err, want)
		}
		r := f.rows(t)
		if it, _ := r.item(t, *r.proposal(t, p).ItemID); r.proposal(t, p).Status != store.ProposalPending || it.State != store.StateDraft {
			t.Errorf("after the refusal: proposal %s, item %s; want pending and draft", r.proposal(t, p).Status, it.State)
		}
	}
}

// AC: a pane in workspace B (blog) cannot promote an item of workspace A (shop), nor approve
// the promotion proposal of it.
func TestPromoteOtherWorkspace(t *testing.T) {
	f := newFixture(t)
	id := f.addActive(t, ptr("ws_a14"), "use pnpm")
	f.panes["w15:p1"] = herdr.Pane{ID: "w15:p1", WorkspaceID: "w15"}
	var out bytes.Buffer
	err := Promote(context.Background(), f.env(f.sock, "w15:p1", "w15", &out), []string{fmt.Sprint(id)})
	if !errors.Is(err, errNotFound) || len(f.rows(t).proposals) != 0 {
		t.Fatalf("Promote(%d) from blog = %v, want not found and no proposal", id, err)
	}
	pid := f.mustPromote(t, id)
	if _, err := f.reviewBlog(t, fmt.Sprint(pid), "--approve"); !errors.Is(err, errNotFound) {
		t.Errorf("Review(%d --approve) from blog = %v, want not found", pid, err)
	}
	if it, _ := f.rows(t).item(t, id); it.Scope != store.ScopeWorkspace {
		t.Errorf("item = %+v, want it in shop", it)
	}
}

// AC (S46, §9.3): the review shows the global text and the "also seen" hint. Approve with an
// edit makes a global revision; a promoted skill is in the global part and not in the
// workspace part. Undo makes a revision with the old scope and workspace key, and the skill
// goes back to its workspace part.
func TestPromoteSkillAndUndo(t *testing.T) {
	f := newFixture(t)
	id := f.approvedSkill(t, skillText("deploy", "shop"))
	pid := f.mustPromote(t, id)

	show := f.mustReview(t, fmt.Sprint(pid))
	for _, want := range []string{"Say shop.", "scope:    global", "diff:\n  scope: workspace w14 (shop) -> global",
		"promote:  the text goes into the global part", "hint:     also seen in 0 other workspaces (only a hint)"} {
		if !strings.Contains(show, want) {
			t.Errorf("show lacks %q:\n%s", want, show)
		}
	}
	if list := f.mustReview(t); !strings.Contains(list, fmt.Sprintf("promote item %d to global", id)) {
		t.Errorf("list = %q, want the promotion", list)
	}

	out := f.mustReview(t, fmt.Sprint(pid), "--approve", "--edit", skillText("deploy", "any workspace"))
	if !strings.Contains(out, fmt.Sprintf("item %d is active at revision 3 (global)", id)) {
		t.Errorf("approve printed %q", out)
	}
	it, rev := f.rows(t).item(t, id)
	if it.Scope != store.ScopeGlobal || it.WorkspaceKey != nil || rev.Scope != store.ScopeGlobal || !strings.Contains(rev.Text, "any workspace") {
		t.Errorf("item %+v, revision %+v; want the edited text in global", it, rev)
	}
	global, ws := f.parts(t)
	if !strings.Contains(global, "- deploy: ") || strings.Contains(ws, "- deploy: ") {
		t.Errorf("after approve: global part %q, workspace part %q; want the skill in the global part only", global, ws)
	}

	f.mustUndo(t, fmt.Sprint(id))
	it, rev = f.rows(t).item(t, id)
	if it.Scope != store.ScopeWorkspace || deref(it.WorkspaceKey) != "ws_a14" || rev.UndoesRevision == nil || *rev.UndoesRevision != 3 {
		t.Errorf("after undo: item %+v, revision %+v; want shop again, undoing revision 3", it, rev)
	}
	global, ws = f.parts(t)
	if strings.Contains(global, "- deploy: ") || !strings.Contains(ws, "- deploy: ") {
		t.Errorf("after undo: global part %q, workspace part %q; want the skill in the workspace part only", global, ws)
	}
}

// AC (§9.3): the hint counts the other workspaces with a candidate of the claim.
func TestPromoteHint(t *testing.T) {
	f := newFixture(t)
	id := f.brainItem(t, 2, "ws_b", "ws_c")
	show := f.mustReview(t, fmt.Sprint(f.mustPromote(t, id)))
	if !strings.Contains(show, "hint:     also seen in 2 other workspaces (only a hint)") {
		t.Errorf("show = %q, want the hint of 2 other workspaces", show)
	}
}

// AC: a promotion into a global skill name that is in use refuses and asks for a new name; no
// file changes. A new name passes.
func TestPromoteSkillNameClash(t *testing.T) {
	f := newFixture(t)
	s := openStore(t, f.store)
	gp, err := saveDraft(context.Background(), s, draft{text: skillText("deploy", "global"), kind: store.KindSkill,
		scope: store.ScopeGlobal, target: "global"}, testNow)
	closeStore(t, s)
	if err != nil {
		t.Fatal(err)
	}
	f.mustReview(t, fmt.Sprint(gp), "--approve")
	id := f.approvedSkill(t, skillText("deploy", "shop"))
	pid := f.mustPromote(t, id)
	before, newest := fileTree(t, filepath.Dir(f.store)), f.rows(t).newest

	_, err = f.review(t, fmt.Sprint(pid), "--approve")
	if !errors.Is(err, store.ErrSkillName) || !strings.Contains(err.Error(), "new name") {
		t.Fatalf("Review(%d --approve) = %v, want ErrSkillName that asks for a new name", pid, err)
	}
	if after := fileTree(t, filepath.Dir(f.store)); !mapsEqualExceptDB(before, after) {
		t.Error("files changed after the refused promotion")
	}
	r := f.rows(t)
	if it, _ := r.item(t, id); r.proposal(t, pid).Status != store.ProposalPending || r.newest != newest || it.Scope != store.ScopeWorkspace {
		t.Errorf("after the refusal: proposal %s, newest generation %d, item %+v; want pending, %d, in shop",
			r.proposal(t, pid).Status, r.newest, it, newest)
	}

	f.mustReview(t, fmt.Sprint(pid), "--approve", "--edit", skillText("deploy-all", "shop"))
	if global, _ := f.parts(t); !strings.Contains(global, "- deploy: ") || !strings.Contains(global, "- deploy-all: ") {
		t.Errorf("global part = %q, want both skills", global)
	}
}

// AC (§8.1): the E6 global budget check applies: a promotion that does not fit the global
// part refuses, and nothing is written.
func TestPromoteBudget(t *testing.T) {
	f := newFixture(t)
	f.addActive(t, nil, strings.Repeat("g", 3000))
	id := f.addActive(t, ptr("ws_a14"), strings.Repeat("w", 1000))
	pid := f.mustPromote(t, id)
	newest := f.rows(t).newest
	_, err := f.review(t, fmt.Sprint(pid), "--approve")
	if err == nil || !strings.Contains(err.Error(), "the global part of the brief is") || !strings.Contains(err.Error(), "nothing was written") {
		t.Fatalf("Review(%d --approve) = %v, want refused: over the global budget", pid, err)
	}
	r := f.rows(t)
	if it, _ := r.item(t, id); r.proposal(t, pid).Status != store.ProposalPending || r.newest != newest || it.Scope != store.ScopeWorkspace {
		t.Errorf("after the refusal: proposal %s, newest %d, item %+v; want pending, %d, in shop", r.proposal(t, pid).Status, r.newest, it, newest)
	}
	f.mustReview(t, fmt.Sprint(pid), "--approve", "--edit", "use pnpm")
	if global, _ := f.parts(t); !strings.Contains(global, fmt.Sprintf("- [%d] use pnpm", id)) {
		t.Errorf("global part lacks the shorter promoted item")
	}
}

// A promote of an item that is global already, or not active, refuses.
func TestPromoteRefused(t *testing.T) {
	f := newFixture(t)
	g := f.addActive(t, nil, "use pnpm")
	p := f.remember(t, "a draft")
	draft := *f.rows(t).proposal(t, p).ItemID
	for id, want := range map[int64]string{g: "is global already", draft: "is draft; only an active item"} {
		if _, err := f.promote(t, fmt.Sprint(id)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("Promote(%d) = %v, want refused: %s", id, err, want)
		}
	}
	if _, err := f.promote(t); err == nil || !strings.Contains(err.Error(), promoteUsage) {
		t.Errorf("Promote() = %v, want the usage", err)
	}
}

// The review pane shows a promotion with its hint, and its edit approves the global text.
func TestPanePromote(t *testing.T) {
	f := newFixture(t)
	id := f.addActive(t, ptr("ws_a14"), "use pnpm in the shop repo")
	pid := f.mustPromote(t, id)
	screen, err := f.pane(t, pid, typed("euse pnpm\r"))
	if err != nil {
		t.Fatalf("pane() = %v\n%s", err, screen)
	}
	if !strings.Contains(screen, "hint:     also seen in 0 other workspaces") || !strings.Contains(screen, "approve with scope: as proposed") {
		t.Errorf("screen lacks the hint or the scope:\n%s", screen)
	}
	if it, rev := f.rows(t).item(t, id); it.Scope != store.ScopeGlobal || rev.Text != "use pnpm" {
		t.Errorf("item %+v, revision %+v; want %q in global", it, rev, "use pnpm")
	}
}
