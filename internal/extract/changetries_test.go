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

// redraft answers a redraft prompt with redraftReply and any other prompt with a pnpm claim that
// commits.
func redraft(redraftReply func(context.Context, string) ([]byte, error)) func(context.Context, string) ([]byte, error) {
	return func(ctx context.Context, prompt string) ([]byte, error) {
		if strings.Contains(prompt, "You redraft one proposed lesson") {
			return redraftReply(ctx, prompt)
		}
		return claimOf("use pnpm")(ctx, prompt)
	}
}

// twoClaims answers with two claims: an invalid answer for one change request (runChange).
func twoClaims(_ context.Context, prompt string) ([]byte, error) {
	c := replyOf(map[string]any{"evidence_refs": promptRefs(prompt)})
	return claudeReply(map[string]any{"claims": []any{c, c}})
}

// down is the answer of a CLI that does not work, as while it is signed out.
func down(context.Context, string) ([]byte, error) { return nil, errors.New("claude: exit status 1") }

// proposal reads proposal id.
func (f *fixture) proposal(id int64) store.Proposal {
	f.t.Helper()
	var p store.Proposal
	f.write(func(ctx context.Context, tx *sql.Tx) (err error) {
		p, err = store.ProposalByID(ctx, tx, id)
		return err
	})
	return p
}

// AC #359: a change request whose answers stay invalid (2 claims for one request) gets maxTries
// calls, though a later call works at every pass, and none after, also after retryAfter. It
// stays invalid, and its proposal is back in review with your request. A new change request of
// the proposal starts with new tries.
func TestInvalidChangeRequestTries(t *testing.T) {
	f := newFixture(t)
	p := f.proposed()
	cr := f.ask(p, "split it")
	f.cli.reply = redraft(twoClaims)
	f.mustRun()
	for i := range 6 {
		f.worked(fmt.Sprintf("b%d", i))
	}
	f.now = f.now.Add(2 * retryAfter)
	f.mustRun()
	got := f.request(cr.ID)
	if got.Status != store.ChangeInvalid || got.ModelCalls != maxTries || !ChangeSpent(got) {
		t.Fatalf("request = %+v, want invalid after %d calls, with no try left", got, maxTries)
	}
	if n := f.count(fmt.Sprintf(`SELECT COUNT(*) FROM extraction_calls WHERE change_request_id = %d`, cr.ID)); n != maxTries {
		t.Errorf("calls of the request = %d, want %d", n, maxTries)
	}
	back := f.proposal(p.ID)
	if back.Status != store.ProposalPending || back.ChangeRequest == nil || *back.ChangeRequest != "split it" {
		t.Fatalf("proposal = %+v, want it pending again with the request text", back)
	}
	again := f.ask(back, "split it in two")
	if again.ID != cr.ID || again.Status != store.ChangeQueued || again.ModelCalls != 0 {
		t.Fatalf("new request = %+v, want request %d queued with new tries", again, cr.ID)
	}
	f.cli.reply = redraft(claimOf("use pnpm 9"))
	f.mustRun()
	if got := f.request(cr.ID); got.Status != store.ChangeCommitted || got.ModelCalls != 1 || got.NewProposalID == nil {
		t.Fatalf("request = %+v, want committed after 1 call", got)
	}
	if d := f.proposal(*f.request(cr.ID).NewProposalID); d.Status != store.ProposalPending || !strings.Contains(d.Text, "pnpm 9") {
		t.Errorf("new draft = %+v, want the pending redraft", d)
	}
}

// AC #359 and #263: a change request whose calls fail while the CLI is down gets maxTries calls,
// one each retryAfter, then one more after a later call works. When that call works, the draft
// comes. When it fails too, the failure is its own: the request stays failed, no later pass
// calls again, and its proposal is back in review.
func TestFailedChangeRequestTries(t *testing.T) {
	for name, back := range map[string]bool{"the CLI is back": true, "own failure": false} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			p := f.proposed()
			cr := f.ask(p, "split it")
			f.cli.reply = down
			for range 5 {
				f.mustRun()
				f.now = f.now.Add(retryAfter)
			}
			if got := f.request(cr.ID); got.ModelCalls != maxTries || got.Status != store.ChangeFailed || ChangeSpent(got) {
				t.Fatalf("request while the CLI is down = %+v, want failed after %d calls, with one try left", got, maxTries)
			}
			if got := f.proposal(p.ID); got.Status != store.ProposalChanged {
				t.Errorf("proposal = %+v, want it changed while a try is left", got)
			}
			f.cli.reply = redraft(claimOf("use pnpm 9"))
			if !back {
				f.cli.reply = redraft(down)
			}
			for i := range 4 {
				f.worked(fmt.Sprintf("b%d", i))
			}
			got := f.request(cr.ID)
			if got.ModelCalls != maxTries+1 {
				t.Errorf("calls = %d, want %d: one more after a call works", got.ModelCalls, maxTries+1)
			}
			want, status := store.ChangeCommitted, store.ProposalChanged
			if !back {
				want, status = store.ChangeFailed, store.ProposalPending
			}
			if got.Status != want || f.proposal(p.ID).Status != status {
				t.Errorf("request %+v, proposal %s, want %s and %s", got, f.proposal(p.ID).Status, want, status)
			}
		})
	}
}

// AC #359: the redraft of the draft of an item revision (store.AskItemChange) keeps that draft
// through its failed calls, and with no try left gives the draft back to review. A new change
// of the draft starts with new tries.
func TestItemChangeRequestTries(t *testing.T) {
	f := newFixture(t)
	item := f.revisedItem(2)
	f.comebacks(item, 2, 2)
	f.cli.reply = claimOf("use pnpm, never npm, for every install")
	f.mustRun()
	ps := f.proposals()
	if len(ps) != 1 {
		t.Fatalf("proposals = %+v, want the first draft", ps)
	}
	draft := ps[0]
	ask := func(text string) store.ChangeRequest {
		var cr store.ChangeRequest
		f.write(func(ctx context.Context, tx *sql.Tx) (err error) {
			cr, err = store.AskItemChange(ctx, tx, draft.ID, text, store.SurfaceCLI, f.now)
			return err
		})
		return cr
	}
	cr := ask("make it shorter")
	f.cli.reply = redraft(twoClaims)
	for range 5 {
		f.mustRun()
		f.now = f.now.Add(retryAfter)
	}
	got := f.request(cr.ID)
	if got.Status != store.ChangeInvalid || got.ModelCalls != maxTries || got.NewProposalID == nil || *got.NewProposalID != draft.ID {
		t.Fatalf("request = %+v, want invalid after %d calls, still with draft %d", got, maxTries, draft.ID)
	}
	if p := f.proposal(draft.ID); p.Status != store.ProposalPending {
		t.Fatalf("draft = %+v, want it pending again", p)
	}
	if again := ask("make it shorter, please"); again.ModelCalls != 0 || again.Status != store.ChangeQueued {
		t.Fatalf("new request = %+v, want it queued with new tries", again)
	}
	f.cli.reply = redraft(claimOf("use pnpm"))
	f.mustRun()
	if got := f.request(cr.ID); got.Status != store.ChangeCommitted || got.ModelCalls != 1 {
		t.Errorf("request = %+v, want committed after 1 call", got)
	}
}

// AC #359: a request with no try left whose proposal you then decide is no longer current: the
// next pass discards it, with no call.
func TestSpentChangeRequestDecided(t *testing.T) {
	f := newFixture(t)
	p := f.proposed()
	cr := f.ask(p, "split it")
	f.cli.reply = redraft(twoClaims)
	for range 4 {
		f.mustRun()
		f.now = f.now.Add(retryAfter)
	}
	if !ChangeSpent(f.request(cr.ID)) {
		t.Fatalf("request = %+v, want no try left", f.request(cr.ID))
	}
	f.write(func(ctx context.Context, tx *sql.Tx) error {
		return store.DecideProposal(ctx, tx, p.ID, store.ProposalRejected, store.SurfaceCLI, f.now)
	})
	calls := f.cli.calls()
	f.mustRun()
	if got := f.request(cr.ID); got.Status != store.ChangeDiscarded || f.cli.calls() != calls {
		t.Errorf("request = %+v, calls %d, want discarded with no call", got, f.cli.calls()-calls)
	}
}
