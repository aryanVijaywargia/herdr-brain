package extract

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

// The rule of item 3 of the M2 eval rerun (2026-10-08), its restatement in proposal 9, and the
// chain that an agent ran when it followed the rule (candidate 16).
const (
	item3Situation = "finishing a change in the notes-api project"
	item3Rule      = "run npm run lint, npm test and npm run build before you say the change is done"
	proposal9Rule  = "Run npm run lint && npm test && npm run build as one chained command before you finish"
	item3Chain     = "npm run lint && npm test && npm run build"
	item3Prompt    = "no, lint, test and build it before you tell me it is done"
)

var (
	knownItem3 = regexp.MustCompile(`\[(cl_[0-9a-f]+)\] situation: ` + item3Situation + `; rule: ` + item3Rule)
	toolRefID  = regexp.MustCompile(`\[(e\d+)\] tool:`)
)

// remembered stores an item as brain remember makes it and you approve it: an active
// preference with text, in workspace key or global for nil, whose claim id has no claims row.
// It returns the claim id and the item id.
func (f *fixture) remembered(key *string, text string) (string, int64) {
	f.t.Helper()
	scope := store.ScopeGlobal
	if key != nil {
		scope = store.ScopeWorkspace
	}
	claim := fmt.Sprintf("cl_%016x", f.count(`SELECT COUNT(*) FROM items`)+1)
	var item int64
	f.write(func(ctx context.Context, tx *sql.Tx) (err error) {
		if item, err = store.InsertItem(ctx, tx, store.Item{ClaimID: claim, CurrentRevision: 1, Kind: store.KindPreference,
			Scope: scope, WorkspaceKey: key, State: store.StateActive, Created: t0}); err != nil {
			return err
		}
		base := int64(1)
		pid, err := store.InsertProposal(ctx, tx, store.Proposal{ItemID: &item, BaseRevision: &base, Text: text,
			Scope: scope, Target: "global", Diff: "+ " + text, Status: store.ProposalApproved, Created: t0})
		if err != nil {
			return err
		}
		return store.InsertItemRevision(ctx, tx, store.ItemRevision{ItemID: item, Revision: 1, Text: text, Scope: scope,
			WorkspaceKey: key, State: store.StateActive, Author: store.AuthorYou, Origin: store.OriginRemember,
			ProposalID: &pid, Created: t0})
	})
	return claim, item
}

// ruleReply is a claude result with one correction claim of rule, with each ref of the prompt
// as evidence; same_as names the claim that known finds in the prompt, when known is not nil.
func ruleReply(situation, rule string, known *regexp.Regexp) func(context.Context, string) ([]byte, error) {
	return func(_ context.Context, prompt string) ([]byte, error) {
		var id any
		if known != nil {
			m := known.FindStringSubmatch(prompt)
			if m == nil {
				return nil, fmt.Errorf("the input has no known claim for %s:\n%s", known, prompt)
			}
			id = m[1]
		}
		return claudeReply(map[string]any{"claims": []any{replyOf(map[string]any{"situation": situation,
			"rule": rule, "evidence_refs": promptRefs(prompt), "same_as": id})}})
	}
}

// item3 runs the two sessions of the repeated prompt P that made proposal 3 (candidate 4) of
// the M2 eval rerun, and approves it when approve is true, as brain review does: item 3. It
// returns the claim id.
func (f *fixture) item3(approve bool) string {
	f.t.Helper()
	f.cli.reply = ruleReply(item3Situation, item3Rule, nil)
	f.session("p1", true, line{"ws-A", item3Prompt, t0})
	f.session("p2", true, line{"ws-A", item3Prompt, t0.Add(time.Minute)})
	f.mustRun()
	ps := f.proposals()
	if len(ps) != 1 {
		f.t.Fatalf("proposals = %+v, want proposal 3", ps)
	}
	var claim string
	f.write(func(ctx context.Context, tx *sql.Tx) error {
		if err := tx.QueryRowContext(ctx, `SELECT claim_id FROM candidates`).Scan(&claim); err != nil || !approve {
			return err
		}
		if err := store.DecideProposal(ctx, tx, ps[0].ID, store.ProposalApproved, store.SurfaceCLI, t0); err != nil {
			return err
		}
		for _, table := range []string{"items", "item_revisions"} {
			if _, err := tx.ExecContext(ctx, `UPDATE `+table+` SET state = ?`, store.StateActive); err != nil {
				return err
			}
		}
		return nil
	})
	return claim
}

// AC 3 (#312): the claims of brain remember items, workspace and global, are known claims of
// the extractor input, by claim id, with the item text as the rule.
func TestRememberClaimsInPrompt(t *testing.T) {
	f := newFixture(t)
	a, b := "ws-A", "ws-B"
	inA, _ := f.remembered(&a, logRule)
	global, _ := f.remembered(nil, "Write commit messages in the imperative")
	inB, _ := f.remembered(&b, "Use bun in this repo")
	f.session("s", true, line{"ws-A", pnpm, t0})
	f.mustRun()
	if f.cli.calls() != 1 {
		t.Fatalf("calls = %d, want 1", f.cli.calls())
	}
	_, known, ok := strings.Cut(f.cli.prompts[0], "<known_claims>\n")
	known, _, _ = strings.Cut(known, "</known_claims>")
	if !ok {
		t.Fatalf("the prompt has no known claims:\n%s", f.cli.prompts[0])
	}
	for _, want := range []string{"[" + inA + "] rule: " + logRule + "\n",
		"[" + global + "] rule: Write commit messages in the imperative\n"} {
		if !strings.Contains(known, want) {
			t.Errorf("the known claims have no %q:\n%s", want, known)
		}
	}
	if strings.Contains(known, inB) {
		t.Errorf("the known claims of ws-A have the claim of ws-B:\n%s", known)
	}
}

// AC 1 and 4 (#312), proposal 4 of the M2 eval rerun: the agent saved the C2 rule with brain
// remember (item 2), and 20 minutes later the extractor restates it as a correction, with
// same_as to the claim of item 2. The evidence has no place: no candidate and no proposal in 2
// sessions, over the gate of 2, and the claim stays known. The link stays for review, so a
// correction in a session that got item 2 is an uncertain comeback (§5.4).
func TestRestatedRememberItem(t *testing.T) {
	f := newFixture(t)
	a := "ws-A"
	claim, item := f.remembered(&a, logRule)
	f.cli.reply = ruleReply(c2Situation, logRule,
		regexp.MustCompile(`\[(cl_[0-9a-f]+)\] rule: `+regexp.QuoteMeta(logRule)))
	for i, agent := range []string{"c2", "c3"} {
		at := t0.Add(time.Duration(20+i) * time.Minute)
		f.deliver(receiptOf("default", agent, store.ResultEmitted, at.Add(-time.Minute), store.RevisionRef{Item: item, Revision: 1}))
		f.session(agent, true, line{"ws-A", logText, at})
	}
	f.mustRun()
	if f.cli.calls() != 2 {
		t.Fatalf("calls = %d, want 2", f.cli.calls())
	}
	for _, b := range f.batches() {
		if b.Status != store.BatchCommitted {
			t.Errorf("batch %+v, want it committed", b)
		}
	}
	if n := f.count(`SELECT COUNT(*) FROM candidates`); n != 0 {
		t.Errorf("candidates = %d, want none", n)
	}
	if n := f.count(`SELECT COUNT(*) FROM proposals WHERE status = 'pending'`); n != 0 {
		t.Errorf("pending proposals = %d, want none", n)
	}
	if n := f.count(`SELECT COUNT(*) FROM claims WHERE same_as = '` + claim + `' AND same_as_status = 'proposed'`); n != 1 {
		t.Errorf("proposed links to the claim of item 2 = %d, want 1", n)
	}
	if got, want := f.outcomes(), []string{"uncertain 1", "uncertain 1"}; !slices.Equal(got, want) {
		t.Errorf("outcomes = %v, want %v", got, want)
	}
}

// AC 1 and 4 (#312), proposal 9 of the M2 eval rerun: a later claim restates item 3 in other
// words, with same_as to its claim. The new evidence goes to candidate 4, marked as merged, so
// the evidence view of proposal 3 shows it. No candidate and no proposal of the reworded claim
// come in 2 sessions, over the gate of 2. The same holds while proposal 3 waits for review.
func TestRestatedClaim(t *testing.T) {
	for name, approve := range map[string]bool{"proposed": false, "approved": true} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			claim := f.item3(approve)
			f.cli.reply = ruleReply(item3Situation, proposal9Rule, knownItem3)
			f.session("s9", true, line{"ws-A", item3Prompt + " again", t0.Add(time.Hour)})
			f.session("s10", true, line{"ws-A", "no, chain lint, test and build before you finish", t0.Add(2 * time.Hour)})
			f.mustRun()
			if f.cli.calls() != 4 {
				t.Fatalf("calls = %d, want 4", f.cli.calls())
			}
			if ps := f.proposals(); len(ps) != 1 {
				t.Errorf("proposals = %+v, want only proposal 3", ps)
			}
			cs := f.candidates()
			if len(cs) != 1 || cs[0].ClaimID != claim || cs[0].Count != 4 {
				t.Fatalf("candidates = %+v, want only candidate 4 with count 4", cs)
			}
			if n := f.count(`SELECT COUNT(*) FROM evidence_links e JOIN claims k ON k.id = e.via_claim
				WHERE k.same_as = '` + claim + `' AND k.same_as_status = 'proposed'`); n != 2 {
				t.Errorf("merged evidence = %d, want 2", n)
			}
			var excerpts []store.Excerpt
			f.write(func(ctx context.Context, tx *sql.Tx) (err error) {
				excerpts, err = store.CandidateExcerpts(ctx, tx, cs[0].ID)
				return err
			})
			if len(excerpts) != 4 {
				t.Errorf("the evidence view of proposal 3 has %d excerpts, want 4", len(excerpts))
			}
		})
	}
}

// AC 5 (#312), candidates 16 and 18 of the M2 eval rerun: in 3 sessions where the agent follows
// item 3, it runs the chain of the rule, and the extractor gives a procedure claim over the
// command records alone. With no same_as, that is a command chain candidate, and at its gate of
// 3 sessions a preference proposal: the kind of item 3. With same_as to the claim of item 3, the
// claim restates it: no candidate and no proposal.
func TestRestatedCommandChain(t *testing.T) {
	for name, sameAs := range map[string]bool{"no same_as": false, "same_as item 3": true} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			claim := f.item3(true)
			f.cli.reply = func(_ context.Context, prompt string) ([]byte, error) {
				refs := []string{}
				for _, m := range toolRefID.FindAllStringSubmatch(prompt, -1) {
					refs = append(refs, m[1])
				}
				var id any
				if sameAs {
					id = claim
				}
				return claudeReply(map[string]any{"claims": []any{replyOf(map[string]any{"kind": "procedure",
					"situation": item3Situation, "rule": "run " + item3Chain + " before you finish",
					"evidence_refs": refs, "same_as": id})}})
			}
			for i := range 3 {
				f.commandSession(fmt.Sprintf("t%d", i), turn{"add the export endpoint",
					[]store.CommandRecord{command(item3Chain)}})
			}
			f.mustRun()
			want, chains := 1, 1
			if sameAs {
				want, chains = 0, 0
			}
			if n := f.count(`SELECT COUNT(*) FROM proposals p JOIN items i ON i.id = p.item_id
				WHERE p.status = 'pending' AND i.kind = 'preference'`); n != want {
				t.Errorf("pending preference proposals = %d, want %d", n, want)
			}
			if n := f.count(`SELECT COUNT(*) FROM candidates WHERE kind = 'command_chain'`); n != chains {
				t.Errorf("command chain candidates = %d, want %d", n, chains)
			}
			if n := f.count(`SELECT count FROM candidates WHERE claim_id = '` + claim + `'`); n != 2 {
				t.Errorf("candidate 4 count = %d, want 2: tool output cannot support a correction", n)
			}
		})
	}
}

// AC 2 (#312), changed by #365: a same_as link to a rejected claim keeps the rejection (§7.2):
// the new claim is the same request in other words, so it stays rejected too. It makes no
// candidate of its own; its evidence goes to the candidate of the rejected claim, and this
// rejected claim has none, so it has no place. A link to an open candidate merges as before
// (TestSameAsIsProposed).
func TestSameAsRejectedClaim(t *testing.T) {
	f := newFixture(t)
	f.rejected("ws-A", "use pnpm")
	f.cli.reply = ruleReply("installing packages", "always pick pnpm",
		regexp.MustCompile(`\[(cl_[0-9a-f]+)\] situation: rejected; rule: use pnpm`))
	f.session("a", true, line{"ws-A", pnpm, t0})
	f.mustRun()
	if n := f.count(`SELECT COUNT(*) FROM rejections WHERE reopened IS NULL`); n != 1 {
		t.Errorf("rejections that hold = %d, want 1", n)
	}
	if n := f.count(`SELECT COUNT(*) FROM claims WHERE same_as IS NOT NULL AND same_as_status = 'proposed'`); n != 1 {
		t.Errorf("proposed links = %d, want 1", n)
	}
	if cs := f.candidates(); len(cs) != 0 || len(f.proposals()) != 0 {
		t.Errorf("candidates = %+v, proposals %+v, want none: the new claim stays rejected", cs, f.proposals())
	}
}

// A restatement of a forgotten claim adds nothing (§6.7): no evidence, no candidate, no
// proposal, as for new evidence of the same claim id (#312).
func TestRestatedForgottenClaim(t *testing.T) {
	f := newFixture(t)
	claim := f.item3(true)
	f.write(func(ctx context.Context, tx *sql.Tx) error {
		return store.InsertTombstone(ctx, tx, store.Tombstone{ClaimID: claim, SourceKeys: []string{}, ForgottenAt: t0})
	})
	f.cli.reply = ruleReply(item3Situation, proposal9Rule, knownItem3)
	f.session("s9", true, line{"ws-A", item3Prompt + " again", t0.Add(time.Hour)})
	f.mustRun()
	if f.cli.calls() != 3 {
		t.Fatalf("calls = %d, want 3", f.cli.calls())
	}
	cs := f.candidates()
	if len(cs) != 1 || cs[0].Count != 2 || len(f.proposals()) != 1 {
		t.Errorf("candidates = %+v, %d proposals, want only candidate 4 with count 2 and proposal 3", cs, len(f.proposals()))
	}
}

// A same_as link to the claim of a brain remember item that is a permission or a safety rule
// holds nothing (§7.2): the item text counts as its key, so the restatement keeps its own
// candidate and its proposal goes to review, for an approved item and for a draft (#312 review).
func TestRestatedRememberedPermissionRule(t *testing.T) {
	const rule = "Never run commands outside the sandbox"
	for name, draft := range map[string]bool{"approved": false, "waits for review": true} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			a := "ws-A"
			_, item := f.remembered(&a, rule)
			if draft {
				f.write(func(ctx context.Context, tx *sql.Tx) error {
					for _, q := range []string{`UPDATE items SET state = 'draft' WHERE id = ?`,
						`UPDATE item_revisions SET state = 'draft' WHERE item_id = ?`,
						`UPDATE proposals SET status = 'pending' WHERE item_id = ?`} {
						if _, err := tx.ExecContext(ctx, q, item); err != nil {
							return err
						}
					}
					return nil
				})
			}
			f.cli.reply = ruleReply("running shell commands", "execute commands only inside the isolated environment",
				regexp.MustCompile(`\[(cl_[0-9a-f]+)\] rule: `+rule))
			f.session("s1", true, line{"ws-A", "no, keep the commands inside the isolated environment", t0})
			f.session("s2", true, line{"ws-A", "no, keep the commands inside the isolated environment again", t0.Add(time.Minute)})
			f.mustRun()
			if cs := f.candidates(); len(cs) != 1 || cs[0].Count != 2 {
				t.Errorf("candidates = %+v, want the restatement's own with count 2", cs)
			}
			if n := f.count(`SELECT COUNT(*) FROM proposals WHERE candidate_id IS NOT NULL AND status = 'pending'`); n != 1 {
				t.Errorf("pending proposals of candidates = %d, want 1 for review", n)
			}
		})
	}
}
