package extract

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

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

// legacyRequest gives change request id and its proposal the state of an older Brain after
// migration 37: failed with all its tries and a sign-in refusal, and the proposal back in review.
func (f *fixture) legacyRequest(id, proposal int64) {
	f.t.Helper()
	f.write(func(ctx context.Context, tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `UPDATE change_requests SET status = ?, model_calls = ?, refusal = ?,
			reason = 'claude: exit status 1', at = ? WHERE id = ?`, store.ChangeFailed, maxTries+1, store.RefusalSignIn,
			f.now.Format(time.RFC3339Nano), id); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `UPDATE proposals SET status = ?, surface = NULL, decided = NULL WHERE id = ?`,
			store.ProposalPending, proposal)
		return err
	})
}

// nextRevision adds a revision to item, as an edit in review does.
func (f *fixture) nextRevision(item int64) {
	f.t.Helper()
	f.write(func(ctx context.Context, tx *sql.Tx) error {
		it, err := store.ItemByID(ctx, tx, item)
		if err != nil {
			return err
		}
		cur, err := store.ItemRevisionByKey(ctx, tx, item, it.CurrentRevision)
		if err != nil {
			return err
		}
		cur.Revision++
		cur.Text += " (edited)"
		if err := store.InsertItemRevision(ctx, tx, cur); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE items SET current_revision = ? WHERE id = ?`, cur.Revision, item)
		return err
	})
}

// legacyKinds make a recovering change request of each kind: a request on a proposal, and the
// redraft of the draft of an item revision. Each returns the request, its proposal and its item.
var legacyKinds = map[string]func(f *fixture) (store.ChangeRequest, int64, int64){
	"proposal": func(f *fixture) (store.ChangeRequest, int64, int64) {
		p := f.proposed()
		return f.ask(p, "split it"), p.ID, *p.ItemID
	},
	"item draft": func(f *fixture) (store.ChangeRequest, int64, int64) {
		item := f.revisedItem(2)
		f.comebacks(item, 2, 2)
		f.cli.reply = claimOf("use pnpm, never npm, for every install")
		f.mustRun()
		draft := f.proposals()[0]
		var cr store.ChangeRequest
		f.write(func(ctx context.Context, tx *sql.Tx) (err error) {
			cr, err = store.AskItemChange(ctx, tx, draft.ID, "make it shorter", store.SurfaceCLI, f.now)
			return err
		})
		return cr, draft.ID, item
	},
}

// AC #428 (Sol round 4): a recovering change request takes back its proposal only when it runs.
// The pass of another herdr session leaves the proposal in review, and a request that the next
// pass discards, as the item moved to another revision, leaves it in review too. Both kinds of
// request.
func TestRecoveryLeavesReview(t *testing.T) {
	for kind, make := range legacyKinds {
		t.Run(kind+", other binding", func(t *testing.T) {
			f := newFixture(t)
			cr, proposal, _ := make(f)
			f.legacyRequest(cr.ID, proposal)
			f.now = f.now.Add(retryAfter + time.Minute)
			f.cli.reply = redraft(claimOf("use pnpm 9"))
			runs := f.cli.calls()
			other := f.pass()
			other.HerdrSession = "other"
			if err := other.Run(context.Background()); err != nil {
				t.Fatal(err)
			}
			if got := f.proposal(proposal); got.Status != store.ProposalPending || f.cli.calls() != runs {
				t.Errorf("proposal = %s after %d calls, want pending with no call", got.Status, f.cli.calls()-runs)
			}
			f.mustRun()
			if got := f.request(cr.ID); got.Status != store.ChangeCommitted {
				t.Errorf("request after its own pass = %+v, want committed", got)
			}
		})
		t.Run(kind+", new revision", func(t *testing.T) {
			f := newFixture(t)
			cr, proposal, item := make(f)
			f.legacyRequest(cr.ID, proposal)
			f.nextRevision(item)
			f.now = f.now.Add(retryAfter + time.Minute)
			f.cli.reply = redraft(claimOf("use pnpm 9"))
			f.mustRun()
			if got := f.request(cr.ID); got.Status != store.ChangeDiscarded {
				t.Errorf("request = %+v, want discarded", got)
			}
			if got := f.proposal(proposal); got.Status != store.ProposalPending {
				t.Errorf("proposal = %s, want pending: still in review", got.Status)
			}
		})
		// Sol round 5: a pass at the daily call cap makes no call and leaves the proposal in
		// review; a new revision then discards the request, and the proposal stays in review.
		t.Run(kind+", daily cap", func(t *testing.T) {
			f := newFixture(t)
			cr, proposal, item := make(f)
			f.legacyRequest(cr.ID, proposal)
			f.now = f.now.Add(retryAfter + time.Minute)
			f.caps = Caps{Calls: f.day().Calls, Proposals: 15, InputBytes: 32 << 10}
			f.cli.reply = redraft(claimOf("use pnpm 9"))
			runs := f.cli.calls()
			f.mustRun()
			if got := f.proposal(proposal); got.Status != store.ProposalPending || f.cli.calls() != runs {
				t.Errorf("proposal at the cap = %s after %d calls, want pending with no call", got.Status, f.cli.calls()-runs)
			}
			f.nextRevision(item)
			f.mustRun()
			if got := f.request(cr.ID); got.Status != store.ChangeDiscarded {
				t.Errorf("request = %+v, want discarded", got)
			}
			if got := f.proposal(proposal); got.Status != store.ProposalPending {
				t.Errorf("proposal = %s, want pending: still in review", got.Status)
			}
		})
		// Sol round 5: an edit between the plan of a pass and the call of the request discards
		// the request with no call, and the proposal stays in review. The batch of a workspace
		// key with no call yet goes first (order), and its call makes the edit.
		t.Run(kind+", edit between plan and call", func(t *testing.T) {
			f := newFixture(t)
			cr, proposal, item := make(f)
			f.legacyRequest(cr.ID, proposal)
			f.now = f.now.Add(retryAfter + time.Minute)
			f.session("e1", true, line{"ws-C", pnpm, f.now.Add(-time.Hour)})
			edited, redrafts := false, 0
			f.cli.reply = func(ctx context.Context, prompt string) ([]byte, error) {
				if strings.Contains(prompt, "You redraft one proposed lesson") {
					redrafts++
					return claimOf("use pnpm 9")(ctx, prompt)
				}
				if !edited {
					edited = true
					f.nextRevision(item)
				}
				return claimOf("use pnpm")(ctx, prompt)
			}
			f.mustRun()
			if !edited || redrafts != 0 {
				t.Fatalf("edited %v, redraft calls %d, want the edit first and no redraft call", edited, redrafts)
			}
			if got := f.request(cr.ID); got.Status != store.ChangeDiscarded {
				t.Errorf("request = %+v, want discarded", got)
			}
			if got := f.proposal(proposal); got.Status != store.ProposalPending {
				t.Errorf("proposal = %s, want pending: still in review", got.Status)
			}
		})
	}
}
