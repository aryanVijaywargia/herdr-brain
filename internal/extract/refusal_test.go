package extract

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

// AC #428 (Sol round 1): the sign-in check reads the full error text of the CLI. Auth text that
// the token filter hides, and auth text after the cut of the reason, still make a sign-in
// failure.
func TestSignInBeforeHide(t *testing.T) {
	for _, c := range []struct{ name, stderr, want string }{
		{"hidden token", "error: HTTP/1.1-401\n", "claude: error: [hidden]: sign-in failed"},
		{"auth text after the cut", strings.Repeat("x", 300) + " 401 Unauthorized\n",
			"claude: " + strings.Repeat("x", maxCause) + "...: sign-in failed"},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := runError(store.ExtractorClaude, nil, exit("claude", c.stderr))
			if !errors.Is(err, ErrSignIn) || err.Error() != c.want {
				t.Errorf("runError() = %q, want %q as a sign-in failure", err, c.want)
			}
		})
	}
}

// legacyBatch makes batch id a batch of a Brain before #428 after migration 37 (see
// store.TestRefusalMigration): the bare reason, a sign-in refusal and calls counted calls.
func (f *fixture) legacyBatch(id int64, calls int) {
	f.t.Helper()
	f.write(func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE extraction_batches SET reason = 'claude: exit status 1', refusal = ?,
			model_calls = ? WHERE id = ?`, store.RefusalSignIn, calls, id)
		return err
	})
}

// AC #428 (Sol round 1), the owner's case: 4 batches failed before #428 with the bare reason
// "claude: exit status 1", 2 of them with 3 counted calls and 1 with no try left. Within
// retryAfter they wait. Then each gets its call with no other call that works, and all commit
// once the sign-in works.
func TestLegacyFailuresRetry(t *testing.T) {
	f := newFixture(t)
	for _, s := range []string{"s1", "s2", "s3", "s4"} {
		f.session(s, true, line{"ws-" + s, pnpm, t0})
	}
	f.cli.reply = down
	f.mustRun()
	bs := f.batches()
	if len(bs) != 4 {
		t.Fatalf("batches = %d, want 4", len(bs))
	}
	for i, calls := range []int{1, 3, 3, 4} {
		f.legacyBatch(bs[i].ID, calls)
	}
	runs := f.cli.calls()
	f.now = f.now.Add(retryAfter / 2)
	f.mustRun()
	if n := f.cli.calls() - runs; n != 0 {
		t.Errorf("calls within retryAfter = %d, want 0", n)
	}
	f.now = f.now.Add(retryAfter)
	f.cli.reply = claimOf("use pnpm")
	f.mustRun()
	for _, b := range f.batches() {
		if b.Status != store.BatchCommitted {
			t.Errorf("batch %d = %s (%s), want committed after the sign-in works", b.ID, b.Status, b.Reason)
		}
	}
}

// AC #428 (Sol round 1): the recovery of a legacy reason is once per batch. When its call fails
// for another reason, the new reason counts as usual, and the batch with no try left gets no
// call each retryAfter.
func TestLegacyRetryOnce(t *testing.T) {
	f := newFixture(t)
	f.session("s1", true, line{"ws-A", pnpm, t0})
	f.cli.reply = down
	f.mustRun()
	f.legacyBatch(f.batches()[0].ID, maxTries)
	f.cli.reply = func(context.Context, string) ([]byte, error) {
		return []byte(`{"is_error":true,"result":"API Error: Request timed out"}`), exit("claude", "")
	}
	runs := f.cli.calls()
	for range 5 {
		f.now = f.now.Add(retryAfter)
		f.mustRun()
	}
	b := f.batches()[0]
	if n := f.cli.calls() - runs; n != 1 || b.ModelCalls != maxTries+1 || !spent(b) || b.Refusal != store.RefusalNone {
		t.Errorf("calls = %d, batch = %+v, want 1 call, then no try left", n, b)
	}
}

// AC #428 (Sol round 3): a change request that an older Brain failed with the bare reason, which
// migration 37 maps to a sign-in refusal, gets its call again: with 3 counted calls, and with 4,
// where the older Brain gave its proposal back to review. The proposal goes back to changed, and
// the request commits once the sign-in works.
func TestLegacyChangeRetry(t *testing.T) {
	for _, calls := range []int{maxTries, maxTries + 1} {
		t.Run(fmt.Sprint(calls, " calls"), func(t *testing.T) {
			f := newFixture(t)
			p := f.proposed()
			cr := f.ask(p, "split it")
			f.cli.reply = redraft(down)
			for range 5 {
				f.mustRun()
				f.now = f.now.Add(retryAfter)
			}
			if calls > maxTries {
				f.worked("b0")
				f.worked("b1")
			}
			got := f.request(cr.ID)
			if got.ModelCalls != calls || ChangeSpent(got) != (calls > maxTries) {
				t.Fatalf("request = %+v, want %d calls", got, calls)
			}
			f.write(func(ctx context.Context, tx *sql.Tx) error {
				_, err := tx.ExecContext(ctx, `UPDATE change_requests SET reason = 'claude: exit status 1', refusal = ?
					WHERE id = ?`, store.RefusalSignIn, cr.ID)
				return err
			})
			f.now = f.now.Add(retryAfter)
			f.cli.reply = redraft(claimOf("use pnpm 9"))
			f.mustRun()
			if got := f.request(cr.ID); got.Status != store.ChangeCommitted || got.ModelCalls != calls+1 {
				t.Errorf("request after the sign-in = %+v, want committed after one more call", got)
			}
		})
	}
}

// AC #428 (Sol round 3): when you decided the proposal that an older Brain gave back, the request
// gets no call: it is no longer current.
func TestLegacyChangeDecided(t *testing.T) {
	f := newFixture(t)
	p := f.proposed()
	cr := f.ask(p, "split it")
	f.cli.reply = redraft(down)
	for range 5 {
		f.mustRun()
		f.now = f.now.Add(retryAfter)
	}
	f.worked("b0")
	f.worked("b1")
	if got := f.proposal(p.ID); got.Status != store.ProposalPending {
		t.Fatalf("proposal = %s, want pending: given back with no try left", got.Status)
	}
	f.write(func(ctx context.Context, tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `UPDATE change_requests SET reason = 'claude: exit status 1', refusal = ?
			WHERE id = ?`, store.RefusalSignIn, cr.ID); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `UPDATE proposals SET status = ? WHERE id = ?`, store.ProposalRejected, p.ID)
		return err
	})
	runs := f.cli.calls()
	f.now = f.now.Add(retryAfter)
	f.cli.reply = redraft(claimOf("use pnpm 9"))
	f.mustRun()
	if got := f.request(cr.ID); got.Status != store.ChangeDiscarded || f.cli.calls() != runs {
		t.Errorf("request = %+v after %d calls, want discarded with no call", got, f.cli.calls()-runs)
	}
	if got := f.proposal(p.ID); got.Status != store.ProposalRejected {
		t.Errorf("proposal = %s, want it rejected as you decided", got.Status)
	}
}
