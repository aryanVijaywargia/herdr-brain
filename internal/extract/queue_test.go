package extract

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aryanVijaywargia/herdr-brain/internal/signal"
	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

// noClaims is a valid empty extractor result: it makes no proposal.
func noClaims(context.Context, string) ([]byte, error) {
	return claudeReply(map[string]any{"claims": []any{}})
}

// calls returns the extractor calls of the batches of workspace key ws.
func (f *fixture) calls(ws string) int {
	f.t.Helper()
	return f.count(fmt.Sprintf(`SELECT COALESCE(SUM(model_calls), 0) FROM extraction_batches WHERE workspace_key = '%s'`, ws))
}

func (f *fixture) waiting() []Waiting {
	f.t.Helper()
	var ws []Waiting
	f.write(func(ctx context.Context, tx *sql.Tx) (err error) {
		ws, err = f.pass().Waiting(ctx, tx)
		return err
	})
	return ws
}

// manyWorkspaces adds workspaces ws-C and ws-D, and 4 ended sessions with a signal in each of
// the 4 workspaces, one minute apart. It returns the workspace keys.
func (f *fixture) manyWorkspaces() []string {
	f.t.Helper()
	keys := []string{"ws-A", "ws-B", "ws-C", "ws-D"}
	f.write(func(ctx context.Context, tx *sql.Tx) error {
		for i, key := range keys[2:] {
			if err := store.InsertWorkspace(ctx, tx, store.Workspace{Key: key, HerdrSession: "default",
				HerdrWorkspaceID: fmt.Sprintf("w%d", i+3), Label: key, Status: store.WorkspaceLive, LastSeen: t0}); err != nil {
				return err
			}
		}
		return nil
	})
	at := t0
	for i := range 4 {
		for _, key := range keys {
			f.session(fmt.Sprintf("%s-%d", key, i), true, line{key, pnpm, at})
			at = at.Add(time.Minute)
		}
	}
	return keys
}

// hookStore opens the store of the fixture a second time, with the busy timeout of the hook.
func (f *fixture) hookStore() *store.Store {
	f.t.Helper()
	var file string
	f.write(func(ctx context.Context, tx *sql.Tx) error {
		var seq int
		var name string
		return tx.QueryRowContext(ctx, `PRAGMA database_list`).Scan(&seq, &name, &file)
	})
	hook, err := store.Open(context.Background(), filepath.Dir(file), 50*time.Millisecond)
	if err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(func() { _ = hook.Close() })
	return hook
}

// AC S42: many workspaces with signals. The queue takes one batch of each workspace in turn,
// the call cap bounds each day, and the key that waited longest goes first the next day. The
// call runs outside any transaction, so a hook write with the hook's lock wait goes through.
func TestQueueFairAndBounded(t *testing.T) {
	f := newFixture(t)
	keys := f.manyWorkspaces()
	hook := f.hookStore()
	f.cli.reply = func(ctx context.Context, prompt string) ([]byte, error) {
		f.now = f.now.Add(time.Minute)
		if err := hook.Write(ctx, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, `UPDATE extractor_settings SET changed = changed`)
			return err
		}); err != nil {
			t.Errorf("a write during the call: %v", err)
		}
		return noClaims(ctx, prompt)
	}

	f.mustRun()
	if f.cli.calls() != 10 {
		t.Fatalf("calls on day 1 = %d, want the cap 10", f.cli.calls())
	}
	bs := f.batches()
	seen := map[string]bool{}
	for _, b := range bs[:4] {
		seen[b.WorkspaceKey] = true
	}
	if len(seen) != 4 {
		t.Errorf("the first 4 batches are of %v, want one of each workspace", seen)
	}
	for _, key := range keys {
		if n := f.calls(key); n < 2 || n > 3 {
			t.Errorf("calls of %s on day 1 = %d, want 2 or 3", key, n)
		}
	}
	f.mustRun()
	if f.cli.calls() != 10 {
		t.Fatalf("calls after a second pass on day 1 = %d, want 10", f.cli.calls())
	}
	if ws := f.waiting(); len(ws) != 4 || !strings.Contains(ws[0].Why, "daily cap of 10 extractor calls") {
		t.Errorf("waiting = %+v, want 4 workspaces that wait for the call cap", ws)
	}

	f.now = f.now.Add(day)
	f.mustRun()
	if f.cli.calls() != 16 {
		t.Fatalf("calls after day 2 = %d, want 16", f.cli.calls())
	}
	if first := f.batches()[10].WorkspaceKey; first != "ws-C" {
		t.Errorf("the first batch of day 2 is of %s, want ws-C, whose last call is the oldest", first)
	}
	for _, key := range keys {
		if n := f.calls(key); n != 4 {
			t.Errorf("calls of %s = %d, want 4", key, n)
		}
	}
	if ws := f.waiting(); len(ws) != 0 {
		t.Errorf("waiting = %+v, want none", ws)
	}
}

// AC S75, and extraction off with capture on: a workspace with extraction off gets 0 calls,
// and its signals wait. Learning (capture) stays on, and the wait says extraction, not
// learning.
func TestExtractionOffWorkspace(t *testing.T) {
	f := newFixture(t)
	f.write(func(ctx context.Context, tx *sql.Tx) error { return store.SetExtraction(ctx, tx, "ws-A", false, t0) })
	sid, _ := f.session("a", true, line{"ws-A", pnpm, t0})
	f.session("b", true, line{"ws-B", pnpm, t0.Add(time.Minute)})
	f.mustRun()
	if n := f.calls("ws-A"); n != 0 || f.cli.calls() != 1 || f.watermark(sid, "ws-A") != 0 {
		t.Fatalf("calls of ws-A = %d, all calls = %d, want 0 and 1", n, f.cli.calls())
	}
	var learning bool
	f.write(func(ctx context.Context, tx *sql.Tx) (err error) {
		learning, err = store.LearningOn(ctx, tx, true, "default", "ws-A", "")
		return err
	})
	if !learning {
		t.Errorf("learning of ws-A is off, want capture to stay on")
	}
	ws := f.waiting()
	if len(ws) != 1 || ws[0].WorkspaceKey != "ws-A" || ws[0].Why != "extraction is off" || !ws[0].Oldest.Equal(t0) {
		t.Errorf("waiting = %+v, want ws-A, extraction is off, since t0", ws)
	}

	f.write(func(ctx context.Context, tx *sql.Tx) error {
		return store.SetLearning(ctx, tx, store.LearningSetting{Scope: store.LearningWorkspace, Target: "ws-A",
			Changed: t0})
	})
	f.write(func(ctx context.Context, tx *sql.Tx) error { return store.SetExtraction(ctx, tx, "ws-A", true, t0) })
	if ws := f.waiting(); len(ws) != 1 || ws[0].Why != "learning is off" {
		t.Errorf("waiting with learning off = %+v, want learning is off", ws)
	}
}

// A queued batch whose workspace turns off before its dispatch gets 0 calls: the extraction
// switch, the learning switch, and the whole store's extraction switch.
func TestOffBeforeDispatch(t *testing.T) {
	for name, off := range map[string]func(context.Context, *sql.Tx) error{
		"extraction": func(ctx context.Context, tx *sql.Tx) error { return store.SetExtraction(ctx, tx, "ws-B", false, t0) },
		"learning": func(ctx context.Context, tx *sql.Tx) error {
			return store.SetLearning(ctx, tx, store.LearningSetting{Scope: store.LearningWorkspace, Target: "ws-B", Changed: t0})
		},
		"store extraction": func(ctx context.Context, tx *sql.Tx) error { return store.SetExtraction(ctx, tx, "", false, t0) },
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			f.session("a", true, line{"ws-A", pnpm, t0})
			f.session("b", true, line{"ws-B", pnpm, t0.Add(time.Minute)})
			f.cli.reply = func(ctx context.Context, prompt string) ([]byte, error) {
				if err := f.s.Write(ctx, func(tx *sql.Tx) error { return off(ctx, tx) }); err != nil {
					t.Error(err)
				}
				return noClaims(ctx, prompt)
			}
			f.mustRun()
			if f.calls("ws-A") != 1 || f.calls("ws-B") != 0 {
				t.Errorf("calls of ws-A, ws-B = %d, %d, want 1, 0", f.calls("ws-A"), f.calls("ws-B"))
			}
		})
	}
}

// An M1 store upgraded to M2 has no chosen CLI: 0 calls, and the wait says to run brain
// setup. Brain picks no CLI by itself.
func TestNoChosenCLI(t *testing.T) {
	f := newFixture(t)
	f.write(func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `DELETE FROM extractor_settings`)
		return err
	})
	f.session("a", true, line{"ws-A", pnpm, t0})
	f.mustRun()
	if f.cli.calls() != 0 || len(f.batches()) != 0 {
		t.Fatalf("calls %d, batches %+v, want none", f.cli.calls(), f.batches())
	}
	if ws := f.waiting(); len(ws) != 1 || ws[0].Why != "no extractor CLI is chosen; run brain setup" || !ws[0].NoCLI {
		t.Errorf("waiting = %+v, want the brain setup hint", ws)
	}
}

// AC S15: a signed-out extractor CLI fails. The signals wait with the stored reason, and the
// next pass within the retry wait makes no call.
func TestSignedOutCLI(t *testing.T) {
	f := newFixture(t)
	sid, _ := f.session("a", true, line{"ws-A", pnpm, t0})
	f.cli.reply = func(context.Context, string) ([]byte, error) {
		return []byte(`{"is_error":true,"result":"Not logged in. Please run /login"}`), nil
	}
	f.mustRun()
	f.mustRun()
	if f.cli.calls() != 1 || f.watermark(sid, "ws-A") != 0 {
		t.Fatalf("calls %d, watermark %d, want 1 call and no watermark", f.cli.calls(), f.watermark(sid, "ws-A"))
	}
	want := "the last claude call failed: claude reported an error"
	if ws := f.waiting(); len(ws) != 1 || ws[0].Why != want || ws[0].Failed != "claude reported an error" {
		t.Errorf("waiting = %+v, want %q", ws, want)
	}
	f.cli.reply = func(context.Context, string) ([]byte, error) { return nil, errors.New("claude: exit status 1") }
	f.now = f.now.Add(retryAfter)
	f.mustRun()
	if ws := f.waiting(); f.cli.calls() != 2 || len(ws) != 1 || !strings.Contains(ws[0].Why, "exit status 1") {
		t.Errorf("calls %d, waiting %+v, want the retry and its reason", f.cli.calls(), ws)
	}
}

// The proposal cap stops the calls that follow. A batch that crosses the cap commits all its
// proposals, so no candidate at its gate is left with no proposal.
func TestProposalCap(t *testing.T) {
	f := newFixture(t)
	f.session("a", true, line{"ws-A", pnpm, t0}, line{"ws-A", "no, use tabs in go files", t0},
		line{"ws-A", pnpm, t0}, line{"ws-A", "no, use tabs in go files", t0})
	f.session("b", true, line{"ws-B", pnpm, t0.Add(time.Minute)})
	f.cli.reply = func(context.Context, string) ([]byte, error) {
		claim := func(rule string, refs ...string) map[string]any {
			return map[string]any{"kind": "correction", "situation": "coding", "rule": rule, "scope_guess": "workspace",
				"evidence_refs": refs, "same_as": nil, "conflicts_with": nil, "touches_permission": false,
				"expected_benefit": "fewer fixes"}
		}
		return claudeReply(map[string]any{"claims": []any{claim("use pnpm", "e1", "e3"), claim("use tabs", "e2", "e4")}})
	}
	p := f.pass()
	p.Caps = Caps{Calls: 10, Proposals: 1, InputBytes: 1 << 10} // [NEW] test limits: a cap of 1 proposal
	if err := p.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := f.count(`SELECT COUNT(*) FROM proposals`); n != 2 {
		t.Errorf("proposals = %d, want both of the batch that crossed the cap", n)
	}
	if n := f.count(`SELECT COUNT(*) FROM candidates WHERE status = 'open'`); n != 0 {
		t.Errorf("open candidates = %d, want 0", n)
	}
	if f.cli.calls() != 1 || f.calls("ws-B") != 0 {
		t.Errorf("calls = %d, calls of ws-B = %d, want 1 and 0", f.cli.calls(), f.calls("ws-B"))
	}
	f.now = f.now.Add(day + time.Minute)
	if err := p.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.calls("ws-B") != 1 {
		t.Errorf("calls of ws-B the next day = %d, want 1: the work waited", f.calls("ws-B"))
	}
}

// The input cap cuts a batch: the rest goes into the next batch, after the watermark moves.
func TestInputCap(t *testing.T) {
	f := newFixture(t)
	texts := []string{pnpm, "wrong, run make lint first", "no, use tabs in go files"}
	sid, obs := f.session("a", true, line{"ws-A", texts[0], t0}, line{"ws-A", texts[1], t0}, line{"ws-A", texts[2], t0})
	f.cli.reply = noClaims
	p := f.pass()
	p.Caps = Caps{Calls: 10, Proposals: 5, InputBytes: len(pnpm) + 1} // [NEW] test limits: one excerpt a call
	for i := range 3 {
		if err := p.Run(context.Background()); err != nil {
			t.Fatal(err)
		}
		if wm := f.watermark(sid, "ws-A"); wm != obs[i] {
			t.Fatalf("watermark after pass %d = %d, want %d", i+1, wm, obs[i])
		}
	}
	for i, prompt := range f.cli.prompts {
		if !strings.Contains(prompt, texts[i]) || strings.Contains(prompt, "[e2]") {
			t.Errorf("prompt %d = %q, want only %q", i+1, prompt, texts[i])
		}
	}
	if f.cli.calls() != 3 {
		t.Errorf("calls = %d, want 3", f.cli.calls())
	}
}

// The input cap never splits a signal: a command chain that spans the cut moves the cut before
// it, and a run that is one signal stays whole.
func TestFitKeepsSignals(t *testing.T) {
	var run []store.SignalInput
	for id := range int64(4) {
		var o store.SignalInput
		o.ID = id + 1
		run = append(run, o)
	}
	rs := []ref{{id: "e1", obs: 1, excerpt: "aaaa"}, {id: "e2", obs: 2, excerpt: "bbbb"},
		{id: "e3", obs: 3, excerpt: "cccc"}, {id: "e4", obs: 4, excerpt: "dddd"}}
	p := Pass{Caps: Caps{Calls: 1, Proposals: 1, InputBytes: 10}} // [NEW] test limits: two excerpts
	chain := signal.Signal{Kind: signal.CommandChain, Observations: []int64{2, 3, 4}}
	for _, tc := range []struct {
		name string
		sigs []signal.Signal
		want int // the observations that stay
	}{
		{"no signal spans", nil, 2},
		{"a chain spans the cut", []signal.Signal{chain}, 1},
		{"one signal", []signal.Signal{{Kind: signal.CommandChain, Observations: []int64{1, 2, 3, 4}}}, 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gotRun, gotRefs := p.fit(run, rs, tc.sigs)
			if len(gotRun) != tc.want || len(gotRefs) != tc.want {
				t.Errorf("fit = %d observations, %d refs, want %d", len(gotRun), len(gotRefs), tc.want)
			}
		})
	}
}

// The daily call cap counts each call at its dispatch time: a batch retried on a later day
// counts only its new call there.
func TestCallsCountByDispatch(t *testing.T) {
	f := newFixture(t)
	f.session("a", true, line{"ws-A", pnpm, t0})
	f.cli.reply = func(context.Context, string) ([]byte, error) { return nil, errors.New("claude: exit status 1") }
	f.mustRun()
	f.now = f.now.Add(day + time.Hour)
	f.mustRun()
	var d Day
	f.write(func(ctx context.Context, tx *sql.Tx) (err error) {
		d, err = f.pass().Day(ctx, tx)
		return err
	})
	if bs := f.batches(); f.cli.calls() != 2 || len(bs) != 1 || bs[0].ModelCalls != 2 || d.Calls != 1 {
		t.Errorf("calls %d, batches %+v, calls in the last day %d; want 2 calls of one batch, 1 in the last day",
			f.cli.calls(), bs, d.Calls)
	}
}
