package extract

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

// revisedItem stores the active item of the pnpm claim in ws-A with revs revisions, and
// returns its id. The text of revision n ends with "(revision n)".
func (f *fixture) revisedItem(revs int64) int64 {
	f.t.Helper()
	_, item := f.claimItem("installing packages", "use pnpm")
	key := "ws-A"
	f.write(func(ctx context.Context, tx *sql.Tx) error {
		for n := int64(2); n <= revs; n++ {
			if err := store.InsertItemRevision(ctx, tx, store.ItemRevision{ItemID: item, Revision: n,
				Text: fmt.Sprintf("installing packages: use pnpm (revision %d)", n), Scope: store.ScopeWorkspace,
				WorkspaceKey: &key, State: store.StateActive, Author: store.AuthorYou, Origin: store.OriginReview,
				Created: t0}); err != nil {
				return err
			}
		}
		_, err := tx.ExecContext(ctx, `UPDATE items SET current_revision = ? WHERE id = ?`, revs, item)
		return err
	})
	return item
}

// deliver stores receipt d.
func (f *fixture) deliver(ds ...store.Delivery) {
	f.t.Helper()
	f.write(func(ctx context.Context, tx *sql.Tx) error {
		for _, d := range ds {
			if _, err := store.InsertDelivery(ctx, tx, d); err != nil {
				return err
			}
		}
		return nil
	})
}

// receiptOf is a claude receipt of agent session agent in binding at at, with result and the
// item revisions revs.
func receiptOf(binding, agent string, result store.Result, at time.Time, revs ...store.RevisionRef) store.Delivery {
	d := store.Delivery{Profile: "claude", HerdrSession: &binding, PaneID: "w1:p1", AgentSessionID: &agent,
		ItemRevisions: revs, Result: result, At: at}
	if result == store.ResultWithheld {
		reason := "withheld: identity"
		d.WithheldReason = &reason
	}
	return d
}

// outcomes returns the outcomes as "kind revision" lines, by id.
func (f *fixture) outcomes() []string {
	f.t.Helper()
	var os []string
	f.write(func(ctx context.Context, tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT kind, COALESCE(revision, 0) FROM outcomes ORDER BY id`)
		if err != nil {
			return err
		}
		defer rows.Close() //nolint:errcheck // read-only
		for rows.Next() {
			var kind string
			var rev int64
			if err := rows.Scan(&kind, &rev); err != nil {
				return err
			}
			os = append(os, fmt.Sprintf("%s %d", kind, rev))
		}
		return rows.Err()
	})
	return os
}

// AC: one test per branch, with receipt and observation fixtures. The item has 3 revisions;
// the session observes the mistake at t0. The comeback counts against the revision that the
// last receipt before the observation names, never the current revision (S24).
func TestOutcomeBranches(t *testing.T) {
	rev := func(item, n int64) store.RevisionRef { return store.RevisionRef{Item: item, Revision: n} }
	for _, c := range []struct {
		name     string
		receipts func(item int64) []store.Delivery
		want     []string
	}{
		{"no receipt", func(int64) []store.Delivery { return nil }, []string{"delivery_unknown 0"}},
		{"older revision", func(item int64) []store.Delivery {
			return []store.Delivery{receiptOf("default", "a", store.ResultEmitted, t0.Add(-time.Hour), rev(item, 1))}
		}, []string{"comeback 1"}},
		{"emitted then withheld", func(item int64) []store.Delivery {
			return []store.Delivery{receiptOf("default", "a", store.ResultEmitted, t0.Add(-time.Hour), rev(item, 3)),
				receiptOf("default", "a", store.ResultWithheld, t0.Add(-time.Minute))}
		}, []string{"delivery_gap 0"}},
		{"emitted without the item", func(item int64) []store.Delivery {
			return []store.Delivery{receiptOf("default", "a", store.ResultEmitted, t0.Add(-time.Hour), rev(item+1, 1))}
		}, nil},
		{"resume and compaction", func(item int64) []store.Delivery {
			return []store.Delivery{receiptOf("default", "a", store.ResultEmitted, t0.Add(-time.Hour), rev(item, 1)),
				receiptOf("default", "a", store.ResultEmitted, t0.Add(-30*time.Minute), rev(item, 2)),
				receiptOf("default", "a", store.ResultEmitted, t0.Add(time.Minute), rev(item, 3))}
		}, []string{"comeback 2"}},
		// AC: the same pane id and agent session id in another binding never count.
		{"another binding", func(item int64) []store.Delivery {
			return []store.Delivery{receiptOf("work", "a", store.ResultEmitted, t0.Add(-time.Hour), rev(item, 3))}
		}, []string{"delivery_unknown 0"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			item := f.revisedItem(3)
			f.deliver(c.receipts(item)...)
			f.session("a", true, line{"ws-A", pnpm, t0})
			f.mustRun()
			if got := f.outcomes(); !slices.Equal(got, c.want) {
				t.Errorf("outcomes = %v, want %v", got, c.want)
			}
			if n := f.count(`SELECT count FROM candidates`); n != 1 {
				t.Errorf("evidence = %d, want 1: the evidence comes first", n)
			}
		})
	}
}

// AC: a match that rests only on an unconfirmed same_as link is uncertain, not a comeback.
func TestOutcomeUncertain(t *testing.T) {
	f := newFixture(t)
	item := f.revisedItem(1)
	f.deliver(receiptOf("default", "a", store.ResultEmitted, t0.Add(-time.Hour), store.RevisionRef{Item: item, Revision: 1}))
	f.cli.reply = func(_ context.Context, prompt string) ([]byte, error) {
		m := knownPnpm.FindStringSubmatch(prompt)
		if m == nil {
			return nil, fmt.Errorf("the input has no known claim:\n%s", prompt)
		}
		return claudeReply(map[string]any{"claims": []any{replyOf(map[string]any{"rule": "always pick pnpm",
			"evidence_refs": promptRefs(prompt), "same_as": m[1]})}})
	}
	f.session("a", true, line{"ws-A", pnpm, t0})
	f.mustRun()
	if got, want := f.outcomes(), []string{"uncertain 1"}; !slices.Equal(got, want) {
		t.Errorf("outcomes = %v, want %v", got, want)
	}
}

// comebacks stores one ended session for each revision in revs, each with the pnpm mistake and
// a receipt of that revision of item, and runs a pass.
func (f *fixture) comebacks(item int64, revs ...int64) {
	f.t.Helper()
	for i, n := range revs {
		agent := fmt.Sprintf("s%d", i)
		at := t0.Add(time.Duration(i) * time.Hour)
		f.deliver(receiptOf("default", agent, store.ResultEmitted, at.Add(-time.Minute),
			store.RevisionRef{Item: item, Revision: n}))
		f.session(agent, true, line{"ws-A", pnpm, at})
	}
	f.now = t0.Add(time.Duration(len(revs)+1) * time.Hour)
	f.mustRun()
}

// AC: two comebacks on revision 2 give one change request, keyed by (item, revision 2) and
// written in the batch commit, then one draft with base revision 2, and no item change. A
// retry of the committed batch adds no outcome and no second request.
func TestComebackChangeRequest(t *testing.T) {
	f := newFixture(t)
	item := f.revisedItem(2)
	f.comebacks(item, 2, 2)
	if got, want := f.outcomes(), []string{"comeback 2", "comeback 2"}; !slices.Equal(got, want) {
		t.Fatalf("outcomes = %v, want %v", got, want)
	}
	reqs := f.count(`SELECT COUNT(*) FROM change_requests WHERE source = 'item' AND revision = 2 AND status = 'queued' AND source_id = ` +
		fmt.Sprint(item))
	if reqs != 1 || f.count(`SELECT COUNT(*) FROM change_requests`) != 1 {
		t.Fatalf("queued item requests = %d, want 1 and no other", reqs)
	}
	bs := f.batches()
	last := bs[len(bs)-1]
	c := Claim{Kind: "correction", Situation: "installing packages", Rule: "use pnpm", Scope: store.ScopeWorkspace,
		Refs: []ref{{id: "e1", obs: last.LastObservation, identity: last.LastCapture, kind: "correction"}}}
	if err := f.pass().commit(context.Background(), job{batch: last}, last.ID, []Claim{c}, nil, nil); err != nil {
		t.Fatal(err)
	}
	if n := len(f.outcomes()); n != 2 || f.count(`SELECT COUNT(*) FROM change_requests`) != 1 {
		t.Fatalf("after the retry: %d outcomes, %d requests, want 2 and 1", n, f.count(`SELECT COUNT(*) FROM change_requests`))
	}

	calls := f.cli.calls()
	f.cli.reply = claimOf("use pnpm, never npm, for every install")
	f.mustRun()
	if f.cli.calls() != calls+1 {
		t.Fatalf("calls = %d, want %d", f.cli.calls(), calls+1)
	}
	prompt := f.cli.prompts[len(f.cli.prompts)-1]
	for _, want := range []string{comebackRequest, "use pnpm (revision 2)", pnpm} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt lacks %q:\n%s", want, prompt)
		}
	}
	ps := f.proposals()
	if len(ps) != 1 || ps[0].Status != store.ProposalPending || *ps[0].ItemID != item || *ps[0].BaseRevision != 2 ||
		!strings.Contains(ps[0].Text, "never npm") || !strings.Contains(ps[0].Diff, "- installing packages: use pnpm (revision 2)") {
		t.Fatalf("proposals = %+v, want one pending draft of the item on revision 2", ps)
	}
	if n := f.count(`SELECT COUNT(*) FROM change_requests WHERE status = 'committed' AND new_proposal_id = ` + fmt.Sprint(ps[0].ID)); n != 1 {
		t.Errorf("committed requests with the draft = %d, want 1", n)
	}
	// Brain never changes the item by itself.
	if n := f.count(`SELECT COUNT(*) FROM items WHERE current_revision = 2 AND state = 'active'`); n != 1 ||
		f.count(`SELECT COUNT(*) FROM item_revisions`) != 2 {
		t.Errorf("the item changed: %d active items on revision 2, %d revisions", n, f.count(`SELECT COUNT(*) FROM item_revisions`))
	}
	f.mustRun()
	if f.cli.calls() != calls+1 || len(f.proposals()) != 1 {
		t.Errorf("a second pass: %d calls, %d proposals, want %d and 1", f.cli.calls(), len(f.proposals()), calls+1)
	}
}

// AC: a change asked on the draft of an item revision queues the same request again and gives
// one new draft of the changed draft.
func TestComebackDraftChange(t *testing.T) {
	f := newFixture(t)
	item := f.revisedItem(2)
	f.comebacks(item, 2, 2)
	f.cli.reply = claimOf("use pnpm, never npm, for every install")
	f.mustRun()
	ps := f.proposals()
	calls := f.cli.calls()
	if len(ps) != 1 {
		t.Fatalf("proposals = %+v, want the first draft", ps)
	}
	f.write(func(ctx context.Context, tx *sql.Tx) error {
		_, err := store.AskItemChange(ctx, tx, ps[0].ID, "make it a skill", store.SurfaceCLI, f.now)
		return err
	})
	f.cli.reply = claimOf("use pnpm as a skill")
	f.mustRun()
	f.mustRun()
	if f.cli.calls() != calls+1 {
		t.Fatalf("calls = %d, want %d", f.cli.calls(), calls+1)
	}
	prompt := f.cli.prompts[len(f.cli.prompts)-1]
	for _, want := range []string{"make it a skill", ps[0].Text, pnpm} {
		if !strings.Contains(prompt, want) {
			t.Errorf("redraft prompt lacks %q:\n%s", want, prompt)
		}
	}
	ps = f.proposals()
	if len(ps) != 2 || ps[0].Status != store.ProposalChanged || ps[1].Status != store.ProposalPending ||
		*ps[1].BaseRevision != 2 || !strings.Contains(ps[1].Text, "as a skill") {
		t.Fatalf("proposals = %+v, want the changed draft and one new pending draft", ps)
	}
	if n := f.count(`SELECT COUNT(*) FROM change_requests WHERE status = 'committed' AND new_proposal_id = ` + fmt.Sprint(ps[1].ID)); n != 1 ||
		f.count(`SELECT COUNT(*) FROM change_requests`) != 1 {
		t.Errorf("the one request does not hold the new draft")
	}
}

// AC: one comeback on revision 1 and one on revision 2 give no change request.
func TestComebacksOnTwoRevisions(t *testing.T) {
	f := newFixture(t)
	item := f.revisedItem(2)
	f.comebacks(item, 1, 2)
	if got, want := f.outcomes(), []string{"comeback 1", "comeback 2"}; !slices.Equal(got, want) {
		t.Fatalf("outcomes = %v, want %v", got, want)
	}
	if n := f.count(`SELECT COUNT(*) FROM change_requests`); n != 0 {
		t.Errorf("change requests = %d, want 0", n)
	}
}

// brain forget --session deletes the outcomes of the session, and an item forget the outcomes
// of the item.
func TestForgetOutcomes(t *testing.T) {
	f := newFixture(t)
	item := f.revisedItem(2)
	f.comebacks(item, 1, 2)
	f.write(func(ctx context.Context, tx *sql.Tx) error {
		var sid int64
		if err := tx.QueryRowContext(ctx, `SELECT session_id FROM outcomes WHERE revision = 1`).Scan(&sid); err != nil {
			return err
		}
		return store.ForgetSession(ctx, tx, sid)
	})
	if got, want := f.outcomes(), []string{"comeback 2"}; !slices.Equal(got, want) {
		t.Fatalf("after forget --session: outcomes = %v, want %v", got, want)
	}
	f.write(func(ctx context.Context, tx *sql.Tx) error { return store.EraseItem(ctx, tx, item, f.now) })
	if got := f.outcomes(); len(got) != 0 {
		t.Errorf("after the item forget: outcomes = %v, want none", got)
	}
}

// Two comebacks on one revision in one session give the change request too; a retry of the
// committed batch adds no outcome.
func TestComebacksInOneSession(t *testing.T) {
	f := newFixture(t)
	item := f.revisedItem(2)
	f.deliver(receiptOf("default", "a", store.ResultEmitted, t0.Add(-time.Minute), store.RevisionRef{Item: item, Revision: 2}))
	f.session("a", true, line{"ws-A", pnpm, t0}, line{"ws-A", "no, I said pnpm, not npm", t0.Add(time.Minute)})
	f.mustRun()
	if got, want := f.outcomes(), []string{"comeback 2", "comeback 2"}; !slices.Equal(got, want) {
		t.Fatalf("outcomes = %v, want %v", got, want)
	}
	if n := f.count(`SELECT COUNT(*) FROM change_requests WHERE source = 'item' AND revision = 2`); n != 1 {
		t.Errorf("item requests = %d, want 1", n)
	}
	b := f.batches()[0]
	c := Claim{Kind: "correction", Situation: "installing packages", Rule: "use pnpm", Scope: store.ScopeWorkspace,
		Refs: []ref{{id: "e1", obs: b.LastObservation, identity: b.LastCapture}}}
	if err := f.pass().commit(context.Background(), job{batch: b}, b.ID, []Claim{c}, nil, nil); err != nil {
		t.Fatal(err)
	}
	if n := len(f.outcomes()); n != 2 {
		t.Errorf("after the retry: outcomes = %d, want 2", n)
	}
}

// An active item whose candidate is a procedure comes back as a correction too.
func TestComebackOfProcedureItem(t *testing.T) {
	f := newFixture(t)
	item := f.revisedItem(1)
	f.write(func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO candidates (claim_id, scope, workspace_key, kind, harnesses, count,
			sessions, priority, first_seen, last_seen, status) SELECT claim_id, 'workspace', 'ws-A', 'procedure', '[]', 0, 0, 0,
			created, created, 'proposed' FROM items WHERE id = ?`, item)
		return err
	})
	f.deliver(receiptOf("default", "a", store.ResultEmitted, t0.Add(-time.Minute), store.RevisionRef{Item: item, Revision: 1}))
	f.session("a", true, line{"ws-A", pnpm, t0})
	f.mustRun()
	if got, want := f.outcomes(), []string{"comeback 1"}; !slices.Equal(got, want) {
		t.Errorf("outcomes = %v, want %v", got, want)
	}
}

// AC E20-S2 #266 3: brain recall delivers too. A recall on the session's receipt before the
// observation counts its revision, also when the brief did not hold the item; a later recall
// can name a newer revision than the brief.
func TestOutcomeRecall(t *testing.T) {
	rev := func(item, n int64) store.RevisionRef { return store.RevisionRef{Item: item, Revision: n} }
	for _, c := range []struct {
		name     string
		brief    func(item int64) []store.RevisionRef
		withheld bool
		at       time.Duration // the recall time from t0
		want     []string
	}{
		{"item not in the brief", func(item int64) []store.RevisionRef { return []store.RevisionRef{rev(item+1, 1)} },
			false, -time.Minute, []string{"comeback 3"}},
		{"newer than the brief", func(item int64) []store.RevisionRef { return []store.RevisionRef{rev(item, 1)} },
			false, -time.Minute, []string{"comeback 3"}},
		// Recall printed the item, so a withheld start receipt is no gap for it.
		{"withheld receipt", func(int64) []store.RevisionRef { return nil }, true, -time.Minute, []string{"comeback 3"}},
		{"recall after the observation", func(item int64) []store.RevisionRef { return []store.RevisionRef{rev(item+1, 1)} },
			false, time.Minute, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			item := f.revisedItem(3)
			result := store.ResultEmitted
			if c.withheld {
				result = store.ResultWithheld
			}
			f.deliver(receiptOf("default", "a", result, t0.Add(-time.Hour), c.brief(item)...))
			f.write(func(ctx context.Context, tx *sql.Tx) error {
				_, err := store.InsertRecall(ctx, tx, store.Recall{DeliveryID: 1, ItemRevisions: []store.RevisionRef{rev(item, 3)},
					At: t0.Add(c.at)})
				return err
			})
			f.session("a", true, line{"ws-A", pnpm, t0})
			f.mustRun()
			if got := f.outcomes(); !slices.Equal(got, c.want) {
				t.Errorf("outcomes = %v, want %v", got, c.want)
			}
		})
	}
}
