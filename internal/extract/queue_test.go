package extract

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
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
// call runs outside any transaction, so a hook write with the hook's lock wait goes through,
// inside the hook cap (S42).
func TestQueueFairAndBounded(t *testing.T) {
	f := newFixture(t)
	f.caps = Caps{Calls: 10, Proposals: 15, InputBytes: 32 << 10} // [NEW] test limit of 10 calls: the 16 batches need two days
	keys := f.manyWorkspaces()
	hook := f.hookStore()
	f.cli.reply = func(ctx context.Context, prompt string) ([]byte, error) {
		f.now = f.now.Add(time.Minute)
		start := time.Now()
		if err := hook.Write(ctx, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, `UPDATE extractor_settings SET changed = changed`)
			return err
		}); err != nil {
			t.Errorf("a write during the call: %v", err)
		}
		// The hook's receipt write stays inside the hook cap of 300 ms (hook.hookCap)
		// while the queue runs (S42).
		if d := time.Since(start); d > 300*time.Millisecond {
			t.Errorf("a hook write during the call took %v, want at most the hook cap 300ms", d)
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

// AC S15 and #428: a signed-out extractor CLI fails. The signals wait with the stored reason,
// which says why, and the next pass within the retry wait makes no call. The call ran no model,
// so it uses no try.
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
	reason := "claude: Not logged in. Please run /login: sign-in failed"
	if ws := f.waiting(); len(ws) != 1 || ws[0].Why != "the last claude call failed: "+reason || ws[0].Failed != reason {
		t.Errorf("waiting = %+v, want %q", ws, reason)
	}
	if b := f.batches()[0]; b.ModelCalls != 0 {
		t.Errorf("model calls = %d, want 0: a sign-in failure uses no try", b.ModelCalls)
	}
	f.cli.reply = func(context.Context, string) ([]byte, error) { return nil, errors.New("claude: exit status 1") }
	f.now = f.now.Add(retryAfter)
	f.mustRun()
	if ws := f.waiting(); f.cli.calls() != 2 || len(ws) != 1 || !strings.Contains(ws[0].Why, "exit status 1") {
		t.Errorf("calls %d, waiting %+v, want the retry and its reason", f.cli.calls(), ws)
	}
}

// AC #263: after a failed call, a later call that works clears the failure from the wait, and
// the failed batch gets its retry at the next pass, not after retryAfter. The failed batch
// stays in the failed batches until then, with its time.
func TestRetryAfterWorkedCall(t *testing.T) {
	f := newFixture(t)
	sid, _ := f.session("a", true, line{"ws-A", pnpm, t0})
	f.cli.reply = func(context.Context, string) ([]byte, error) { return nil, errors.New("claude: exit status 1") }
	f.mustRun()
	failedAt := f.now
	f.now = f.now.Add(time.Minute)
	f.session("b", true, line{"ws-B", pnpm, t0.Add(time.Minute)})
	f.cli.reply = noClaims
	f.mustRun()
	if f.calls("ws-A") != 1 || f.calls("ws-B") != 1 {
		t.Fatalf("calls of ws-A, ws-B = %d, %d, want 1, 1", f.calls("ws-A"), f.calls("ws-B"))
	}
	if ws := f.waiting(); len(ws) != 1 || ws[0].WorkspaceKey != "ws-A" || ws[0].Why != "" || ws[0].Failed != "" {
		t.Errorf("waiting = %+v, want ws-A with no failure: a later call worked", ws)
	}
	var failed []store.Batch
	f.write(func(ctx context.Context, tx *sql.Tx) (err error) {
		failed, err = store.FailedBatches(ctx, tx)
		return err
	})
	if len(failed) != 1 || failed[0].WorkspaceKey != "ws-A" || !failed[0].At.Equal(failedAt) {
		t.Errorf("failed batches = %+v, want the ws-A batch at %v", failed, failedAt)
	}
	f.now = f.now.Add(time.Minute)
	f.mustRun()
	if f.calls("ws-A") != 2 || f.watermark(sid, "ws-A") == 0 {
		t.Errorf("calls of ws-A %d, watermark %d, want the retry and a watermark", f.calls("ws-A"), f.watermark(sid, "ws-A"))
	}
}

// run4Line is the command of batch 15 of M2 eval run 4 (#347): one shell line, so no tool runs it
// as one argv list.
const run4Line = "./scripts/reset-db.sh && npm test 2>&1 | tail -40; git status --short"

// splitTool answers a prompt with run4Line as the extractor did in run 4: one tool claim whose
// commands split the shell line into two argv lists, so they are no commands of the evidence and
// the check drops the claim. Any other prompt gets a claim that commits.
func splitTool(ctx context.Context, prompt string) ([]byte, error) {
	if !strings.Contains(prompt, run4Line) {
		return claimOf("use pnpm")(ctx, prompt)
	}
	return toolReply(map[string]any{"name": "reset-db-sh", "commands": [][]string{{"./scripts/reset-db.sh"}, {"npm", "test"}},
		"inputs": nil})(ctx, prompt)
}

// worked stores ended session id in ws-B with a pnpm correction, one minute later, and runs a
// pass: its call works.
func (f *fixture) worked(id string) {
	f.t.Helper()
	f.now = f.now.Add(time.Minute)
	f.session(id, true, line{"ws-B", pnpm, f.now.Add(-time.Hour)})
	f.mustRun()
}

// AC #347 with the run 4 shape: the check drops the split tool claim of the shell line, so the
// answer is invalid (the check is right: a tool runs argv lists, and these are no commands of
// the evidence). The batch gets maxTries calls, though a later call works at every pass, and
// none after, also after retryAfter. Its evidence waits with no try left: Waiting and the failed
// batches show it, and doctor gets no failure to retry.
func TestInvalidBatchTries(t *testing.T) {
	f := newFixture(t)
	f.cli.reply = splitTool
	sid := f.commandSession("a", turn{"reset the db and run the tests", []store.CommandRecord{command(run4Line)}})
	f.mustRun()
	bs := f.batches()
	if len(bs) != 1 || bs[0].Status != store.BatchInvalid ||
		!strings.Contains(bs[0].Reason, "the tool commands are not commands of the evidence in one folder") {
		t.Fatalf("batches = %+v, want one invalid batch: the check drops the split tool claim", bs)
	}
	for i := range 6 {
		f.worked(fmt.Sprintf("b%d", i))
	}
	f.now = f.now.Add(2 * retryAfter)
	f.mustRun()
	if got := f.calls("ws-A"); got != maxTries {
		t.Errorf("calls of ws-A = %d, want %d: no call at each later pass", got, maxTries)
	}
	if got := f.calls("ws-B"); got != 6 {
		t.Errorf("calls of ws-B = %d, want 6", got)
	}
	if wm := f.watermark(sid, "ws-A"); wm != 0 {
		t.Errorf("watermark = %d, want 0: an invalid response does not move it", wm)
	}
	ws := f.waiting()
	if len(ws) != 1 || ws[0].WorkspaceKey != "ws-A" || !strings.HasSuffix(ws[0].Why, "; "+noTry) || ws[0].Failed != "" {
		t.Errorf("waiting = %+v, want ws-A with no try left and no failure that waits for a retry", ws)
	}
	var failed []store.Batch
	f.write(func(ctx context.Context, tx *sql.Tx) (err error) {
		failed, err = store.FailedBatches(ctx, tx)
		return err
	})
	if len(failed) != 1 || failed[0].SessionID != sid || failed[0].ModelCalls != maxTries {
		t.Errorf("failed batches = %+v, want the batch of session a with %d calls", failed, maxTries)
	}
}

// AC #347 and #263: a failed batch whose calls all fail while the CLI is down gets maxTries
// calls, one each retryAfter, and then waits for a later call that works, with its failure
// shown. After that call it gets one more call: when it works, the watermark moves; when it
// fails too, the failure is the batch's own, and no later pass calls again.
func TestFailedBatchTries(t *testing.T) {
	for name, back := range map[string]bool{"the CLI is back": true, "own failure": false} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			sid, obs := f.session("a", true, line{"ws-A", "no, use tabs in go files", t0})
			down := func(context.Context, string) ([]byte, error) { return nil, errors.New("claude: exit status 1") }
			f.cli.reply = down
			for range 5 {
				f.mustRun()
				f.now = f.now.Add(retryAfter)
			}
			if got := f.calls("ws-A"); got != maxTries {
				t.Fatalf("calls of ws-A while the CLI is down = %d, want %d", got, maxTries)
			}
			if ws := f.waiting(); len(ws) != 1 || ws[0].Failed != "claude: exit status 1: "+noText {
				t.Errorf("waiting = %+v, want the failure that waits for its retry", ws)
			}
			f.cli.reply = func(ctx context.Context, prompt string) ([]byte, error) {
				if strings.Contains(prompt, "tabs") && !back {
					return down(ctx, prompt)
				}
				return claimOf("use pnpm")(ctx, prompt)
			}
			for i := range 4 {
				f.worked(fmt.Sprintf("b%d", i))
			}
			if got := f.calls("ws-A"); got != maxTries+1 {
				t.Errorf("calls of ws-A = %d, want %d: one more after a call works", got, maxTries+1)
			}
			if back {
				if wm := f.watermark(sid, "ws-A"); wm != obs[0] {
					t.Errorf("watermark = %d, want %d", wm, obs[0])
				}
				return
			}
			if ws := f.waiting(); len(ws) != 1 || !strings.HasSuffix(ws[0].Why, "; "+noTry) || ws[0].Failed != "" {
				t.Errorf("waiting = %+v, want ws-A with no try left", ws)
			}
		})
	}
}

// AC #347: new evidence of the session of a batch with no try left gives it a try. Mostly it
// makes a new batch, which replaces the batch and gets its own tries. When the input cap cut the
// run, the new evidence is after the cut: the batch keeps its range and gets one call, and its
// commit moves the watermark, so the rest of the session goes next.
func TestSpentBatchNewEvidence(t *testing.T) {
	for name, tc := range map[string]struct {
		input    int                 // the input cap; 0 for the default
		statuses []store.BatchStatus // the batches after the new evidence
	}{
		"new batch":     {statuses: []store.BatchStatus{store.BatchDiscarded, store.BatchCommitted}},
		"input cap cut": {input: len(pnpm) + 1, statuses: []store.BatchStatus{store.BatchCommitted}},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			if tc.input > 0 {
				f.caps = Caps{Calls: 40, Proposals: 15, InputBytes: tc.input} // [NEW] test limits: one excerpt a call
			}
			sid, obs := f.session("a", false, line{"ws-A", pnpm, t0}, line{"ws-A", "wrong, run make lint first", t0})
			f.cli.reply = func(context.Context, string) ([]byte, error) { return claudeReply(nil) }
			for range maxTries + 2 {
				f.mustRun()
				f.now = f.now.Add(retryAfter)
			}
			if got := f.cli.calls(); got != maxTries {
				t.Fatalf("calls = %d, want %d", got, maxTries)
			}
			f.write(func(ctx context.Context, tx *sql.Tx) error {
				seg, err := store.StartSegment(ctx, tx, sid, "w1:p1", "ws-A", "recent_unwrapped", f.now)
				if err != nil {
					return err
				}
				_, err = store.AddObservation(ctx, tx, store.Observation{CaptureIdentity: "a-new", Source: store.SourceHerdr,
					SegmentID: seg, Event: "working", Excerpt: "no, use tabs in go files", Status: store.ObservationComplete,
					Sender: store.SenderYou, Counts: true, At: f.now})
				return err
			})
			f.now = f.now.Add(idle)
			f.cli.reply = claimOf("use pnpm")
			f.mustRun()
			if got := f.cli.calls(); got != maxTries+1 || f.watermark(sid, "ws-A") < obs[0] {
				t.Errorf("calls %d, watermark %d, want %d calls and a watermark", got, f.watermark(sid, "ws-A"), maxTries+1)
			}
			var got []store.BatchStatus
			for _, b := range f.batches() {
				got = append(got, b.Status)
			}
			if !slices.Equal(got, tc.statuses) {
				t.Errorf("batch statuses = %v, want %v", got, tc.statuses)
			}
		})
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
	// AC #282: a rejected proposal still counts toward the daily proposal cap.
	f.write(func(ctx context.Context, tx *sql.Tx) error {
		return store.DecideProposal(ctx, tx, 1, store.ProposalRejected, store.SurfaceCLI, f.now)
	})
	if err := p.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	var d Day
	f.write(func(ctx context.Context, tx *sql.Tx) (err error) {
		d, err = p.Day(ctx, tx)
		return err
	})
	if d.Proposals != 2 || d.Full() == "" || f.calls("ws-B") != 0 {
		t.Errorf("day = %+v, calls of ws-B = %d, want 2 proposals, a full cap and no call", d, f.calls("ws-B"))
	}
	// AC #372: the proposal cap has room when the proposal that keeps it full is a day old.
	if !d.Room.Equal(f.now.Add(day)) {
		t.Errorf("day room = %v, want %v", d.Room, f.now.Add(day))
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
		if !strings.Contains(prompt, texts[i]) || strings.Count(prompt, "] user: ") != 1 {
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

// AC #372: a full cap has room again when the call or proposal that keeps it full is a day old:
// with 4 calls in the last day and a cap of 3, that is the 2nd oldest. The pause line names the
// cap and the local time; no full cap gives no room and no line. TestProposalCap checks the
// room of the proposal cap.
func TestCapRoom(t *testing.T) {
	old := time.Local
	time.Local = time.FixedZone("IST", 5*3600+1800)
	t.Cleanup(func() { time.Local = old })
	f := newFixture(t)
	f.write(func(ctx context.Context, tx *sql.Tx) error {
		for _, ago := range []time.Duration{25 * time.Hour, 23 * time.Hour, 20 * time.Hour, 2 * time.Hour, time.Hour} {
			if _, err := tx.ExecContext(ctx, `INSERT INTO extraction_calls (workspace_key, at) VALUES ('ws-A', ?)`,
				f.now.Add(-ago).UTC().Format(time.RFC3339Nano)); err != nil {
				return err
			}
		}
		return nil
	})
	room := f.now.Add(4 * time.Hour) // the call of 20 hours ago is a day old then
	// [NEW] test limits: call caps of 3, 2 and 5 against the 4 calls of the last day.
	for _, tc := range []struct {
		name string
		caps Caps
		room time.Time
		line string
	}{
		{"call cap full", Caps{Calls: 3, Proposals: 15, InputBytes: 1}, room,
			"learning paused: the daily cap of 3 extractor calls is reached; room comes back at " + room.In(time.Local).Format("15:04")},
		{"call cap one over", Caps{Calls: 2, Proposals: 15, InputBytes: 1}, f.now.Add(22 * time.Hour),
			"learning paused: the daily cap of 2 extractor calls is reached; room comes back at " +
				f.now.Add(22*time.Hour).In(time.Local).Format("15:04")},
		{"room left", Caps{Calls: 5, Proposals: 15, InputBytes: 1}, time.Time{}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := f.pass()
			p.Caps = tc.caps
			var d Day
			f.write(func(ctx context.Context, tx *sql.Tx) (err error) {
				d, err = p.Day(ctx, tx)
				return err
			})
			if !d.Room.Equal(tc.room) || d.Paused() != tc.line {
				t.Errorf("Day() = %+v, Paused() = %q, want room %v and %q", d, d.Paused(), tc.room, tc.line)
			}
		})
	}
}
