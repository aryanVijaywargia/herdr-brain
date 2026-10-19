package cli

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/aryanVijaywargia/herdr-brain/internal/store"
	"github.com/aryanVijaywargia/herdr-brain/internal/workspace"
)

// days is the time n days after testNow, when the fixture items became active.
func days(n int) time.Time { return testNow.Add(time.Duration(n) * 24 * time.Hour) }

// archive runs brain archive from pane w14:p1 at day n.
func (f *fixture) archive(t *testing.T, n int, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	e := f.env(f.sock, "w14:p1", "w14", &out)
	e.Now = func() time.Time { return days(n) }
	err := Archive(context.Background(), e, args)
	return out.String(), err
}

func (f *fixture) mustArchive(t *testing.T, n int, args ...string) string {
	t.Helper()
	out, err := f.archive(t, n, args...)
	if err != nil {
		t.Fatalf("Archive(%q) at day %d = %v", args, n, err)
	}
	return out
}

// archiveItems adds an idle item of workspace w14, one of w15 and a pinned one of w14. The
// fixture's global item (1) is idle too.
func (f *fixture) archiveItems(t *testing.T) (own, other int64) {
	t.Helper()
	own = f.addActive(t, ptr("ws_a14"), "use pnpm")
	other = f.addActive(t, ptr("ws_a15"), "blog uses hugo")
	pinned := f.addActive(t, ptr("ws_a14"), "the yearly release steps")
	if _, err := f.pin(t, fmt.Sprint(pinned)); err != nil {
		t.Fatal(err)
	}
	return own, other
}

// The list shows the candidates that the caller sees, with their text; it changes nothing.
// A pinned item stays out after 6 months, and so does an item of another workspace.
func TestArchiveList(t *testing.T) {
	f := newFixture(t)
	own, _ := f.archiveItems(t)
	if out := f.mustArchive(t, 90); out != "no archive candidates\n" {
		t.Errorf("Archive() at day 90 = %q, want no candidates", out)
	}
	want := "archive candidates: no relevant session for 90 days\n" +
		"item 1  global  idle 183d  " + liveText + "\n" +
		fmt.Sprintf("item %d  workspace w14 (shop)  idle 183d  use pnpm\n", own) +
		"\nretire all: brain archive --all; keep all for 90 more days: brain archive --keep\n"
	before := f.rows(t)
	if out := f.mustArchive(t, 183); out != want {
		t.Errorf("Archive() at day 183 =\n%s\nwant\n%s", out, want)
	}
	if after := f.rows(t); after.newest != before.newest {
		t.Errorf("the list wrote output generation %d", after.newest)
	}
}

// AC: brain archive --all retires all the candidates of the caller's scope in one action, and
// restore brings one back. Archive never deletes: the text stays. The item of another
// workspace and the pinned item stay active.
func TestArchiveAll(t *testing.T) {
	f := newFixture(t)
	own, _ := f.archiveItems(t)
	before := f.rows(t)
	out := f.mustArchive(t, 91, "--all")
	if !strings.HasPrefix(out, "retired 2 items;") {
		t.Errorf("Archive(--all) = %q, want 2 retired", out)
	}
	after := f.rows(t)
	if after.newest != before.newest+2 {
		t.Errorf("output generation %d, want %d: one revision per item", after.newest, before.newest+2)
	}
	for i, it := range after.items {
		want := store.StateActive
		if it.ID == 1 || it.ID == own {
			want = store.StateRetired
		}
		if it.State != want || after.revisions[i].Text != before.revisions[i].Text {
			t.Errorf("item %d is %s %q, want %s with its text", it.ID, it.State, after.revisions[i].Text, want)
		}
	}
	if !strings.Contains(f.mustReview(t, "--retired"), "use pnpm") {
		t.Errorf("brain review --retired does not list item %d", own)
	}
	// A restore at day 91 starts the age again.
	var rout bytes.Buffer
	e := f.env(f.sock, "w14:p1", "w14", &rout)
	e.Now = func() time.Time { return days(91) }
	if err := Review(context.Background(), e, []string{"--restore", fmt.Sprint(own)}); err != nil {
		t.Fatalf("Review(--restore) = %v", err)
	}
	if it, _ := f.rows(t).item(t, own); it.State != store.StateActive {
		t.Errorf("restored item %d is %s", own, it.State)
	}
	if out := f.mustArchive(t, 92); out != "no archive candidates\n" {
		t.Errorf("Archive() after the restore = %q, want none", out)
	}
	if out := f.mustArchive(t, 182); !strings.Contains(out, fmt.Sprintf("item %d ", own)) {
		t.Errorf("Archive() 91 days after the restore = %q, want item %d", out, own)
	}
}

// --keep keeps all the candidates and starts a new 90-day period; it writes no revision.
func TestArchiveKeep(t *testing.T) {
	f := newFixture(t)
	own, _ := f.archiveItems(t)
	before := f.rows(t)
	if out := f.mustArchive(t, 100, "--keep"); out != "kept 2 items; a new 90-day period starts now\n" {
		t.Errorf("Archive(--keep) = %q", out)
	}
	after := f.rows(t)
	if after.newest != before.newest {
		t.Errorf("--keep wrote output generation %d", after.newest)
	}
	if it, _ := after.item(t, own); it.State != store.StateActive || it.CurrentRevision != 1 {
		t.Errorf("kept item = %+v, want active at revision 1", it)
	}
	if out := f.mustArchive(t, 190); out != "no archive candidates\n" {
		t.Errorf("Archive() at day 190 = %q, want none", out)
	}
	if out := f.mustArchive(t, 191); !strings.Contains(out, fmt.Sprintf("item %d ", own)) {
		t.Errorf("Archive() at day 191 = %q, want item %d", out, own)
	}
	if out := f.mustArchive(t, 1, "--all"); out != "no archive candidates\n" {
		t.Errorf("Archive(--all) with no candidates = %q", out)
	}
}

func TestArchiveArgs(t *testing.T) {
	f := newFixture(t)
	for _, c := range []struct {
		args    []string
		wantErr string
	}{
		{[]string{"x"}, "unexpected argument"},
		{[]string{"--all", "--keep"}, "exclude each other"},
		{[]string{"--nope"}, "flag provided but not defined"},
	} {
		if _, err := f.archive(t, 0, c.args...); err == nil || !strings.Contains(err.Error(), c.wantErr) {
			t.Errorf("Archive(%q) = %v, want an error with %q", c.args, err, c.wantErr)
		}
	}
}

// AC: a closed workspace shows as a count in brain status, with no item text and no prompt.
// No pane can read or archive its items before a link. status shows the count and the oldest
// candidates of the caller's scope, by id.
func TestStatusArchive(t *testing.T) {
	f := newStatusFixture(t)
	own, other := f.archiveItems(t)
	f.addActive(t, ptr("ws_a15"), "blog deploys on friday")
	s := openStore(t, f.store)
	write(t, s, func(tx *sql.Tx) error { return store.CloseWorkspace(context.Background(), tx, "ws_a15") })
	closeStore(t, s)

	out, err := f.statusAt(t, f.sock, days(91))
	if err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("\narchive: 2 candidates with no relevant session for 90 days, the oldest item 1 (idle 91d), item %d (idle 91d); list them: brain archive\n"+
		"  closed workspace ws_a15 (blog): 2 items, 2 archive candidates; act on them after a relink\n", own)
	if !strings.Contains(out, want) {
		t.Errorf("brain status:\n%s\nwant the lines:\n%s", out, want)
	}
	for _, text := range []string{"blog uses hugo", "blog deploys", "use pnpm", liveText} {
		if strings.Contains(out, text) {
			t.Errorf("brain status shows item text %q", text)
		}
	}
	// brain archive from a pane of a live workspace neither lists nor retires them.
	if out := f.mustArchive(t, 91); strings.Contains(out, "blog") {
		t.Errorf("Archive() lists an item of the closed workspace:\n%s", out)
	}
	f.mustArchive(t, 91, "--all")
	if it, _ := f.rows(t).item(t, other); it.State != store.StateActive {
		t.Errorf("item %d of the closed workspace is %s after --all", other, it.State)
	}
	if _, err := f.pin(t, fmt.Sprint(other)); err == nil || !strings.Contains(err.Error(), "not found in this workspace") {
		t.Errorf("Pin(%d) of the closed workspace = %v, want not found", other, err)
	}
	out, err = f.statusAt(t, f.sock, days(91))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "\narchive: no candidates\n  closed workspace ws_a15 (blog): 2 items, 2 archive candidates;") {
		t.Errorf("brain status after --all:\n%s", out)
	}
}

// Review finding: a workspace that closes after the caller's key was read, while archive waits
// for the store, neither lists nor retires its items.
func TestArchiveWorkspaceClosesDuringCall(t *testing.T) {
	f := newFixture(t)
	own, _ := f.archiveItems(t)
	var out bytes.Buffer
	e := f.env(f.sock, "w14:p1", "w14", &out)
	e.Now = func() time.Time { return days(91) }
	sync := e.Sync
	e.Sync = func(ctx context.Context, s workspace.Session, st *store.Store) error {
		if err := sync(ctx, s, st); err != nil {
			return err
		}
		return st.Write(ctx, func(tx *sql.Tx) error { return store.CloseWorkspace(ctx, tx, "ws_a14") })
	}
	if err := Archive(context.Background(), e, []string{"--all"}); err != nil || !strings.HasPrefix(out.String(), "retired 1 items;") {
		t.Errorf("Archive(--all) = %q, %v; want only the global item retired", out.String(), err)
	}
	if it, _ := f.rows(t).item(t, own); it.State != store.StateActive {
		t.Errorf("item %d of the closed workspace is %s", own, it.State)
	}
}
