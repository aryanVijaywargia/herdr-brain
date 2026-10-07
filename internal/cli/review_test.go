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
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aryanVijaywargia/herdr-brain/internal/adapter"
	"github.com/aryanVijaywargia/herdr-brain/internal/bindings"
	"github.com/aryanVijaywargia/herdr-brain/internal/herdr"
	"github.com/aryanVijaywargia/herdr-brain/internal/hook"
	"github.com/aryanVijaywargia/herdr-brain/internal/output"
	"github.com/aryanVijaywargia/herdr-brain/internal/secret"
	"github.com/aryanVijaywargia/herdr-brain/internal/signal"
	"github.com/aryanVijaywargia/herdr-brain/internal/store"
	"github.com/aryanVijaywargia/herdr-brain/internal/workspace"
)

// review runs `brain review args` from pane w14:p1 of workspace w14 ("shop").
func (f *fixture) review(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := Review(context.Background(), f.env(f.sock, "w14:p1", "w14", &out), args)
	return out.String(), err
}

func (f *fixture) mustReview(t *testing.T, args ...string) string {
	t.Helper()
	out, err := f.review(t, args...)
	if err != nil {
		t.Fatalf("Review(%q) = %v", args, err)
	}
	return out
}

// remember drafts text from pane w14:p1 and returns the proposal id.
func (f *fixture) remember(t *testing.T, args ...string) int64 {
	t.Helper()
	var out bytes.Buffer
	if err := Remember(context.Background(), f.env(f.sock, "w14:p1", "w14", &out), args); err != nil {
		t.Fatalf("Remember(%q) = %v", args, err)
	}
	r := f.rows(t)
	return r.proposals[len(r.proposals)-1].ID
}

// addActive commits an active item at revision 1 with text, in workspace key (nil: global),
// then revises it to each of more, and builds the output. It returns the item id.
func (f *fixture) addActive(t *testing.T, key *string, text string, more ...string) int64 {
	t.Helper()
	ctx := context.Background()
	s := openStore(t, f.store)
	defer closeStore(t, s)
	r := store.ItemRevision{Text: text, Scope: store.ScopeGlobal, WorkspaceKey: key, State: store.StateActive,
		Author: store.AuthorYou, Origin: store.OriginRemember, Created: testNow}
	if key != nil {
		r.Scope = store.ScopeWorkspace
	}
	var id int64
	write(t, s, func(tx *sql.Tx) (err error) {
		id, _, err = store.CreateItem(ctx, tx, store.Item{ClaimID: "c-" + text, Kind: store.KindFact, Created: testNow}, r)
		for i, m := range more {
			r.ItemID, r.Text = id, m
			if _, err := store.WriteRevision(ctx, tx, int64(i+1), r); err != nil {
				return err
			}
		}
		return err
	})
	if _, err := output.Build(ctx, s, filepath.Join(f.store, outDir), nil, now); err != nil {
		t.Fatalf("Build() = %v", err)
	}
	return id
}

// brief is the brief that a hook in workspace ws_a14 reads now.
func (f *fixture) brief(t *testing.T) string {
	t.Helper()
	b, err := output.Read(filepath.Join(f.store, outDir), "ws_a14")
	if err != nil {
		t.Fatalf("output.Read() = %v", err)
	}
	return b.Global + b.Workspace
}

func (r rows) item(t *testing.T, id int64) (store.Item, store.ItemRevision) {
	t.Helper()
	for i, it := range r.items {
		if it.ID == id {
			return it, r.revisions[i]
		}
	}
	t.Fatalf("no item %d", id)
	return store.Item{}, store.ItemRevision{}
}

func (r rows) proposal(t *testing.T, id int64) store.Proposal {
	t.Helper()
	for _, p := range r.proposals {
		if p.ID == id {
			return p
		}
	}
	t.Fatalf("no proposal %d", id)
	return store.Proposal{}
}

func ptr(s string) *string { return &s }

// AC: each action on a temp store. Approve sets state active and author you (review).
func TestReviewActions(t *testing.T) {
	cases := []struct {
		name     string
		remember []string
		args     []string // after the proposal id
		text     string
		scope    store.Scope
		key      *string
		pinned   bool
	}{
		{name: "approve", remember: []string{"use pnpm"}, args: []string{"--approve"}, text: "use pnpm",
			scope: store.ScopeWorkspace, key: ptr("ws_a14")},
		{name: "edit then approve", remember: []string{"use pnpm"}, args: []string{"--approve", "--edit", "use pnpm 9"},
			text: "use pnpm 9", scope: store.ScopeWorkspace, key: ptr("ws_a14")},
		{name: "change scope to global", remember: []string{"use pnpm"}, args: []string{"--approve", "--scope", "global"},
			text: "use pnpm", scope: store.ScopeGlobal},
		{name: "change scope to workspace", remember: []string{"--global", "use pnpm"}, args: []string{"--approve", "--scope", "workspace"},
			text: "use pnpm", scope: store.ScopeWorkspace, key: ptr("ws_a14")},
		{name: "pin", remember: []string{"use pnpm"}, args: []string{"--approve", "--pin"}, text: "use pnpm",
			scope: store.ScopeWorkspace, key: ptr("ws_a14"), pinned: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			id := f.remember(t, c.remember...)
			out := f.mustReview(t, append([]string{fmt.Sprint(id)}, c.args...)...)
			if !strings.Contains(out, fmt.Sprintf("proposal %d approved", id)) || strings.Contains(out, "pending") {
				t.Errorf("Review(%d %q) printed %q, want approved", id, c.args, out)
			}
			r := f.rows(t)
			p := r.proposal(t, id)
			it, rev := r.item(t, *p.ItemID)
			switch {
			case p.Status != store.ProposalApproved || p.Surface == nil || *p.Surface != store.SurfaceCLI || p.Decided.IsZero():
				t.Errorf("proposal = %+v, want approved from the cli", p)
			case it.State != store.StateActive || it.CurrentRevision != 2 || it.Scope != c.scope || !equal(it.WorkspaceKey, c.key) || it.Pinned != c.pinned:
				t.Errorf("item = %+v, want active at revision 2 in %s %s, pinned %v", it, c.scope, deref(c.key), c.pinned)
			case rev.Text != c.text || rev.Author != store.AuthorYou || rev.Origin != store.OriginReview ||
				rev.ProposalID == nil || *rev.ProposalID != id:
				t.Errorf("revision = %+v, want %q by you (review) from proposal %d", rev, c.text, id)
			}
			// The post-commit build publishes the item at once.
			if b := f.brief(t); !strings.Contains(b, c.text) {
				t.Errorf("brief = %q, want %q", b, c.text)
			}
		})
	}
}

func TestReviewRejectAndLater(t *testing.T) {
	f := newFixture(t)
	later, reject := f.remember(t, "a"), f.remember(t, "b")
	if out := f.mustReview(t, fmt.Sprint(later), "--later"); !strings.Contains(out, "stays in the queue") {
		t.Errorf("--later printed %q", out)
	}
	if out := f.mustReview(t, fmt.Sprint(reject), "--reject"); !strings.Contains(out, "rejected") {
		t.Errorf("--reject printed %q", out)
	}
	r := f.rows(t)
	if p := r.proposal(t, later); p.Status != store.ProposalPending {
		t.Errorf("later proposal = %+v, want pending", p)
	}
	p := r.proposal(t, reject)
	if it, _ := r.item(t, *p.ItemID); p.Status != store.ProposalRejected || it.State != store.StateDraft || it.CurrentRevision != 1 {
		t.Errorf("rejected proposal = %+v, item = %+v; want rejected and a draft at revision 1", p, it)
	}
	out := f.mustReview(t)
	if !strings.Contains(out, fmt.Sprintf("%d  workspace w14 (shop)  a", later)) || strings.Contains(out, "  b") {
		t.Errorf("list = %q, want only proposal %d", out, later)
	}
	if _, err := f.review(t, fmt.Sprint(reject), "--approve"); err == nil || !strings.Contains(err.Error(), "conflict: it is rejected") {
		t.Errorf("approve of a rejected proposal = %v, want a conflict", err)
	}
}

func TestReviewShow(t *testing.T) {
	f := newFixture(t)
	id := f.remember(t, "use pnpm")
	out := f.mustReview(t, fmt.Sprint(id))
	for _, want := range []string{"proposal 1 (pending)", "text:     use pnpm", "scope:    workspace",
		"target:   workspace w14 (shop)", "revision: item 2 revision 2, from revision 1", "+ use pnpm",
		"source:   remember, sender unconfirmed", "--approve"} {
		if !strings.Contains(out, want) {
			t.Errorf("show = %q, want %q", out, want)
		}
	}
	if strings.Contains(out, "warning") {
		t.Errorf("show = %q, want no warning", out)
	}
}

// An old item text with ".:" shows clean in the review pane (#284).
func TestReviewShowCleanText(t *testing.T) {
	f := newFixture(t)
	id := f.remember(t, "Running the full project check in notes-api.: Run npm run lint..")
	out := f.mustReview(t, fmt.Sprint(id))
	if want := "text:     Running the full project check in notes-api: Run npm run lint.\n"; !strings.Contains(out, want) {
		t.Errorf("show = %q, want %q", out, want)
	}
}

// AC: the permission warning shows, and approve needs --confirm.
func TestPermissionWarning(t *testing.T) {
	f := newFixture(t)
	id := f.remember(t, "always run the tests in the sandbox")
	if p := f.rows(t).proposal(t, id); !p.PermissionWarning {
		t.Errorf("proposal = %+v, want the permission warning recorded", p)
	}
	if out := f.mustReview(t); !strings.Contains(out, "[permission warning]") {
		t.Errorf("list = %q, want the warning mark", out)
	}
	if out := f.mustReview(t, fmt.Sprint(id)); !strings.Contains(out, "warning:  this may change a permission") {
		t.Errorf("show = %q, want the warning line", out)
	}
	if _, err := f.review(t, fmt.Sprint(id), "--approve"); err == nil || !strings.Contains(err.Error(), "--confirm") {
		t.Errorf("approve with no --confirm = %v, want an error", err)
	}
	if p := f.rows(t).proposal(t, id); p.Status != store.ProposalPending {
		t.Errorf("proposal = %+v, want pending", p)
	}
	// An edit that adds a keyword needs the confirm too.
	plain := f.remember(t, "use pnpm")
	if _, err := f.review(t, fmt.Sprint(plain), "--approve", "--edit", "allow npm too"); err == nil || !strings.Contains(err.Error(), "--confirm") {
		t.Errorf("approve of an edit with a keyword = %v, want an error", err)
	}
	f.mustReview(t, fmt.Sprint(id), "--approve", "--confirm")
	if p := f.rows(t).proposal(t, id); p.Status != store.ProposalApproved {
		t.Errorf("proposal = %+v, want approved", p)
	}
}

// A change to an active item warns when the old text or the new text has a keyword: a retire
// or an edit that removes a safety rule changes it (§7.4).
func TestChangeWarning(t *testing.T) {
	cases := []struct {
		name, text string
		args       []string
	}{
		{"retire of a safety rule", "never run rm -rf in the home folder", []string{"--retire"}},
		{"edit that removes the keyword", "never run the deploy script", []string{"--edit", "run the deploy script when you like"}},
		{"edit that adds a keyword", "use pnpm", []string{"--edit", "allow npm too"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			item := f.addActive(t, ptr("ws_a14"), c.text)
			f.mustReview(t, append([]string{"--item", fmt.Sprint(item)}, c.args...)...)
			if out := f.mustReview(t); !strings.Contains(out, "[permission warning]") {
				t.Errorf("list = %q, want the warning mark", out)
			}
			if out := f.mustReview(t, "1"); !strings.Contains(out, "warning:") {
				t.Errorf("show = %q, want the warning line", out)
			}
			if _, err := f.review(t, "1", "--approve"); err == nil || !strings.Contains(err.Error(), "--confirm") {
				t.Errorf("approve with no --confirm = %v, want an error", err)
			}
			if it, _ := f.rows(t).item(t, item); it.CurrentRevision != 1 {
				t.Errorf("item = %+v, want revision 1: nothing written", it)
			}
			f.mustReview(t, "1", "--approve", "--confirm")
			if it, _ := f.rows(t).item(t, item); it.CurrentRevision != 2 {
				t.Errorf("item after --confirm = %+v, want revision 2", it)
			}
		})
	}
}

func TestPermissionWords(t *testing.T) {
	for text, want := range map[string]bool{
		"Allow git push": true, "ask for permission": true, "no sandbox": true, "use --dangerously-skip": true,
		"never run rm": true, "Always  run lint": true, "use pnpm": false, "the runner is slow": false,
	} {
		if got := permissionWords.MatchString(text); got != want {
			t.Errorf("permissionWords.MatchString(%q) = %v, want %v", text, got, want)
		}
	}
}

// AC: an edit goes through the secret filter; a hit writes nothing and prints the class only.
func TestReviewSecretFilter(t *testing.T) {
	token := "ghp_" + strings.Repeat("a1B2", 9)
	f := newFixture(t)
	id := f.remember(t, "use pnpm")
	item := f.addActive(t, ptr("ws_a14"), "old text")
	before := f.rows(t)
	for _, args := range [][]string{
		{fmt.Sprint(id), "--approve", "--edit", "token " + token},
		{"--item", fmt.Sprint(item), "--edit", "token " + token},
	} {
		out, err := f.review(t, args...)
		var se *secret.Error
		if !errors.As(err, &se) || strings.Contains(err.Error(), token) || out != "" {
			t.Errorf("Review(%q) = %v, %q; want a *secret.Error with no token", args[:2], err, out)
		}
	}
	if after := f.rows(t); !reflect.DeepEqual(after, before) {
		t.Errorf("rows after a secret = %+v, want %+v", after, before)
	}
}

// AC: active revision 2, a pending text change, approve → revision 3; undo → revision 2
// fields again. Revision 2 stays live until the approve. `brain undo` is E4-S5, so the undo
// here is the store revision primitive that it uses.
func TestChangeActiveItem(t *testing.T) {
	f := newFixture(t)
	item := f.addActive(t, ptr("ws_a14"), "text 1", "text 2")
	out := f.mustReview(t, "--item", fmt.Sprint(item), "--edit", "text 3")
	var pid int64
	if _, err := fmt.Sscanf(out, "proposal %d:", &pid); err != nil {
		t.Fatalf("--item --edit printed %q", out)
	}
	r := f.rows(t)
	p := r.proposal(t, pid)
	if it, _ := r.item(t, item); it.CurrentRevision != 2 || p.BaseRevision == nil || *p.BaseRevision != 2 ||
		p.Diff != "- text 2\n+ text 3" || p.Text != "text 3" {
		t.Errorf("item = %+v, proposal = %+v; want revision 2 and a pending change from it", it, p)
	}
	if b := f.brief(t); !strings.Contains(b, "text 2") || strings.Contains(b, "text 3") {
		t.Errorf("brief before approve = %q, want revision 2 live", b)
	}
	f.mustReview(t, fmt.Sprint(pid), "--approve")
	it, rev3 := f.rows(t).item(t, item)
	if it.CurrentRevision != 3 || rev3.Text != "text 3" {
		t.Fatalf("item after approve = %+v %+v, want revision 3 with text 3", it, rev3)
	}
	if b := f.brief(t); !strings.Contains(b, "text 3") || strings.Contains(b, "text 2") {
		t.Errorf("brief after approve = %q, want revision 3", b)
	}

	s := openStore(t, f.store)
	defer closeStore(t, s)
	ctx := context.Background()
	var rev2, rev4 store.ItemRevision
	write(t, s, func(tx *sql.Tx) (err error) {
		if rev2, err = store.ItemRevisionByKey(ctx, tx, item, 2); err != nil {
			return err
		}
		undo := rev2
		undo.UndoesRevision = ptr64(3)
		if _, err := store.WriteRevision(ctx, tx, 3, undo); err != nil {
			return err
		}
		rev4, err = store.ItemRevisionByKey(ctx, tx, item, 4)
		return err
	})
	if rev4.Text != rev2.Text || rev4.Scope != rev2.Scope || !equal(rev4.WorkspaceKey, rev2.WorkspaceKey) ||
		rev4.State != rev2.State || rev4.Pinned != rev2.Pinned {
		t.Errorf("revision 4 = %+v, want the fields of revision 2 %+v", rev4, rev2)
	}
}

func ptr64(v int64) *int64 { return &v }

// AC: approve checks the base revision; a stale base fails with a clear message.
func TestStaleBase(t *testing.T) {
	f := newFixture(t)
	item := f.addActive(t, nil, "text 1")
	f.mustReview(t, "--item", fmt.Sprint(item), "--edit", "first")
	f.mustReview(t, "--item", fmt.Sprint(item), "--edit", "second")
	f.mustReview(t, "1", "--approve")
	before := f.rows(t)
	_, err := f.review(t, "2", "--approve")
	if err == nil || !strings.Contains(err.Error(), "changed after proposal 2 was made") {
		t.Errorf("approve of a stale proposal = %v, want a clear stale error", err)
	}
	if after := f.rows(t); !reflect.DeepEqual(after, before) {
		t.Errorf("a stale approve wrote rows")
	}
}

// Retire through a proposal, list the retired items, and restore.
func TestRetireAndRestore(t *testing.T) {
	f := newFixture(t)
	item := f.addActive(t, ptr("ws_a14"), "retire me")
	if out := f.mustReview(t); !strings.Contains(out, "no proposals") {
		t.Errorf("empty list = %q", out)
	}
	f.mustReview(t, "--item", fmt.Sprint(item), "--retire")
	if out := f.mustReview(t); !strings.Contains(out, fmt.Sprintf("retire item %d", item)) {
		t.Errorf("list = %q, want the retire proposal", out)
	}
	if _, err := f.review(t, "1", "--approve", "--pin"); err == nil || !strings.Contains(err.Error(), "retires item") {
		t.Errorf("approve --pin of a retire = %v, want an error", err)
	}
	f.mustReview(t, "1", "--approve")
	it, rev := f.rows(t).item(t, item)
	if it.State != store.StateRetired || it.CurrentRevision != 2 || rev.Text != "retire me" {
		t.Errorf("item = %+v %+v, want retired at revision 2 with its text", it, rev)
	}
	if b := f.brief(t); strings.Contains(b, "retire me") {
		t.Errorf("brief = %q, want no retired item", b)
	}
	if out := f.mustReview(t, "--retired"); !strings.Contains(out, fmt.Sprintf("item %d  workspace w14 (shop)  retire me", item)) {
		t.Errorf("--retired = %q, want the item", out)
	}
	if out := f.mustReview(t, "--restore", fmt.Sprint(item)); !strings.Contains(out, "restored at revision 3") {
		t.Errorf("--restore printed %q", out)
	}
	if it, rev := f.rows(t).item(t, item); it.State != store.StateActive || it.CurrentRevision != 3 || rev.Author != store.AuthorYou {
		t.Errorf("item = %+v %+v, want active at revision 3", it, rev)
	}
	if b := f.brief(t); !strings.Contains(b, "retire me") {
		t.Errorf("brief = %q, want the restored item", b)
	}
	if _, err := f.review(t, "--restore", fmt.Sprint(item)); err == nil || !strings.Contains(err.Error(), "not retired") {
		t.Errorf("restore of an active item = %v, want an error", err)
	}
}

// AC: two approvals of one proposal from two processes: one wins, one gets "conflict". Each
// call opens its own store handle, as a process does.
func TestConcurrentApprove(t *testing.T) {
	f := newFixture(t)
	id := f.remember(t, "use pnpm")
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i := range errs {
		e := f.env(f.sock, "w14:p1", "w14", &bytes.Buffer{})
		e.Sync = func(ctx context.Context, s workspace.Session, st *store.Store) error {
			return s.Refresh(ctx, nil, st, []herdr.Workspace{{ID: "w14", Label: "shop"}}, testNow)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = Review(context.Background(), e, []string{fmt.Sprint(id), "--approve"})
		}()
	}
	wg.Wait()
	won, lost := 0, 0
	for _, err := range errs {
		switch {
		case err == nil:
			won++
		case strings.Contains(err.Error(), "conflict"):
			lost++
		default:
			t.Errorf("Review(--approve) = %v, want nil or a conflict", err)
		}
	}
	if won != 1 || lost != 1 {
		t.Errorf("approvals: %d won, %d conflicts; want 1 and 1", won, lost)
	}
	p := f.rows(t).proposal(t, id)
	if it, _ := f.rows(t).item(t, *p.ItemID); it.CurrentRevision != 2 {
		t.Errorf("item = %+v, want revision 2 only", it)
	}
}

// AC: approve, then at once a fresh hook with no other brain call: the hook prints the item.
func TestApproveThenHook(t *testing.T) {
	f := newFixture(t)
	id := f.remember(t, "use pnpm, not npm")
	f.mustReview(t, fmt.Sprint(id), "--approve")
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
	if !strings.Contains(out.String(), "use pnpm, not npm") {
		t.Errorf("hook printed %q, want the approved item", out.String())
	}
}

// A failed post-commit build keeps the commit and says that delivery is pending.
func TestDeliveryPending(t *testing.T) {
	f := newFixture(t)
	id := f.remember(t, "use pnpm")
	out := filepath.Join(f.store, outDir)
	cur, err := os.ReadFile(filepath.Join(out, "current"))
	if err != nil {
		t.Fatal(err)
	}
	gen := filepath.Join(out, strings.TrimSpace(string(cur)))
	if err := os.WriteFile(filepath.Join(gen, "global.md"), []byte("hand edit"), 0o600); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	e := f.env(f.sock, "w14:p1", "w14", &buf)
	if err := Review(context.Background(), e, []string{fmt.Sprint(id), "--approve"}); err != nil {
		t.Fatalf("Review(--approve) = %v", err)
	}
	if !strings.Contains(buf.String(), "approved") || !strings.Contains(buf.String(), "committed, delivery pending: output generation") {
		t.Errorf("Review(--approve) printed %q, want approved and delivery pending with the reason", buf.String())
	}
	if p := f.rows(t).proposal(t, id); p.Status != store.ProposalApproved {
		t.Errorf("proposal = %+v, want approved", p)
	}
}

// addSession binds herdr session "work" to the same main store. Its workspace w14 has its
// own key ws_b14, and its pane w14:p1 is live in it.
func (f *fixture) addSession(t *testing.T) (sock string) {
	t.Helper()
	dir := filepath.Dir(filepath.Dir(f.sock))
	sock = filepath.Join(dir, "work", "herdr.sock")
	proj := canonical(t, filepath.Join(dir, "proj"))
	if err := os.MkdirAll(filepath.Dir(sock), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := bindings.Write(f.file, []bindings.Binding{
		{Session: "default", Socket: f.sock, Store: f.store, Kind: bindings.KindMain, Created: testNow},
		{Session: "work", Socket: sock, Store: f.store, Kind: bindings.KindMain, Created: testNow},
	}); err != nil {
		t.Fatal(err)
	}
	writeJSON(t, filepath.Join(filepath.Dir(sock), "session.json"), map[string]any{"version": 3,
		"workspaces": []map[string]string{{"id": "w14", "identity_cwd": proj}}})
	entry := func(key, session, id string) map[string]string {
		return map[string]string{"key": key, "session": session, "workspace_id": id, "label": id, "project": proj}
	}
	writeJSON(t, filepath.Join(f.store, "workspaces.json"), map[string]any{"version": 1, "entries": []map[string]string{
		entry("ws_a14", "default", "w14"), entry("ws_a15", "default", "w15"), entry("ws_b14", "work", "w14")}})
	s := openStore(t, f.store)
	defer closeStore(t, s)
	write(t, s, func(tx *sql.Tx) error {
		return store.InsertWorkspace(context.Background(), tx, store.Workspace{Key: "ws_b14", HerdrSession: "work",
			HerdrWorkspaceID: "w14", Status: store.WorkspaceLive, LastSeen: testNow})
	})
	return sock
}

func canonical(t *testing.T, p string) string {
	t.Helper()
	c, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// AC: scope. Two workspaces share a store, also with the same herdr workspace id in two bound
// sessions, and a review pane moved from A to B. B cannot read or change A's proposal or item.
func TestReviewScope(t *testing.T) {
	const secretText = "text of workspace A"
	f := newFixture(t)
	work := f.addSession(t)
	pid := f.remember(t, secretText)
	item := f.addActive(t, ptr("ws_a14"), secretText+" item")
	retired := f.addActive(t, ptr("ws_a14"), secretText+" retired")
	f.mustReview(t, "--item", fmt.Sprint(retired), "--retire")
	f.mustReview(t, fmt.Sprint(pid+1), "--approve")
	global := f.remember(t, "--global", "global text")
	f.panes["w14:p3"] = herdr.Pane{ID: "w14:p3", WorkspaceID: "w15"} // a review pane moved from A to B

	callers := []struct {
		name, sock, pane, ws string
	}{
		{"another workspace", f.sock, "w15:p1", "w15"},
		{"same workspace id in another session", work, "w14:p1", "w14"},
		{"review pane moved to B", f.sock, "w14:p3", "w15"},
	}
	for _, c := range callers {
		t.Run(c.name, func(t *testing.T) {
			f.panes["w15:p1"] = herdr.Pane{ID: "w15:p1", WorkspaceID: "w15"}
			run := func(args ...string) (string, error) {
				var out bytes.Buffer
				e := f.env(c.sock, c.pane, c.ws, &out)
				if c.sock == work {
					e.Environ = append(e.Environ, "HERDR_SESSION=work")
				}
				e.Pane = func(_ context.Context, _ herdr.Target, id string) (herdr.Pane, error) {
					p, ok := f.panes[id]
					if !ok {
						return herdr.Pane{}, herdr.ErrPaneNotFound
					}
					return p, nil
				}
				e.Sync = func(ctx context.Context, s workspace.Session, st *store.Store) error {
					return s.Refresh(ctx, nil, st, []herdr.Workspace{{ID: "w14", Label: "shop"}, {ID: "w15", Label: "blog"}}, testNow)
				}
				var err error
				if len(args) > 0 && args[0] == "pin" {
					err = Pin(context.Background(), e, args[1:])
				} else {
					err = Review(context.Background(), e, args)
				}
				return out.String(), err
			}
			before := f.rows(t)
			out, err := run("--retired")
			if err != nil || strings.Contains(out, secretText) {
				t.Errorf("--retired = %q, %v; want no text of A", out, err)
			}
			out, err = run()
			if err != nil || strings.Contains(out, secretText) || !strings.Contains(out, fmt.Sprintf("%d  global  global text", global)) {
				t.Errorf("list = %q, %v; want the global proposal and no text of A", out, err)
			}
			for _, args := range [][]string{
				{fmt.Sprint(pid)}, {fmt.Sprint(pid), "--approve"}, {fmt.Sprint(pid), "--reject"}, {fmt.Sprint(pid), "--later"},
				{fmt.Sprint(pid), "--approve", "--scope", "workspace"},
				{"--item", fmt.Sprint(item), "--edit", "x"}, {"--item", fmt.Sprint(item), "--retire"},
				{"--restore", fmt.Sprint(retired)}, {"pin", fmt.Sprint(item)}, {"pin", "--off", fmt.Sprint(item)},
			} {
				out, err := run(args...)
				if !errors.Is(err, errNotFound) || strings.Contains(err.Error(), secretText) || out != "" {
					t.Errorf("%q = %q, %v; want not found in this workspace, with no text", args, out, err)
				}
			}
			if after := f.rows(t); !reflect.DeepEqual(after, before) {
				t.Errorf("B changed the store")
			}
		})
	}
	// The moved pane with its old workspace in the environment is not confirmed at all.
	var out bytes.Buffer
	if err := Review(context.Background(), f.env(f.sock, "w14:p3", "w14", &out), nil); err == nil {
		t.Errorf("Review from a moved pane with its old workspace = nil, want an error")
	}
}

func TestParseReview(t *testing.T) {
	cases := []struct {
		args    []string
		wantErr string
	}{
		{nil, ""},
		{[]string{"3"}, ""},
		{[]string{"3", "--approve", "--edit", "x", "--scope", "global", "--pin", "--confirm"}, ""},
		{[]string{"--approve", "3"}, ""},
		{[]string{"3", "--reject"}, ""},
		{[]string{"--retired"}, ""},
		{[]string{"--restore", "4"}, ""},
		{[]string{"--item", "4", "--retire"}, ""},
		{[]string{"--item", "4", "--edit", "x"}, ""},
		{[]string{"3", "4"}, "at most one proposal id"},
		{[]string{"x"}, "bad proposal id"},
		{[]string{"0"}, "bad proposal id"},
		{[]string{"3", "--approve", "--reject"}, "give one action"},
		{[]string{"--approve"}, "needs a proposal id"},
		{[]string{"--reject"}, "need a proposal id"},
		{[]string{"3", "--pin"}, "go with --approve"},
		{[]string{"3", "--edit", "x"}, "go with --approve"},
		{[]string{"3", "--approve", "--scope", "team"}, `unknown scope "team"`},
		{[]string{"3", "--approve", "--edit", " "}, "the --edit text is empty"},
		{[]string{"--item", "4"}, "one of --edit, --retire"},
		{[]string{"--item", "4", "--edit", "x", "--retire"}, "one of --edit, --retire"},
		{[]string{"3", "--item", "4", "--retire"}, "no proposal id"},
		{[]string{"--retire"}, "--retire needs --item"},
		{[]string{"3", "--retire"}, ""},
		{[]string{"3", "--keep"}, ""},
		{[]string{"--keep"}, "--keep needs the proposal id"},
		{[]string{"3", "--keep", "--retire"}, "give one action"},
		{[]string{"3", "--keep", "--approve"}, "give one action"},
		{[]string{"--item", "4", "--exception-to", "1"}, ""},
		{[]string{"--item", "4", "--conflicts-with", "1"}, ""},
		{[]string{"--item", "4", "--exception-to", "1", "--conflicts-with", "1"}, "one of --edit, --retire"},
		{[]string{"--item", "4", "--exception-to", "1", "--pin"}, "one of --edit, --retire"},
		{[]string{"--exception-to", "1"}, "need --item"},
		{[]string{"3", "--retired"}, "take no proposal id"},
		{[]string{"--all"}, ""},
		{[]string{"3", "--all"}, "take no proposal id"},
		{[]string{"--all", "--retired"}, "give one action"},
		{[]string{"--rejected"}, ""},
		{[]string{"--reopen", "1"}, ""},
		{[]string{"--reset", "1"}, ""},
		{[]string{"3", "--reject", "--reason", "x"}, ""},
		{[]string{"3", "--rejected"}, "take no proposal id"},
		{[]string{"3", "--reopen", "1"}, "take no proposal id"},
		{[]string{"--reopen", "1", "--reset", "1"}, "give one action"},
		{[]string{"--reason", "x"}, "--reason goes with --reject"},
		{[]string{"3", "--approve", "--reason", "x"}, "--reason goes with --reject"},
		{[]string{"--nope"}, "flag provided but not defined"},
	}
	for _, c := range cases {
		_, err := parseReview(c.args)
		if c.wantErr == "" {
			if err != nil {
				t.Errorf("parseReview(%q) = %v, want nil", c.args, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), c.wantErr) {
			t.Errorf("parseReview(%q) = %v, want an error with %q", c.args, err, c.wantErr)
		}
	}
}

// AC: one failure gives a candidate below its gate. `brain review --all` lists it with the
// candidates that reached their gate, and nothing becomes an item or a proposal without your
// approval. A candidate of another workspace does not show.
func TestReviewAll(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if out := f.mustReview(t, "--all"); out != "no candidates\n" {
		t.Errorf("--all with no candidates = %q", out)
	}
	s := openStore(t, f.store)
	var failure, pnpm string
	write(t, s, func(tx *sql.Tx) error {
		n := 0
		// evidence adds one observation of sender in a new capture session of harness in
		// workspace ws, and attaches it to the candidate of claim, kind and scope.
		evidence := func(harness, ws string, sender store.Sender, claim string, kind store.CandidateKind, scope store.Scope) error {
			n++
			key := store.SessionKey{HerdrSession: "default", Harness: harness, Kind: store.KeyCapture, ID: fmt.Sprint("cap-", n)}
			id, err := store.OpenSession(ctx, tx, key, "w14:p1", ws, testNow)
			if err != nil {
				return err
			}
			seg, err := store.StartSegment(ctx, tx, id, "w14:p1", ws, "visible", testNow)
			if err != nil {
				return err
			}
			o, err := store.AddObservation(ctx, tx, store.Observation{CaptureIdentity: fmt.Sprint(n), Source: store.SourceHerdr,
				SegmentID: seg, Event: "idle", Excerpt: "text", Status: store.ObservationComplete, Sender: sender,
				Counts: true, At: testNow})
			if err != nil {
				return err
			}
			c := store.Candidate{ClaimID: claim, Scope: scope, Kind: kind}
			if scope == store.ScopeWorkspace {
				c.WorkspaceKey = ws
			}
			_, err = store.Attach(ctx, tx, c, o, store.DefaultGates(), signal.Supports)
			return err
		}
		claim := func(situation, rule string) string {
			id, err := store.ClaimID(ctx, tx, store.ClaimKey{Situation: situation, Rule: rule}, testNow)
			if err != nil {
				t.Fatal(err)
			}
			return id
		}
		failure, pnpm = claim("go build", "run go mod tidy first"), claim("installing packages", "use pnpm")
		other := claim("deploy", "zq-other-workspace")
		for _, err := range []error{
			evidence("codex", "ws_a14", store.SenderTool, failure, store.CandidateFailure, store.ScopeWorkspace),
			evidence("claude", "ws_a14", store.SenderUnconfirmed, pnpm, store.CandidateCorrection, store.ScopeGlobal),
			evidence("codex", "ws_b", store.SenderUnconfirmed, pnpm, store.CandidateCorrection, store.ScopeGlobal),
			evidence("claude", "ws_b", store.SenderUnconfirmed, other, store.CandidateCorrection, store.ScopeWorkspace),
		} {
			if err != nil {
				return err
			}
		}
		return nil
	})
	counts := func() (items, proposals int) {
		write(t, s, func(tx *sql.Tx) error {
			if err := tx.QueryRow(`SELECT count(*) FROM items`).Scan(&items); err != nil {
				return err
			}
			return tx.QueryRow(`SELECT count(*) FROM proposals`).Scan(&proposals)
		})
		return items, proposals
	}
	items, proposals := counts()
	closeStore(t, s)

	out := f.mustReview(t, "--all")
	seen := testNow.Format("2006-01-02 15:04")
	want := "candidate 2  global  correction  2 occurrences in 2 sessions  claude, codex  last seen " + seen + "  gate reached  open\n" +
		"  claim " + pnpm + ": When installing packages: use pnpm\n" +
		"candidate 1  workspace w14 (shop)  failure  1 occurrences in 1 sessions  codex  last seen " + seen + "  below gate (1 of 2 sessions)  open\n" +
		"  claim " + failure + ": When go build: run go mod tidy first\n" +
		"\nread only: a candidate becomes an item only through a proposal that you approve\n"
	if out != want {
		t.Errorf("--all printed\n%s\nwant\n%s", out, want)
	}
	s = openStore(t, f.store)
	if i, p := counts(); i != items || p != proposals {
		t.Errorf("after --all: %d items, %d proposals; want %d, %d: nothing becomes active without approval", i, p, items, proposals)
	}
	closeStore(t, s)
}
