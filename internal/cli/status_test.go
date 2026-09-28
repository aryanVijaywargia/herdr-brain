package cli

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aryanVijaywargia/herdr-brain/internal/bindings"
	"github.com/aryanVijaywargia/herdr-brain/internal/harness"
	"github.com/aryanVijaywargia/herdr-brain/internal/herdr"
	"github.com/aryanVijaywargia/herdr-brain/internal/resolver"
	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

var update = flag.Bool("update", false, "rewrite the golden files of brain status")

// fakeStatusHerdr is the herdr of brain status: one pane list and the server version.
type fakeStatusHerdr struct {
	panes    []herdr.Pane
	panesErr error
	version  string
	untested bool
	lists    int // the pane list calls
}

func (h *fakeStatusHerdr) Panes(context.Context) ([]herdr.Pane, error) {
	h.lists++
	return h.panes, h.panesErr
}

func (h *fakeStatusHerdr) CheckServer(context.Context) (string, bool, error) {
	return h.version, h.untested, nil
}

// statusFixture is a fixture with a data home, fake agent CLIs on the PATH and a fake herdr.
type statusFixture struct {
	*fixture
	data  string
	herdr *fakeStatusHerdr
}

func newStatusFixture(t *testing.T) *statusFixture {
	t.Helper()
	f := newFixture(t)
	data := filepath.Join(filepath.Dir(f.store), "data")
	bin := filepath.Join(filepath.Dir(f.store), "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	// claude is at its tested version; codex is not, so it needs a retest.
	for kind, v := range map[string]string{"claude": "2.1.283 (Claude Code)", "codex": "codex-cli 0.155.0"} {
		script := "#!/bin/sh\necho '" + v + "'\n"
		if err := os.WriteFile(filepath.Join(bin, kind), []byte(script), 0o700); err != nil { //nolint:gosec // a test CLI must be executable
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin)
	return &statusFixture{fixture: f, data: data, herdr: &fakeStatusHerdr{version: "0.9.1"}}
}

// status runs brain status from pane w14:p1 at testNow + 3 h.
func (f *statusFixture) status(t *testing.T, socket string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	e := f.env(socket, "w14:p1", "w14", &out)
	e.Data = f.data
	e.Adapters = harness.Files{Install: filepath.Join(f.data, "install.json"),
		Conformance: filepath.Join(f.data, "conformance.json"), Retest: filepath.Join(f.data, retestDir)}
	e.StatusHerdr = func(herdr.Target) (StatusHerdr, error) { return f.herdr, nil }
	e.Now = func() time.Time { return testNow.Add(3 * time.Hour) }
	err := Status(context.Background(), e, nil)
	return out.String(), err
}

func (f *statusFixture) mustStatus(t *testing.T) string {
	t.Helper()
	out, err := f.status(t, f.sock)
	if err != nil {
		t.Fatalf("Status() = %v", err)
	}
	return out
}

// golden compares out with testdata/status/<name>.txt, with the temp folder as <tmp>.
func (f *statusFixture) golden(t *testing.T, name, out string) {
	t.Helper()
	dir := filepath.Dir(f.store)
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	out = strings.ReplaceAll(strings.ReplaceAll(out, real, "<tmp>"), dir, "<tmp>")
	path := filepath.Join("testdata", "status", name+".txt")
	if *update {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(out), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if out != string(want) {
		t.Errorf("brain status:\n%s\nwant (%s):\n%s", out, path, want)
	}
}

// agent is a live pane of kind with agent session id; "" is no id.
func agent(id, kind, session string) herdr.Pane {
	ws, _, _ := strings.Cut(id, ":")
	p := herdr.Pane{ID: id, WorkspaceID: ws, TabID: ws + ":t1", Agent: ptr(kind)}
	if session != "" {
		p.AgentSession = &herdr.AgentSession{Agent: kind, Source: "herdr:" + kind, Kind: "id", Value: session}
	}
	return p
}

// receipt writes a receipt in the store at dir.
func receipt(t *testing.T, dir, session, profile, pane, agentSession string, r store.Result, reason string) {
	t.Helper()
	d := store.Delivery{Profile: profile, HerdrSession: ptr(session), PaneID: pane, Result: r, At: testNow}
	if agentSession != "" {
		d.AgentSessionID = ptr(agentSession)
	}
	if reason != "" {
		d.WithheldReason = ptr(reason)
	}
	s := openStore(t, dir)
	defer closeStore(t, s)
	write(t, s, func(tx *sql.Tx) error {
		_, err := store.InsertDelivery(context.Background(), tx, d)
		return err
	})
}

// AC: the screen of a new setup: no live agent, no receipt, an empty queue.
func TestStatusNewSetup(t *testing.T) {
	f := newStatusFixture(t)
	f.golden(t, "new", f.mustStatus(t))
}

// AC: one gap of each kind, the three "unknown" cases, the withheld history apart, an agent
// kind with no profile (D3), and the review queue. Receipts of a test store and of another
// herdr session never count for the main binding.
func TestStatusGaps(t *testing.T) {
	f := newStatusFixture(t)
	f.remember(t, "use pnpm")
	main := "default"
	receipt(t, f.store, main, "claude", "w14:p1", "s-emitted", store.ResultEmitted, "")
	receipt(t, f.store, main, "claude", "w14:p2", "s-unmapped", store.ResultUnmapped, "")
	receipt(t, f.store, main, "claude", "w14:p3", "s-withheld", store.ResultWithheld, "not a member")
	// An older agent in pane w14:p5 has a receipt; the new agent there has none.
	receipt(t, f.store, main, "claude", "w14:p5", "s-old", store.ResultEmitted, "")
	// A withheld call from a pane with no agent session id shows in the history only.
	receipt(t, f.store, main, "claude", "w16:p9", "", store.ResultWithheld, "pane not live")
	receipt(t, f.store, main, "codex", "w15:p1", "s-codex", store.ResultEmitted, "")
	// The same agent session id in another herdr session of the main store, and in a test store.
	receipt(t, f.store, "other", "claude", "w14:p4", "s-none", store.ResultEmitted, "")
	test := filepath.Join(filepath.Dir(f.store), "test-store")
	if err := os.MkdirAll(test, 0o700); err != nil {
		t.Fatal(err)
	}
	receipt(t, test, "t1", "claude", "w14:p4", "s-none", store.ResultEmitted, "")
	bs, _, err := bindings.Read(f.file)
	if err != nil {
		t.Fatal(err)
	}
	tsock := filepath.Join(filepath.Dir(f.store), "t1", "herdr.sock")
	if err := os.MkdirAll(filepath.Dir(tsock), 0o700); err != nil {
		t.Fatal(err)
	}
	bs = append(bs, bindings.Binding{Session: "t1", Socket: tsock, Store: test, Kind: bindings.KindTest, Created: testNow})
	if err := bindings.Write(f.file, bs); err != nil {
		t.Fatal(err)
	}

	f.herdr.panes = []herdr.Pane{
		agent("w14:p1", "claude", "s-emitted"),
		agent("w14:p2", "claude", "s-unmapped"),
		agent("w14:p3", "claude", "s-withheld"),
		agent("w14:p4", "claude", "s-none"),    // unknown: no receipt
		agent("w14:p5", "claude", "s-new"),     // unknown: a new agent in a pane with an old receipt
		agent("w14:p6", "claude", ""),          // unknown: no agent session id
		agent("w15:p1", "codex", "s-codex"),    // emitted
		agent("w15:p2", "opencode", "s-other"), // no profile
		{ID: "w16:p1", WorkspaceID: "w16", TabID: "w16:t1"},
	}
	out := f.mustStatus(t)
	f.golden(t, "gaps", out)
	if f.herdr.lists != 1 {
		t.Errorf("pane lists = %d, want 1", f.herdr.lists)
	}
	if strings.Contains(out, "use pnpm") {
		t.Errorf("status shows item text:\n%s", out)
	}
}

// AC: when the pane list fails, the coverage is unavailable, never zero gaps.
func TestStatusPaneListFails(t *testing.T) {
	f := newStatusFixture(t)
	f.herdr.panesErr = errors.New("herdr pane list: timeout")
	out := f.mustStatus(t)
	if !strings.Contains(out, "coverage unavailable: herdr pane list: timeout") {
		t.Errorf("status:\n%s\nwant coverage unavailable", out)
	}
	if strings.Contains(out, "no live agent") {
		t.Errorf("status:\n%s\nwant no counts", out)
	}
}

// AC S54: an untested herdr version shows the warning.
func TestStatusUntestedHerdr(t *testing.T) {
	f := newStatusFixture(t)
	f.herdr.version, f.herdr.untested = "0.9.3", true
	if out := f.mustStatus(t); !strings.Contains(out, "herdr 0.9.3: not tested with Brain yet; Brain keeps working\n") {
		t.Errorf("status:\n%s\nwant the S54 warning", out)
	}
}

// AC S66: a hand edit of the current generation shows as an output conflict. The repair of
// the call does not build past it, so the brief line shows why the hook withholds it.
func TestStatusConflict(t *testing.T) {
	f := newStatusFixture(t)
	f.handEdit(t, f.rows(t).newest)
	f.golden(t, "conflict", f.mustStatus(t))
}

// AC S77: a forget that a hand edit blocked shows as pending, with no item text.
func TestStatusForgetPending(t *testing.T) {
	f := newStatusFixture(t)
	id := f.approved(t, "marker-s77")
	f.handEdit(t, f.rows(t).newest)
	f.mustForget(t, clock(), fmt.Sprint(id))
	out := f.mustStatus(t)
	f.golden(t, "forget", out)
	if strings.Contains(out, "marker-s77") {
		t.Errorf("status shows item text:\n%s", out)
	}
}

// AC S83: an entry that a remap replaced, and a closed entry with the root of a current
// entry, wait for a relink. The caller's workspace shows the remap offer.
func TestStatusRelink(t *testing.T) {
	f := newStatusFixture(t)
	dir := filepath.Dir(f.store)
	proj, err := filepath.EvalSymlinks(filepath.Join(dir, "proj"))
	if err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(dir, "other")
	if err := os.MkdirAll(other, 0o700); err != nil {
		t.Fatal(err)
	}
	// herdr saved w14 at another folder than its project: the map offers a remap.
	writeJSON(t, filepath.Join(filepath.Dir(f.sock), "session.json"), map[string]any{"version": 3,
		"workspaces": []map[string]string{{"id": "w14", "identity_cwd": other}, {"id": "w15", "identity_cwd": proj}}})
	entry := func(key, id string) map[string]any {
		return map[string]any{"key": key, "session": "default", "workspace_id": id, "label": id, "project": proj}
	}
	old, closed := entry("ws_a13", "w13"), entry("ws_a12", "w12")
	old["replaced_by"], closed["closed"] = "ws_a15", true
	writeJSON(t, filepath.Join(f.store, "workspaces.json"), map[string]any{"version": 1,
		"entries": []map[string]any{entry("ws_a14", "w14"), entry("ws_a15", "w15"), old, closed}})
	f.golden(t, "relink", f.mustStatus(t))
}

// AC S84: an unbound session gets the explanation and the setup command. Status reads no
// store and writes nothing.
func TestStatusUnbound(t *testing.T) {
	f := newStatusFixture(t)
	dir := filepath.Dir(f.store)
	before := files(t, dir)
	sock := filepath.Join(dir, "unbound", "herdr.sock")
	var out bytes.Buffer
	e := f.env(sock, "w14:p1", "w14", &out)
	e.Environ = append(e.Environ, "HERDR_SESSION=scratch")
	e.Data = f.data
	e.StatusHerdr = func(herdr.Target) (StatusHerdr, error) { return f.herdr, nil }
	if err := Status(context.Background(), e, nil); err != nil {
		t.Fatalf("Status() = %v", err)
	}
	f.golden(t, "unbound", out.String())
	diff(t, files(t, dir), before)
	if f.syncs != 0 || f.herdr.lists != 0 {
		t.Errorf("syncs %d, pane lists %d, want none", f.syncs, f.herdr.lists)
	}
}

func diff(t *testing.T, got, want map[string]string) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("files: got %d, want %d", len(got), len(want))
	}
	for p, w := range want {
		if got[p] != w {
			t.Errorf("%s changed", p)
		}
	}
}

// AC: status takes no argument.
func TestStatusArgs(t *testing.T) {
	f := newStatusFixture(t)
	var out bytes.Buffer
	if err := Status(context.Background(), f.env(f.sock, "w14:p1", "w14", &out), []string{"x"}); err == nil {
		t.Error("Status(x) = nil, want the usage")
	}
}

// AC: status runs the deferred session-id check like any store command: a pending agent
// record of the calling pane gets its check.
func TestStatusChecksAgents(t *testing.T) {
	f := newStatusFixture(t)
	s := openStore(t, f.store)
	write(t, s, func(tx *sql.Tx) error {
		_, err := store.InsertDelivery(context.Background(), tx, store.Delivery{Profile: "claude",
			HerdrSession: ptr("default"), PaneID: "w14:p1", AgentSessionID: ptr("s-1"),
			Result: store.ResultEmitted, At: testNow, AgentCheck: ptr(string(resolver.AgentPending))})
		return err
	})
	closeStore(t, s)
	f.mustStatus(t)
	s = openStore(t, f.store)
	defer closeStore(t, s)
	ids, err := (commandStore{s}).UncheckedAgentSessions(context.Background(), "default", "w14:p1")
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 0 {
		t.Errorf("unchecked agent sessions %v after status, want none", ids)
	}
}
