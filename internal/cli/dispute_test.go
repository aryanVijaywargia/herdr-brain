package cli

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/aryanVijaywargia/herdr-brain/internal/bindings"
	"github.com/aryanVijaywargia/herdr-brain/internal/brained"
	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

// gitRepo makes a temp git repo with a commit of each file in files, and makes it the root
// folder of workspace ws_a14. git in Brain code uses a scratch HOME and no user or system
// config. It returns the repo folder and a function that runs git in it for the test setup.
func (f *fixture) gitRepo(t *testing.T, files ...string) (string, func(args ...string) string) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	root := canonical(t, t.TempDir())
	git := func(args ...string) string {
		t.Helper()
		out, err := exec.Command("git", append([]string{"-C", root, "-c", "user.name=t", "-c", "user.email=t@example.com",
			"-c", "commit.gpgsign=false"}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-q")
	if err := os.MkdirAll(filepath.Join(root, "config"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range files {
		f.writeRepo(t, root, name, name)
		git("add", name)
	}
	git("commit", "-q", "-m", "add")
	f.setRoot(t, root)
	return root, git
}

func (f *fixture) writeRepo(t *testing.T, root, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// setRoot makes root the root folder of workspace ws_a14 in the store.
func (f *fixture) setRoot(t *testing.T, root string) {
	t.Helper()
	s := openStore(t, f.store)
	defer closeStore(t, s)
	write(t, s, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(context.Background(), `UPDATE workspaces SET root_folder = ? WHERE key = 'ws_a14'`, root)
		return err
	})
}

// stale runs the brained stale check of workspace ws_a14 after a session ends there.
func (f *fixture) stale(t *testing.T) {
	t.Helper()
	g := brained.NewGate(bindings.Binding{Session: "default", Socket: f.sock, Store: f.store, Kind: bindings.KindMain})
	if err := brained.Stale(context.Background(), g, "ws_a14", now); err != nil {
		t.Fatalf("Stale() = %v", err)
	}
}

// pendingDispute returns the newest pending proposal, which must be a dispute.
func (f *fixture) pendingDispute(t *testing.T) store.Proposal {
	t.Helper()
	ps := f.rows(t).proposals
	p := ps[len(ps)-1]
	if p.Status != store.ProposalPending || len(p.Dispute) == 0 {
		t.Fatalf("newest proposal = %+v, want a pending dispute", p)
	}
	return p
}

const loaderText = "The config loader is in config/load.go; run make test after a change."

// AC S9 and S72: approval anchors the file that the item names. A rename of the file makes the
// item disputed: the queue shows the anchor diff, the brief shows the item with the mark, and
// reject is refused. After keep, the mark is gone, the item is active, and the anchor of the
// renamed path is gone.
func TestDisputeKeep(t *testing.T) {
	f := newFixture(t)
	root, git := f.gitRepo(t, "config/load.go", "Makefile")
	item := f.approved(t, loaderText)
	blob := git("rev-parse", "HEAD:config/load.go")
	it, _ := f.rows(t).item(t, item)
	if want := []store.Anchor{{Root: root, Path: "config/load.go", Blob: blob}}; !reflect.DeepEqual(it.Anchors, want) {
		t.Fatalf("anchors = %+v, want %+v", it.Anchors, want)
	}

	git("mv", "config/load.go", "config/loader.go") // no commit: the work tree counts
	f.stale(t)
	it, rev := f.rows(t).item(t, item)
	if it.State != store.StateDisputed || it.CurrentRevision != 2 || rev.Text != loaderText {
		t.Fatalf("item = %+v %+v, want disputed at revision 2 (the draft is 1), text unchanged", it, rev)
	}
	p := f.pendingDispute(t)
	if out := f.mustReview(t); !strings.Contains(out, fmt.Sprintf("%d  workspace w14 (shop)  item %d disputed: config/load.go changed: %s", p.ID, item, loaderText)) {
		t.Errorf("list = %q, want the dispute", out)
	}
	diff := fmt.Sprintf("diff:\n  - config/load.go %s\n  + config/loader.go %s (moved)\n", blob[:7], blob[:7])
	out := f.mustReview(t, fmt.Sprint(p.ID))
	for _, want := range []string{diff, fmt.Sprintf("source:   the anchor check of item %d: config/load.go changed", item),
		fmt.Sprintf("actions:  brain review %d --keep | --approve --edit \"<text>\" | --retire | --later", p.ID)} {
		if !strings.Contains(out, want) {
			t.Errorf("show = %q, want %q", out, want)
		}
	}
	mark := loaderText + " (disputed: config/load.go changed on 2026-09-28)"
	if b := f.brief(t); !strings.Contains(b, mark) {
		t.Errorf("brief = %q, want %q", b, mark)
	}
	if _, err := f.review(t, fmt.Sprint(p.ID), "--reject"); err == nil || !strings.Contains(err.Error(), "is a dispute") {
		t.Errorf("reject of a dispute = %v, want an error", err)
	}

	if out := f.mustReview(t, fmt.Sprint(p.ID), "--keep"); !strings.Contains(out, fmt.Sprintf("item %d kept", item)) {
		t.Errorf("keep printed %q", out)
	}
	it, rev = f.rows(t).item(t, item)
	if it.State != store.StateActive || it.CurrentRevision != 3 || rev.Text != loaderText || rev.Author != store.AuthorYou ||
		len(it.Anchors) != 0 {
		t.Errorf("item = %+v %+v, want active at revision 3 with no anchor", it, rev)
	}
	if p := f.rows(t).proposal(t, p.ID); p.Status != store.ProposalApproved {
		t.Errorf("proposal = %+v, want approved", p)
	}
	if b := f.brief(t); strings.Contains(b, "disputed") || !strings.Contains(b, loaderText) {
		t.Errorf("brief = %q, want the item with no mark", b)
	}
	f.stale(t)
	if it, _ := f.rows(t).item(t, item); it.State != store.StateActive {
		t.Errorf("item = %+v after a second check, want active", it)
	}
}

// Keep stores the new blob of a changed path, and the next check finds no change.
func TestDisputeKeepChanged(t *testing.T) {
	f := newFixture(t)
	root, git := f.gitRepo(t, "config/load.go", "Makefile")
	item := f.approved(t, loaderText)
	f.writeRepo(t, root, "config/load.go", "new content")
	git("commit", "-q", "-am", "change")
	f.stale(t)
	p := f.pendingDispute(t)
	blob := git("rev-parse", "HEAD:config/load.go")
	if !strings.Contains(p.Diff, "+ config/load.go "+blob[:7]+" (changed)") {
		t.Errorf("diff = %q, want the changed blob", p.Diff)
	}
	f.mustReview(t, fmt.Sprint(p.ID), "--keep")
	it, _ := f.rows(t).item(t, item)
	if want := []store.Anchor{{Root: root, Path: "config/load.go", Blob: blob}}; !reflect.DeepEqual(it.Anchors, want) {
		t.Errorf("anchors = %+v, want %+v", it.Anchors, want)
	}
	f.stale(t)
	if it, _ := f.rows(t).item(t, item); it.State != store.StateActive {
		t.Errorf("item = %+v after a check with no change, want active", it)
	}
}

// Edit then approve: the new text gets its own anchors.
func TestDisputeEdit(t *testing.T) {
	f := newFixture(t)
	root, git := f.gitRepo(t, "config/load.go")
	item := f.approved(t, loaderText)
	git("mv", "config/load.go", "config/loader.go")
	git("commit", "-q", "-m", "rename")
	f.stale(t)
	p := f.pendingDispute(t)
	edit := "The config loader is in config/loader.go."
	f.mustReview(t, fmt.Sprint(p.ID), "--approve", "--edit", edit)
	it, rev := f.rows(t).item(t, item)
	want := []store.Anchor{{Root: root, Path: "config/loader.go", Blob: git("rev-parse", "HEAD:config/loader.go")}}
	if it.State != store.StateActive || rev.Text != edit || !reflect.DeepEqual(it.Anchors, want) {
		t.Errorf("item = %+v %+v, want active with the edit and anchors %+v", it, rev, want)
	}
	if b := f.brief(t); !strings.Contains(b, edit+"\n") {
		t.Errorf("brief = %q, want the edit with no mark", b)
	}
}

// AC: retire from the dispute keeps the history. brain review --retired lists the item, and
// --restore makes it active again, with the anchors of its text now.
func TestDisputeRetire(t *testing.T) {
	f := newFixture(t)
	root, git := f.gitRepo(t, "config/load.go")
	item := f.approved(t, loaderText)
	git("rm", "-q", "config/load.go")
	git("commit", "-q", "-m", "drop")
	f.stale(t)
	p := f.pendingDispute(t)
	if !strings.Contains(p.Diff, "+ (deleted)") {
		t.Errorf("diff = %q, want a deleted anchor", p.Diff)
	}
	if out := f.mustReview(t, fmt.Sprint(p.ID), "--retire"); !strings.Contains(out, fmt.Sprintf("item %d retired", item)) {
		t.Errorf("retire printed %q", out)
	}
	r := f.rows(t)
	it, rev := r.item(t, item)
	if it.State != store.StateRetired || it.CurrentRevision != 3 || rev.Text != loaderText {
		t.Errorf("item = %+v %+v, want retired at revision 3 with its text", it, rev)
	}
	if b := f.brief(t); strings.Contains(b, loaderText) {
		t.Errorf("brief = %q, want no retired item", b)
	}
	if out := f.mustReview(t, "--retired"); !strings.Contains(out, fmt.Sprintf("item %d  workspace w14 (shop)  %s", item, loaderText)) {
		t.Errorf("--retired = %q, want the item", out)
	}
	if err := os.MkdirAll(filepath.Join(root, "config"), 0o700); err != nil {
		t.Fatal(err)
	}
	f.writeRepo(t, root, "config/load.go", "back")
	git("add", "config/load.go")
	git("commit", "-q", "-m", "back")
	f.mustReview(t, "--restore", fmt.Sprint(item))
	it, _ = f.rows(t).item(t, item)
	want := []store.Anchor{{Root: root, Path: "config/load.go", Blob: git("rev-parse", "HEAD:config/load.go")}}
	if it.State != store.StateActive || it.CurrentRevision != 4 || !reflect.DeepEqual(it.Anchors, want) {
		t.Errorf("item = %+v, want active at revision 4 with anchors %+v", it, want)
	}
	if _, err := f.review(t, fmt.Sprint(p.ID), "--keep"); err == nil || !strings.Contains(err.Error(), "conflict") {
		t.Errorf("keep of a decided dispute = %v, want a conflict", err)
	}
}

// AC: a workspace root that is not a git repo stores no anchor and gives no error. A global
// item gets no anchor either (E16-S6 open point). Keep and retire take only a dispute.
func TestNoAnchors(t *testing.T) {
	f := newFixture(t)
	root := canonical(t, t.TempDir())
	f.writeRepo(t, root, "Makefile", "test:")
	f.setRoot(t, root)
	item := f.approved(t, "Run make test; see Makefile.")
	if it, _ := f.rows(t).item(t, item); len(it.Anchors) != 0 {
		t.Errorf("anchors = %+v outside git, want none", it.Anchors)
	}
	f.stale(t)

	_, _ = f.gitRepo(t, "Makefile")
	gp := f.remember(t, "--global", "Run make test; see Makefile.")
	f.mustReview(t, fmt.Sprint(gp), "--approve")
	if it, _ := f.rows(t).item(t, *f.rows(t).proposal(t, gp).ItemID); len(it.Anchors) != 0 {
		t.Errorf("anchors of a global item = %+v, want none", it.Anchors)
	}
	pid := f.remember(t, "a draft")
	for _, action := range []string{"--keep", "--retire"} {
		if _, err := f.review(t, fmt.Sprint(pid), action); err == nil || !strings.Contains(err.Error(), "not a dispute") {
			t.Errorf("%s of a draft proposal = %v, want an error", action, err)
		}
	}
}
