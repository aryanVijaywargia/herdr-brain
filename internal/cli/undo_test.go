package cli

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/aryanVijaywargia/herdr-brain/internal/adapter"
	"github.com/aryanVijaywargia/herdr-brain/internal/herdr"
	"github.com/aryanVijaywargia/herdr-brain/internal/hook"
	"github.com/aryanVijaywargia/herdr-brain/internal/output"
	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

func (f *fixture) undo(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := Undo(context.Background(), f.env(f.sock, "w14:p1", "w14", &out), args)
	return out.String(), err
}

func (f *fixture) mustUndo(t *testing.T, args ...string) string {
	t.Helper()
	out, err := f.undo(t, args...)
	if err != nil {
		t.Fatalf("Undo(%q) = %v", args, err)
	}
	return out
}

// revision reads revision rev of item id.
func (f *fixture) revision(t *testing.T, id, rev int64) store.ItemRevision {
	t.Helper()
	s := openStore(t, f.store)
	defer closeStore(t, s)
	var r store.ItemRevision
	write(t, s, func(tx *sql.Tx) (err error) {
		r, err = store.ItemRevisionByKey(context.Background(), tx, id, rev)
		return err
	})
	return r
}

// addEdited commits a workspace item of ws_a14 with an exception link and a conflict link to
// the fixture's global item 1, then an edit that changes its text, scope, pin and links, and
// builds the output. It returns the item id.
func (f *fixture) addEdited(t *testing.T) int64 {
	t.Helper()
	ctx := context.Background()
	s := openStore(t, f.store)
	defer closeStore(t, s)
	r := store.ItemRevision{Text: "old text", Scope: store.ScopeWorkspace, WorkspaceKey: ptr("ws_a14"),
		State: store.StateActive, Author: store.AuthorYou, Origin: store.OriginRemember,
		ExceptionToItem: ptr64(1), ExceptionToRevision: ptr64(1), ConflictsWith: []int64{1}, Created: testNow}
	var id int64
	write(t, s, func(tx *sql.Tx) (err error) {
		if id, _, err = store.CreateItem(ctx, tx, store.Item{ClaimID: "c-edited", Kind: store.KindFact, Created: testNow}, r); err != nil {
			return err
		}
		r.ItemID, r.Text, r.Scope, r.WorkspaceKey, r.Pinned = id, "new text", store.ScopeGlobal, nil, true
		r.ExceptionToItem, r.ExceptionToRevision, r.ConflictsWith = nil, nil, []int64{}
		_, err = store.WriteRevision(ctx, tx, 1, r)
		return err
	})
	if _, err := output.Build(ctx, s, filepath.Join(f.store, outDir), nil, now); err != nil {
		t.Fatalf("Build() = %v", err)
	}
	return id
}

// AC (edit): undo of an edit restores text, scope, pin and links in a new revision that
// records the undone revision, and the post-commit build delivers it. A second undo does
// nothing.
func TestUndoEdit(t *testing.T) {
	f := newFixture(t)
	item := f.addEdited(t)
	before := f.rows(t)
	out := f.mustUndo(t, fmt.Sprint(item))
	if !strings.Contains(out, fmt.Sprintf("item %d: revision 2 undone; the item is active at revision 3 (workspace w14 (shop))", item)) {
		t.Errorf("Undo printed %q", out)
	}
	it, rev := f.rows(t).item(t, item)
	want := f.revision(t, item, 1)
	want.Revision, want.Origin, want.UndoesRevision = 3, store.OriginReview, ptr64(2)
	if !reflect.DeepEqual(rev, want) {
		t.Errorf("revision 3 = %+v, want %+v", rev, want)
	}
	if it.CurrentRevision != 3 || it.Scope != store.ScopeWorkspace || *it.WorkspaceKey != "ws_a14" || it.Pinned {
		t.Errorf("item = %+v, want revision 3 in ws_a14, not pinned", it)
	}
	if after := f.rows(t); after.newest != before.newest+1 {
		t.Errorf("newest generation = %d, want %d", after.newest, before.newest+1)
	}
	if b := f.brief(t); !strings.Contains(b, "old text") || strings.Contains(b, "new text") {
		t.Errorf("brief = %q, want the restored text only", b)
	}

	before = f.rows(t)
	if out := f.mustUndo(t, fmt.Sprint(item)); !strings.Contains(out, "revision 2 is undone already; nothing to do") {
		t.Errorf("second Undo printed %q", out)
	}
	if after := f.rows(t); !reflect.DeepEqual(after, before) {
		t.Errorf("second Undo changed the store")
	}
}

// AC (creation): undo of a creation retires the item, for an item made at once and for
// one approved from a draft. Undo of a retirement restores it.
func TestUndoCreationAndRetirement(t *testing.T) {
	f := newFixture(t)
	created := f.addActive(t, ptr("ws_a14"), "made at once")
	pid := f.remember(t, "approved draft")
	f.mustReview(t, fmt.Sprint(pid), "--approve")
	approved := *f.rows(t).proposal(t, pid).ItemID
	for _, c := range []struct {
		item int64
		text string
		rev  int64
	}{{created, "made at once", 2}, {approved, "approved draft", 3}} {
		f.mustUndo(t, fmt.Sprint(c.item))
		it, rev := f.rows(t).item(t, c.item)
		if it.State != store.StateRetired || it.CurrentRevision != c.rev || rev.Text != c.text || *rev.UndoesRevision != c.rev-1 {
			t.Errorf("item %d = %+v %+v, want retired at revision %d with its text", c.item, it, rev, c.rev)
		}
		if b := f.brief(t); strings.Contains(b, c.text) {
			t.Errorf("brief = %q, want no %q", b, c.text)
		}
	}

	retired := f.addActive(t, ptr("ws_a14"), "retire me")
	f.mustReview(t, "--item", fmt.Sprint(retired), "--retire")
	f.mustReview(t, fmt.Sprint(len(f.rows(t).proposals)), "--approve")
	f.mustUndo(t, fmt.Sprint(retired))
	if it, _ := f.rows(t).item(t, retired); it.State != store.StateActive || it.CurrentRevision != 3 {
		t.Errorf("item = %+v, want active at revision 3", it)
	}
	if b := f.brief(t); !strings.Contains(b, "retire me") {
		t.Errorf("brief = %q, want the restored item", b)
	}
}

// AC: undo, then at once a fresh hook with no other brain call: the hook prints the restored
// revision, not the undone one.
func TestUndoThenHook(t *testing.T) {
	f := newFixture(t)
	item := f.addActive(t, ptr("ws_a14"), "use pnpm", "use npm")
	f.mustUndo(t, fmt.Sprint(item))
	p, err := adapter.Load("claude")
	if err != nil {
		t.Fatal(err)
	}
	e := f.env(f.sock, "w14:p1", "w14", nil)
	// The hook's lookup deadline counts from Start, so the hook uses the real clock.
	h := hook.Hook{Profile: p, Environ: e.Environ, Bindings: f.file, Pane: e.Pane, Start: time.Now(), Now: time.Now}
	var out bytes.Buffer
	if err := h.Run(context.Background(), strings.NewReader(`{"session_id":"s1","cwd":"/tmp"}`), &out); err != nil {
		t.Fatalf("hook Run() = %v", err)
	}
	if !strings.Contains(out.String(), "use pnpm") || strings.Contains(out.String(), "use npm") {
		t.Errorf("hook printed %q, want the restored revision only", out.String())
	}
}

// AC: scope. Workspace B cannot undo an item of A, and cannot undo a change that would
// restore an item into A.
func TestUndoScope(t *testing.T) {
	const secretText = "text of workspace A"
	f := newFixture(t)
	item := f.addActive(t, ptr("ws_a14"), secretText)
	moved := f.addEdited(t) // now global, and its undo restores it into ws_a14
	f.panes["w15:p1"] = herdr.Pane{ID: "w15:p1", WorkspaceID: "w15"}
	before := f.rows(t)
	for _, id := range []int64{item, moved} {
		var out bytes.Buffer
		err := Undo(context.Background(), f.env(f.sock, "w15:p1", "w15", &out), []string{fmt.Sprint(id)})
		if !errors.Is(err, errNotFound) || strings.Contains(err.Error(), secretText) || out.String() != "" {
			t.Errorf("Undo(%d) from B = %q, %v; want not found in this workspace, with no text", id, out.String(), err)
		}
	}
	if after := f.rows(t); !reflect.DeepEqual(after, before) {
		t.Errorf("B changed the store")
	}
}

func TestUndoErrors(t *testing.T) {
	f := newFixture(t)
	pid := f.remember(t, "a draft")
	draft := *f.rows(t).proposal(t, pid).ItemID
	for _, c := range []struct {
		args    []string
		wantErr string
	}{
		{nil, "want one item id"},
		{[]string{"1", "2"}, "want one item id"},
		{[]string{"x"}, "bad item id"},
		{[]string{"--nope", "1"}, "flag provided but not defined"},
		{[]string{"99"}, "item 99: not found in this workspace"},
		{[]string{fmt.Sprint(draft)}, "is a draft"},
	} {
		if _, err := f.undo(t, c.args...); err == nil || !strings.Contains(err.Error(), c.wantErr) {
			t.Errorf("Undo(%q) = %v, want an error with %q", c.args, err, c.wantErr)
		}
	}
}
