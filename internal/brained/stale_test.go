package brained

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aryanVijaywargia/herdr-brain/internal/bindings"
	"github.com/aryanVijaywargia/herdr-brain/internal/output"
	"github.com/aryanVijaywargia/herdr-brain/internal/paths"
	"github.com/aryanVijaywargia/herdr-brain/internal/signal"
	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

// git runs git args in dir for the test setup, with no user or system config.
func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@example.com",
		"-c", "commit.gpgsign=false"}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull, "HOME="+t.TempDir())
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// gitEnv makes git in Brain code use a scratch HOME and no user or system config.
func gitEnv(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
}

// repo makes a git repo with one commit of each file in files, and returns its folder.
func repo(t *testing.T, files ...string) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	git(t, dir, "init", "-q")
	for _, f := range files {
		if err := os.WriteFile(filepath.Join(dir, f), []byte(f), 0o600); err != nil {
			t.Fatal(err)
		}
		git(t, dir, "add", f)
	}
	git(t, dir, "commit", "-q", "-m", "add")
	return dir
}

// staleStore holds workspace ws_1 with root root and one active item for each text, anchored
// at approval. It returns the item ids.
func staleStore(t *testing.T, dir, root string, texts ...string) []int64 {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, dir, busyTimeout)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close() //nolint:errcheck // test
	key := "ws_1"
	var ids []int64
	err = st.Write(ctx, func(tx *sql.Tx) error {
		if err := store.InsertWorkspace(ctx, tx, store.Workspace{Key: key, HerdrSession: "s1", HerdrWorkspaceID: "w1",
			Label: "shop", RootFolder: &root, Status: store.WorkspaceLive, LastSeen: time.Now()}); err != nil {
			return err
		}
		for i, text := range texts {
			r := store.ItemRevision{Text: text, Scope: store.ScopeWorkspace, WorkspaceKey: &key, State: store.StateActive,
				Author: store.AuthorYou, Origin: store.OriginReview, Created: time.Now()}
			id, _, err := store.CreateItem(ctx, tx, store.Item{ClaimID: fmt.Sprintf("c%d", i+1), Kind: store.KindFact,
				Created: time.Now()}, r)
			if err != nil {
				return err
			}
			anchors, err := signal.FindAnchors(ctx, root, text)
			if err != nil {
				return err
			}
			if err := store.SetAnchors(ctx, tx, id, anchors); err != nil {
				return err
			}
			ids = append(ids, id)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := output.Build(ctx, st, filepath.Join(dir, outDir), nil, time.Now); err != nil {
		t.Fatal(err)
	}
	return ids
}

// staleRows reads item id, its current revision and the proposals of the store in dir.
func staleRows(t *testing.T, dir string, id int64) (it store.Item, revisions int, ps []store.Proposal) {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, dir, busyTimeout)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close() //nolint:errcheck // test
	err = st.Write(ctx, func(tx *sql.Tx) (err error) {
		if it, err = store.ItemByID(ctx, tx, id); err != nil {
			return err
		}
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM item_revisions WHERE item_id = ?`, id).Scan(&revisions); err != nil {
			return err
		}
		ids, err := store.PendingProposalIDs(ctx, tx)
		for _, pid := range ids {
			p, err := store.ProposalByID(ctx, tx, pid)
			if err != nil {
				return err
			}
			ps = append(ps, p)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return it, revisions, ps
}

// AC in brained: a renamed file makes its item disputed, with the anchor diff in
// the queue and the mark in the built brief. Brain does not change the item: no new revision,
// and the next check adds no second proposal.
func TestStale(t *testing.T) {
	gitEnv(t)
	ctx := context.Background()
	root := repo(t, "load.go", "keep.go")
	dir := t.TempDir()
	id := staleStore(t, dir, root, "The loader is in load.go, next to keep.go.")[0]
	g := NewGate(bindings.Binding{Session: "s1", Store: dir, Kind: bindings.KindMain})
	now := func() time.Time { return time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC) }
	if err := Stale(ctx, g, "ws_1", now); err != nil {
		t.Fatal(err)
	}
	if it, _, ps := staleRows(t, dir, id); it.State != store.StateActive || len(ps) != 0 {
		t.Fatalf("no change: item %+v, proposals %+v, want active and no proposal", it, ps)
	}

	blob := git(t, root, "rev-parse", "HEAD:load.go")
	git(t, root, "mv", "load.go", "loader.go")
	git(t, root, "commit", "-q", "-m", "rename")
	for range 2 {
		if err := Stale(ctx, g, "", now); err != nil {
			t.Fatal(err)
		}
	}
	it, revisions, ps := staleRows(t, dir, id)
	if it.State != store.StateDisputed || it.CurrentRevision != 1 || revisions != 1 || len(it.Anchors) != 2 {
		t.Errorf("item = %+v with %d revisions, want disputed at revision 1, with its anchors", it, revisions)
	}
	wantDiff := "- load.go " + blob[:7] + "\n+ loader.go " + blob[:7] + " (moved)"
	if len(ps) != 1 || *ps[0].ItemID != id || ps[0].Diff != wantDiff || !slices.Equal(ps[0].Dispute, []string{"load.go"}) ||
		ps[0].Text != "The loader is in load.go, next to keep.go." || ps[0].Target != "workspace w1 (shop)" {
		t.Errorf("proposals = %+v, want one dispute with diff %q", ps, wantDiff)
	}
	b, err := output.Read(filepath.Join(dir, outDir), "ws_1")
	if err != nil {
		t.Fatal(err)
	}
	if want := "- [1] The loader is in load.go, next to keep.go. (disputed: load.go moved on 2026-10-03)\n"; !strings.Contains(b.Workspace, want) {
		t.Errorf("brief workspace part = %q, want %q", b.Workspace, want)
	}
}

// AC (#252): the anchor file of one item is deleted, and the anchor file of another item
// gets a normal edit. The first item is disputed with the deleted mark until you decide. The
// second stays active with no mark and no review task, and its anchor gets the new blob, so
// the next check finds nothing to do.
func TestStaleDeletedAndEdited(t *testing.T) {
	gitEnv(t)
	ctx := context.Background()
	root := repo(t, "drop.go", "edit.go")
	dir := t.TempDir()
	ids := staleStore(t, dir, root, "The old helpers are in drop.go.", "Every function in edit.go has a comment.")
	g := NewGate(bindings.Binding{Session: "s1", Store: dir, Kind: bindings.KindMain})
	now := func() time.Time { return time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC) }
	git(t, root, "rm", "-q", "drop.go")
	if err := os.WriteFile(filepath.Join(root, "edit.go"), []byte("edit.go with a new function"), 0o600); err != nil {
		t.Fatal(err)
	}
	git(t, root, "commit", "-q", "-am", "drop and edit")
	for range 2 {
		if err := Stale(ctx, g, "", now); err != nil {
			t.Fatal(err)
		}
	}
	dropped, _, ps := staleRows(t, dir, ids[0])
	if dropped.State != store.StateDisputed || len(ps) != 1 || *ps[0].ItemID != ids[0] ||
		!slices.Equal(ps[0].Dispute, []string{"drop.go"}) || !strings.HasSuffix(ps[0].Diff, "+ (deleted)") {
		t.Errorf("item %+v, proposals %+v, want the first item disputed with one deleted dispute", dropped, ps)
	}
	edited, revisions, _ := staleRows(t, dir, ids[1])
	blob := git(t, root, "rev-parse", "HEAD:edit.go")
	if want := []store.Anchor{{Root: root, Path: "edit.go", Blob: blob}}; edited.State != store.StateActive ||
		revisions != 1 || !slices.Equal(edited.Anchors, want) {
		t.Errorf("item = %+v with %d revisions, want active at revision 1 with anchors %+v", edited, revisions, want)
	}
	b, err := output.Read(filepath.Join(dir, outDir), "ws_1")
	if err != nil {
		t.Fatal(err)
	}
	want := "- [1] The old helpers are in drop.go. (disputed: drop.go deleted on 2026-10-07)\n" +
		"- [2] Every function in edit.go has a comment.\n"
	if !strings.Contains(b.Workspace, want) {
		t.Errorf("brief workspace part = %q, want %q", b.Workspace, want)
	}
}

// AC 5 (#252): an item that an older build disputed for a normal edit is active again at the
// next check, with no mark, its dispute closed as superseded and the new blob as its anchor.
// An item disputed for a deleted file stays disputed, also when the file is back.
func TestStaleOldEditDispute(t *testing.T) {
	gitEnv(t)
	ctx := context.Background()
	root := repo(t, "edit.go", "drop.go")
	dir := t.TempDir()
	ids := staleStore(t, dir, root, "The loader is in edit.go.", "The old helpers are in drop.go.")
	if err := os.WriteFile(filepath.Join(root, "edit.go"), []byte("new content"), 0o600); err != nil {
		t.Fatal(err)
	}
	git(t, root, "rm", "-q", "drop.go")
	git(t, root, "commit", "-q", "-am", "edit and drop")
	blob := git(t, root, "rev-parse", "HEAD:edit.go")
	st, err := store.Open(ctx, dir, busyTimeout)
	if err != nil {
		t.Fatal(err)
	}
	var old int64
	err = st.Write(ctx, func(tx *sql.Tx) error {
		// The dispute of an older build: a normal edit of edit.go.
		it, err := store.ItemByID(ctx, tx, ids[0])
		if err != nil {
			return err
		}
		base := it.CurrentRevision
		old, err = store.Dispute(ctx, tx, store.Proposal{ItemID: &it.ID, BaseRevision: &base, Text: "The loader is in edit.go.",
			Scope: store.ScopeWorkspace, Target: "workspace w1 (shop)", Diff: "- edit.go 1234567\n+ edit.go " + blob[:7] + " (changed)",
			Dispute: []string{"edit.go"}, Status: store.ProposalPending, Created: time.Now()})
		return err
	})
	if err := errors.Join(err, st.Close()); err != nil || old == 0 {
		t.Fatalf("old dispute = %d, %v", old, err)
	}
	g := NewGate(bindings.Binding{Session: "s1", Store: dir, Kind: bindings.KindMain})
	now := func() time.Time { return time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC) }
	if err := Stale(ctx, g, "", now); err != nil {
		t.Fatal(err)
	}
	it, revisions, ps := staleRows(t, dir, ids[0])
	if want := []store.Anchor{{Root: root, Path: "edit.go", Blob: blob}}; it.State != store.StateActive ||
		revisions != 1 || !slices.Equal(it.Anchors, want) {
		t.Errorf("item = %+v with %d revisions, want active at revision 1 with anchors %+v", it, revisions, want)
	}
	if len(ps) != 1 || *ps[0].ItemID != ids[1] {
		t.Errorf("pending proposals = %+v, want only the dispute of the deleted file", ps)
	}
	if it, _, _ := staleRows(t, dir, ids[1]); it.State != store.StateDisputed {
		t.Errorf("item = %+v, want disputed: its file is deleted", it)
	}
	b, err := output.Read(filepath.Join(dir, outDir), "ws_1")
	if err != nil {
		t.Fatal(err)
	}
	if want := "- [1] The loader is in edit.go.\n"; !strings.Contains(b.Workspace, want) {
		t.Errorf("brief workspace part = %q, want %q", b.Workspace, want)
	}

	// The deleted file is back: its dispute waits for your decision.
	git(t, root, "checkout", "-q", "HEAD~1", "--", "drop.go")
	git(t, root, "commit", "-q", "-m", "restore")
	if err := Stale(ctx, g, "", now); err != nil {
		t.Fatal(err)
	}
	if it, _, ps := staleRows(t, dir, ids[1]); it.State != store.StateDisputed || len(ps) != 1 {
		t.Errorf("item = %+v, proposals %+v, want disputed with its dispute: a restored file is not your decision", it, ps)
	}
}

// AC: a workspace root that is not a git repo stores no anchor, and the check gives no error.
func TestStaleNoRepo(t *testing.T) {
	gitEnv(t)
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "load.go"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	id := staleStore(t, dir, root, "The loader is in load.go.")[0]
	g := NewGate(bindings.Binding{Session: "s1", Store: dir, Kind: bindings.KindMain})
	if err := Stale(context.Background(), g, "", time.Now); err != nil {
		t.Fatal(err)
	}
	if it, _, ps := staleRows(t, dir, id); it.State != store.StateActive || len(it.Anchors) != 0 || len(ps) != 0 {
		t.Errorf("item %+v, proposals %+v, want active with no anchor and no proposal", it, ps)
	}
}

// The end of a captured session runs the stale check of its workspace. An end with no
// session runs none.
func TestCaptureEndStale(t *testing.T) {
	k := newCapture(t, "s1", t.TempDir())
	proj, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(filepath.Dir(k.f.path), "session.json"), map[string]any{"version": 3,
		"workspaces": []map[string]string{{"id": "w1", "identity_cwd": proj}}})
	writeFile(t, paths.WorkspaceMap(k.gate.Binding.Store), map[string]any{"version": 1, "entries": []map[string]string{
		{"key": "ws_1", "session": "s1", "workspace_id": "w1", "label": "shop", "project": proj}}})
	var keys []string
	k.c.stale = func(_ context.Context, g Gate, key string, _ func() time.Time) error {
		if g.Binding.Store != k.gate.Binding.Store {
			t.Errorf("stale check of store %s, want %s", g.Binding.Store, k.gate.Binding.Store)
		}
		keys = append(keys, key)
		return nil
	}
	k.pane("w1:p1", "codex", codexRun2)
	k.reads(t, "w1:p1", "codex-s31-0-base.txt")
	k.must(t, End, "w1:p2", "", GapNone) // no session
	k.must(t, Baseline, "w1:p1", "", GapNone)
	k.must(t, End, "w1:p1", "", GapNone)
	if !slices.Equal(keys, []string{"ws_1"}) {
		t.Errorf("stale checks = %q, want one of ws_1", keys)
	}
}

func writeFile(t *testing.T, path string, v any) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// A tick starts the stale check of every workspace of the bound store (once a day). No
// second check starts less than staleEvery after the last start.
func TestStaleTrigger(t *testing.T) {
	h := newHarness(t, bindings.KindMain)
	checks := make(chan Gate, 4)
	h.d.stale = func(_ context.Context, g Gate) error {
		checks <- g
		return nil
	}
	h.start(t)
	h.check(t)
	select {
	case g := <-checks:
		if g.Binding.Store != h.store {
			t.Errorf("check store = %s, want %s", g.Binding.Store, h.store)
		}
	case <-time.After(wait):
		t.Fatal("no stale check")
	}
	h.check(t)
	h.check(t)
	select {
	case <-checks:
		t.Error("a second check started within a day")
	default:
	}
}
