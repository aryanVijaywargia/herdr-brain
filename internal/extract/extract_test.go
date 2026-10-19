package extract

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aryanVijaywargia/herdr-brain/internal/fingerprint"
	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

var t0 = time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)

// fakeCLI is the extractor CLI of the tests: no model call. It counts the calls, keeps each
// prompt, and answers with reply.
type fakeCLI struct {
	mu      sync.Mutex
	prompts []string
	cmds    []Command
	reply   func(ctx context.Context, prompt string) ([]byte, error)
}

func (f *fakeCLI) Run(ctx context.Context, c Command) ([]byte, error) {
	f.mu.Lock()
	prompt := c.Argv[len(c.Argv)-1]
	f.prompts = append(f.prompts, prompt)
	f.cmds = append(f.cmds, c)
	f.mu.Unlock()
	return f.reply(ctx, prompt)
}

func (f *fakeCLI) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.prompts)
}

var refID = regexp.MustCompile(`\[(e\d+)\]`)

// claimOf is a claude result with one claim of rule, with each ref of prompt as evidence.
func claimOf(rule string) func(context.Context, string) ([]byte, error) {
	return func(_ context.Context, prompt string) ([]byte, error) {
		refs := []string{}
		for _, m := range refID.FindAllStringSubmatch(prompt, -1) {
			refs = append(refs, m[1])
		}
		return claudeReply(map[string]any{"claims": []any{map[string]any{"kind": "correction",
			"situation": "installing packages", "rule": rule, "scope_guess": "workspace", "evidence_refs": refs,
			"same_as": nil, "conflicts_with": nil, "touches_permission": false, "expected_benefit": "no npm lockfile"}}})
	}
}

func claudeReply(out any) ([]byte, error) {
	return json.Marshal(map[string]any{"is_error": false, "structured_output": out})
}

type fixture struct {
	t     *testing.T
	s     *store.Store
	cli   *fakeCLI
	now   time.Time
	panes int
	full  map[string]string // the full prompt of an excerpt, for the prompt index; none when it is the excerpt
	caps  Caps              // the caps of the pass; the zero value for DefaultCaps
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	s, err := store.Open(context.Background(), t.TempDir(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	f := &fixture{t: t, s: s, cli: &fakeCLI{reply: claimOf("use pnpm")}, now: t0.Add(time.Hour)}
	f.write(func(ctx context.Context, tx *sql.Tx) error {
		for i, key := range []string{"ws-A", "ws-B"} {
			if err := store.InsertWorkspace(ctx, tx, store.Workspace{Key: key, HerdrSession: "default",
				HerdrWorkspaceID: fmt.Sprintf("w%d", i+1), Label: key, Status: store.WorkspaceLive, LastSeen: t0}); err != nil {
				return err
			}
		}
		return store.SetExtractor(ctx, tx, store.ExtractorSetting{On: true, CLI: store.ExtractorClaude, Changed: t0})
	})
	return f
}

func (f *fixture) write(fn func(context.Context, *sql.Tx) error) {
	f.t.Helper()
	ctx := context.Background()
	if err := f.s.Write(ctx, func(tx *sql.Tx) error { return fn(ctx, tx) }); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) pass() Pass {
	return Pass{Store: f.s, Runner: f.cli, HerdrSession: "default", DefaultOn: true, Now: func() time.Time { return f.now },
		Caps: f.caps}
}

func (f *fixture) run() error { return f.pass().Run(context.Background()) }

func (f *fixture) mustRun() {
	f.t.Helper()
	if err := f.run(); err != nil {
		f.t.Fatal(err)
	}
}

// line is one observation of a session: its workspace key, text and time.
type line struct {
	ws, text string
	at       time.Time
}

// session stores a claude session of agent id with lines, a new segment at each workspace
// change, and ends it when ended. It returns the session id and the observation ids.
func (f *fixture) session(id string, ended bool, lines ...line) (int64, []int64) {
	f.t.Helper()
	return f.harnessSession("claude", id, ended, lines...)
}

// harnessSession is session for an agent of harness.
func (f *fixture) harnessSession(harness, id string, ended bool, lines ...line) (int64, []int64) {
	f.t.Helper()
	key := store.SessionKey{HerdrSession: "default", Harness: harness, Kind: store.KeyAgent, ID: id}
	f.panes++
	pane := fmt.Sprintf("w1:p%d", f.panes)
	var sid int64
	var obs []int64
	f.write(func(ctx context.Context, tx *sql.Tx) (err error) {
		if sid, err = store.OpenSession(ctx, tx, key, pane, lines[0].ws, lines[0].at); err != nil {
			return err
		}
		var seg int64
		ws := "\x00"
		for i, l := range lines {
			if l.ws != ws {
				if seg, err = store.StartSegment(ctx, tx, sid, pane, l.ws, "recent_unwrapped", l.at); err != nil {
					return err
				}
				ws = l.ws
			}
			o, err := store.AddObservation(ctx, tx, store.Observation{CaptureIdentity: fmt.Sprintf("%s-%d", id, i),
				Source: store.SourceHerdr, SegmentID: seg, Event: "working", Excerpt: l.text,
				Status: store.ObservationComplete, Sender: store.SenderYou, Counts: true, At: l.at})
			if err != nil {
				return err
			}
			full := cmp.Or(f.full[l.text], l.text)
			if err := store.IndexPrompt(ctx, tx, o, fingerprint.Prompt(full), l.text); err != nil {
				return err
			}
			obs = append(obs, o)
		}
		if ended {
			return store.EndSession(ctx, tx, key, lines[len(lines)-1].at)
		}
		return nil
	})
	return sid, obs
}

func (f *fixture) batches() []store.Batch {
	f.t.Helper()
	var bs []store.Batch
	f.write(func(ctx context.Context, tx *sql.Tx) (err error) {
		bs, err = store.Batches(ctx, tx)
		return err
	})
	return bs
}

func (f *fixture) watermark(sid int64, ws string) int64 {
	f.t.Helper()
	var wm int64
	f.write(func(ctx context.Context, tx *sql.Tx) (err error) {
		wm, err = store.Watermark(ctx, tx, sid, ws)
		return err
	})
	return wm
}

func (f *fixture) count(query string) int {
	f.t.Helper()
	var n int
	f.write(func(ctx context.Context, tx *sql.Tx) error { return tx.QueryRowContext(ctx, query).Scan(&n) })
	return n
}

const pnpm = "no, use pnpm instead of npm here"

// A crash before the commit: the batch stays active, the retry calls again and counts the
// call, and its evidence counts once.
func TestCrashBeforeCommit(t *testing.T) {
	f := newFixture(t)
	sid, obs := f.session("a", true, line{"ws-A", pnpm, t0})
	ctx, cancel := context.WithCancel(context.Background())
	f.cli.reply = func(context.Context, string) ([]byte, error) {
		cancel()
		return nil, context.Canceled
	}
	if err := f.pass().Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run = %v, want the cancel", err)
	}
	if bs := f.batches(); len(bs) != 1 || bs[0].Status != store.BatchActive || f.watermark(sid, "ws-A") != 0 {
		t.Fatalf("after the crash: batches %+v, want one active batch and no watermark", bs)
	}
	f.cli.reply = claimOf("use pnpm")
	f.mustRun()
	bs := f.batches()
	if len(bs) != 1 || bs[0].Status != store.BatchCommitted || bs[0].ModelCalls != 2 {
		t.Fatalf("after the retry: batches %+v, want one committed batch with 2 model calls", bs)
	}
	if n := f.count(`SELECT COUNT(*) FROM evidence_links`); n != 1 {
		t.Errorf("evidence links = %d, want 1", n)
	}
	if wm := f.watermark(sid, "ws-A"); wm != obs[0] {
		t.Errorf("watermark = %d, want %d", wm, obs[0])
	}
}

// A crash after the extractor returns a valid result and before the commit: a test trigger
// aborts the watermark write, the last write of the commit transaction. The evidence links,
// the candidate and the proposal of that transaction roll back with it, and the batch stays
// active. The retry calls again, and its evidence and proposal count once.
func TestCrashAfterResultBeforeCommit(t *testing.T) {
	f := newFixture(t)
	sid, obs := f.session("z", true, line{"ws-A", pnpm, t0}, line{"ws-A", pnpm, t0.Add(time.Minute)})
	f.write(func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `CREATE TRIGGER crash BEFORE INSERT ON extraction_watermarks
			BEGIN SELECT RAISE(ABORT, 'crash before the commit'); END`)
		return err
	})
	if err := f.run(); err == nil || !strings.Contains(err.Error(), "crash before the commit") {
		t.Fatalf("Run = %v, want the crash", err)
	}
	if bs := f.batches(); len(bs) != 1 || bs[0].Status != store.BatchActive || f.watermark(sid, "ws-A") != 0 {
		t.Fatalf("after the crash: batches %+v, want one active batch and no watermark", bs)
	}
	for _, table := range []string{"evidence_links", "candidates", "proposals"} {
		if n := f.count(`SELECT COUNT(*) FROM ` + table); n != 0 {
			t.Errorf("after the crash: %d rows in %s, want 0", n, table)
		}
	}
	f.write(func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `DROP TRIGGER crash`)
		return err
	})
	f.mustRun()
	bs := f.batches()
	if len(bs) != 1 || bs[0].Status != store.BatchCommitted || bs[0].ModelCalls != 2 || f.cli.calls() != 2 {
		t.Fatalf("after the retry: batches %+v, calls %d, want one committed batch with 2 model calls", bs, f.cli.calls())
	}
	if n := f.count(`SELECT COUNT(*) FROM evidence_links`); n != 2 {
		t.Errorf("evidence links = %d, want 2: each observation once", n)
	}
	if n := f.count(`SELECT COUNT(*) FROM proposals`); n != 1 {
		t.Errorf("proposals = %d, want 1", n)
	}
	if wm := f.watermark(sid, "ws-A"); wm != obs[1] {
		t.Errorf("watermark = %d, want %d", wm, obs[1])
	}
}

// A crash after the commit: the commit again and a pass again add nothing.
func TestCrashAfterCommit(t *testing.T) {
	f := newFixture(t)
	f.session("z", true, line{"ws-A", pnpm, t0})
	f.session("a", true, line{"ws-A", pnpm, t0.Add(time.Minute)})
	f.mustRun()
	links := f.count(`SELECT COUNT(*) FROM evidence_links`)
	props := f.count(`SELECT COUNT(*) FROM proposals`)
	if f.cli.calls() != 2 || links != 2 || props != 1 {
		t.Fatalf("calls %d, links %d, proposals %d, want 2, 2, 1", f.cli.calls(), links, props)
	}
	// The commit of the last batch comes again, as after a crash before its reply, with a new
	// claim: it adds nothing.
	bs := f.batches()
	last := bs[len(bs)-1]
	c := Claim{Kind: "correction", Situation: "s", Rule: "another rule", Scope: store.ScopeWorkspace,
		Refs: []ref{{id: "e1", obs: last.LastObservation, identity: last.LastCapture}}}
	if err := f.pass().commit(context.Background(), job{batch: last}, last.ID, []Claim{c}, nil, nil); err != nil {
		t.Fatal(err)
	}
	f.mustRun()
	if f.cli.calls() != 2 || f.count(`SELECT COUNT(*) FROM evidence_links`) != links ||
		f.count(`SELECT COUNT(*) FROM proposals`) != props || !slices.EqualFunc(f.batches(), bs, sameBatch) {
		t.Errorf("the retry changed the store: calls %d, batches %+v", f.cli.calls(), f.batches())
	}
}

func sameBatch(a, b store.Batch) bool {
	return a.ID == b.ID && a.Status == b.Status && a.ModelCalls == b.ModelCalls
}

// A forget during the extraction discards the whole result; a later batch extracts from the
// evidence that stays.
func TestForgetDuringExtraction(t *testing.T) {
	f := newFixture(t)
	f.session("z", true, line{"ws-A", pnpm, t0})
	sid, _ := f.session("a", true, line{"ws-A", pnpm, t0.Add(time.Minute)})
	f.cli.reply = func(ctx context.Context, prompt string) ([]byte, error) {
		if f.cli.calls() == 2 { // the batch of session a
			f.write(func(ctx context.Context, tx *sql.Tx) error { return store.ForgetSession(ctx, tx, sid) })
		}
		return claimOf("use pnpm")(ctx, prompt)
	}
	f.mustRun()
	bs := f.batches()
	if len(bs) != 2 || bs[1].Status != store.BatchDiscarded || bs[1].SessionID != sid {
		t.Fatalf("batches %+v, want the batch of session a discarded", bs)
	}
	if n := f.count(`SELECT count FROM candidates`); n != 1 {
		t.Errorf("candidate count = %d, want 1: only the kept evidence of z", n)
	}
	// Session a goes on after the forget; its new text is a later batch.
	var obs int64
	f.write(func(ctx context.Context, tx *sql.Tx) error {
		seg, err := store.StartSegment(ctx, tx, sid, "w1:p2", "ws-A", "recent_unwrapped", t0.Add(2*time.Minute))
		if err != nil {
			return err
		}
		obs, err = store.AddObservation(ctx, tx, store.Observation{CaptureIdentity: "a-new", Source: store.SourceHerdr,
			SegmentID: seg, Event: "working", Excerpt: pnpm, Status: store.ObservationComplete, Sender: store.SenderYou,
			Counts: true, At: t0.Add(2 * time.Minute)})
		return err
	})
	f.cli.reply = claimOf("use pnpm")
	f.mustRun()
	if bs := f.batches(); len(bs) != 3 || bs[2].Status != store.BatchCommitted || f.cli.calls() != 3 {
		t.Fatalf("batches %+v, calls %d: want a later committed batch", bs, f.cli.calls())
	}
	if n := f.count(`SELECT count FROM candidates`); n != 2 {
		t.Errorf("candidate count = %d, want 2", n)
	}
	if wm := f.watermark(sid, "ws-A"); wm != obs {
		t.Errorf("watermark = %d, want %d", wm, obs)
	}
}

// A tombstone with the capture identity of an observation stops that evidence: a forget erases
// the claim key, so the same text gets a new claim id.
func TestTombstoneStopsEvidence(t *testing.T) {
	f := newFixture(t)
	f.write(func(ctx context.Context, tx *sql.Tx) error {
		return store.InsertTombstone(ctx, tx, store.Tombstone{ClaimID: "cl_old", SourceKeys: []string{"a-0"}, ForgottenAt: t0})
	})
	sid, obs := f.session("a", true, line{"ws-A", pnpm, t0})
	f.mustRun()
	if n := f.count(`SELECT COUNT(*) FROM candidates`); n != 0 || f.cli.calls() != 1 {
		t.Errorf("candidates %d, calls %d, want 0, 1", n, f.cli.calls())
	}
	if wm := f.watermark(sid, "ws-A"); wm != obs[0] {
		t.Errorf("watermark = %d, want %d", wm, obs[0])
	}
}

// A valid empty result moves the watermark; an invalid one does not, and the failed batches
// show it.
func TestEmptyAndInvalidResults(t *testing.T) {
	f := newFixture(t)
	sid, obs := f.session("a", true, line{"ws-A", pnpm, t0})
	f.cli.reply = func(context.Context, string) ([]byte, error) { return claudeReply(nil) }
	f.mustRun()
	var failed []store.Batch
	f.write(func(ctx context.Context, tx *sql.Tx) (err error) {
		failed, err = store.FailedBatches(ctx, tx)
		return err
	})
	if len(failed) != 1 || failed[0].Status != store.BatchInvalid || !strings.Contains(failed[0].Reason, "no structured output") {
		t.Fatalf("failed batches %+v, want one invalid batch", failed)
	}
	if wm := f.watermark(sid, "ws-A"); wm != 0 {
		t.Errorf("watermark after the invalid result = %d, want 0", wm)
	}
	f.mustRun()
	if f.cli.calls() != 1 {
		t.Errorf("calls = %d, want 1: the invalid batch waits for retryAfter", f.cli.calls())
	}
	f.now = f.now.Add(retryAfter)
	f.cli.reply = func(context.Context, string) ([]byte, error) { return claudeReply(map[string]any{"claims": []any{}}) }
	f.mustRun()
	if bs := f.batches(); len(bs) != 1 || bs[0].Status != store.BatchEmpty || bs[0].ModelCalls != 2 {
		t.Fatalf("batches %+v, want one empty batch", bs)
	}
	if wm := f.watermark(sid, "ws-A"); wm != obs[0] {
		t.Errorf("watermark after the empty result = %d, want %d", wm, obs[0])
	}
}

// A session that moves from A to B gives a batch for each key. Each call gets only the
// evidence of its workspace, and each batch stays processable after a failure (E13-S2).
func TestWorkspaceMove(t *testing.T) {
	f := newFixture(t)
	sid, obs := f.session("a", true, line{"ws-A", "no, use tabs in go files", t0},
		line{"ws-B", "wrong, run make lint first", t0.Add(time.Minute)})
	f.cli.reply = func(ctx context.Context, prompt string) ([]byte, error) {
		if strings.Contains(prompt, "tabs") {
			return []byte(`{"is_error":true}`), nil
		}
		return claimOf("run make lint")(ctx, prompt)
	}
	f.mustRun() // the failure is stored, not returned
	if len(f.cli.prompts) != 2 {
		t.Fatalf("calls = %d, want 2", len(f.cli.prompts))
	}
	for _, p := range f.cli.prompts {
		if strings.Contains(p, "tabs") == strings.Contains(p, "lint") {
			t.Errorf("prompt %q has the evidence of both workspaces", p)
		}
	}
	if a, b := f.watermark(sid, "ws-A"), f.watermark(sid, "ws-B"); a != 0 || b != obs[1] {
		t.Errorf("watermarks A %d, B %d, want 0, %d", a, b, obs[1])
	}
	f.now = f.now.Add(retryAfter)
	f.cli.reply = claimOf("use tabs")
	f.mustRun()
	if f.cli.calls() != 3 || f.watermark(sid, "ws-A") != obs[0] {
		t.Errorf("calls %d, watermark A %d, want 3, %d", f.cli.calls(), f.watermark(sid, "ws-A"), obs[0])
	}
	if n := f.count(`SELECT COUNT(*) FROM candidates WHERE workspace_key = 'ws-A'`); n != 1 {
		t.Errorf("A candidates = %d, want 1", n)
	}
}

// With extraction off for A and on for B, A gets no model call; with learning off, no batch
// gets one (E13-S2).
func TestSwitchesStopCalls(t *testing.T) {
	f := newFixture(t)
	f.write(func(ctx context.Context, tx *sql.Tx) error {
		return store.SetExtractor(ctx, tx, store.ExtractorSetting{WorkspaceKey: "ws-A", On: false, Changed: t0})
	})
	sid, _ := f.session("a", true, line{"ws-A", "no, use tabs in go files", t0},
		line{"ws-B", "wrong, run make lint first", t0.Add(time.Minute)})
	f.mustRun()
	if f.cli.calls() != 1 || strings.Contains(f.cli.prompts[0], "tabs") {
		t.Fatalf("prompts %q, want only the B call", f.cli.prompts)
	}
	if wm := f.watermark(sid, "ws-A"); wm != 0 {
		t.Errorf("watermark A = %d, want 0: the evidence waits", wm)
	}

	g := newFixture(t)
	g.write(func(ctx context.Context, tx *sql.Tx) error {
		return store.SetLearning(ctx, tx, store.LearningSetting{Scope: store.LearningStore, On: false, Changed: t0})
	})
	g.session("a", true, line{"ws-A", pnpm, t0})
	g.mustRun()
	if g.cli.calls() != 0 {
		t.Errorf("calls with learning off = %d, want 0", g.cli.calls())
	}
}

// A session is finished when it ended or after idle with no work (E13-S2).
func TestFinishedSession(t *testing.T) {
	f := newFixture(t)
	f.session("a", false, line{"ws-A", pnpm, t0})
	f.now = t0.Add(5 * time.Minute)
	f.mustRun()
	if f.cli.calls() != 0 {
		t.Fatalf("calls after 5 minutes = %d, want 0", f.cli.calls())
	}
	f.now = t0.Add(11 * time.Minute)
	f.mustRun()
	if f.cli.calls() != 1 {
		t.Fatalf("calls after 11 minutes = %d, want 1", f.cli.calls())
	}
}

// A run with no signal needs no call: it is an empty batch, and the watermark moves.
func TestNoSignalIsEmptyBatch(t *testing.T) {
	f := newFixture(t)
	sid, obs := f.session("a", true, line{"ws-A", "add a readme", t0})
	f.mustRun()
	if bs := f.batches(); f.cli.calls() != 0 || len(bs) != 1 || bs[0].Status != store.BatchEmpty || bs[0].ModelCalls != 0 {
		t.Fatalf("calls %d, batches %+v, want one empty batch and no call", f.cli.calls(), bs)
	}
	if wm := f.watermark(sid, "ws-A"); wm != obs[0] {
		t.Errorf("watermark = %d, want %d", wm, obs[0])
	}
}

// A proposal goes through ProposeCandidate with a draft item in the scope of the candidate,
// and model text goes through the secret filter.
func TestProposal(t *testing.T) {
	f := newFixture(t)
	f.session("z", true, line{"ws-A", pnpm, t0})
	f.session("a", true, line{"ws-A", pnpm, t0.Add(time.Minute)})
	f.cli.reply = claimOf("use pnpm, token sk-ant-abcdefghijklmnopqrstuvwxyz")
	f.mustRun()
	var scope, key, state, text, target, origin string
	f.write(func(ctx context.Context, tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `SELECT i.scope, i.workspace_key, i.state, p.text, p.target, r.origin
			FROM proposals p JOIN items i ON i.id = p.item_id JOIN item_revisions r ON r.item_id = i.id`).
			Scan(&scope, &key, &state, &text, &target, &origin)
	})
	if scope != "workspace" || key != "ws-A" || state != string(store.StateDraft) || target != "workspace w1 (ws-A)" ||
		origin != string(store.OriginExtractor) {
		t.Errorf("proposal item %s %s %s, target %q, origin %s", scope, key, state, target, origin)
	}
	if strings.Contains(text, "sk-ant") || !strings.Contains(text, "[secret]") {
		t.Errorf("proposal text %q, want the secret replaced", text)
	}
	if n := f.count(`SELECT COUNT(*) FROM claims WHERE rule LIKE '%sk-ant%'`); n != 0 {
		t.Errorf("claims with the secret = %d, want 0", n)
	}
}

// The claude call has the E13-S1 argv and the environment of brained with no variable of the
// denylist (#428): a sign-in variable passes, and HERDR_* and the session markers do not.
func TestClaudeCall(t *testing.T) {
	t.Setenv("HERDR_SESSION", "x")
	t.Setenv("CLAUDE_CODE_ENTRYPOINT", "cli")
	t.Setenv("CLAUDECODE", "1")
	t.Setenv("ANTHROPIC_API_KEY", "x")
	f := newFixture(t)
	f.session("a", true, line{"ws-A", pnpm, t0})
	f.mustRun()
	c := f.cli.cmds[0]
	if c.Argv[0] != "claude" || !slices.Contains(c.Argv, "--no-session-persistence") || c.Dir == "" {
		t.Errorf("argv %q in %q", c.Argv, c.Dir)
	}
	// The messages name a variable, never its value: the test environment can hold a real one.
	for _, e := range c.Env {
		if name, _, _ := strings.Cut(e, "="); strings.HasPrefix(name, "HERDR_") || name == "CLAUDE_CODE_ENTRYPOINT" ||
			name == "CLAUDECODE" {
			t.Errorf("env has %s", name)
		}
	}
	if !slices.Contains(c.Env, "ANTHROPIC_API_KEY=x") {
		t.Errorf("env has no ANTHROPIC_API_KEY=x")
	}
}

func TestCheck(t *testing.T) {
	j := job{refs: []ref{{id: "e1"}, {id: "e2"}}, known: []store.Claim{{ID: "c1"}}}
	valid := `{"kind":"fact","situation":"s","rule":"r","scope_guess":"global","evidence_refs":["e1","e1"],` +
		`"same_as":"c1","conflicts_with":null,"touches_permission":true,"expected_benefit":"b"}`
	claims, _, err := check(json.RawMessage(`{"claims":[`+valid+`]}`), j)
	if err != nil || len(claims) != 1 || len(claims[0].Refs) != 1 || claims[0].SameAs != "c1" || claims[0].Scope != store.ScopeGlobal {
		t.Fatalf("check(valid) = %+v, %v", claims, err)
	}
	for name, out := range map[string]string{
		"no claims":     `{}`,
		"extra field":   `{"claims":[],"x":1}`,
		"unknown ref":   `{"claims":[` + strings.Replace(valid, `"e1","e1"`, `"e9"`, 1) + `]}`,
		"no refs":       `{"claims":[` + strings.Replace(valid, `"e1","e1"`, ``, 1) + `]}`,
		"bad kind":      `{"claims":[` + strings.Replace(valid, `"fact"`, `"tip"`, 1) + `]}`,
		"bad scope":     `{"claims":[` + strings.Replace(valid, `"global"`, `"team"`, 1) + `]}`,
		"empty rule":    `{"claims":[` + strings.Replace(valid, `"rule":"r"`, `"rule":" "`, 1) + `]}`,
		"missing field": `{"claims":[` + strings.Replace(valid, `,"expected_benefit":"b"`, ``, 1) + `]}`,
		"bad same_as":   `{"claims":[` + strings.Replace(valid, `"same_as":"c1"`, `"same_as":1`, 1) + `]}`,
	} {
		if _, _, err := check(json.RawMessage(out), j); !errors.Is(err, ErrInvalid) {
			t.Errorf("check(%s) = %v, want ErrInvalid", name, err)
		}
	}
}

func TestCodexResult(t *testing.T) {
	dir := t.TempDir()
	last := dir + "/last.json"
	if _, err := codexResult([]byte(`{"type":"item.completed","item":{"type":"agent_message"}}`), last); err == nil ||
		errors.Is(err, ErrInvalid) {
		t.Errorf("no last file: %v, want a failure", err)
	}
	if _, err := codexResult([]byte(`{"type":"item.started","item":{"type":"command_execution"}}`), last); !errors.Is(err, ErrInvalid) {
		t.Errorf("tool item: %v, want ErrInvalid", err)
	}
	servers, err := enabledServers([]byte(`[{"name":"a"},{"name":"b","enabled":false},{"name":"c","enabled":true}]`))
	if err != nil || !slices.Equal(servers, []string{"a", "c"}) {
		t.Errorf("enabledServers = %v, %v", servers, err)
	}
	argv := codexArgv("", servers, "p")
	if !slices.Contains(argv, "mcp_servers.a.enabled=false") || argv[len(argv)-1] != "p" {
		t.Errorf("codexArgv = %q", argv)
	}
}
