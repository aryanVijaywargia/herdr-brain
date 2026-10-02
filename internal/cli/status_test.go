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
	"github.com/aryanVijaywargia/herdr-brain/internal/paths"
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
	checkErr error // the CheckServer error, such as a herdr that is too old
	lists    int   // the pane list calls
}

func (h *fakeStatusHerdr) Panes(context.Context) ([]herdr.Pane, error) {
	h.lists++
	return h.panes, h.panesErr
}

func (h *fakeStatusHerdr) CheckServer(context.Context) (string, bool, error) {
	return h.version, h.untested, h.checkErr
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
	canon, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	out = strings.ReplaceAll(strings.ReplaceAll(out, canon, "<tmp>"), dir, "<tmp>")
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

// AC #136: a missing part of the current generation joins ErrInconsistent and fs.ErrNotExist.
// Status shows the damage, not "not built yet".
func TestStatusDamagedBrief(t *testing.T) {
	f := newStatusFixture(t)
	if err := os.Remove(filepath.Join(f.genDir(f.rows(t).newest), "global.md")); err != nil {
		t.Fatal(err)
	}
	out := f.mustStatus(t)
	if strings.Contains(out, "not built yet") || !strings.Contains(out, "brief: ") ||
		!strings.Contains(out, "output files do not match their manifest") {
		t.Errorf("status:\n%s\nwant the inconsistent brief", out)
	}
}

// AC #136: when the pane list fails, a CLI with no hook shows "unknown", not "none": status
// cannot tell whether it is detected.
func TestStatusPaneListFailsCLIUnknown(t *testing.T) {
	f := newStatusFixture(t)
	f.herdr.panesErr = errors.New("herdr pane list: timeout")
	if out := f.mustStatus(t); !strings.Contains(out, "  claude 2.1.283: unknown\n") {
		t.Errorf("status:\n%s\nwant claude unknown", out)
	}
}

// E10-S2: brain status shows the last eval run of the brain-eval test store as one line.
func TestStatusEvalLine(t *testing.T) {
	dir := t.TempDir()
	file, testStore := filepath.Join(dir, "bindings.json"), filepath.Join(dir, "test")
	if err := os.MkdirAll(testStore, 0o700); err != nil {
		t.Fatal(err)
	}
	now := testNow.Add(3 * time.Hour)
	if got := evalLine(file, now); got != "evals: no run yet" {
		t.Errorf("no bindings file: %q", got)
	}
	if err := bindings.Write(file, []bindings.Binding{{Session: evalSession, Socket: filepath.Join(dir, "herdr.sock"),
		Store: testStore, Kind: bindings.KindTest, Created: testNow}}); err != nil {
		t.Fatal(err)
	}
	if got := evalLine(file, now); got != "evals: no run yet" {
		t.Errorf("no results file: %q", got)
	}
	run := store.EvalRun{Started: testNow, Finished: testNow.Add(time.Hour), Results: []store.EvalResult{
		{Case: "a", Label: "PASS", ControlPass: true}, {Case: "a", Control: true, Label: "PASS"},
		{Case: "b", Label: "UNSURE", Status: "reply incomplete"}, {Case: "c", Status: "skipped"},
		{Case: "d", Status: "not delivered"},
	}}
	if err := store.WriteEvalRun(paths.EvalResults(testStore), run); err != nil {
		t.Fatal(err)
	}
	want := "evals: last run 2h ago, 5 results: 2 PASS, 1 UNSURE, 1 no label, 1 skipped, 1 control_pass"
	if got := evalLine(file, now); got != want {
		t.Errorf("evalLine() = %q, want %q", got, want)
	}
}

// AC S70: an invalid extractor response keeps the watermark, and status shows the batch.
func TestStatusFailedBatch(t *testing.T) {
	f := newStatusFixture(t)
	s := openStore(t, f.store)
	write(t, s, func(tx *sql.Tx) error {
		ctx := context.Background()
		key := store.SessionKey{HerdrSession: "default", Harness: "claude", Kind: store.KeyAgent, ID: "a1"}
		sid, err := store.OpenSession(ctx, tx, key, "w14:p1", "ws_a14", testNow)
		if err != nil {
			return err
		}
		b, err := store.StartBatch(ctx, tx, store.Batch{SessionID: sid, WorkspaceKey: "ws_a14", FirstCapture: "c1",
			LastCapture: "c2", LastObservation: 2}, testNow)
		if err != nil {
			return err
		}
		_, err = store.EndBatch(ctx, tx, b.ID, store.BatchInvalid, "claude gave no structured output", testNow)
		return err
	})
	closeStore(t, s)
	out := f.mustStatus(t)
	want := "  failed batches: 1; their evidence waits for the next pass\n" +
		"    batch 1, session 1, workspace ws_a14: invalid, claude gave no structured output, 3h ago\n"
	if !strings.Contains(out, want) {
		t.Errorf("brain status:\n%s\nwant it to hold:\n%s", out, want)
	}
}

// capture opens the session with key k in pane paneID of workspace key ws at at, and adds an
// observation of each status in sts, one minute apart. It returns the session and observation ids.
func capture(ctx context.Context, tx *sql.Tx, k store.SessionKey, paneID, ws string, at time.Time,
	sts ...store.ObservationStatus) (session int64, obs []int64, err error) {
	session, err = store.OpenSession(ctx, tx, k, paneID, ws, at)
	if err != nil {
		return 0, nil, err
	}
	seg, err := store.StartSegment(ctx, tx, session, paneID, ws, "recent_unwrapped", at)
	if err != nil {
		return 0, nil, err
	}
	for i, st := range sts {
		o := at.Add(time.Duration(i) * time.Minute)
		id, err := store.AddObservation(ctx, tx, store.Observation{CaptureIdentity: fmt.Sprintf("%s/%d", k.ID, i),
			Source: store.SourceHerdr, SegmentID: seg, Event: "idle", Excerpt: "no, use pnpm", Status: st,
			Sender: store.SenderUnconfirmed, Counts: st != store.ObservationGap, At: o})
		if err != nil {
			return 0, nil, err
		}
		obs = append(obs, id)
	}
	return session, obs, nil
}

// AC E15-S2: the M2 counts of §12 from a fixture store, with the sessions observed and the
// capture gaps beside each count: comebacks by item revision with example observations and the
// uncertain matches apart, correction candidates, proposals and their review, live suggestions,
// the delivery gaps of observed sessions by adapter, and a backlog that waits for extraction.
// The capture off and extraction off switches stay on their own lines (E13-S4).
func TestStatusCounts(t *testing.T) {
	f := newStatusFixture(t)
	f.signalSession(t, "ws_a14", "a1") // claude, week of 2026-09-28: no receipt, waits for extraction
	f.setCLI(t, store.ExtractorClaude)
	approved, edited, rejected, changed := f.remember(t, "use pnpm"), f.remember(t, "run make lint"),
		f.remember(t, "use tabs"), f.remember(t, "keep a changelog")
	later := f.remember(t, "squash merge")
	f.mustReview(t, fmt.Sprint(approved), "--approve")
	f.mustReview(t, fmt.Sprint(edited), "--approve", "--edit", "run make lint before a push")
	f.mustReview(t, fmt.Sprint(rejected), "--reject")
	item := *f.rows(t).proposal(t, approved).ItemID
	lastWeek := testNow.AddDate(0, 0, -7)
	f.storeWrite(t, func(ctx context.Context, tx *sql.Tx) error {
		claude := func(kind store.KeyKind, id string) store.SessionKey {
			return store.SessionKey{HerdrSession: "default", Harness: "claude", Kind: kind, ID: id}
		}
		// a2 (last week): a withheld receipt, four comebacks on revision 1, a delivery gap outcome
		// and a capture gap.
		ok := store.ObservationComplete
		a2, comebacks, err := capture(ctx, tx, claude(store.KeyAgent, "a2"), "w14:p2", "ws_a14", lastWeek,
			ok, ok, ok, ok, ok, store.ObservationGap)
		if err != nil {
			return err
		}
		withheld := store.Delivery{Profile: "claude", HerdrSession: ptr("default"), PaneID: "w14:p2",
			AgentSessionID: ptr("a2"), Result: store.ResultWithheld, WithheldReason: ptr("not a member"), At: lastWeek}
		if _, err := store.InsertDelivery(ctx, tx, withheld); err != nil {
			return err
		}
		// c1 (this week): herdr gave no agent session id; an uncertain match and an unknown delivery.
		c1, obs, err := capture(ctx, tx, claude(store.KeyCapture, "c1"), "w14:p3", "ws_a14", testNow.Add(time.Hour), ok, ok)
		if err != nil {
			return err
		}
		uncertain, unknown := obs[0], obs[1]
		// c2 (this week): no agent session id, but its pane receipt ties after the previous session
		// of the pane (TiedReceipt), and it is withheld.
		prev := claude(store.KeyAgent, "p0")
		if _, err := store.OpenSession(ctx, tx, prev, "w14:p4", "ws_a14", testNow.Add(-2*time.Hour)); err != nil {
			return err
		}
		if err := store.EndSession(ctx, tx, prev, testNow.Add(-90*time.Minute)); err != nil {
			return err
		}
		if _, _, err := capture(ctx, tx, claude(store.KeyCapture, "c2"), "w14:p4", "ws_a14", testNow, ok); err != nil {
			return err
		}
		if _, err := store.InsertDelivery(ctx, tx, store.Delivery{Profile: "claude", HerdrSession: ptr("default"),
			PaneID: "w14:p4", AgentSessionID: ptr("z9"), Result: store.ResultWithheld, WithheldReason: ptr("pane not live"),
			At: testNow.Add(-time.Hour)}); err != nil {
			return err
		}
		// x1 (two weeks ago): a codex session with an emitted receipt.
		codex := store.SessionKey{HerdrSession: "default", Harness: "codex", Kind: store.KeyAgent, ID: "x1"}
		if _, _, err := capture(ctx, tx, codex, "w15:p1", "ws_a15", testNow.AddDate(0, 0, -14), ok); err != nil {
			return err
		}
		if _, err := store.InsertDelivery(ctx, tx, store.Delivery{Profile: "codex", HerdrSession: ptr("default"),
			PaneID: "w15:p1", AgentSessionID: ptr("x1"), Result: store.ResultEmitted, At: testNow.AddDate(0, 0, -14)}); err != nil {
			return err
		}
		outcomes := []store.Outcome{{ItemID: item, SessionID: c1, Kind: store.OutcomeUncertain, Revision: 1, ObservationID: uncertain},
			{ItemID: item, SessionID: c1, Kind: store.OutcomeUnknown, ObservationID: unknown}}
		for _, obs := range comebacks[:4] {
			outcomes = append(outcomes, store.Outcome{ItemID: item, Revision: 1, SessionID: a2, Kind: store.OutcomeComeback,
				ObservationID: obs})
		}
		outcomes = append(outcomes, store.Outcome{ItemID: item, Revision: 1, SessionID: a2, Kind: store.OutcomeGap,
			ObservationID: comebacks[4]})
		for _, o := range outcomes {
			o.At = testNow
			if _, err := store.AddOutcome(ctx, tx, o); err != nil {
				return err
			}
		}
		// One correction candidate with evidence in both weeks, one with evidence last week only.
		supports := func(store.CandidateKind, store.Sender) bool { return true }
		for i, obs := range []int64{comebacks[0], uncertain, comebacks[1]} {
			claim, err := store.ClaimID(ctx, tx, store.ClaimKey{Situation: "install", Rule: fmt.Sprint("rule ", i/2)}, testNow)
			if err != nil {
				return err
			}
			c := store.Candidate{ClaimID: claim, Scope: store.ScopeWorkspace, WorkspaceKey: "ws_a14", Kind: store.CandidateCorrection}
			if _, err := store.Attach(ctx, tx, c, obs, store.DefaultGates(), supports); err != nil {
				return err
			}
		}
		// The approved proposal came from a suggestion shown 10 minutes before testNow; the later
		// one is still pending. The rejected one has a stored review time.
		for i, s := range []struct {
			proposal int64
			result   store.SuggestionResult
		}{{approved, store.SuggestionApproved}, {later, store.SuggestionLater}} {
			if _, err := tx.ExecContext(ctx, `INSERT INTO suggestions (observation_id, proposal_id, session_id, generation,
				herdr_session, agent_pane_id, review_pane_id, prompt_hash, reserved, shown_at, result)
				VALUES (?, ?, ?, 0, 'default', 'w14:p3', 'w14:p9', 'h', ?, ?, ?)`, []int64{uncertain, unknown}[i], s.proposal,
				c1, testNow.Add(-10*time.Minute).Format(storeTime), testNow.Add(-10*time.Minute).Format(storeTime),
				s.result); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE proposals SET review_seconds = 30 WHERE id = ?`, rejected); err != nil {
			return err
		}
		// A change request needs an extractor draft; the fixture sets its result directly.
		_, err = tx.ExecContext(ctx, `UPDATE proposals SET status = ?, decided = ? WHERE id = ?`, store.ProposalChanged,
			testNow.Add(time.Hour).Format(storeTime), changed)
		return err
	})
	f.storeWrite(t, func(ctx context.Context, tx *sql.Tx) error {
		if err := store.SetExtraction(ctx, tx, "ws_a15", false, testNow); err != nil {
			return err
		}
		return store.SetLearning(ctx, tx, store.LearningSetting{Scope: store.LearningWorkspace, Target: "ws_a15", On: false,
			Changed: testNow})
	})
	out := f.mustStatus(t)
	f.golden(t, "counts", out)
	for _, text := range []string{"use pnpm", "no, use pnpm", "make lint", "install"} {
		if strings.Contains(out, text) {
			t.Errorf("status shows text %q:\n%s", text, out)
		}
	}
}

// storeTime is the time layout of the store columns.
const storeTime = "2006-01-02T15:04:05.000Z07:00"

// [NEW] a week starts on Monday 00:00 UTC: a Sunday night and a Monday midnight are in two weeks.
func TestWeekStarts(t *testing.T) {
	monday := time.Date(2026, 9, 28, 0, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		now  time.Time
		want time.Time
	}{
		{monday, monday},
		{monday.Add(-time.Nanosecond), monday.AddDate(0, 0, -7)},
		{monday.AddDate(0, 0, 6).Add(23 * time.Hour), monday},
		{time.Date(2026, 9, 28, 1, 0, 0, 0, time.FixedZone("UTC+2", 2*3600)), monday.AddDate(0, 0, -7)},
	} {
		got := weekStarts(c.now, 2)
		if !got[0].Equal(c.want) || !got[1].Equal(c.want.AddDate(0, 0, -7)) {
			t.Errorf("weekStarts(%v) = %v, want %v and the week before", c.now, got, c.want)
		}
	}
}

func TestMedian(t *testing.T) {
	for _, c := range []struct {
		xs   []int64
		want int64
	}{{[]int64{7}, 7}, {[]int64{1, 2, 9}, 2}, {[]int64{30, 600}, 315}, {[]int64{1, 2}, 1}} {
		if got := median(c.xs); got != c.want {
			t.Errorf("median(%v) = %d, want %d", c.xs, got, c.want)
		}
	}
}
