package cli

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aryanVijaywargia/herdr-brain/internal/bindings"
	"github.com/aryanVijaywargia/herdr-brain/internal/herdr"
	"github.com/aryanVijaywargia/herdr-brain/internal/output"
	"github.com/aryanVijaywargia/herdr-brain/internal/secret"
	"github.com/aryanVijaywargia/herdr-brain/internal/store"
	"github.com/aryanVijaywargia/herdr-brain/internal/workspace"
)

var testNow = time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)

// liveText is the one active item of the fixture store.
const liveText = "live global item"

// fixture is a main store bound to herdr session default. Workspace w14 ("shop") and w15
// ("blog") are mapped; w16 is live in herdr but not mapped yet.
type fixture struct {
	file, sock, store string
	panes             map[string]herdr.Pane // the panes that herdr has
	syncs             int
	herdr             *fakeHerdr
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	dir := t.TempDir()
	f := &fixture{
		file:  filepath.Join(dir, "data", "bindings.json"),
		sock:  filepath.Join(dir, "herdr", "herdr.sock"),
		store: filepath.Join(dir, "main"),
		herdr: &fakeHerdr{next: "w14:p5", procs: map[string][]herdr.Process{}},
	}
	proj := filepath.Join(dir, "proj")
	for _, d := range []string{filepath.Dir(f.sock), f.store, proj} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	proj, err := filepath.EvalSymlinks(proj)
	if err != nil {
		t.Fatal(err)
	}
	if err := bindings.Write(f.file, []bindings.Binding{
		{Session: "default", Socket: f.sock, Store: f.store, Kind: bindings.KindMain, Created: testNow},
	}); err != nil {
		t.Fatal(err)
	}
	writeJSON(t, filepath.Join(filepath.Dir(f.sock), "session.json"), map[string]any{"version": 3,
		"workspaces": []map[string]string{{"id": "w14", "identity_cwd": proj}, {"id": "w15", "identity_cwd": proj}}})
	entry := func(key, id string) map[string]string {
		return map[string]string{"key": key, "session": "default", "workspace_id": id, "label": id, "project": proj}
	}
	writeJSON(t, filepath.Join(f.store, "workspaces.json"), map[string]any{"version": 1,
		"entries": []map[string]string{entry("ws_a14", "w14"), entry("ws_a15", "w15")}})

	s := openStore(t, f.store)
	defer closeStore(t, s)
	write(t, s, func(tx *sql.Tx) error {
		for _, k := range []string{"ws_a14", "ws_a15"} {
			if err := store.InsertWorkspace(context.Background(), tx, store.Workspace{Key: k, HerdrSession: "default",
				HerdrWorkspaceID: strings.Replace(k, "ws_a", "w", 1), Status: store.WorkspaceLive, LastSeen: testNow}); err != nil {
				return err
			}
		}
		_, _, err := store.CreateItem(context.Background(), tx, store.Item{ClaimID: "c1", Kind: store.KindFact, Created: testNow},
			store.ItemRevision{Text: liveText, Scope: store.ScopeGlobal, State: store.StateActive,
				Author: store.AuthorYou, Origin: store.OriginRemember, Created: testNow})
		return err
	})
	if _, err := output.Build(context.Background(), s, filepath.Join(f.store, outDir), nil, now); err != nil {
		t.Fatalf("Build() = %v", err)
	}

	pane := func(id, ws string) herdr.Pane { return herdr.Pane{ID: id, WorkspaceID: ws, TabID: ws + ":t1"} }
	f.panes = map[string]herdr.Pane{
		"w14:p1": pane("w14:p1", "w14"),
		"w14:p2": pane("w20:p2", "w20"), // moved to w20 (S49)
		"w16:p1": pane("w16:p1", "w16"),
	}
	return f
}

func (f *fixture) env(socket, paneID, workspaceID string, stdout *bytes.Buffer) Env {
	return Env{
		Environ: []string{"HERDR_ENV=1", "HERDR_SOCKET_PATH=" + socket, "HERDR_PANE_ID=" + paneID,
			"HERDR_WORKSPACE_ID=" + workspaceID},
		Bindings: f.file,
		Pane: func(_ context.Context, t herdr.Target, id string) (herdr.Pane, error) {
			p, ok := f.panes[id]
			if !ok || t.Socket != f.sock {
				return herdr.Pane{}, fmt.Errorf("pane %s: %w", id, herdr.ErrPaneNotFound)
			}
			return p, nil
		},
		// herdr lists the labels "shop" and "blog"; w16 has no saved root yet.
		Sync: func(ctx context.Context, s workspace.Session, st *store.Store) error {
			f.syncs++
			return s.Refresh(ctx, nil, st, []herdr.Workspace{{ID: "w14", Label: "shop"}, {ID: "w15", Label: "blog"},
				{ID: "w16", Label: "new"}}, testNow)
		},
		Herdr: func(t herdr.Target) (PaneHerdr, error) {
			f.herdr.targets = append(f.herdr.targets, t)
			return f.herdr, nil
		},
		Now:    now,
		Stdout: stdout,
		Stderr: &bytes.Buffer{},
	}
}

func now() time.Time { return testNow }

// rows is what the store holds after a call.
type rows struct {
	items     []store.Item
	revisions []store.ItemRevision
	proposals []store.Proposal
	newest    int64 // the newest output generation
}

func (f *fixture) rows(t *testing.T) rows {
	t.Helper()
	s := openStore(t, f.store)
	defer closeStore(t, s)
	var r rows
	ctx := context.Background()
	write(t, s, func(tx *sql.Tx) (err error) {
		for id := int64(1); ; id++ {
			it, err := store.ItemByID(ctx, tx, id)
			if errors.Is(err, sql.ErrNoRows) {
				break
			}
			if err != nil {
				return err
			}
			rev, err := store.ItemRevisionByKey(ctx, tx, id, it.CurrentRevision)
			if err != nil {
				return err
			}
			r.items, r.revisions = append(r.items, it), append(r.revisions, rev)
		}
		for id := int64(1); ; id++ {
			p, err := store.ProposalByID(ctx, tx, id)
			if errors.Is(err, sql.ErrNoRows) {
				break
			}
			if err != nil {
				return err
			}
			r.proposals = append(r.proposals, p)
		}
		r.newest, err = store.NewestGeneration(ctx, tx)
		return err
	})
	return r
}

func TestRemember(t *testing.T) {
	ptr := func(s string) *string { return &s }
	cases := []struct {
		name      string
		args      []string
		text      string // the text of the draft
		sock      string // "" is the bound socket
		pane, ws  string
		kind      store.Kind
		key       *string // the workspace key of the draft; nil for a global draft
		wantErr   string  // "" when the call writes a draft
		wantNoRun bool    // the call fails before the sync
		moveAt    string  // the calling pane moves to workspace moveAt during the sync
	}{
		{name: "workspace by default", args: []string{"use pnpm, not npm"}, text: "use pnpm, not npm", pane: "w14:p1", ws: "w14",
			kind: store.KindFact, key: ptr("ws_a14")},
		{name: "workspace by id", args: []string{"--workspace", "w14", "use pnpm"}, text: "use pnpm", pane: "w14:p1", ws: "w14",
			kind: store.KindFact, key: ptr("ws_a14")},
		{name: "workspace by label, flags last", args: []string{"use pnpm", "--workspace", "shop", "--kind", "preference"}, text: "use pnpm",
			pane: "w14:p1", ws: "w14", kind: store.KindPreference, key: ptr("ws_a14")},
		{name: "handoff", args: []string{"--kind", "handoff", "next: fix the lint"}, text: "next: fix the lint", pane: "w14:p1", ws: "w14",
			kind: store.KindHandoff, key: ptr("ws_a14")},
		{name: "global", args: []string{"--global", "--kind", "decision", "answer in English"}, text: "answer in English", pane: "w14:p1", ws: "w14",
			kind: store.KindDecision},
		{name: "global from an unmapped workspace", args: []string{"--global", "x"}, text: "x", pane: "w16:p1", ws: "w16",
			kind: store.KindFact},
		{name: "another workspace refused", args: []string{"--workspace", "w15", "x"}, pane: "w14:p1", ws: "w14",
			wantErr: "not the workspace of this pane"},
		{name: "another label refused", args: []string{"--workspace", "blog", "x"}, pane: "w14:p1", ws: "w14",
			wantErr: "not the workspace of this pane"},
		{name: "unmapped workspace refused", args: []string{"x"}, pane: "w16:p1", ws: "w16",
			wantErr: "not mapped yet"},
		{name: "unbound session (S64)", args: []string{"--global", "x"}, sock: "/tmp/other/herdr.sock", pane: "w14:p1", ws: "w14",
			wantErr: "unbound session", wantNoRun: true},
		{name: "pane not live", args: []string{"--global", "x"}, pane: "w14:p9", ws: "w14",
			wantErr: "pane not live", wantNoRun: true},
		{name: "pane moved (S49)", args: []string{"x"}, pane: "w14:p2", ws: "w14",
			wantErr: "not a member", wantNoRun: true},
		{name: "pane moved during the sync", args: []string{"x"}, pane: "w14:p1", ws: "w14", moveAt: "w15",
			wantErr: "moved to workspace w15"},
		{name: "secret in a flag value", args: []string{"--kind=ghp_" + strings.Repeat("a1B2", 9), "x"}, pane: "w14:p1", ws: "w14",
			wantErr: "github-token", wantNoRun: true},
		{name: "secret", args: []string{"--global", "password=hunter22"}, pane: "w14:p1", ws: "w14",
			wantErr: "password-pair", wantNoRun: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			before := f.rows(t)
			sock := c.sock
			if sock == "" {
				sock = f.sock
			}
			var out bytes.Buffer
			e := f.env(sock, c.pane, c.ws, &out)
			if c.moveAt != "" {
				sync := e.Sync
				e.Sync = func(ctx context.Context, s workspace.Session, st *store.Store) error {
					f.panes[c.pane] = herdr.Pane{ID: c.pane, WorkspaceID: c.moveAt}
					return sync(ctx, s, st)
				}
			}
			err := Remember(context.Background(), e, c.args)
			after := f.rows(t)
			if (f.syncs == 0) != c.wantNoRun {
				t.Errorf("Remember(%q) synced %d times, want a sync: %v", c.args, f.syncs, !c.wantNoRun)
			}
			if c.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) || strings.Contains(err.Error(), "a1B2") {
					t.Errorf("Remember(%q) = %v, want an error with %q", c.args, err, c.wantErr)
				}
				if len(after.items) != len(before.items) || len(after.proposals) != len(before.proposals) || out.Len() != 0 {
					t.Errorf("Remember(%q) wrote %d items, %d proposals, output %q; want nothing", c.args,
						len(after.items)-len(before.items), len(after.proposals)-len(before.proposals), out.String())
				}
				return
			}
			if err != nil {
				t.Fatalf("Remember(%q) = %v", c.args, err)
			}
			checkDraft(t, c.args, c.text, before, after, c.kind, c.key)
			want := fmt.Sprintf("proposal %d: ", after.proposals[len(after.proposals)-1].ID)
			if !strings.HasPrefix(out.String(), want) || !strings.Contains(out.String(), "waiting for your review") {
				t.Errorf("Remember(%q) printed %q, want %q... waiting for your review", c.args, out.String(), want)
			}
		})
	}
}

// checkDraft checks that the call added one draft item at revision 1 and one pending proposal
// for it, and recorded no output generation.
func checkDraft(t *testing.T, args []string, text string, before, after rows, kind store.Kind, key *string) {
	t.Helper()
	if len(after.items) != len(before.items)+1 || len(after.proposals) != len(before.proposals)+1 {
		t.Fatalf("Remember(%q) wrote %d items, %d proposals; want 1 and 1", args,
			len(after.items)-len(before.items), len(after.proposals)-len(before.proposals))
	}
	it, rev, p := after.items[len(after.items)-1], after.revisions[len(after.revisions)-1], after.proposals[len(after.proposals)-1]
	scope := store.ScopeGlobal
	if key != nil {
		scope = store.ScopeWorkspace
	}
	switch {
	case it.State != store.StateDraft || it.CurrentRevision != 1 || it.Kind != kind || it.Scope != scope ||
		!equal(it.WorkspaceKey, key) || !strings.HasPrefix(it.ClaimID, "cl_"):
		t.Errorf("Remember(%q) item = %+v, want a %s draft at revision 1 in %s %v with a claim id", args, it, kind, scope, deref(key))
	case rev.Text != text || rev.Author != store.AuthorBrain || rev.Origin != store.OriginRemember ||
		rev.State != store.StateDraft || rev.ProposalID == nil || *rev.ProposalID != p.ID:
		t.Errorf("Remember(%q) revision = %+v, want text %q by brain from remember, proposal %d", args, rev, text, p.ID)
	case p.Status != store.ProposalPending || p.ItemID == nil || *p.ItemID != it.ID || p.BaseRevision == nil ||
		*p.BaseRevision != 1 || p.Text != text || p.Scope != scope || p.Surface != nil:
		t.Errorf("Remember(%q) proposal = %+v, want pending for item %d at revision 1", args, p, it.ID)
	case after.newest != before.newest:
		t.Errorf("Remember(%q) newest generation = %d, want %d (a draft changes no output)", args, after.newest, before.newest)
	}
}

// TestDraftNotLive is the S52 path: an agent's call makes a draft, and nothing goes live. The
// next command's repair builds no output with the draft in it.
func TestDraftNotLive(t *testing.T) {
	f := newFixture(t)
	const text = "agent says: skip the tests"
	var out bytes.Buffer
	e := f.env(f.sock, "w14:p1", "w14", &out)
	for _, args := range [][]string{{text}, {"--global", text}} {
		if err := Remember(context.Background(), e, args); err != nil {
			t.Fatalf("Remember(%q) = %v", args, err)
		}
	}
	// A later command runs the repair: the output must still hold only the live item.
	c, err := e.Open(context.Background())
	if err != nil {
		t.Fatalf("Open() = %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	b, err := output.Read(filepath.Join(f.store, outDir), "ws_a14")
	if err != nil {
		t.Fatalf("output.Read() = %v", err)
	}
	if got := b.Global + b.Workspace; strings.Contains(got, text) || !strings.Contains(got, liveText) {
		t.Errorf("brief = %q, want %q and no draft", got, liveText)
	}
	for _, r := range f.rows(t).revisions {
		if r.Text == text && r.State != store.StateDraft {
			t.Errorf("draft revision = %+v, want state draft", r)
		}
	}
}

func TestParseRemember(t *testing.T) {
	cases := []struct {
		args    []string
		wantErr string
	}{
		{nil, "want one text, got 0"},
		{[]string{"a", "b"}, "want one text, got 2"},
		{[]string{"  "}, "the text is empty"},
		{[]string{"--global", "--workspace", "w14", "x"}, "exclude each other"},
		{[]string{"--kind", "skill", "x"}, `unknown kind "skill"`},
		{[]string{"--global", "--kind", "handoff", "x"}, "a handoff is a workspace item"},
		{[]string{"--nope", "x"}, "flag provided but not defined"},
		{[]string{"--", "--global is a flag"}, ""},
	}
	for _, c := range cases {
		_, err := parseRemember(c.args)
		if c.wantErr == "" {
			if err != nil {
				t.Errorf("parseRemember(%q) = %v, want nil", c.args, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), c.wantErr) {
			t.Errorf("parseRemember(%q) = %v, want an error with %q", c.args, err, c.wantErr)
		}
	}
}

// TestSecretClassOnly checks that a refused text prints the class, never the matched text.
func TestSecretClassOnly(t *testing.T) {
	token := "ghp_" + strings.Repeat("a1B2", 9)
	f := newFixture(t)
	var out bytes.Buffer
	err := Remember(context.Background(), f.env(f.sock, "w14:p1", "w14", &out), []string{"token " + token})
	var se *secret.Error
	if !errors.As(err, &se) || !strings.Contains(err.Error(), "github-token") || strings.Contains(err.Error(), token) {
		t.Errorf("Remember(token) = %v, want a *secret.Error with the class and no token", err)
	}
}

func equal(a, b *string) bool { return (a == nil) == (b == nil) && (a == nil || *a == *b) }

func deref(p *string) string {
	if p == nil {
		return "<nil>"
	}
	return *p
}

func writeJSON(t *testing.T, path string, v any) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func openStore(t *testing.T, dir string) *store.Store {
	t.Helper()
	s, err := store.Open(context.Background(), dir, busyTimeout)
	if err != nil {
		t.Fatalf("Open() = %v", err)
	}
	return s
}

func closeStore(t *testing.T, s *store.Store) {
	t.Helper()
	if err := s.Close(); err != nil {
		t.Fatalf("Close() = %v", err)
	}
}

func write(t *testing.T, s *store.Store, fn func(tx *sql.Tx) error) {
	t.Helper()
	if err := s.Write(context.Background(), fn); err != nil {
		t.Fatalf("Write() = %v", err)
	}
}
