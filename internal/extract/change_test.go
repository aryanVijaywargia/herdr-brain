package extract

import (
	"context"
	"database/sql"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aryanVijaywargia/herdr-brain/internal/fingerprint"
	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

// proposed runs a pass over two ended sessions with the pnpm correction and returns the one
// proposal that it makes.
func (f *fixture) proposed() store.Proposal {
	f.t.Helper()
	f.session("z", true, line{"ws-A", pnpm, t0})
	f.session("a", true, line{"ws-A", pnpm, t0.Add(time.Minute)})
	f.mustRun()
	ps := f.proposals()
	if len(ps) != 1 {
		f.t.Fatalf("proposals = %+v, want 1", ps)
	}
	return ps[0]
}

func (f *fixture) proposals() []store.Proposal {
	f.t.Helper()
	var ps []store.Proposal
	f.write(func(ctx context.Context, tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT id FROM proposals ORDER BY id`)
		if err != nil {
			return err
		}
		var ids []int64
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				return err
			}
			ids = append(ids, id)
		}
		if err := rows.Close(); err != nil {
			return err
		}
		for _, id := range ids {
			p, err := store.ProposalByID(ctx, tx, id)
			if err != nil {
				return err
			}
			ps = append(ps, p)
		}
		return nil
	})
	return ps
}

// ask asks for a change of proposal p with text, as brain review --change does.
func (f *fixture) ask(p store.Proposal, text string) store.ChangeRequest {
	f.t.Helper()
	var cr store.ChangeRequest
	f.write(func(ctx context.Context, tx *sql.Tx) error {
		c, err := store.CandidateByID(ctx, tx, *p.CandidateID)
		if err != nil {
			return err
		}
		cr, err = store.AskChange(ctx, tx, p.ID, c, text, store.SurfacePane, f.now)
		return err
	})
	return cr
}

func (f *fixture) request(id int64) store.ChangeRequest {
	f.t.Helper()
	var cr store.ChangeRequest
	f.write(func(ctx context.Context, tx *sql.Tx) (err error) {
		cr, err = store.ChangeRequestByID(ctx, tx, id)
		return err
	})
	return cr
}

// claimItem stores an active workspace item of claim key (situation, rule) in ws-A and
// returns its claim id and item id.
func (f *fixture) claimItem(situation, rule string) (string, int64) {
	f.t.Helper()
	var claim string
	var item int64
	key := "ws-A"
	f.write(func(ctx context.Context, tx *sql.Tx) (err error) {
		if claim, err = store.ClaimID(ctx, tx, store.ClaimKey{Situation: situation, Rule: rule}, t0); err != nil {
			return err
		}
		if item, err = store.InsertItem(ctx, tx, store.Item{ClaimID: claim, CurrentRevision: 1, Kind: store.KindPreference,
			Scope: store.ScopeWorkspace, WorkspaceKey: &key, State: store.StateActive, Created: t0}); err != nil {
			return err
		}
		return store.InsertItemRevision(ctx, tx, store.ItemRevision{ItemID: item, Revision: 1, Text: situation + ": " + rule,
			Scope: store.ScopeWorkspace, WorkspaceKey: &key, State: store.StateActive, Author: store.AuthorYou,
			Origin: store.OriginRemember, Created: t0})
	})
	return claim, item
}

// conflictOf is claimOf with conflicts_with set to claim.
func conflictOf(rule, claim string) func(context.Context, string) ([]byte, error) {
	return func(ctx context.Context, prompt string) ([]byte, error) {
		out, err := claimOf(rule)(ctx, prompt)
		return []byte(strings.Replace(string(out), `"conflicts_with":null`, `"conflicts_with":"`+claim+`"`, 1)), err
	}
}

// The proposal stores the fingerprints of its prompt evidence for E14, each once, and no
// excerpt text (§7.4, E14-S1).
func TestTriggerPrompts(t *testing.T) {
	f := newFixture(t)
	p := f.proposed()
	if want := []string{fingerprint.Prompt(pnpm)}; !slices.Equal(p.TriggerPrompts, want) {
		t.Errorf("trigger prompts = %v, want %v", p.TriggerPrompts, want)
	}
	if n := f.count(`SELECT COUNT(*) FROM proposals WHERE text LIKE '%instead of npm%' OR diff LIKE '%instead of npm%'`); n != 0 {
		t.Errorf("proposals with excerpt text = %d, want 0", n)
	}
}

// A trigger prompt is the fingerprint of the full prompt in the prompt index, not of its
// stored excerpt: a long prompt and one with blank lines match their own trigger (E14-S1).
func TestTriggerPromptsFullText(t *testing.T) {
	f := newFixture(t)
	full := pnpm + "\n\n" + strings.Repeat("and keep the lockfile ", 30)
	f.full = map[string]string{pnpm: full}
	f.session("z", true, line{"ws-A", pnpm, t0})
	f.session("a", true, line{"ws-A", pnpm, t0.Add(time.Minute)})
	f.mustRun()
	ps := f.proposals()
	if len(ps) != 1 {
		t.Fatalf("proposals = %+v, want 1", ps)
	}
	if want := []string{fingerprint.Prompt(full)}; !slices.Equal(ps[0].TriggerPrompts, want) {
		t.Errorf("trigger prompts = %v, want %v, the hash of the full prompt", ps[0].TriggerPrompts, want)
	}
	f.write(func(ctx context.Context, tx *sql.Tx) error {
		ids, err := store.MatchPrompt(ctx, tx, fingerprint.Prompt(full), "ws-A")
		if err != nil || !slices.Equal(ids, []int64{ps[0].ID}) {
			t.Errorf("MatchPrompt(full) = %v, %v, want [%d]", ids, err, ps[0].ID)
		}
		return nil
	})
}

// conflicts_with names a claim; the proposal stores the active items of that claim in its
// scope. A claim with no active item gives no conflict [NEW].
func TestConflictsWith(t *testing.T) {
	f := newFixture(t)
	claim, item := f.claimItem("installing packages", "use npm")
	f.cli.reply = conflictOf("use pnpm", claim)
	if p := f.proposed(); !slices.Equal(p.ConflictsWith, []int64{item}) {
		t.Errorf("conflicts = %v, want [%d]", p.ConflictsWith, item)
	}

	g := newFixture(t)
	claim, item = g.claimItem("installing packages", "use npm")
	g.write(func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE items SET state = 'retired' WHERE id = ?`, item)
		return err
	})
	// A rejection keeps the claim in the input, with no active item.
	g.write(func(ctx context.Context, tx *sql.Tx) error {
		_, err := store.Reject(ctx, tx, store.Candidate{ClaimID: claim, Scope: store.ScopeWorkspace, WorkspaceKey: "ws-A"}, "", t0)
		return err
	})
	g.cli.reply = conflictOf("use pnpm", claim)
	if p := g.proposed(); len(p.ConflictsWith) != 0 {
		t.Errorf("conflicts = %v, want none: the claim has no active item", p.ConflictsWith)
	}
}

// AC: a change asked after the session ended, with no new observation, gives one draft to
// review. A retry after the commit gives no second draft. Nothing becomes active (S56).
func TestChangeRequestDraft(t *testing.T) {
	f := newFixture(t)
	p := f.proposed()
	calls := f.cli.calls()
	cr := f.ask(p, "say pnpm for every install")
	if cr.Revision != *p.BaseRevision || cr.Status != store.ChangeQueued {
		t.Fatalf("request %+v, want queued for revision %d", cr, *p.BaseRevision)
	}
	f.cli.reply = claimOf("use pnpm for every install")
	f.now = f.now.Add(time.Minute)
	f.mustRun()
	if f.cli.calls() != calls+1 {
		t.Fatalf("calls = %d, want %d", f.cli.calls(), calls+1)
	}
	prompt := f.cli.prompts[len(f.cli.prompts)-1]
	if !strings.Contains(prompt, "say pnpm for every install") || !strings.Contains(prompt, p.Text) ||
		!strings.Contains(prompt, pnpm) {
		t.Errorf("prompt lacks the request, the proposal or the evidence:\n%s", prompt)
	}
	ps := f.proposals()
	if len(ps) != 2 || ps[0].Status != store.ProposalChanged || ps[1].Status != store.ProposalPending ||
		*ps[1].CandidateID != *p.CandidateID || *ps[1].ItemID != *p.ItemID || *ps[1].BaseRevision != *p.BaseRevision ||
		!strings.Contains(ps[1].Text, "every install") {
		t.Fatalf("proposals = %+v, want the changed original and one new pending draft", ps)
	}
	got := f.request(cr.ID)
	if got.Status != store.ChangeCommitted || got.NewProposalID == nil || *got.NewProposalID != ps[1].ID || got.ModelCalls != 1 {
		t.Errorf("request = %+v, want committed with the new draft", got)
	}
	// A change request moves no watermark and adds no evidence.
	links := f.count(`SELECT COUNT(*) FROM evidence_links`)
	// A retry of the commit and a new pass give no second draft and no call.
	j := job{change: &change{req: cr}}
	if err := f.pass().commitChange(context.Background(), j, []Claim{{Situation: "s", Rule: "r"}}, nil); err != nil {
		t.Fatal(err)
	}
	f.mustRun()
	if n := len(f.proposals()); n != 2 || f.cli.calls() != calls+1 || f.count(`SELECT COUNT(*) FROM evidence_links`) != links {
		t.Errorf("after the retry: %d proposals, %d calls, want 2 and %d", n, f.cli.calls(), calls+1)
	}
	if n := f.count(`SELECT COUNT(*) FROM items WHERE state != 'draft'`); n != 0 {
		t.Errorf("items not in draft = %d, want 0: nothing is active without review", n)
	}
}

// AC: a reject or a forget during the call gives no draft.
func TestChangeRequestStaleDuringCall(t *testing.T) {
	for _, c := range []struct {
		name string
		stop func(ctx context.Context, tx *sql.Tx, p store.Proposal, sid int64) error
		text bool // the request text stays
	}{
		{"reject", func(ctx context.Context, tx *sql.Tx, p store.Proposal, _ int64) error {
			cand, err := store.CandidateByID(ctx, tx, *p.CandidateID)
			if err != nil {
				return err
			}
			_, err = store.Reject(ctx, tx, cand, "no", t0)
			return err
		}, true},
		{"forget session", func(ctx context.Context, tx *sql.Tx, _ store.Proposal, sid int64) error {
			return store.ForgetSession(ctx, tx, sid)
		}, false},
		{"forget item", func(ctx context.Context, tx *sql.Tx, p store.Proposal, _ int64) error {
			it, err := store.ItemByID(ctx, tx, *p.ItemID)
			if err != nil {
				return err
			}
			if err := store.EraseItem(ctx, tx, it.ID, t0); err != nil {
				return err
			}
			return store.InsertTombstone(ctx, tx, store.Tombstone{ClaimID: it.ClaimID, SourceKeys: []string{}, ForgottenAt: t0})
		}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			p := f.proposed()
			var sid int64
			f.write(func(ctx context.Context, tx *sql.Tx) error {
				return tx.QueryRowContext(ctx, `SELECT MAX(session_id) FROM evidence_links`).Scan(&sid)
			})
			cr := f.ask(p, "say pnpm for every install")
			f.cli.reply = func(ctx context.Context, prompt string) ([]byte, error) {
				f.write(func(ctx context.Context, tx *sql.Tx) error { return c.stop(ctx, tx, p, sid) })
				return claimOf("use pnpm for every install")(ctx, prompt)
			}
			f.mustRun()
			if n := len(f.proposals()); n != 1 {
				t.Errorf("proposals = %d, want 1: no draft", n)
			}
			if got := f.request(cr.ID); got.Status != store.ChangeDiscarded || got.NewProposalID != nil {
				t.Errorf("request = %+v, want discarded", got)
			}
			if n := f.count(`SELECT COUNT(*) FROM proposals WHERE change_request IS NOT NULL`); (n == 1) != c.text {
				t.Errorf("proposals with request text = %d", n)
			}
		})
	}
}

// A change request counts under the daily call cap, and waits when it is full.
func TestChangeRequestCallCap(t *testing.T) {
	f := newFixture(t)
	p := f.proposed()
	cr := f.ask(p, "say pnpm for every install")
	pass := f.pass()
	pass.Caps = Caps{Calls: f.cli.calls(), Proposals: 5, InputBytes: 32 << 10}
	calls := f.cli.calls()
	if err := pass.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.cli.calls() != calls || f.request(cr.ID).Status != store.ChangeQueued {
		t.Fatalf("calls = %d, request %+v: want no call under the full cap", f.cli.calls(), f.request(cr.ID))
	}
	f.mustRun()
	if n := f.count(`SELECT COUNT(*) FROM extraction_calls WHERE change_request_id IS NOT NULL`); n != 1 {
		t.Errorf("change request calls = %d, want 1", n)
	}
}

// A reply with more than one claim is invalid; the request waits for a retry. A second claim
// that check drops counts too (#310).
func TestChangeRequestInvalid(t *testing.T) {
	for name, dropped := range map[string]bool{"two claims": false, "a dropped second claim": true} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			p := f.proposed()
			cr := f.ask(p, "split it")
			f.cli.reply = func(_ context.Context, prompt string) ([]byte, error) {
				refs := []string{refID.FindStringSubmatch(prompt)[1]}
				claim := func(evidence []string) map[string]any {
					return map[string]any{"kind": "correction", "situation": "s", "rule": "r", "scope_guess": "workspace",
						"evidence_refs": evidence, "same_as": nil, "conflicts_with": nil, "touches_permission": false,
						"expected_benefit": ""}
				}
				second := claim(refs)
				if dropped {
					second = claim([]string{})
				}
				return claudeReply(map[string]any{"claims": []any{claim(refs), second}})
			}
			f.mustRun()
			if got := f.request(cr.ID); got.Status != store.ChangeInvalid || len(f.proposals()) != 1 {
				t.Errorf("request %+v, want invalid and no draft", got)
			}
		})
	}
}

// A global proposal has evidence from a workspace. With extraction off there, its change
// request sends nothing and waits; with extraction on again, it is drafted (§7.3).
func TestChangeRequestWorkspaceOff(t *testing.T) {
	f := newFixture(t)
	f.cli.reply = func(ctx context.Context, prompt string) ([]byte, error) {
		out, err := claimOf("use pnpm")(ctx, prompt)
		return []byte(strings.Replace(string(out), `"scope_guess":"workspace"`, `"scope_guess":"global"`, 1)), err
	}
	p := f.proposed()
	if p.Scope != store.ScopeGlobal {
		t.Fatalf("proposal scope = %s, want global", p.Scope)
	}
	f.write(func(ctx context.Context, tx *sql.Tx) error { return store.SetExtraction(ctx, tx, "ws-A", false, f.now) })
	cr := f.ask(p, "say pnpm for every install")
	calls := f.cli.calls()
	f.mustRun()
	if f.cli.calls() != calls || f.request(cr.ID).Status != store.ChangeQueued {
		t.Fatalf("calls %d, request %+v: want no call and the request waiting", f.cli.calls(), f.request(cr.ID))
	}
	f.write(func(ctx context.Context, tx *sql.Tx) error { return store.SetExtraction(ctx, tx, "ws-A", true, f.now) })
	f.mustRun()
	if f.cli.calls() != calls+1 || f.request(cr.ID).Status != store.ChangeCommitted {
		t.Errorf("calls %d, request %+v: want one call and a draft", f.cli.calls(), f.request(cr.ID))
	}
}

// Two passes on one change request: a second pass leaves a running attempt alone. After a
// crash (an active attempt older than changeStale), a later pass starts it again; the old
// attempt then commits nothing, so one draft comes.
func TestChangeRequestOneAttempt(t *testing.T) {
	f := newFixture(t)
	p := f.proposed()
	cr := f.ask(p, "say pnpm for every install")
	calls := f.cli.calls()
	first := true
	f.cli.reply = func(ctx context.Context, prompt string) ([]byte, error) {
		if first {
			first = false
			f.mustRun() // a second pass while the first call runs
			if f.cli.calls() != calls+1 {
				t.Errorf("calls = %d, want %d: the second pass called again", f.cli.calls(), calls+1)
			}
			f.now = f.now.Add(changeStale + time.Minute)
			f.mustRun() // a pass after the stale time takes the request over and commits
		}
		return claimOf("use pnpm for every install")(ctx, prompt)
	}
	f.mustRun()
	if n := len(f.proposals()); n != 2 {
		t.Errorf("proposals = %d, want the original and one draft", n)
	}
	if got := f.request(cr.ID); got.Status != store.ChangeCommitted || got.ModelCalls != 2 {
		t.Errorf("request = %+v, want committed after 2 calls", got)
	}
}

// The commit of an old attempt adds nothing while a newer attempt of the request is active.
func TestChangeRequestOldAttempt(t *testing.T) {
	f := newFixture(t)
	p := f.proposed()
	cr := f.ask(p, "say pnpm for every install")
	var old int
	f.write(func(ctx context.Context, tx *sql.Tx) (err error) {
		if old, err = store.StartChange(ctx, tx, cr.ID, f.now, f.now); err != nil {
			return err
		}
		_, err = store.StartChange(ctx, tx, cr.ID, f.now, f.now.Add(time.Hour)) // the crash take-over
		return err
	})
	j := job{change: &change{req: cr, attempt: old}}
	if err := f.pass().commitChange(context.Background(), j, []Claim{{Situation: "s", Rule: "r"}}, nil); err != nil {
		t.Fatal(err)
	}
	if got := f.request(cr.ID); got.Status != store.ChangeActive || got.ModelCalls != 2 || len(f.proposals()) != 1 {
		t.Errorf("request = %+v, %d proposals; want the newer attempt active and no draft", got, len(f.proposals()))
	}
}
