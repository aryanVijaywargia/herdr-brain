package extract

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/aryanVijaywargia/herdr-brain/internal/signal"
	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

// promptP is the repeated prompt P of the M2 eval rerun (2026-10-08), byte-identical each time.
const promptP = "Run the full check now: lint, then tests, then build, in one chained command, and tell me what fails."

// releaseText is a release telling R of the M2 eval rerun, with version v: the procedure signal
// finds two of them similar.
func releaseText(v string) string {
	return fmt.Sprintf("Release %s: update the version in package.json to %s, then run npm test, then commit as "+
		"'Release %s' and tag that commit 'v%s'. Push the tag when it is done.", v, v, v, v)
}

// procedureReply answers each prompt with one procedure claim of situation, with each ref of the
// prompt as evidence: the same claim key each time, as the M2 run had.
func procedureReply(situation string) func(context.Context, string) ([]byte, error) {
	return func(_ context.Context, prompt string) ([]byte, error) {
		return claudeReply(map[string]any{"claims": []any{replyOf(map[string]any{"kind": "procedure",
			"situation": situation, "rule": "follow the same steps", "evidence_refs": promptRefs(prompt)})}})
	}
}

// AC 1, 4: the first telling of a procedure or a repeated prompt joins the evidence of the claim
// of its later tellings, so 3 tellings reach the gate of 3 (#314). The M2 shapes (N5): the
// release R1 to R4, a new version each time, and the repeated prompt P, in codex and claude
// sessions. The proposal comes after the third telling, not the fourth. The first telling alone
// has no signal: an empty batch and no call.
func TestFirstTellingCounts(t *testing.T) {
	for name, texts := range map[string][]string{
		"release R1 to R4":  {releaseText("0.2.0"), releaseText("0.3.0"), releaseText("0.4.0"), releaseText("0.5.0")},
		"repeated prompt P": {promptP, promptP, promptP, promptP},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			f.cli.reply = procedureReply("preparing a project release")
			for i, text := range texts {
				harness := []string{"codex", "claude"}[i%2]
				f.harnessSession(harness, fmt.Sprintf("s%d", i+1), true,
					line{"ws-A", text, t0.Add(time.Duration(i) * time.Minute)})
				f.mustRun()
				wantCount, wantProposals := []int{0, 2, 3, 4}[i], []int{0, 0, 1, 1}[i]
				if got := f.count(`SELECT COALESCE(MAX(count), 0) FROM candidates`); got != wantCount {
					t.Errorf("count after telling %d = %d, want %d", i+1, got, wantCount)
				}
				if got := len(f.proposals()); got != wantProposals {
					t.Errorf("proposals after telling %d = %d, want %d", i+1, got, wantProposals)
				}
			}
			if calls := f.cli.calls(); calls != 3 {
				t.Errorf("calls = %d, want 3: the first telling has no signal", calls)
			}
			if n := f.count(`SELECT COUNT(*) FROM candidates WHERE kind = 'procedure' AND sessions = 4
				AND harnesses = '["claude","codex"]'`); n != 1 {
				t.Errorf("procedure candidates of 4 sessions in claude and codex = %d, want 1", n)
			}
		})
	}
}

// AC 2: the earlier occurrence passes the evidence checks of any ref at the commit. A change
// during the extractor call of the second telling stops only the first telling: the batch still
// commits, as the first telling is not its input. A first telling in another workspace never
// joins, and one that another claim holds stays the evidence of that claim only (#279).
func TestEarlierOccurrenceChecks(t *testing.T) {
	update := func(q string) func(*fixture, int64, int64) {
		return func(f *fixture, _, obs int64) {
			f.write(func(ctx context.Context, tx *sql.Tx) error {
				_, err := tx.ExecContext(ctx, q, obs)
				return err
			})
		}
	}
	for name, tc := range map[string]struct {
		ws string // the workspace of the first telling
		// before runs before the pass of the second telling, during in its extractor call.
		before, during func(f *fixture, session, obs int64)
		want           int // the count of the ws-A candidate
	}{
		"kept":             {ws: "ws-A", want: 2},
		"other workspace":  {ws: "ws-B", want: 1},
		"assistant sender": {ws: "ws-A", during: update(`UPDATE observations SET sender = 'assistant' WHERE id = ?`), want: 1},
		"does not count":   {ws: "ws-A", during: update(`UPDATE observations SET counts = 0 WHERE id = ?`), want: 1},
		"forgotten": {ws: "ws-A", want: 1, during: func(f *fixture, session, _ int64) {
			f.write(func(ctx context.Context, tx *sql.Tx) error { return store.ForgetSession(ctx, tx, session) })
		}},
		"tombstoned": {ws: "ws-A", want: 1, during: func(f *fixture, _, _ int64) {
			f.write(func(ctx context.Context, tx *sql.Tx) error {
				return store.InsertTombstone(ctx, tx, store.Tombstone{ClaimID: "cl_old", SourceKeys: []string{"s1-0"},
					ForgottenAt: t0})
			})
		}},
		"held by another claim": {ws: "ws-A", want: 1, before: func(f *fixture, _, obs int64) {
			f.write(func(ctx context.Context, tx *sql.Tx) error {
				claim, err := store.ClaimID(ctx, tx, store.ClaimKey{Situation: "running checks", Rule: "chain them"}, t0)
				if err != nil {
					return err
				}
				_, err = store.Attach(ctx, tx, store.Candidate{ClaimID: claim, Scope: store.ScopeWorkspace,
					WorkspaceKey: "ws-A", Kind: store.CandidateProcedure}, obs, store.DefaultGates(), signal.Supports)
				return err
			})
		}},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			session, obs := f.session("s1", true, line{tc.ws, promptP, t0})
			f.mustRun()
			f.cli.reply = func(ctx context.Context, prompt string) ([]byte, error) {
				if tc.during != nil {
					tc.during(f, session, obs[0])
				}
				return procedureReply("running the full check")(ctx, prompt)
			}
			if tc.before != nil {
				tc.before(f, session, obs[0])
			}
			f.session("s2", true, line{"ws-A", promptP, t0.Add(time.Minute)})
			f.mustRun()
			if bs := f.batches(); len(bs) != 2 || bs[1].Status != store.BatchCommitted {
				t.Fatalf("batches %+v, want the batch of the second telling committed", bs)
			}
			if got := f.count(`SELECT count FROM candidates WHERE workspace_key = 'ws-A' AND claim_id IN
				(SELECT id FROM claims WHERE situation = 'running the full check')`); got != tc.want {
				t.Errorf("count = %d, want %d", got, tc.want)
			}
		})
	}
}

// AC 2: an earlier occurrence counts once, also when two later texts are similar to it, in one
// batch or in two.
func TestEarlierOccurrenceCountsOnce(t *testing.T) {
	f := newFixture(t)
	f.cli.reply = procedureReply("running the full check")
	f.session("s1", true, line{"ws-A", promptP, t0})
	f.session("s2", true, line{"ws-A", promptP, t0.Add(time.Minute)}, line{"ws-A", promptP, t0.Add(2 * time.Minute)})
	f.mustRun()
	if got := f.count(`SELECT count FROM candidates`); got != 3 {
		t.Errorf("count after one batch with two later texts = %d, want 3", got)
	}
	f.session("s3", true, line{"ws-A", promptP, t0.Add(3 * time.Minute)})
	f.mustRun()
	if got := f.count(`SELECT count FROM candidates`); got != 4 {
		t.Errorf("count after a later batch = %d, want 4", got)
	}
	if n := f.count(`SELECT COUNT(*) FROM evidence_links`); n != 4 {
		t.Errorf("evidence links = %d, want 4: one for each telling", n)
	}
}
