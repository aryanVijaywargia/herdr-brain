package cli

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/aryanVijaywargia/herdr-brain/internal/herdr"
	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

// linked adds global item "use npm" and workspace item "use pnpm" of ws_a14, and returns
// their ids as text.
func (f *fixture) linked(t *testing.T) (global, ws string) {
	t.Helper()
	return fmt.Sprint(f.addActive(t, nil, "use npm")), fmt.Sprint(f.addActive(t, ptr("ws_a14"), "use pnpm"))
}

// AC (S67, linked): the link is on the workspace item's revision, with the global item's
// revision, and the brief shows the exception mark. A second link does nothing.
func TestExceptionLink(t *testing.T) {
	f := newFixture(t)
	g, ws := f.linked(t)
	if out := f.mustReview(t, "--item", ws, "--exception-to", g); !strings.Contains(out, "is an exception to global item "+g+" at revision 2") {
		t.Errorf("link printed %q", out)
	}
	_, rev := f.rows(t).item(t, 3)
	if rev.Revision != 2 || *rev.ExceptionToItem != 2 || *rev.ExceptionToRevision != 1 || rev.Author != store.AuthorYou {
		t.Errorf("revision = %+v, want revision 2 linked to item 2 revision 1", rev)
	}
	if b := f.brief(t); !strings.Contains(b, "use pnpm (exception to global item 2)") {
		t.Errorf("brief = %q, want the exception mark", b)
	}
	if out := f.mustReview(t, "--item", ws, "--exception-to", g); !strings.Contains(out, "already") {
		t.Errorf("second link printed %q, want already", out)
	}
	if it, _ := f.rows(t).item(t, 3); it.CurrentRevision != 2 {
		t.Errorf("second link wrote revision %d", it.CurrentRevision)
	}
}

// AC (S67, not linked): with no link the brief shows both items with no mark, so the
// precedence line of the brief frame applies (E6 tests the frame).
func TestNotLinked(t *testing.T) {
	f := newFixture(t)
	f.linked(t)
	b := f.brief(t)
	if !strings.Contains(b, "- [2] use npm\n") || !strings.Contains(b, "- [3] use pnpm\n") {
		t.Errorf("brief = %q, want both items with no mark", b)
	}
}

// AC (S67, refused): a link to a global item with the permission warning writes nothing.
// A conflict mark stays possible.
func TestExceptionRefused(t *testing.T) {
	f := newFixture(t)
	g := fmt.Sprint(f.addActive(t, nil, "never run git push --force"))
	ws := fmt.Sprint(f.addActive(t, ptr("ws_a14"), "force push is fine here"))
	before := f.rows(t)
	if _, err := f.review(t, "--item", ws, "--exception-to", g); err == nil || !strings.Contains(err.Error(), "cannot be an exception") {
		t.Errorf("link = %v, want refused", err)
	}
	if after := f.rows(t); !reflect.DeepEqual(after, before) {
		t.Errorf("a refused link changed the store")
	}
	f.mustReview(t, "--item", ws, "--conflicts-with", g)
	if b := f.brief(t); !strings.Contains(b, "(conflicts with 2; ask the user)") || strings.Contains(b, "(exception to") {
		t.Errorf("brief = %q, want the conflict mark only", b)
	}
}

// AC 1 (#388, run 7 R7-1): a global item whose claim has the permission flag refuses an
// exception, also when its text has no permission word; the refusal writes nothing.
func TestExceptionRefusedFlaggedClaim(t *testing.T) {
	const rule = "Never add a new npm dependency without asking the user first."
	if permissionWords.MatchString(rule) {
		t.Fatalf("%q has a permission word; the case needs the flag only", rule)
	}
	f := newFixture(t)
	g := f.addActive(t, nil, rule)
	ws := fmt.Sprint(f.addActive(t, ptr("ws_a14"), "the user authorizes adding dev dependencies for tests without asking first"))
	ctx := context.Background()
	s := openStore(t, f.store)
	write(t, s, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO claims (id, created, touches_permission)
			SELECT claim_id, created, 1 FROM items WHERE id = ?`, g)
		return err
	})
	closeStore(t, s)
	before := f.rows(t)
	_, err := f.review(t, "--item", ws, "--exception-to", fmt.Sprint(g))
	if want := fmt.Sprintf("mark a conflict with --conflicts-with %d; nothing was written", g); err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("link = %v, want refused with %q", err, want)
	}
	if after := f.rows(t); !reflect.DeepEqual(after, before) {
		t.Errorf("a refused link changed the store")
	}
}

// AC: a new revision of the global item sends the link back to review: the brief drops the
// mark, review lists the link, and a new link names the new revision.
func TestLinkBackToReview(t *testing.T) {
	f := newFixture(t)
	g, ws := f.linked(t)
	f.mustReview(t, "--item", ws, "--exception-to", g)
	if _, err := f.pin(t, g); err != nil {
		t.Fatal(err)
	}
	if b := f.brief(t); strings.Contains(b, "(exception to") {
		t.Errorf("brief = %q, want no mark after the global item changed", b)
	}
	if out := f.mustReview(t); !strings.Contains(out, "item 3  was an exception to item 2 revision 1, which changed") {
		t.Errorf("review list = %q, want the stale link", out)
	}
	f.mustReview(t, "--item", ws, "--exception-to", g)
	if _, rev := f.rows(t).item(t, 3); *rev.ExceptionToRevision != 2 {
		t.Errorf("new link = %+v, want revision 2 of item 2", rev)
	}
	if b := f.brief(t); !strings.Contains(b, "(exception to global item 2)") {
		t.Errorf("brief = %q, want the mark again", b)
	}
	if out := f.mustReview(t); strings.Contains(out, "was an exception") {
		t.Errorf("review list = %q, want no stale link", out)
	}
}

// A conflict mark and an exception between the same two items replace each other, and undo
// takes a link back.
func TestConflictMark(t *testing.T) {
	f := newFixture(t)
	g, ws := f.linked(t)
	steps := []struct {
		args      []string
		exception bool
		conflicts []int64
	}{
		{[]string{"--conflicts-with", g}, false, []int64{2}},
		{[]string{"--exception-to", g}, true, []int64{}},
		{[]string{"--conflicts-with", g}, false, []int64{2}},
	}
	for _, s := range steps {
		f.mustReview(t, append([]string{"--item", ws}, s.args...)...)
		_, rev := f.rows(t).item(t, 3)
		if (rev.ExceptionToItem != nil) != s.exception || !slices.Equal(rev.ConflictsWith, s.conflicts) {
			t.Errorf("after %q revision = %+v, want exception %v and conflicts %v", s.args, rev, s.exception, s.conflicts)
		}
	}
	f.mustUndo(t, ws)
	if _, rev := f.rows(t).item(t, 3); rev.ExceptionToItem == nil || len(rev.ConflictsWith) != 0 {
		t.Errorf("after undo revision = %+v, want the exception back", rev)
	}
}

// A workspace item that goes global loses its exception link, and undo brings it back.
func TestPromoteDropsLink(t *testing.T) {
	f := newFixture(t)
	g, ws := f.linked(t)
	f.mustReview(t, "--item", ws, "--exception-to", g)
	f.mustReview(t, "--item", ws, "--edit", "use pnpm everywhere")
	r := f.rows(t)
	f.mustReview(t, fmt.Sprint(r.proposals[len(r.proposals)-1].ID), "--approve", "--scope", "global")
	if _, rev := f.rows(t).item(t, 3); rev.Scope != store.ScopeGlobal || rev.ExceptionToItem != nil || rev.ExceptionToRevision != nil {
		t.Errorf("promoted revision = %+v, want global with no exception link", rev)
	}
	if b := f.brief(t); strings.Contains(b, "(exception to") {
		t.Errorf("brief = %q, want no exception mark", b)
	}
	f.mustUndo(t, ws)
	if _, rev := f.rows(t).item(t, 3); rev.Scope != store.ScopeWorkspace || rev.ExceptionToItem == nil {
		t.Errorf("after undo revision = %+v, want the workspace item with its link", rev)
	}
}

func TestLinkErrors(t *testing.T) {
	f := newFixture(t)
	g, ws := f.linked(t)
	other := fmt.Sprint(f.addActive(t, ptr("ws_a15"), "blog item"))
	pid := f.remember(t, "a draft")
	draft := fmt.Sprint(*f.rows(t).proposal(t, pid).ItemID)
	for _, c := range []struct {
		args    []string
		wantErr string
	}{
		{[]string{"--item", g, "--exception-to", "1"}, "starts from an active workspace item"},
		{[]string{"--item", draft, "--exception-to", g}, "starts from an active workspace item"},
		{[]string{"--item", ws, "--exception-to", ws}, "goes to an active global item"},
		{[]string{"--item", ws, "--conflicts-with", "99"}, "item 99: not found in this workspace"},
		{[]string{"--item", other, "--exception-to", g}, "not found in this workspace"},
		{[]string{"--item", ws, "--exception-to", other}, "not found in this workspace"},
	} {
		if _, err := f.review(t, c.args...); err == nil || !strings.Contains(err.Error(), c.wantErr) {
			t.Errorf("Review(%q) = %v, want an error with %q", c.args, err, c.wantErr)
		}
	}
	// Workspace B does not see the stale link of workspace A.
	f.mustReview(t, "--item", ws, "--exception-to", g)
	if _, err := f.pin(t, g); err != nil {
		t.Fatal(err)
	}
	f.panes["w15:p1"] = herdr.Pane{ID: "w15:p1", WorkspaceID: "w15"}
	var out bytes.Buffer
	if err := Review(context.Background(), f.env(f.sock, "w15:p1", "w15", &out), nil); err != nil ||
		strings.Contains(out.String(), "was an exception") {
		t.Errorf("review list from B = %q, %v; want no stale link of A", out.String(), err)
	}
}

// failWriter fails every write, as a closed stdout pipe does.
type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, errors.New("closed pipe") }

// current is the output generation that the pointer file names.
func (f *fixture) current(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(f.store, outDir, "current"))
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(b))
}

// AC (#121): approve, restore, pin and link build after the commit even when the message
// fails.
func TestBuildAfterFailedPrint(t *testing.T) {
	f := newFixture(t)
	pid := fmt.Sprint(f.remember(t, "use pnpm"))
	retired := fmt.Sprint(f.addActive(t, nil, "retire me"))
	f.mustReview(t, "--item", retired, "--retire")
	r := f.rows(t)
	f.mustReview(t, fmt.Sprint(r.proposals[len(r.proposals)-1].ID), "--approve")
	g, ws := f.linked(t)
	for _, c := range []struct {
		name string
		run  func(Env) error
	}{
		{"approve", func(e Env) error { return Review(context.Background(), e, []string{pid, "--approve"}) }},
		{"restore", func(e Env) error { return Review(context.Background(), e, []string{"--restore", retired}) }},
		{"pin", func(e Env) error { return Pin(context.Background(), e, []string{g}) }},
		{"link", func(e Env) error {
			return Review(context.Background(), e, []string{"--item", ws, "--exception-to", g})
		}},
	} {
		e := f.env(f.sock, "w14:p1", "w14", nil)
		e.Stdout = failWriter{}
		if err := c.run(e); err == nil || !strings.Contains(err.Error(), "closed pipe") {
			t.Errorf("%s = %v, want the print error", c.name, err)
		}
		if got, want := f.current(t), fmt.Sprintf("gen-%d", f.rows(t).newest); got != want {
			t.Errorf("after %s the output is at %s, want %s", c.name, got, want)
		}
	}
}
