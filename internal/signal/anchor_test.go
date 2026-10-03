package signal

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

func TestPathWords(t *testing.T) {
	got := pathWords("Run `make test` in ./cmd/brain/main.go:12:3, see (docs/a.md). Not /etc/x, ~/y, ../z or https://e.com/a; keep .gitignore.")
	want := []string{"Run", "make", "test", "in", "cmd/brain/main.go", "see", "docs/a.md", "Not", "or", "keep", ".gitignore"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("pathWords = %q, want %q", got, want)
	}
}

func TestFindAnchors(t *testing.T) {
	gitEnv(t)
	ctx := context.Background()
	dir := t.TempDir()
	if as, err := FindAnchors(ctx, dir, "see a.txt"); err != nil || as != nil {
		t.Errorf("FindAnchors(not a repo) = %v, %v, want none and no error", as, err)
	}
	if as, err := FindAnchors(ctx, filepath.Join(dir, "gone"), "see a.txt"); err != nil || as != nil {
		t.Errorf("FindAnchors(no folder) = %v, %v, want none and no error", as, err)
	}
	run(t, dir, "init", "-q")
	if as, err := FindAnchors(ctx, dir, "see a.txt"); err != nil || as != nil {
		t.Errorf("FindAnchors(no commit) = %v, %v, want none", as, err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	commit(t, dir, "a.txt", "a", "add a")
	commit(t, dir, "sub/b.txt", "b", "add b")
	if err := os.WriteFile(filepath.Join(dir, "untracked.txt"), []byte("u"), 0o600); err != nil {
		t.Fatal(err)
	}
	top := run(t, dir, "rev-parse", "--show-toplevel")
	blob := func(p string) string { return run(t, dir, "rev-parse", "HEAD:"+p) }

	as, err := FindAnchors(ctx, dir, "Edit `a.txt` and sub/b.txt:4, then a.txt again; not untracked.txt or c.txt.")
	if err != nil {
		t.Fatal(err)
	}
	want := []store.Anchor{{Root: top, Path: "a.txt", Blob: blob("a.txt")}, {Root: top, Path: "sub/b.txt", Blob: blob("sub/b.txt")}}
	if !reflect.DeepEqual(as, want) {
		t.Errorf("FindAnchors = %+v, want %+v", as, want)
	}
	// In a sub folder of the repo, a path is relative to that folder, and the anchor holds the
	// path in the repo.
	as, err = FindAnchors(ctx, filepath.Join(dir, "sub"), "b.txt, not a.txt")
	if err != nil {
		t.Fatal(err)
	}
	if want := []store.Anchor{{Root: top, Path: "sub/b.txt", Blob: blob("sub/b.txt")}}; !reflect.DeepEqual(as, want) {
		t.Errorf("FindAnchors(sub folder) = %+v, want %+v", as, want)
	}
	if as, err := FindAnchors(ctx, "", "a.txt"); err != nil || as != nil {
		t.Errorf("FindAnchors(no root) = %v, %v, want none", as, err)
	}
}

func TestCheckAnchors(t *testing.T) {
	gitEnv(t)
	ctx := context.Background()
	dir := t.TempDir()
	run(t, dir, "init", "-q")
	commit(t, dir, "same.txt", "s", "add same")
	commit(t, dir, "edit.txt", "e1", "add edit")
	commit(t, dir, "move.txt", "m", "add move")
	commit(t, dir, "drop.txt", "d", "add drop")
	as, err := FindAnchors(ctx, dir, "same.txt edit.txt move.txt drop.txt")
	if err != nil || len(as) != 4 {
		t.Fatalf("FindAnchors = %v, %v, want 4 anchors", as, err)
	}
	if changes, now, err := CheckAnchors(ctx, as); err != nil || changes != nil || !reflect.DeepEqual(now, as) {
		t.Fatalf("CheckAnchors(no change) = %v, %v, %v, want no change", changes, now, err)
	}

	commit(t, dir, "edit.txt", "e2", "change edit")
	run(t, dir, "mv", "move.txt", "moved.txt")
	run(t, dir, "rm", "-q", "drop.txt")
	run(t, dir, "commit", "-q", "-m", "move and drop")
	changes, now, err := CheckAnchors(ctx, as)
	if err != nil {
		t.Fatal(err)
	}
	edited := run(t, dir, "rev-parse", "HEAD:edit.txt")
	wantChanges := []AnchorChange{{Anchor: as[1], Blob: edited}, {Anchor: as[2], MovedTo: "moved.txt"}, {Anchor: as[3]}}
	if !reflect.DeepEqual(changes, wantChanges) {
		t.Errorf("changes = %+v, want %+v", changes, wantChanges)
	}
	// Keep stores the new blob, and drops the paths that are gone.
	wantNow := []store.Anchor{as[0], {Root: as[1].Root, Path: "edit.txt", Blob: edited}}
	if !reflect.DeepEqual(now, wantNow) {
		t.Errorf("now = %+v, want %+v", now, wantNow)
	}
	wantDiff := "- edit.txt " + as[1].Blob[:7] + "\n+ edit.txt " + edited[:7] + " (changed)\n" +
		"- move.txt " + as[2].Blob[:7] + "\n+ moved.txt " + as[2].Blob[:7] + " (moved)\n" +
		"- drop.txt " + as[3].Blob[:7] + "\n+ (deleted)"
	if diff := AnchorDiff(changes); diff != wantDiff {
		t.Errorf("AnchorDiff =\n%s\nwant\n%s", diff, wantDiff)
	}

	// A repo that is gone holds none of its paths.
	gone := []store.Anchor{{Root: filepath.Join(dir, "nothing"), Path: "same.txt", Blob: as[0].Blob}}
	changes, now, err = CheckAnchors(ctx, gone)
	if err != nil || len(changes) != 1 || changes[0].Blob != "" || now != nil {
		t.Errorf("CheckAnchors(gone repo) = %+v, %v, %v, want one deleted anchor", changes, now, err)
	}
}

// A saved edit, a git mv and a deletion count before a commit: the anchors follow the work
// tree of the tracked files.
func TestCheckAnchorsUncommitted(t *testing.T) {
	gitEnv(t)
	ctx := context.Background()
	dir := t.TempDir()
	run(t, dir, "init", "-q")
	commit(t, dir, "edit.txt", "e1", "add edit")
	commit(t, dir, "move.txt", "m", "add move")
	commit(t, dir, "drop.txt", "d", "add drop")
	as, err := FindAnchors(ctx, dir, "edit.txt move.txt drop.txt")
	if err != nil || len(as) != 3 {
		t.Fatalf("FindAnchors = %v, %v, want 3 anchors", as, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "edit.txt"), []byte("e2"), 0o600); err != nil {
		t.Fatal(err)
	}
	run(t, dir, "mv", "move.txt", "moved.txt")
	if err := os.Remove(filepath.Join(dir, "drop.txt")); err != nil {
		t.Fatal(err)
	}
	changes, now, err := CheckAnchors(ctx, as)
	if err != nil {
		t.Fatal(err)
	}
	edited := run(t, dir, "hash-object", "edit.txt")
	want := []AnchorChange{{Anchor: as[0], Blob: edited}, {Anchor: as[1], MovedTo: "moved.txt"}, {Anchor: as[2]}}
	if !reflect.DeepEqual(changes, want) {
		t.Errorf("changes = %+v, want %+v", changes, want)
	}
	if wantNow := []store.Anchor{{Root: as[0].Root, Path: "edit.txt", Blob: edited}}; !reflect.DeepEqual(now, wantNow) {
		t.Errorf("now = %+v, want %+v", now, wantNow)
	}
	if n := run(t, dir, "count-objects"); !strings.HasPrefix(n, "9 objects") {
		t.Errorf("count-objects = %q, want the 9 objects of the 3 commits: the check writes none", n)
	}
}
