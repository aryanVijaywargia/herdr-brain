package hook

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"testing"
	"testing/fstest"
	"time"

	"github.com/aryanVijaywargia/herdr-brain/internal/adapter"
	"github.com/aryanVijaywargia/herdr-brain/internal/bindings"
	"github.com/aryanVijaywargia/herdr-brain/internal/herdr"
	"github.com/aryanVijaywargia/herdr-brain/internal/output"
	"github.com/aryanVijaywargia/herdr-brain/internal/paths"
	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

var testNow = time.Date(2026, 9, 28, 10, 30, 15, 123_000_000, time.UTC)

// testTimeout is the busy timeout of the test stores. [NEW]
const testTimeout = 5 * time.Second

const input = `{"session_id":"sess-1","cwd":"/tmp/x"}`

// The item texts. Each text appears in one store and one part only.
const (
	mainGlobal = "main global item"
	mainA14    = "workspace item of w14 in session default"
	testGlobal = "test global item"
)

// fakeHerdr answers `pane get` per socket. A missing pane is gone.
type fakeHerdr struct {
	panes map[string]map[string]herdr.Pane // socket → pane id → pane
	slow  map[string]bool                  // pane ids that never answer before the deadline
	calls int
}

func (h *fakeHerdr) pane(ctx context.Context, t herdr.Target, id string) (herdr.Pane, error) {
	h.calls++
	if h.slow[id] {
		<-ctx.Done()
		return herdr.Pane{}, fmt.Errorf("herdr pane get: %w", ctx.Err())
	}
	p, ok := h.panes[t.Socket][id]
	if !ok {
		return herdr.Pane{}, fmt.Errorf("pane %s: %w", id, herdr.ErrPaneNotFound)
	}
	return p, nil
}

// fixture has the main store, bound to herdr sessions default (A) and work (B), and a test
// store bound to the test session brain-eval. Both A and B have a workspace w14.
type fixture struct {
	dir                          string
	file                         string
	mainSock, workSock, testSock string
	mainStore, testStore         string
	herdr                        *fakeHerdr
	gen, testGen                 int64 // the current generations of the main and the test store
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	dir := t.TempDir()
	f := &fixture{
		dir:       dir,
		file:      filepath.Join(dir, "data", "bindings.json"),
		mainSock:  filepath.Join(dir, "herdr", "herdr.sock"),
		workSock:  filepath.Join(dir, "herdr", "sessions", "work", "herdr.sock"),
		testSock:  filepath.Join(dir, "herdr", "sessions", "brain-eval", "herdr.sock"),
		mainStore: filepath.Join(dir, "main"),
		testStore: filepath.Join(dir, "eval"),
	}
	proj := filepath.Join(dir, "proj")
	other := filepath.Join(dir, "other")
	for _, d := range []string{filepath.Dir(f.mainSock), filepath.Dir(f.workSock), filepath.Dir(f.testSock),
		f.mainStore, f.testStore, filepath.Join(proj, "sub"), other} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	proj = canonical(t, proj)
	err := bindings.Write(f.file, []bindings.Binding{
		{Session: "default", Socket: f.mainSock, Store: f.mainStore, Kind: bindings.KindMain, Created: testNow},
		{Session: "work", Socket: f.workSock, Store: f.mainStore, Kind: bindings.KindMain, Created: testNow},
		{Session: "brain-eval", Socket: f.testSock, Store: f.testStore, Kind: bindings.KindTest, Created: testNow},
	})
	if err != nil {
		t.Fatal(err)
	}
	// w16's saved root lies outside its project folder: "withheld: identity".
	writeJSON(t, filepath.Join(filepath.Dir(f.mainSock), "session.json"), sessionFile(map[string]string{
		"w14": filepath.Join(proj, "sub"), "w15": proj, "w16": canonical(t, other)}))
	writeJSON(t, filepath.Join(filepath.Dir(f.workSock), "session.json"), sessionFile(map[string]string{"w14": proj}))
	writeJSON(t, filepath.Join(filepath.Dir(f.testSock), "session.json"), sessionFile(map[string]string{"w14": proj}))
	writeJSON(t, filepath.Join(f.mainStore, "workspaces.json"), mapFile(
		entry("ws_a14", "default", "w14", proj), entry("ws_a16", "default", "w16", proj), entry("ws_b14", "work", "w14", proj)))
	writeJSON(t, filepath.Join(f.testStore, "workspaces.json"), mapFile(entry("ws_t14", "brain-eval", "w14", proj)))

	s := openStore(t, f.mainStore)
	addWorkspace(t, s, "ws_a14", "default", "w14")
	addWorkspace(t, s, "ws_a16", "default", "w16")
	addWorkspace(t, s, "ws_b14", "work", "w14")
	addItem(t, s, "", mainGlobal)
	addItem(t, s, "ws_a14", mainA14)
	f.gen = build(t, s, f.mainStore)
	closeStore(t, s)

	s = openStore(t, f.testStore)
	addWorkspace(t, s, "ws_t14", "brain-eval", "w14")
	addItem(t, s, "", testGlobal)
	f.testGen = build(t, s, f.testStore)
	closeStore(t, s)

	pane := func(id, ws string) herdr.Pane { return herdr.Pane{ID: id, WorkspaceID: ws, TabID: ws + ":t1"} }
	f.herdr = &fakeHerdr{
		panes: map[string]map[string]herdr.Pane{
			f.mainSock: {
				"w14:p1": pane("w14:p1", "w14"),
				// w14:p2 moved to w20; herdr answers its old id with the new one (S49).
				"w14:p2": pane("w20:p2", "w20"),
				"w15:p1": pane("w15:p1", "w15"),
				"w16:p1": pane("w16:p1", "w16"),
			},
			f.workSock: {"w14:p1": pane("w14:p1", "w14")},
			f.testSock: {"w14:p1": pane("w14:p1", "w14"), "w14:p2": pane("w20:p2", "w20")},
		},
		slow: map[string]bool{"w14:p9": true},
	}
	return f
}

func (f *fixture) hook(p *adapter.Profile, environ []string) Hook {
	return Hook{Profile: p, Environ: environ, Bindings: f.file, Pane: f.herdr.pane, Start: time.Now(),
		Now: func() time.Time { return testNow }}
}

func environ(socket, session, paneID, workspaceID string) []string {
	e := []string{"HOME=/home/me", "HERDR_ENV=1", "HERDR_SOCKET_PATH=" + socket,
		"HERDR_PANE_ID=" + paneID, "HERDR_WORKSPACE_ID=" + workspaceID}
	if session != "" {
		e = append(e, "HERDR_SESSION="+session)
	}
	return e
}

func canonical(t *testing.T, p string) string {
	t.Helper()
	c, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	return c
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

func sessionFile(roots map[string]string) any {
	type ws struct {
		ID          string `json:"id"`
		IdentityCwd string `json:"identity_cwd"`
	}
	var list []ws
	for id, root := range roots {
		list = append(list, ws{id, root})
	}
	return map[string]any{"version": 3, "workspaces": list}
}

func entry(key, session, id, project string) map[string]string {
	return map[string]string{"key": key, "session": session, "workspace_id": id, "label": id, "project": project}
}

func mapFile(entries ...map[string]string) any {
	return map[string]any{"version": 1, "entries": entries}
}

func openStore(t *testing.T, dir string) *store.Store {
	t.Helper()
	s, err := store.Open(context.Background(), dir, testTimeout)
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

func addWorkspace(t *testing.T, s *store.Store, key, session, id string) {
	t.Helper()
	write(t, s, func(tx *sql.Tx) error {
		return store.InsertWorkspace(context.Background(), tx, store.Workspace{Key: key, HerdrSession: session,
			HerdrWorkspaceID: id, Label: id, Status: store.WorkspaceLive, LastSeen: testNow})
	})
}

// addItem commits a new active item. key "" makes a global item.
func addItem(t *testing.T, s *store.Store, key, text string) {
	t.Helper()
	r := store.ItemRevision{Text: text, Scope: store.ScopeGlobal, State: store.StateActive,
		Author: store.AuthorYou, Origin: store.OriginRemember, Created: testNow}
	if key != "" {
		r.Scope, r.WorkspaceKey = store.ScopeWorkspace, &key
	}
	write(t, s, func(tx *sql.Tx) error {
		_, _, err := store.CreateItem(context.Background(), tx,
			store.Item{ClaimID: text, Kind: store.KindFact, Created: testNow}, r)
		return err
	})
}

func build(t *testing.T, s *store.Store, dir string) int64 {
	t.Helper()
	gen, err := output.Build(context.Background(), s, filepath.Join(dir, outDir), nil, func() time.Time { return testNow })
	if err != nil {
		t.Fatalf("Build() = %v", err)
	}
	return gen
}

// deliveries returns every receipt in the store folder dir.
func deliveries(t *testing.T, dir string) []store.Delivery {
	t.Helper()
	s := openStore(t, dir)
	defer closeStore(t, s)
	var ds []store.Delivery
	write(t, s, func(tx *sql.Tx) error {
		rows, err := tx.Query(`SELECT id FROM deliveries ORDER BY id`)
		if err != nil {
			return err
		}
		var ids []int64
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				return errors.Join(err, rows.Close())
			}
			ids = append(ids, id)
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			return err
		}
		for _, id := range ids {
			d, err := store.DeliveryByID(context.Background(), tx, id)
			if err != nil {
				return err
			}
			ds = append(ds, d)
		}
		return nil
	})
	return ds
}

// tree returns every path below dir with its size and time, to show that a run touched nothing.
func tree(t *testing.T, dir string) map[string]string {
	t.Helper()
	m := map[string]string{}
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		m[p] = fmt.Sprint(fi.Size(), fi.ModTime().UnixNano())
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// briefOf returns the brief in the hook output, or "" when the hook printed nothing.
func briefOf(t *testing.T, out string) string {
	t.Helper()
	if out == "" {
		return ""
	}
	var v struct {
		HookSpecificOutput struct {
			HookEventName     string `json:"hookEventName"`
			AdditionalContext string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		t.Fatalf("hook output %q: %v", out, err)
	}
	if v.HookSpecificOutput.HookEventName != "SessionStart" || v.HookSpecificOutput.AdditionalContext == "" {
		t.Fatalf("hook output %q: want a SessionStart brief", out)
	}
	return v.HookSpecificOutput.AdditionalContext
}

func claude(t *testing.T) *adapter.Profile {
	t.Helper()
	p, err := adapter.Load("claude")
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func ptr[T any](v T) *T { return &v }

// TestRun has one case per step of §8.3 with a store. The brief holds the named texts and
// none of the other item texts; the receipt, if any, lands in the named store only.
func TestRun(t *testing.T) {
	all := []string{mainGlobal, mainA14, testGlobal}
	tests := []struct {
		name      string
		env       func(f *fixture) []string
		prep      func(t *testing.T, f *fixture)
		want      []string // item texts in the brief; nil: nothing printed
		inTest    bool     // the receipt is in the test store, not the main store
		receipt   *store.Delivery
		noReceipt bool // no store changes at all
	}{
		{
			name: "mapped workspace gets global and its part",
			env:  func(f *fixture) []string { return environ(f.mainSock, "", "w14:p1", "w14") },
			want: []string{mainGlobal, mainA14},
			receipt: &store.Delivery{HerdrSession: ptr("default"), PaneID: "w14:p1", Result: store.ResultEmitted, WorkspaceKey: ptr("ws_a14"),
				ItemRevisions: []store.RevisionRef{{Item: 1, Revision: 1}, {Item: 2, Revision: 1}}},
		},
		{
			// S79: B's w14 is another workspace than A's w14.
			name: "S79 other session same workspace id",
			env:  func(f *fixture) []string { return environ(f.workSock, "work", "w14:p1", "w14") },
			want: []string{mainGlobal},
			receipt: &store.Delivery{HerdrSession: ptr("work"), PaneID: "w14:p1", Result: store.ResultEmitted, WorkspaceKey: ptr("ws_b14"),
				ItemRevisions: []store.RevisionRef{{Item: 1, Revision: 1}}},
		},
		{
			name: "S48 unmapped workspace gets global only",
			env:  func(f *fixture) []string { return environ(f.mainSock, "", "w15:p1", "w15") },
			want: []string{mainGlobal},
			receipt: &store.Delivery{HerdrSession: ptr("default"), PaneID: "w15:p1", Result: store.ResultUnmapped,
				ItemRevisions: []store.RevisionRef{{Item: 1, Revision: 1}}},
		},
		{
			name: "root outside project gets global only",
			env:  func(f *fixture) []string { return environ(f.mainSock, "", "w16:p1", "w16") },
			want: []string{mainGlobal},
			receipt: &store.Delivery{HerdrSession: ptr("default"), PaneID: "w16:p1", Result: store.ResultWithheld, WithheldReason: ptr(reasonIdentity),
				WorkspaceKey: ptr("ws_a16"), ItemRevisions: []store.RevisionRef{{Item: 1, Revision: 1}}},
		},
		{
			name:    "S74 test pane gets test items only",
			env:     func(f *fixture) []string { return environ(f.testSock, "brain-eval", "w14:p1", "w14") },
			want:    []string{testGlobal},
			inTest:  true,
			receipt: &store.Delivery{HerdrSession: ptr("brain-eval"), PaneID: "w14:p1", Result: store.ResultEmitted, WorkspaceKey: ptr("ws_t14"), ItemRevisions: []store.RevisionRef{{Item: 1, Revision: 1}}},
		},
		{
			name:    "S63 closed pane",
			env:     func(f *fixture) []string { return environ(f.mainSock, "", "w14:p7", "w14") },
			receipt: &store.Delivery{HerdrSession: ptr("default"), PaneID: "w14:p7", Result: store.ResultWithheld, WithheldReason: ptr("pane not live"), ItemRevisions: []store.RevisionRef{}},
		},
		{
			name:    "S49 moved pane",
			env:     func(f *fixture) []string { return environ(f.mainSock, "", "w14:p2", "w14") },
			receipt: &store.Delivery{HerdrSession: ptr("default"), PaneID: "w14:p2", Result: store.ResultWithheld, WithheldReason: ptr("not a member"), ItemRevisions: []store.RevisionRef{}},
		},
		{
			name:    "closed pane of the test session",
			env:     func(f *fixture) []string { return environ(f.testSock, "brain-eval", "w14:p7", "w14") },
			inTest:  true,
			receipt: &store.Delivery{HerdrSession: ptr("brain-eval"), PaneID: "w14:p7", Result: store.ResultWithheld, WithheldReason: ptr("pane not live"), ItemRevisions: []store.RevisionRef{}},
		},
		{
			name:    "moved pane of the test session",
			env:     func(f *fixture) []string { return environ(f.testSock, "brain-eval", "w14:p2", "w14") },
			inTest:  true,
			receipt: &store.Delivery{HerdrSession: ptr("brain-eval"), PaneID: "w14:p2", Result: store.ResultWithheld, WithheldReason: ptr("not a member"), ItemRevisions: []store.RevisionRef{}},
		},
		{
			name: "S64 unbound session",
			env: func(f *fixture) []string {
				return environ(filepath.Join(f.dir, "other.sock"), "other", "w14:p1", "w14")
			},
			noReceipt: true,
		},
		{
			name:      "lookup timeout",
			env:       func(f *fixture) []string { return environ(f.mainSock, "", "w14:p9", "w14") },
			noReceipt: true,
		},
		{
			name: "forget pending",
			env:  func(f *fixture) []string { return environ(f.mainSock, "", "w14:p1", "w14") },
			prep: func(t *testing.T, f *fixture) {
				t.Helper()
				s := openStore(t, f.mainStore)
				defer closeStore(t, s)
				write(t, s, func(tx *sql.Tx) error {
					_, err := tx.Exec(`UPDATE outputs SET holds_forgotten_text = 1`)
					return err
				})
			},
			receipt: &store.Delivery{HerdrSession: ptr("default"), PaneID: "w14:p1", Result: store.ResultWithheld, WithheldReason: ptr(reasonForget),
				WorkspaceKey: ptr("ws_a14"), ItemRevisions: []store.RevisionRef{}},
		},
		{
			name: "S66 manifest mismatch",
			env:  func(f *fixture) []string { return environ(f.mainSock, "", "w14:p1", "w14") },
			prep: func(t *testing.T, f *fixture) {
				t.Helper()
				name := filepath.Join(f.mainStore, outDir, fmt.Sprintf("gen-%d", f.gen), "workspaces", "ws_a14.md")
				if err := os.WriteFile(name, []byte("edited by hand\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			receipt: &store.Delivery{HerdrSession: ptr("default"), PaneID: "w14:p1", Result: store.ResultWithheld, WithheldReason: ptr(reasonInconsistent),
				WorkspaceKey: ptr("ws_a14"), ItemRevisions: []store.RevisionRef{}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			if tt.prep != nil {
				tt.prep(t, f)
			}
			before := map[string]map[string]string{f.mainStore: tree(t, f.mainStore), f.testStore: tree(t, f.testStore)}
			var out bytes.Buffer
			if err := f.hook(claude(t), tt.env(f)).Run(context.Background(), strings.NewReader(input), &out); err != nil {
				t.Fatalf("Run() = %v, want nil", err)
			}
			got := briefOf(t, out.String())
			for _, text := range all {
				if want := slices.Contains(tt.want, text); strings.Contains(got, text) != want {
					t.Errorf("brief has %q = %v, want %v; brief:\n%s", text, !want, want, got)
				}
			}
			touched, untouched := f.mainStore, f.testStore
			if tt.inTest {
				touched, untouched = untouched, touched
			}
			if !reflect.DeepEqual(tree(t, untouched), before[untouched]) {
				t.Errorf("store %s changed, want it untouched", untouched)
			}
			if tt.noReceipt {
				if !reflect.DeepEqual(tree(t, touched), before[touched]) {
					t.Errorf("store %s changed, want no store opened", touched)
				}
				return
			}
			gen := f.gen
			if tt.inTest {
				gen = f.testGen
			}
			checkReceipt(t, deliveries(t, touched), tt.receipt, got, gen)
		})
	}
}

// checkReceipt checks that ds is the one receipt want, with the fields that every receipt has
// and, for a printed brief, its hash and generation.
func checkReceipt(t *testing.T, ds []store.Delivery, want *store.Delivery, brief string, gen int64) {
	t.Helper()
	if len(ds) != 1 {
		t.Fatalf("receipts = %d, want 1: %+v", len(ds), ds)
	}
	w := *want
	w.ID, w.Profile, w.AgentSessionID, w.At = ds[0].ID, "claude", ptr("sess-1"), testNow
	if brief != "" {
		sum := sha256.Sum256([]byte(brief))
		w.BriefHash, w.OutputGeneration = ptr(hex.EncodeToString(sum[:])), &gen
	}
	if !reflect.DeepEqual(ds[0], w) {
		t.Errorf("receipt = %s, want %s", show(ds[0]), show(w))
	}
}

func show(d store.Delivery) string {
	b, err := json.Marshal(d)
	if err != nil {
		return err.Error()
	}
	return string(b)
}

// TestForgetDuringRead: a forget that starts while the hook reads the output waits for the
// receipt, so the hook never prints text after forget became pending (§6.7). The global part
// is a FIFO, so the hook stops in its read until the test writes the checked bytes.
func TestForgetDuringRead(t *testing.T) {
	f := newFixture(t)
	name := filepath.Join(f.mainStore, outDir, fmt.Sprintf("gen-%d", f.gen), "global.md")
	global, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(name); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(name, 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	done := make(chan error, 1)
	go func() {
		done <- f.hook(claude(t), environ(f.mainSock, "", "w14:p1", "w14")).Run(context.Background(), strings.NewReader(input), &out)
	}()
	// The open returns when the hook opens the FIFO to read it.
	w, err := os.OpenFile(name, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(context.Background(), f.mainStore, 50*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	forget := func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE outputs SET holds_forgotten_text = 1`)
		return err
	}
	if err := s.Write(context.Background(), forget); !errors.Is(err, store.ErrBusy) {
		t.Errorf("forget during the hook read = %v, want %v", err, store.ErrBusy)
	}
	if _, err := w.Write(global); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("Run() = %v, want nil", err)
	}
	if err := s.Write(context.Background(), forget); err != nil {
		t.Errorf("forget after the hook = %v, want nil", err)
	}
	closeStore(t, s)
	got := briefOf(t, out.String())
	if !strings.Contains(got, mainGlobal) || !strings.Contains(got, mainA14) {
		t.Errorf("brief = %q, want the items of gen %d", got, f.gen)
	}
	checkReceipt(t, deliveries(t, f.mainStore), &store.Delivery{HerdrSession: ptr("default"), PaneID: "w14:p1",
		Result: store.ResultEmitted, WorkspaceKey: ptr("ws_a14"),
		ItemRevisions: []store.RevisionRef{{Item: 1, Revision: 1}, {Item: 2, Revision: 1}}}, got, f.gen)
}

// TestOutsideHerdr: the hook reads no file and asks no herdr outside herdr (S61). The
// bindings path is a folder, so a read of it would fail.
func TestOutsideHerdr(t *testing.T) {
	for _, env := range [][]string{{"HOME=/home/me"}, {"HERDR_ENV=0", "HERDR_PANE_ID=w14:p1"}} {
		f := newFixture(t)
		before := tree(t, f.dir)
		h := f.hook(claude(t), env)
		h.Bindings = f.dir
		var out bytes.Buffer
		if err := h.Run(context.Background(), strings.NewReader(input), &out); err != nil {
			t.Errorf("Run(%q) = %v, want nil", env, err)
		}
		if out.Len() != 0 || f.herdr.calls != 0 || !reflect.DeepEqual(tree(t, f.dir), before) {
			t.Errorf("Run(%q): output %q, %d herdr calls, files changed; want none", env, out.String(), f.herdr.calls)
		}
	}
}

// TestStartCondition: hook input that fails the profile's start condition prints nothing and
// asks no herdr; input of an unknown shape prints nothing.
func TestStartCondition(t *testing.T) {
	p, err := adapter.LoadFS(fstest.MapFS{"agy.toml": {Data: []byte(`kind = "agy"
install_target = "~/x"
events.start = "SessionStart"
start_condition = "invocationNum == 0"
output_form = "hookSpecificOutput.additionalContext"
input.session_id = "session_id"
input.cwd = "cwd"
tested_version = "1"
`)}}, "agy")
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		input   string
		wantErr error
	}{
		{`{"invocationNum":1,"session_id":"s","cwd":"/x"}`, nil},
		{`not json`, adapter.ErrRetest},
	}
	for _, tt := range tests {
		f := newFixture(t)
		var out bytes.Buffer
		err := f.hook(p, environ(f.mainSock, "", "w14:p1", "w14")).Run(context.Background(), strings.NewReader(tt.input), &out)
		if !errors.Is(err, tt.wantErr) || out.Len() != 0 || f.herdr.calls != 0 {
			t.Errorf("Run(%s) = %v, output %q, %d herdr calls; want %v, none, 0", tt.input, err, out.String(), f.herdr.calls, tt.wantErr)
		}
	}
	f := newFixture(t)
	var out bytes.Buffer
	in := `{"invocationNum":0,"session_id":"s","cwd":"/x"}`
	if err := f.hook(p, environ(f.mainSock, "", "w14:p1", "w14")).Run(context.Background(), strings.NewReader(in), &out); err != nil || out.Len() == 0 {
		t.Errorf("Run(first invocation) = %v, output %q; want a brief", err, out.String())
	}
}

// TestPartialBuild (S65): while a build stops between two files, the hook prints one whole
// generation or nothing.
func TestPartialBuild(t *testing.T) {
	f := newFixture(t)
	out := filepath.Join(f.mainStore, outDir)
	run := func() string {
		t.Helper()
		var b bytes.Buffer
		if err := f.hook(claude(t), environ(f.mainSock, "", "w14:p1", "w14")).Run(context.Background(), strings.NewReader(input), &b); err != nil {
			t.Fatalf("Run() = %v", err)
		}
		return briefOf(t, b.String())
	}
	whole := run()

	// The next build wrote a new global part, and stopped before the workspace part and the
	// switch: the pointer still names the old generation.
	next := filepath.Join(out, fmt.Sprintf("gen-%d", f.gen+1))
	if err := os.MkdirAll(filepath.Join(next, "workspaces"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(next, "global.md"), []byte("## Global (all workspaces)\n- [9] new\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := run(); got != whole {
		t.Errorf("brief during a build = %q, want the whole old generation %q", got, whole)
	}

	// A pointer on a generation with a missing file (the manifest has it): nothing prints.
	old := filepath.Join(out, fmt.Sprintf("gen-%d", f.gen))
	if err := os.Remove(filepath.Join(old, "workspaces", "ws_a14.md")); err != nil {
		t.Fatal(err)
	}
	if got := run(); got != "" {
		t.Errorf("brief with a part missing = %q, want nothing", got)
	}
}

// TestProfileArg: the hook takes only the frozen arguments.
func TestProfileArg(t *testing.T) {
	tests := []struct {
		args []string
		want string
		ok   bool
	}{
		{[]string{"--profile", "codex", "--event", "start"}, "codex", true},
		{[]string{"--profile", "codex", "--event", "stop"}, "", false},
		{[]string{"--event", "start", "--profile", "codex"}, "", false},
		{[]string{"--profile", "", "--event", "start"}, "", false},
		{[]string{"--profile", "codex"}, "", false},
	}
	for _, tt := range tests {
		got, err := profileArg(tt.args)
		if got != tt.want || (err == nil) != tt.ok {
			t.Errorf("profileArg(%q) = %q, %v; want %q, ok %v", tt.args, got, err, tt.want, tt.ok)
		}
	}
}

// capSlack is how far past the cap a capped Run may return on a slow test machine. [NEW]
const capSlack = 200 * time.Millisecond

// blockedWriter stalls every write until release closes; then it fails the write.
type blockedWriter struct {
	release chan struct{}
	started chan struct{}
}

func (w *blockedWriter) Write([]byte) (int, error) {
	close(w.started)
	<-w.release
	return 0, errors.New("stalled output")
}

// TestCap (S50, E7-S4 FINDINGS): stdin held open, a stalled lookup and a stalled output each
// end Run by the cap. The stalled step goes on after Run returns, as it would until the
// process exits: it prints nothing late and writes no receipt.
func TestCap(t *testing.T) {
	tests := []struct {
		name  string
		setup func(f *fixture, h *Hook, release <-chan struct{}) (stdin io.Reader, stdout io.Writer)
	}{
		{"stdin held open", func(_ *fixture, _ *Hook, release <-chan struct{}) (io.Reader, io.Writer) {
			r, w := io.Pipe()
			go func() {
				<-release
				_, _ = w.Write([]byte(input))
				_ = w.Close()
			}()
			return r, &bytes.Buffer{}
		}},
		{"stalled lookup", func(f *fixture, h *Hook, release <-chan struct{}) (io.Reader, io.Writer) {
			// This herdr ignores the lookup deadline.
			h.Pane = func(_ context.Context, t herdr.Target, id string) (herdr.Pane, error) {
				<-release
				return f.herdr.pane(context.Background(), t, id)
			}
			return strings.NewReader(input), &bytes.Buffer{}
		}},
		{"stalled output", func(_ *fixture, _ *Hook, _ <-chan struct{}) (io.Reader, io.Writer) {
			return strings.NewReader(input), &blockedWriter{release: make(chan struct{}), started: make(chan struct{})}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			h := f.hook(claude(t), environ(f.mainSock, "", "w14:p1", "w14"))
			ended := make(chan struct{})
			h.ended = func() { close(ended) }
			release := make(chan struct{})
			stdin, stdout := tt.setup(f, &h, release)
			bw, stalledOut := stdout.(*blockedWriter)
			buf, _ := stdout.(*bytes.Buffer)
			if stalledOut {
				// The stalled print ends with the test, as it ends with the process.
				t.Cleanup(func() { close(bw.release) })
			}
			err := h.Run(context.Background(), stdin, stdout)
			if took := time.Since(h.Start); !errors.Is(err, errCapped) || took > hookCap+capSlack {
				t.Fatalf("Run() = %v after %v, want %v by %v", err, took, errCapped, hookCap+capSlack)
			}
			if stalledOut {
				<-bw.started // the print started before the cap and stalled
			} else {
				close(release)
				<-ended
				if out := buf.String(); out != "" {
					t.Errorf("late print %q, want none", out)
				}
			}
			if ds := deliveries(t, f.mainStore); len(ds) != 0 {
				t.Errorf("receipts = %s, want none", show(ds[0]))
			}
		})
	}
}

// TestReceiptFailsAfterPrint (S50): the brief is printed, and the failed receipt write leaves
// no receipt, so status shows the delivery as unknown, not withheld.
func TestReceiptFailsAfterPrint(t *testing.T) {
	f := newFixture(t)
	s := openStore(t, f.mainStore)
	write(t, s, func(tx *sql.Tx) error {
		_, err := tx.Exec(`CREATE TRIGGER no_receipt BEFORE INSERT ON deliveries BEGIN SELECT RAISE(ABORT, 'disk full'); END`)
		return err
	})
	closeStore(t, s)
	var out bytes.Buffer
	err := f.hook(claude(t), environ(f.mainSock, "", "w14:p1", "w14")).Run(context.Background(), strings.NewReader(input), &out)
	if err == nil {
		t.Error("Run() = nil, want the receipt error")
	}
	if got := briefOf(t, out.String()); !strings.Contains(got, mainGlobal) || !strings.Contains(got, mainA14) {
		t.Errorf("brief = %q, want the printed brief", got)
	}
	if ds := deliveries(t, f.mainStore); len(ds) != 0 {
		t.Errorf("receipts = %s, want none", show(ds[0]))
	}
}

// TestReceiptNamesPrintedRevision: with the store at revision 3 and the pointer on a
// generation of revision 2, the receipt names revision 2, from the manifest.
func TestReceiptNamesPrintedRevision(t *testing.T) {
	f := newFixture(t)
	s := openStore(t, f.mainStore)
	revise := func(rev int64, text string) {
		t.Helper()
		write(t, s, func(tx *sql.Tx) error {
			_, err := store.WriteRevision(context.Background(), tx, rev-1, store.ItemRevision{ItemID: 1, Text: text,
				Scope: store.ScopeGlobal, State: store.StateActive, Author: store.AuthorYou, Origin: store.OriginRemember,
				Created: testNow})
			return err
		})
	}
	revise(2, "global item at revision 2")
	gen := build(t, s, f.mainStore)
	revise(3, "global item at revision 3")
	closeStore(t, s)

	var out bytes.Buffer
	if err := f.hook(claude(t), environ(f.mainSock, "", "w14:p1", "w14")).Run(context.Background(), strings.NewReader(input), &out); err != nil {
		t.Fatalf("Run() = %v", err)
	}
	got := briefOf(t, out.String())
	if !strings.Contains(got, "revision 2") || strings.Contains(got, "revision 3") {
		t.Errorf("brief = %q, want revision 2 only", got)
	}
	checkReceipt(t, deliveries(t, f.mainStore), &store.Delivery{HerdrSession: ptr("default"), PaneID: "w14:p1",
		Result: store.ResultEmitted, WorkspaceKey: ptr("ws_a14"),
		ItemRevisions: []store.RevisionRef{{Item: 1, Revision: 2}, {Item: 2, Revision: 1}}}, got, gen)
}

// TestBudget: the lookup, the herdr pipe wait and the store busy wait fit inside the cap and
// leave time for the file reads and the print.
func TestBudget(t *testing.T) {
	const herdrWaitDelay = 50 * time.Millisecond // waitDelay of internal/herdr
	if left := hookCap - lookupDeadline - herdrWaitDelay - busyTimeout; left < 50*time.Millisecond {
		t.Errorf("cap left for the reads and the print = %v, want at least 50ms", left)
	}
}

// timingRuns is the number of hook runs that TestTiming times. [NEW]
const timingRuns = 200

// TestTiming times Main, the whole hook after the process start, against a store of the
// E3-S1 schema: the stdin read, the bindings read, one `pane get` through a fake herdr
// process, the workspace map, the manifest check, the print and the receipt (store open and
// insert). It logs p50 and p99; run it with -v to see them. The target is < 100 ms (§8.3).
func TestTiming(t *testing.T) {
	if testing.Short() {
		t.Skip("timing runs")
	}
	f := newFixture(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	file, err := paths.BindingsFile()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(f.file)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, data, 0o600); err != nil {
		t.Fatal(err)
	}
	// The fake herdr is a shell script, so each lookup starts a process as the real one does.
	bin := filepath.Join(home, "herdr")
	reply := `{"id":"cli:pane:get","result":{"pane":{"agent_status":"idle","cwd":"/tmp","focused":true,` +
		`"pane_id":"w14:p1","revision":0,"tab_id":"w14:t1","terminal_id":"term_1","workspace_id":"w14"},"type":"pane_info"}}`
	if err := os.WriteFile(bin, []byte("#!/bin/sh\necho '"+reply+"'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	env := append(environ(f.mainSock, "", "w14:p1", "w14"), "PATH=/usr/bin:/bin", "HERDR_BIN_PATH="+bin)
	args := []string{"--profile", "claude", "--event", "start"}
	// The first start of a new executable can be slow on macOS (a lookup past its deadline
	// withholds), so one untimed run comes first.
	if err := Main(args, env, strings.NewReader(input), io.Discard); err != nil {
		t.Fatalf("Main() = %v", err)
	}
	times := make([]time.Duration, timingRuns)
	emitted := 0
	for i := range times {
		var out bytes.Buffer
		start := time.Now()
		err := Main(args, env, strings.NewReader(input), &out)
		times[i] = time.Since(start)
		if err != nil {
			t.Fatalf("run %d: Main() = %v", i, err)
		}
		if strings.Contains(briefOf(t, out.String()), mainA14) {
			emitted++
		}
	}
	slices.Sort(times)
	// Nearest-rank percentiles, as in the E7-S4 FINDINGS.
	rank := func(p int) time.Duration { return times[(p*len(times)+99)/100-1] }
	t.Logf("whole hook, %d runs (%d emitted), %s/%s: p50 %v, p99 %v, max %v", timingRuns, emitted,
		runtime.GOOS, runtime.GOARCH, rank(50), rank(99), times[len(times)-1])
}
