package cli

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"io/fs"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/aryanVijaywargia/herdr-brain/internal/herdr"
	"github.com/aryanVijaywargia/herdr-brain/internal/output"
	"github.com/aryanVijaywargia/herdr-brain/internal/store"
	"github.com/aryanVijaywargia/herdr-brain/internal/workspace"
)

// blockFiles lists the workspace block files of every output generation, relative to the out
// folder.
func (f *fixture) blockFiles(t *testing.T) []string {
	t.Helper()
	out := filepath.Join(f.store, outDir)
	var files []string
	err := filepath.WalkDir(out, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || filepath.Base(filepath.Dir(path)) != "workspaces" {
			return err
		}
		rel, err := filepath.Rel(out, path)
		files = append(files, filepath.Base(rel))
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(files)
	return slices.Compact(files)
}

// AC S19 (M3 exit, #165): a rename of workspace "shop" keeps its key. Its item and its skill
// stay linked, a new item lands in the same key, and the block file keeps its name.
func TestRenameS19(t *testing.T) {
	f := newFixture(t)
	item := f.approved(t, "shop fact marker-s19")
	skill := f.approvedSkill(t, skillText("deploy", "s19"))
	before := f.blockFiles(t)
	if !slices.Contains(before, "ws_a14.md") {
		t.Fatalf("block files = %v, want ws_a14.md", before)
	}

	var buf bytes.Buffer
	e := f.env(f.sock, "w14:p1", "w14", &buf)
	e.Sync = func(ctx context.Context, s workspace.Session, st *store.Store) error {
		return s.Refresh(ctx, nil, st, []herdr.Workspace{{ID: "w14", Label: "shop-2"}, {ID: "w15", Label: "blog"}}, testNow)
	}
	if err := Remember(context.Background(), e, []string{"after the rename marker-s19b"}); err != nil {
		t.Fatalf("Remember() after the rename = %v", err)
	}
	r := f.rows(t)
	pid := r.proposals[len(r.proposals)-1].ID
	if err := Review(context.Background(), e, []string{fmt.Sprint(pid), "--approve"}); err != nil {
		t.Fatalf("Review(--approve) after the rename = %v", err)
	}

	r = f.rows(t)
	added := *r.proposal(t, pid).ItemID
	for _, id := range []int64{item, skill, added} {
		if k := r.keyOf(t, id); k != "ws_a14" {
			t.Errorf("item %d key = %q after the rename, want ws_a14", id, k)
		}
	}
	f.storeWrite(t, func(ctx context.Context, tx *sql.Tx) error {
		w, err := store.WorkspaceByKey(ctx, tx, "ws_a14")
		if err == nil && w.Label != "shop-2" {
			t.Errorf("workspace ws_a14 label = %q, want shop-2", w.Label)
		}
		return err
	})
	b := f.brief(t)
	for _, want := range []string{"marker-s19", "marker-s19b", f.skillFile(skill, 2)} {
		if !strings.Contains(b, want) {
			t.Errorf("shop brief after the rename = %q, want %q", b, want)
		}
	}
	if after := f.blockFiles(t); !slices.Equal(after, before) {
		t.Errorf("block files after the rename = %v, want %v", after, before)
	}
}

// AC S20 (M3 exit, #165): "shop" closed, and a later workspace has the same root folder. A
// brain call links nothing by itself and asks nothing: the old items keep the closed key, and
// the new workspace's brief does not hold them. brain status notes the old workspace with the
// link command. The link itself, on request, is TestWorkspaceLink.
func TestClosedSameRootS20(t *testing.T) {
	f := newStatusFixture(t)
	fact, skill, _, own := f.oldShop(t)
	f.mustReview(t) // a brain call: it syncs the workspace map
	out := f.mustStatus(t)
	for _, want := range []string{"ws_a9 (w9 old w9): closed; same root as ws_a14 (w14 shop)",
		"link on request, from a pane in the new workspace: brain workspace link <old> <new>"} {
		if !strings.Contains(out, want) {
			t.Errorf("Status() = %q, want %q", out, want)
		}
	}
	r := f.rows(t)
	if r.keyOf(t, fact) != "ws_a9" || r.keyOf(t, skill) != "ws_a9" || r.keyOf(t, own) != "ws_a14" {
		t.Errorf("keys = %q %q %q, want ws_a9 ws_a9 ws_a14: no automatic link",
			r.keyOf(t, fact), r.keyOf(t, skill), r.keyOf(t, own))
	}
	if b := f.brief(t); strings.Contains(b, "marker-old") || strings.Contains(b, "deploy") || !strings.Contains(b, "own item") {
		t.Errorf("shop brief = %q, want its own item and none of the closed key", b)
	}
}

// AC S27 (M3 exit, #165): "shop" (w14) and w15 (the "shop-exp" of §14, "blog" in the fixture)
// share one checkout. Each has a workspace item and a workspace skill; each brief holds only
// its own.
func TestSameCheckoutS27(t *testing.T) {
	f := newFixture(t)
	shopItem := f.addActive(t, ptr("ws_a14"), "shop item marker-shop")
	expItem := f.addActive(t, ptr("ws_a15"), "exp item marker-exp")
	shopSkill := f.approvedSkill(t, skillText("deploy", "shop"))
	s := openStore(t, f.store)
	key := "ws_a15"
	pid, err := saveDraft(context.Background(), s, draft{text: skillText("release", "exp"), kind: store.KindSkill,
		scope: store.ScopeWorkspace, workspaceKey: &key, target: "workspace w15 (blog)"}, testNow)
	closeStore(t, s)
	if err != nil {
		t.Fatalf("saveDraft() = %v", err)
	}
	f.panes["w15:p1"] = herdr.Pane{ID: "w15:p1", WorkspaceID: "w15", TabID: "w15:t1"}
	if err := Review(context.Background(), f.env(f.sock, "w15:p1", "w15", &bytes.Buffer{}), []string{fmt.Sprint(pid), "--approve"}); err != nil {
		t.Fatalf("Review(--approve) from w15 = %v", err)
	}
	expSkill := *f.rows(t).proposal(t, pid).ItemID

	read := func(k string) string {
		b, err := output.Read(filepath.Join(f.store, outDir), k)
		if err != nil {
			t.Fatalf("output.Read(%s) = %v", k, err)
		}
		return b.Workspace
	}
	for _, c := range []struct {
		key        string
		want, none []string
		items      []int64
	}{
		{"ws_a14", []string{"marker-shop", f.skillFile(shopSkill, 2)}, []string{"marker-exp", "release"}, []int64{shopItem, shopSkill}},
		{"ws_a15", []string{"marker-exp", f.skillFile(expSkill, 2)}, []string{"marker-shop", "deploy"}, []int64{expItem, expSkill}},
	} {
		b := read(c.key)
		for _, w := range c.want {
			if !strings.Contains(b, w) {
				t.Errorf("%s workspace part = %q, want %q", c.key, b, w)
			}
		}
		for _, n := range c.none {
			if strings.Contains(b, n) {
				t.Errorf("%s workspace part = %q, want no %q", c.key, b, n)
			}
		}
		for _, id := range c.items {
			if k := f.rows(t).keyOf(t, id); k != c.key {
				t.Errorf("item %d key = %q, want %s", id, k, c.key)
			}
		}
	}
}

// AC S29 (M3 exit, #165): a pinned procedure (a workspace skill) with no relevant session for 6
// months stays active and is no archive candidate; the same skill unpinned is one.
func TestPinnedSkillS29(t *testing.T) {
	f := newFixture(t)
	pinned := f.approvedSkill(t, skillText("release", "pinned"))
	idle := f.approvedSkill(t, skillText("deploy", "idle"))
	if _, err := f.pin(t, fmt.Sprint(pinned)); err != nil {
		t.Fatal(err)
	}
	out := f.mustArchive(t, 183)
	if strings.Contains(out, fmt.Sprintf("item %d ", pinned)) || !strings.Contains(out, fmt.Sprintf("item %d ", idle)) {
		t.Errorf("Archive() at day 183 =\n%s\nwant item %d and not the pinned item %d", out, idle, pinned)
	}
	f.mustArchive(t, 183, "--all")
	r := f.rows(t)
	if it, _ := r.item(t, pinned); it.State != store.StateActive {
		t.Errorf("pinned skill after archive --all: %s, want active", it.State)
	}
	if it, _ := r.item(t, idle); it.State != store.StateRetired {
		t.Errorf("idle skill after archive --all: %s, want retired", it.State)
	}
}
