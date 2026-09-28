package resolver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aryanVijaywargia/herdr-brain/internal/bindings"
	"github.com/aryanVijaywargia/herdr-brain/internal/herdr"
)

// record is one (pane, agent session id) record of the fake store.
type record struct {
	session, paneID, agentSession string
	check                         AgentCheck
}

// fakeData is what the fake stores of one test keep, per store folder, across opens.
type fakeData struct {
	opened   []string // store folders, in open order
	calls    []string // store methods, in call order
	withheld []Withheld
	records  map[string][]record // by store folder
}

func (d *fakeData) open(_ context.Context, dir string) (Store, error) {
	d.opened = append(d.opened, dir)
	return &fakeStore{d: d, dir: dir}, nil
}

type fakeStore struct {
	d   *fakeData
	dir string
}

func (s *fakeStore) WriteWithheld(_ context.Context, w Withheld) error {
	s.d.calls = append(s.d.calls, "WriteWithheld")
	s.d.withheld = append(s.d.withheld, w)
	return nil
}

func (s *fakeStore) RecordAgentSession(_ context.Context, session, paneID, agentSession string) error {
	s.d.calls = append(s.d.calls, "RecordAgentSession")
	s.d.records[s.dir] = append(s.d.records[s.dir], record{session, paneID, agentSession, AgentUnknown})
	return nil
}

func (s *fakeStore) UncheckedAgentSessions(_ context.Context, session, paneID string) ([]string, error) {
	s.d.calls = append(s.d.calls, "UncheckedAgentSessions")
	var ids []string
	for _, r := range s.d.records[s.dir] {
		if r.session == session && r.paneID == paneID && r.check == AgentUnknown {
			ids = append(ids, r.agentSession)
		}
	}
	return ids, nil
}

func (s *fakeStore) SetAgentCheck(_ context.Context, session, paneID, agentSession string, c AgentCheck) error {
	s.d.calls = append(s.d.calls, "SetAgentCheck")
	rs := s.d.records[s.dir]
	i := slices.IndexFunc(rs, func(r record) bool {
		return r.session == session && r.paneID == paneID && r.agentSession == agentSession
	})
	if i < 0 {
		return fmt.Errorf("no record for %s %s", paneID, agentSession)
	}
	rs[i].check = c
	return nil
}

func (s *fakeStore) Close() error {
	s.d.calls = append(s.d.calls, "Close")
	return nil
}

// answer is what the fake herdr says for one pane id.
type answer struct {
	pane herdr.Pane
	err  error
}

// fakeHerdr answers `pane get` per socket, and records each target it was asked.
type fakeHerdr struct {
	panes   map[string]map[string]answer // socket → pane id → answer
	targets []herdr.Target
}

func (h *fakeHerdr) pane(_ context.Context, t herdr.Target, id string) (herdr.Pane, error) {
	h.targets = append(h.targets, t)
	a, ok := h.panes[t.Socket][id]
	if !ok {
		return herdr.Pane{}, fmt.Errorf("pane %s not found: %w", id, herdr.ErrPaneNotFound)
	}
	return a.pane, a.err
}

// fixture is a bindings file with the main session, a second real session and one test
// session. The same pane id w1:p1 lives in the main and the test session.
type fixture struct {
	dir, file                              string
	mainSock, workSock, testSock, linkSock string // linkSock is mainSock through a link
	mainStore, testStore                   string
	herdr                                  *fakeHerdr
	data                                   *fakeData
	resolver                               *Resolver
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
		data:      &fakeData{records: map[string][]record{}},
	}
	for _, d := range []string{filepath.Dir(f.mainSock), f.mainStore, f.testStore} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(filepath.Dir(f.mainSock), link); err != nil {
		t.Fatal(err)
	}
	f.linkSock = filepath.Join(link, "herdr.sock")
	created := time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC)
	err := bindings.Write(f.file, []bindings.Binding{
		{Session: "default", Socket: f.mainSock, Store: f.mainStore, Kind: bindings.KindMain, Created: created},
		{Session: "work", Socket: f.workSock, Store: f.mainStore, Kind: bindings.KindMain, Created: created},
		{Session: "brain-eval", Socket: f.testSock, Store: f.testStore, Kind: bindings.KindTest, Created: created},
	})
	if err != nil {
		t.Fatal(err)
	}
	w1p1 := answer{pane: herdr.Pane{ID: "w1:p1", WorkspaceID: "w1", TabID: "w1:t1"}}
	f.herdr = &fakeHerdr{panes: map[string]map[string]answer{
		f.mainSock: {
			"w1:p1": w1p1,
			// w1:p2 moved to workspace w2; herdr answers its old id with the new one.
			"w1:p2": {pane: herdr.Pane{ID: "w2:p2", WorkspaceID: "w2", TabID: "w2:t1"}},
			"w1:p3": {err: fmt.Errorf("herdr pane get: %w", context.DeadlineExceeded)},
			"w1:p4": {err: &herdr.ShapeError{Field: "pane.tab_id", Reason: "missing"}},
		},
		f.workSock: {
			"w3:p1": {pane: herdr.Pane{ID: "w3:p1", WorkspaceID: "w3", TabID: "w3:t1"}},
			"w1:p1": w1p1,
		},
		f.testSock: {"w1:p1": w1p1},
	}}
	f.resolver = New(f.file, f.data.open, f.herdr.pane)
	return f
}

// environ is the environment of a pane of session (empty: the default session) with socket.
func environ(socket, session, paneID, workspaceID string) []string {
	e := []string{"HOME=/home/me", "HERDR_ENV=1", "HERDR_SOCKET_PATH=" + socket,
		"HERDR_PANE_ID=" + paneID, "HERDR_WORKSPACE_ID=" + workspaceID}
	if session != "" {
		e = append(e, "HERDR_SESSION="+session)
	}
	return e
}

func deadline(t *testing.T) context.Context {
	t.Helper()
	// [NEW] 5 s: no passing test comes near it; the fake herdr answers at once.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// tree returns every path below dir with its size, to show that a call wrote nothing.
func tree(t *testing.T, dir string) map[string]int64 {
	t.Helper()
	m := map[string]int64{}
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		m[p] = fi.Size()
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestNoStore(t *testing.T) {
	f := newFixture(t)
	tests := []struct {
		name      string
		environ   []string
		want      NoStoreReason
		wantHerdr bool // want one `pane get`
	}{
		{name: "no HERDR_ENV", environ: []string{"HERDR_SOCKET_PATH=" + f.mainSock, "HERDR_PANE_ID=w1:p1", "HERDR_WORKSPACE_ID=w1"}, want: NotInHerdr},
		{name: "HERDR_ENV=0", environ: append(environ(f.mainSock, "", "w1:p1", "w1"), "HERDR_ENV=0"), want: NotInHerdr},
		{name: "no socket", environ: environ("", "", "w1:p1", "w1"), want: NotInHerdr},
		{name: "relative socket", environ: environ("herdr.sock", "", "w1:p1", "w1"), want: NotInHerdr},
		// Astra, CODE_E2-S3_REVIEW finding 1: Clean would make it the main socket.
		{name: "socket with dot-dot", environ: environ(filepath.Join(f.dir, "link")+"/../herdr/herdr.sock", "", "w1:p1", "w1"), want: NotInHerdr},
		{name: "no pane id", environ: environ(f.mainSock, "", "", "w1"), want: NotInHerdr},
		{name: "no workspace id", environ: environ(f.mainSock, "", "w1:p1", ""), want: NotInHerdr},
		{name: "unbound socket (S64)", environ: environ(filepath.Join(f.dir, "other.sock"), "other", "w1:p1", "w1"), want: Unbound},
		{name: "unbound socket never falls back to default", environ: environ(filepath.Join(f.dir, "other.sock"), "", "w1:p1", "w1"), want: Unbound},
		{name: "test socket with HERDR_SESSION=default", environ: environ(f.testSock, "default", "w1:p1", "w1"), want: Mismatch},
		{name: "test socket with no HERDR_SESSION", environ: environ(f.testSock, "", "w1:p1", "w1"), want: Mismatch},
		{name: "main socket with HERDR_SESSION of the test", environ: environ(f.mainSock, "brain-eval", "w1:p1", "w1"), want: Mismatch},
		{name: "main socket with HERDR_SESSION of no binding", environ: environ(f.mainSock, "other", "w1:p1", "w1"), want: Mismatch},
		{name: "herdr timeout", environ: environ(f.mainSock, "", "w1:p3", "w1"), want: HerdrTimeout, wantHerdr: true},
		{name: "herdr reply of unknown shape", environ: environ(f.mainSock, "", "w1:p4", "w1"), want: HerdrFailed, wantHerdr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f.herdr.targets = nil
			before := tree(t, f.dir)
			res, err := f.resolver.Resolve(deadline(t), tt.environ, "agent-1")
			if err != nil {
				t.Fatalf("Resolve() err = %v", err)
			}
			ns, ok := res.(*NoStore)
			if !ok || ns.Reason != tt.want {
				t.Fatalf("Resolve() = %#v, want NoStore %q", res, tt.want)
			}
			if len(f.data.opened) != 0 || len(f.data.calls) != 0 {
				t.Errorf("stores opened %q, store calls %q, want none", f.data.opened, f.data.calls)
			}
			if got := len(f.herdr.targets); got != map[bool]int{false: 0, true: 1}[tt.wantHerdr] {
				t.Errorf("herdr asked %d times (%+v), want herdr asked: %v", got, f.herdr.targets, tt.wantHerdr)
			}
			if after := tree(t, f.dir); !reflect.DeepEqual(after, before) {
				t.Errorf("files changed: %v, want %v", after, before)
			}
		})
	}
}

func TestNoBindingsFile(t *testing.T) {
	dir := t.TempDir()
	data := &fakeData{records: map[string][]record{}}
	h := &fakeHerdr{}
	r := New(filepath.Join(dir, "bindings.json"), data.open, h.pane)
	res, err := r.Resolve(deadline(t), environ(filepath.Join(dir, "herdr.sock"), "", "w1:p1", "w1"), "")
	if ns, ok := res.(*NoStore); err != nil || !ok || ns.Reason != Unbound {
		t.Fatalf("Resolve() = %#v, %v, want NoStore %q", res, err, Unbound)
	}
	if len(data.opened) != 0 || len(h.targets) != 0 {
		t.Errorf("stores opened %q, herdr asked %+v, want none", data.opened, h.targets)
	}
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
		t.Errorf("files in %s: %v, %v, want none", dir, entries, err)
	}
}

func TestBrokenBindingsFile(t *testing.T) {
	f := newFixture(t)
	if err := os.WriteFile(f.file, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := f.resolver.Resolve(deadline(t), environ(f.mainSock, "", "w1:p1", "w1"), "")
	if err == nil || res != nil {
		t.Errorf("Resolve() = %#v, %v, want an error", res, err)
	}
	if len(f.data.opened) != 0 {
		t.Errorf("stores opened %q, want none", f.data.opened)
	}
}

func TestDenied(t *testing.T) {
	tests := []struct {
		name      string
		environ   func(f *fixture) []string
		want      DenyReason
		wantStore func(f *fixture) string
		wantPane  string
		wantWS    string
	}{
		{
			name:      "pane closed (S63)",
			environ:   func(f *fixture) []string { return environ(f.mainSock, "", "w1:p9", "w1") },
			want:      PaneGone,
			wantStore: func(f *fixture) string { return f.mainStore },
			wantPane:  "w1:p9", wantWS: "w1",
		},
		{
			name:      "stale HERDR_WORKSPACE_ID after a pane move (S49, S63)",
			environ:   func(f *fixture) []string { return environ(f.mainSock, "", "w1:p2", "w1") },
			want:      NotMember,
			wantStore: func(f *fixture) string { return f.mainStore },
			wantPane:  "w1:p2", wantWS: "w1",
		},
		{
			name:      "pane in another workspace",
			environ:   func(f *fixture) []string { return environ(f.mainSock, "", "w1:p1", "w2") },
			want:      NotMember,
			wantStore: func(f *fixture) string { return f.mainStore },
			wantPane:  "w1:p1", wantWS: "w2",
		},
		{
			name:      "test session pane closed",
			environ:   func(f *fixture) []string { return environ(f.testSock, "brain-eval", "w1:p9", "w1") },
			want:      PaneGone,
			wantStore: func(f *fixture) string { return f.testStore },
			wantPane:  "w1:p9", wantWS: "w1",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			res, err := f.resolver.Resolve(deadline(t), tt.environ(f), "agent-1")
			if err != nil {
				t.Fatalf("Resolve() err = %v", err)
			}
			d, ok := res.(*Denied)
			if !ok || d.Reason != tt.want || d.PaneID != tt.wantPane || d.WorkspaceID != tt.wantWS || d.Binding.Store != tt.wantStore(f) {
				t.Fatalf("Resolve() = %#v, want Denied %q for %s in %s with store %s", res, tt.want, tt.wantPane, tt.wantWS, tt.wantStore(f))
			}
			if len(f.data.opened) != 0 {
				t.Fatalf("Resolve() opened stores %q, want none before the receipt", f.data.opened)
			}

			if err := d.Withheld(deadline(t)); err != nil {
				t.Fatalf("Withheld() = %v", err)
			}
			want := Withheld{Session: d.Binding.Session, PaneID: tt.wantPane, WorkspaceID: tt.wantWS, AgentSession: "agent-1", Reason: tt.want}
			if !slices.Equal(f.data.opened, []string{tt.wantStore(f)}) || !slices.Equal(f.data.withheld, []Withheld{want}) {
				t.Errorf("Withheld() opened %q and wrote %+v, want %s and %+v", f.data.opened, f.data.withheld, tt.wantStore(f), want)
			}
			if want := []string{"WriteWithheld", "Close"}; !slices.Equal(f.data.calls, want) {
				t.Errorf("store calls = %q, want %q", f.data.calls, want)
			}
		})
	}
}

// TestDeniedGivesNoStore shows that the only store access of Denied is the receipt write: it
// has one method, and no exported field is or holds a Store or an Opener.
func TestDeniedGivesNoStore(t *testing.T) {
	typ := reflect.TypeOf(&Denied{})
	var methods []string
	for i := range typ.NumMethod() {
		methods = append(methods, typ.Method(i).Name)
	}
	if !slices.Equal(methods, []string{"Withheld"}) {
		t.Errorf("Denied methods = %q, want only Withheld", methods)
	}
	store := reflect.TypeOf((*Store)(nil)).Elem()
	opener := reflect.TypeOf(Opener(nil))
	for i := range typ.Elem().NumField() {
		fd := typ.Elem().Field(i)
		if fd.IsExported() && (fd.Type.Implements(store) || fd.Type == store || fd.Type == opener) {
			t.Errorf("Denied.%s gives store access", fd.Name)
		}
	}
}

func TestResolved(t *testing.T) {
	tests := []struct {
		name      string
		environ   func(f *fixture) []string
		session   string
		wantStore func(f *fixture) string
		wantPane  string
	}{
		{
			name:      "default session",
			environ:   func(f *fixture) []string { return environ(f.mainSock, "", "w1:p1", "w1") },
			session:   "default",
			wantStore: func(f *fixture) string { return f.mainStore },
			wantPane:  "w1:p1",
		},
		{
			name:      "HERDR_SESSION=default",
			environ:   func(f *fixture) []string { return environ(f.mainSock, "default", "w1:p1", "w1") },
			session:   "default",
			wantStore: func(f *fixture) string { return f.mainStore },
			wantPane:  "w1:p1",
		},
		{
			name:      "second real session shares the main store",
			environ:   func(f *fixture) []string { return environ(f.workSock, "work", "w3:p1", "w3") },
			session:   "work",
			wantStore: func(f *fixture) string { return f.mainStore },
			wantPane:  "w3:p1",
		},
		{
			name:      "test session with the same pane id",
			environ:   func(f *fixture) []string { return environ(f.testSock, "brain-eval", "w1:p1", "w1") },
			session:   "brain-eval",
			wantStore: func(f *fixture) string { return f.testStore },
			wantPane:  "w1:p1",
		},
		{
			name:      "socket path through a link",
			environ:   func(f *fixture) []string { return environ(f.linkSock, "", "w1:p1", "w1") },
			session:   "default",
			wantStore: func(f *fixture) string { return f.mainStore },
			wantPane:  "w1:p1",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			res, err := f.resolver.Resolve(deadline(t), tt.environ(f), "")
			if err != nil {
				t.Fatalf("Resolve() err = %v", err)
			}
			r, ok := res.(*Resolved)
			if !ok || r.Binding.Session != tt.session || r.Binding.Store != tt.wantStore(f) || r.PaneID != tt.wantPane || r.Store == nil {
				t.Fatalf("Resolve() = %#v, want Resolved in session %q with store %s", res, tt.session, tt.wantStore(f))
			}
			// The one `pane get` goes to the socket of the selected binding.
			want := []herdr.Target{{Session: r.Binding.Session, Socket: r.Binding.Socket}}
			if !slices.Equal(f.herdr.targets, want) {
				t.Errorf("herdr targets = %+v, want %+v", f.herdr.targets, want)
			}
			if !slices.Equal(f.data.opened, []string{tt.wantStore(f)}) {
				t.Errorf("stores opened = %q, want %s", f.data.opened, tt.wantStore(f))
			}
		})
	}
}

func TestAgentCheck(t *testing.T) {
	const parent, nested = "6a345ec1-3050-4e61-b736-d0e9d2edab82", "b7c1d2e3-0000-4000-8000-000000000001"
	f := newFixture(t)
	env := environ(f.mainSock, "", "w1:p1", "w1")
	resolve := func(agentSession string) {
		t.Helper()
		res, err := f.resolver.Resolve(deadline(t), env, agentSession)
		r, ok := res.(*Resolved)
		if err != nil || !ok {
			t.Fatalf("Resolve() = %#v, %v, want Resolved", res, err)
		}
		if err := r.Store.Close(); err != nil {
			t.Fatal(err)
		}
	}
	setAgent := func(value string) {
		a := f.herdr.panes[f.mainSock]["w1:p1"]
		a.pane.AgentSession = nil
		if value != "" {
			a.pane.AgentSession = &herdr.AgentSession{Agent: "claude", Source: "herdr:claude", Kind: "id", Value: value}
		}
		f.herdr.panes[f.mainSock]["w1:p1"] = a
	}

	// At hook time herdr shows no agent session yet: the record stays unknown.
	resolve(parent)
	resolve("")
	// Later herdr shows the parent's agent session. A nested `claude -p` in that pane sees it
	// too at its hook time, so the hook only records its own id.
	setAgent(parent)
	resolve(nested)
	resolve("")

	want := []record{
		{"default", "w1:p1", parent, AgentMatch},
		{"default", "w1:p1", nested, NotThisPanesAgent},
	}
	if got := f.data.records[f.mainStore]; !slices.Equal(got, want) {
		t.Errorf("records = %+v, want %+v", got, want)
	}
}

func TestAgentCheckUnknownWithoutAgent(t *testing.T) {
	f := newFixture(t)
	env := environ(f.mainSock, "", "w1:p1", "w1")
	for _, id := range []string{"agent-1", ""} {
		res, err := f.resolver.Resolve(deadline(t), env, id)
		if _, ok := res.(*Resolved); err != nil || !ok {
			t.Fatalf("Resolve() = %#v, %v, want Resolved", res, err)
		}
	}
	want := []record{{"default", "w1:p1", "agent-1", AgentUnknown}}
	if got := f.data.records[f.mainStore]; !slices.Equal(got, want) {
		t.Errorf("records = %+v, want %+v", got, want)
	}
	if slices.Contains(f.data.calls, "SetAgentCheck") {
		t.Errorf("store calls = %q, want no SetAgentCheck while herdr shows no agent", f.data.calls)
	}
}

// TestHerdrPane shows that HerdrPane sends `pane get` to the socket of the target, with no
// --session flag, whatever session the parent environment names.
func TestHerdrPane(t *testing.T) {
	dir := t.TempDir()
	reply, err := os.ReadFile(filepath.Join("..", "herdr", "testdata", "pane-agent.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "reply.json"), reply, 0o600); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "herdr")
	script := "#!/bin/sh\nprintf '%s|%s|%s\\n' \"$HERDR_SOCKET_PATH\" \"$HERDR_SESSION\" \"$*\" > \"$0.call\"\ncat \"$(dirname \"$0\")/reply.json\"\n"
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	parent := []string{"PATH=/usr/bin:/bin", "HERDR_BIN_PATH=" + bin, "HERDR_SESSION=main", "HERDR_SOCKET_PATH=/main.sock"}
	target := herdr.Target{Session: "brain-eval", Socket: "/tmp/eval/herdr.sock"}
	p, err := HerdrPane(parent)(deadline(t), target, "w14:p2F")
	if err != nil || p.ID != "w14:p2F" || p.AgentSession == nil {
		t.Fatalf("HerdrPane() = %+v, %v, want pane w14:p2F with its agent session", p, err)
	}
	call, err := os.ReadFile(bin + ".call")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.TrimSpace(string(call)), "/tmp/eval/herdr.sock|brain-eval|pane get w14:p2F"; got != want {
		t.Errorf("herdr call = %q, want %q", got, want)
	}
}

func TestResolveStoreOpenError(t *testing.T) {
	f := newFixture(t)
	boom := errors.New("store busy")
	r := New(f.file, func(context.Context, string) (Store, error) { return nil, boom }, f.herdr.pane)
	res, err := r.Resolve(deadline(t), environ(f.mainSock, "", "w1:p1", "w1"), "")
	if !errors.Is(err, boom) || res != nil {
		t.Errorf("Resolve() = %#v, %v, want %v", res, err, boom)
	}
}

// A test binding whose store is a dangling link is dropped: its session gets no store, and
// the default session still resolves (owner decision, 2026-09-28).
func TestDroppedBinding(t *testing.T) {
	f := newFixture(t)
	bs, dropped, err := bindings.Read(f.file)
	if err != nil || len(dropped) != 0 {
		t.Fatalf("Read = %v, %v", dropped, err)
	}
	dangling := filepath.Join(f.dir, "dangling")
	if err := os.Symlink(filepath.Join(f.dir, "gone"), dangling); err != nil {
		t.Fatal(err)
	}
	bs[2].Store = dangling // brain-eval
	data, err := json.Marshal(map[string][]bindings.Binding{"bindings": bs})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.file, data, 0o600); err != nil {
		t.Fatal(err)
	}

	for _, e := range [][]string{
		environ(f.testSock, "brain-eval", "w1:p1", "w1"),
		environ(filepath.Join(f.dir, "gone.sock"), "brain-eval", "w1:p1", "w1"), // matched by session name
	} {
		res, err := f.resolver.Resolve(deadline(t), e, "agent-1")
		ns, ok := res.(*NoStore)
		if err != nil || !ok || ns.Reason != BindingDropped || ns.Err == nil || !strings.Contains(ns.Err.Error(), "link to a missing target") {
			t.Fatalf("Resolve(%q) = %#v, %v, want NoStore %q with the reason", e, res, err, BindingDropped)
		}
	}
	if len(f.data.opened) != 0 || len(f.herdr.targets) != 0 {
		t.Errorf("stores opened %q, herdr asked %+v, want none", f.data.opened, f.herdr.targets)
	}

	res, err := f.resolver.Resolve(deadline(t), environ(f.mainSock, "", "w1:p1", "w1"), "")
	r, ok := res.(*Resolved)
	if err != nil || !ok || r.Binding.Session != "default" {
		t.Fatalf("Resolve(default) = %#v, %v, want Resolved for default", res, err)
	}
	if err := r.Store.Close(); err != nil {
		t.Fatal(err)
	}
}

// Astra, CODE_E2-S3_REVIEW finding 2: two real sessions share the main store and both have
// pane w1:p1. A check in one session must not touch the records of the other.
func TestAgentCheckStaysInItsSession(t *testing.T) {
	f := newFixture(t)
	resolve := func(socket, session, agentSession string) {
		t.Helper()
		res, err := f.resolver.Resolve(deadline(t), environ(socket, session, "w1:p1", "w1"), agentSession)
		r, ok := res.(*Resolved)
		if err != nil || !ok {
			t.Fatalf("Resolve(%s) = %#v, %v, want Resolved", session, res, err)
		}
		if err := r.Store.Close(); err != nil {
			t.Fatal(err)
		}
	}
	resolve(f.mainSock, "", "default-agent")
	a := f.herdr.panes[f.workSock]["w1:p1"]
	a.pane.AgentSession = &herdr.AgentSession{Agent: "claude", Source: "herdr:claude", Kind: "id", Value: "work-agent"}
	f.herdr.panes[f.workSock]["w1:p1"] = a
	resolve(f.workSock, "work", "")

	want := []record{{"default", "w1:p1", "default-agent", AgentUnknown}}
	if got := f.data.records[f.mainStore]; !slices.Equal(got, want) {
		t.Errorf("records = %+v, want %+v", got, want)
	}
}
