package extract

import (
	"context"
	"database/sql"
	"fmt"
	"maps"
	"strings"
	"testing"
	"time"

	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

// sandboxSteps are the release steps of M2 eval run 4 (#348) with a permission word in a step, as
// codex users often name the sandbox (#358).
const sandboxSteps = "1. Bump the version to <version>.\n2. Update the changelog.\n" +
	"3. Run lint and the tests outside the sandbox.\n4. Commit.\n5. Tag the release."

// waitingLink runs release tellings 1 and 2 with the release skill of the run 4 shape (#348):
// the model flag set and sandboxSteps, and telling 3 with " in that order" added to the rule and
// its steps, and a same_as link to the claim of the first two. The steps hold a permission
// word, so Brain holds the link for review: two candidates, no proposal.
func (f *fixture) waitingLink() {
	f.t.Helper()
	flagged := map[string]any{"touches_permission": true, "steps": sandboxSteps}
	f.cli.reply = skillReply(flagged)
	f.release(1)
	f.release(2)
	f.cli.reply = sameAsTelling(flagged, strings.Replace(sandboxSteps, "Tag", "Tag, in that order,", 1))
	f.release(3)
	if n, ps := f.count(`SELECT COUNT(*) FROM candidates`), f.proposals(); n != 2 || len(ps) != 0 {
		f.t.Fatalf("candidates %d, proposals %+v, want 2 and none: the link waits for review", n, ps)
	}
}

// sameAsTelling answers each prompt with the release skill of set and steps, " in that order"
// added to its rule, and a same_as link to the known release claim.
func sameAsTelling(set map[string]any, steps string) func(context.Context, string) ([]byte, error) {
	return func(ctx context.Context, prompt string) ([]byte, error) {
		m := knownKey(skillSituation).FindStringSubmatch(prompt)
		if m == nil {
			return nil, fmt.Errorf("the input has no known release claim:\n%s", prompt)
		}
		set := maps.Clone(set)
		set["situation"], set["rule"], set["same_as"], set["steps"] = otherSituation, "follow the release steps in that order",
			m[1], steps
		return skillReply(set)(ctx, prompt)
	}
}

// links returns the links that wait for review.
func (f *fixture) links() []Link {
	f.t.Helper()
	var ls []Link
	f.write(func(ctx context.Context, tx *sql.Tx) (err error) {
		ls, err = Links(ctx, tx)
		return err
	})
	return ls
}

// claimOf returns the claim id of the claim of situation.
func (f *fixture) claimOf(situation string) string {
	f.t.Helper()
	var id string
	f.write(func(ctx context.Context, tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `SELECT id FROM claims WHERE situation = ?`, situation).Scan(&id)
	})
	return id
}

// linkOf returns the candidate of the claim of situation.
func (f *fixture) linkOf(situation string) int64 {
	f.t.Helper()
	return int64(f.count(`SELECT id FROM candidates WHERE claim_id IN (SELECT id FROM claims WHERE situation = '` +
		situation + `')`))
}

// merge merges the link of candidate id, as brain review --merge does.
func (f *fixture) merge(id int64) (Merged, error) {
	f.t.Helper()
	var m Merged
	err := f.s.Write(context.Background(), func(tx *sql.Tx) (err error) {
		m, err = MergeLink(context.Background(), tx, id, store.DefaultGates(), f.now)
		return err
	})
	return m, err
}

// AC #358 with the run 4 shape of #348 and a permission word in the steps: Links shows the link
// that waits, with the text of both claims and the reason. A merge confirms it, and the
// evidence counts together: the 3 tellings make one skill proposal, with the steps of the newest
// telling and the permission warning, for your review, and no model call. A merge approves
// nothing. A later telling with the merged key joins the same candidate.
func TestMergeWaitingLink(t *testing.T) {
	f := newFixture(t)
	f.waitingLink()
	ls := f.links()
	if len(ls) != 1 || !ls[0].Permission || ls[0].Candidate.Count != 1 || ls[0].OtherCount != 2 ||
		ls[0].Text != "When shipping a new version: follow the release steps in that order" ||
		ls[0].OtherText != "When releasing: follow the release steps" {
		t.Fatalf("links = %+v, want the permission link of the third telling to the first two", ls)
	}
	calls, other := f.cli.calls(), ls[0].OtherCandidateID
	got, err := f.merge(ls[0].Candidate.ID)
	if err != nil || len(got.Candidates) != 1 || got.Candidates[0] != other || len(got.Superseded) != 0 {
		t.Fatalf("MergeLink = %+v, %v, want candidate %d and nothing superseded", got, err, other)
	}
	if f.cli.calls() != calls {
		t.Errorf("calls = %d, want %d: a merge makes no model call", f.cli.calls(), calls)
	}
	if n := f.count(`SELECT COUNT(*) FROM candidates`); n != 1 || f.count(`SELECT count FROM candidates`) != 3 {
		t.Errorf("candidates = %d, want one with the 3 tellings", n)
	}
	if n := f.count(`SELECT COUNT(*) FROM evidence_links WHERE via_claim IN (SELECT id FROM claims WHERE situation = '` +
		otherSituation + `')`); n != 1 {
		t.Errorf("evidence marked as merged = %d, want 1", n)
	}
	if n := f.count(`SELECT COUNT(*) FROM claims WHERE same_as_status = 'confirmed'`); n != 1 || len(f.links()) != 0 {
		t.Errorf("confirmed links = %d, links %+v, want 1 and none to review", n, f.links())
	}
	ps := f.proposals()
	if len(ps) != 1 || ps[0].Status != store.ProposalPending || !ps[0].PermissionWarning {
		t.Fatalf("proposals = %+v, want one pending proposal with the permission warning", ps)
	}
	if sk, err := store.ParseSkill(ps[0].Text); err != nil || !strings.Contains(sk.Steps, "Tag, in that order,") {
		t.Errorf("ParseSkill(draft) = %+v, %v, want the steps of the newest telling", sk, err)
	}
	if n := f.count(`SELECT COUNT(*) FROM items WHERE state = 'active'`); n != 0 {
		t.Errorf("active items = %d, want none: a merge approves nothing", n)
	}
	f.release(4)
	if n := f.count(`SELECT COUNT(*) FROM candidates`); n != 1 || f.count(`SELECT count FROM candidates`) != 4 ||
		len(f.proposals()) != 1 {
		t.Errorf("after telling 4: %d candidates, count %d, %d proposals, want 1, 4, 1", n,
			f.count(`SELECT MAX(count) FROM candidates`), len(f.proposals()))
	}
}

// AC #358: a merge proposes the text of the newest telling of the two claims: when the first
// claim has the newer draft, as after a telling in another scope, its steps.
func TestMergeNewestTelling(t *testing.T) {
	f := newFixture(t)
	f.waitingLink()
	id := f.claimOf(skillSituation)
	f.write(func(ctx context.Context, tx *sql.Tx) error {
		first, err := store.ClaimByID(ctx, tx, id)
		if err != nil {
			return err
		}
		c, _, ok, err := telling(ctx, tx, first)
		if err != nil || !ok {
			return fmt.Errorf("telling of the first claim: %v, %w, want its draft", ok, err)
		}
		c.Skill.Steps = strings.Replace(c.Skill.Steps, "Commit.", "Commit as Release <version>.", 1)
		return setDraft(ctx, tx, first.ID, c, f.now.Add(time.Minute))
	})
	if _, err := f.merge(f.linkOf(otherSituation)); err != nil {
		t.Fatal(err)
	}
	ps := f.proposals()
	if len(ps) != 1 {
		t.Fatalf("proposals = %+v, want one", ps)
	}
	if sk, err := store.ParseSkill(ps[0].Text); err != nil || !strings.Contains(sk.Steps, "Commit as Release") {
		t.Errorf("ParseSkill(draft) = %+v, %v, want the steps of the newer draft", sk, err)
	}
}

// AC #358: keep apart ends the link and records the pair. Both claims keep their evidence, and
// a later telling with the same link adds no link: the pair does not come back for review.
func TestKeepApartLink(t *testing.T) {
	f := newFixture(t)
	f.waitingLink()
	id := f.linkOf(otherSituation)
	f.write(func(ctx context.Context, tx *sql.Tx) error {
		other, _, err := KeepApart(ctx, tx, id, store.DefaultGates(), f.now)
		if err == nil && other == "" {
			err = fmt.Errorf("KeepApart = %q, want the other claim", other)
		}
		return err
	})
	if ls := f.links(); len(ls) != 0 || f.count(`SELECT COUNT(*) FROM claims_apart`) != 1 {
		t.Fatalf("links = %+v, kept-apart pairs %d, want none and 1", ls, f.count(`SELECT COUNT(*) FROM claims_apart`))
	}
	f.release(4)
	if ls := f.links(); len(ls) != 0 || f.count(`SELECT COUNT(*) FROM claims WHERE same_as IS NOT NULL`) != 0 {
		t.Errorf("links after telling 4 = %+v, want no link again", ls)
	}
	if n := f.count(`SELECT COUNT(*) FROM candidates WHERE count = 2`); n != 2 || len(f.proposals()) != 0 {
		t.Errorf("candidates of count 2 = %d, proposals %d, want 2 and none", n, len(f.proposals()))
	}
	f.write(func(ctx context.Context, tx *sql.Tx) error {
		if _, _, err := KeepApart(ctx, tx, id, store.DefaultGates(), f.now); err == nil {
			return fmt.Errorf("KeepApart of a link that is gone: no error")
		}
		return nil
	})
}

// AC #358: only a link that waits can be merged: a candidate with no link, and the candidate of
// a merged link, give an error and write nothing.
func TestMergeNoLink(t *testing.T) {
	f := newFixture(t)
	f.waitingLink()
	id, first := f.linkOf(otherSituation), f.linkOf(skillSituation)
	if _, err := f.merge(first); err == nil {
		t.Errorf("merge of candidate %d with no link: no error", first)
	}
	if _, err := f.merge(id); err != nil {
		t.Fatal(err)
	}
	if _, err := f.merge(id); err == nil {
		t.Errorf("second merge of candidate %d: no error", id)
	}
	if n := f.count(`SELECT COUNT(*) FROM proposals`); n != 1 {
		t.Errorf("proposals = %d, want 1", n)
	}
}

// AC #358: a link that Brain holds back for another reason than a permission is an unclear
// match: a correction told in other words links to a procedure, another claim kind. A merge
// counts the evidence together, and the gate of the procedure makes the proposal of the newest
// telling.
func TestMergeUnclearLink(t *testing.T) {
	f := newFixture(t)
	f.cli.reply = procedureReply(procedureSituation)
	f.release(1)
	f.release(2)
	f.cli.reply = func(_ context.Context, prompt string) ([]byte, error) {
		m := knownKey(procedureSituation).FindStringSubmatch(prompt)
		if m == nil {
			return nil, fmt.Errorf("the input has no known procedure claim:\n%s", prompt)
		}
		return claudeReply(map[string]any{"claims": []any{replyOf(map[string]any{"situation": otherSituation,
			"rule": "bump, test, commit and tag", "evidence_refs": promptRefs(prompt), "same_as": m[1]})}})
	}
	f.release(3)
	ls := f.links()
	if len(ls) != 1 || ls[0].Permission || ls[0].OtherCount != 2 {
		t.Fatalf("links = %+v, want one unclear link to the procedure of count 2", ls)
	}
	if _, err := f.merge(ls[0].Candidate.ID); err != nil {
		t.Fatal(err)
	}
	ps := f.proposals()
	if len(ps) != 1 || ps[0].Text != "When shipping a new version: bump, test, commit and tag" || ps[0].PermissionWarning {
		t.Errorf("proposals = %+v, want the text of the newest telling", ps)
	}
}

// Sol review of #360 (AC #358): a link whose candidate reached its gate on its own still waits
// for review: tellings 3 to 5 in other words give that claim its own skill proposal. A merge
// supersedes that proposal, and the merged candidate proposes the newest telling once.
func TestMergeProposedLink(t *testing.T) {
	f := newFixture(t)
	f.waitingLink()
	f.release(4)
	f.release(5)
	own := f.proposals()
	if len(own) != 1 || own[0].Status != store.ProposalPending {
		t.Fatalf("proposals = %+v, want the own proposal of the later tellings", own)
	}
	ls := f.links()
	if len(ls) != 1 || ls[0].Candidate.Status != store.CandidateProposed || ls[0].Candidate.Count != 3 {
		t.Fatalf("links = %+v, want the link of the proposed candidate", ls)
	}
	m, err := f.merge(ls[0].Candidate.ID)
	if err != nil || len(m.Superseded) != 1 || m.Superseded[0] != own[0].ID || len(m.Candidates) != 1 {
		t.Fatalf("MergeLink = %+v, %v, want proposal %d superseded", m, err, own[0].ID)
	}
	ps := f.proposals()
	if len(ps) != 2 || ps[0].Status != store.ProposalSuperseded || ps[1].Status != store.ProposalPending ||
		*ps[1].CandidateID != m.Candidates[0] || ps[1].Text != own[0].Text {
		t.Fatalf("proposals = %+v, want the own one superseded and one pending of the merged candidate", ps)
	}
	if n := f.count(`SELECT count FROM candidates`); n != 5 || f.count(`SELECT COUNT(*) FROM candidates`) != 1 {
		t.Errorf("candidates = %d of count %d, want one with the 5 tellings", f.count(`SELECT COUNT(*) FROM candidates`), n)
	}
}

// releaseCommands are the release commands that the agent ran in release session R1 of M2 eval
// run 5 (#361): 6 command records in one session.
func releaseCommands(v string) []store.CommandRecord {
	return []store.CommandRecord{command("npm version " + v + " --no-git-tag-version"), command("npm test"),
		command("git add package.json package-lock.json"), command("git commit -m 'Release " + v + "'"),
		command("git tag v" + v), command("git push origin v" + v)}
}

// agentRelease runs release session R1 of M2 eval run 5 (#361): you tell the release once (no
// signal yet), the agent runs the 6 release commands, and the extractor makes a skill claim of
// the agent's own commands, with the model flag on. Its candidate is a command chain.
func (f *fixture) agentRelease() string {
	f.t.Helper()
	f.cli.reply = skillReply(map[string]any{"touches_permission": true})
	f.commandSession("r1", turn{prompt: releaseText("0.2.0"), cmds: releaseCommands("0.2.0")})
	f.mustRun()
	cs := f.candidates()
	if len(cs) != 1 || cs[0].Kind != store.CandidateCommandChain || cs[0].Count != 6 {
		f.t.Fatalf("candidates after R1 = %+v, want one command chain of the 6 commands", cs)
	}
	return cs[0].ClaimID
}

// tellingAs answers each prompt with one skill claim of the release told as situation, with a
// same_as link to the known claim of the agent's release commands: an unclear match.
func tellingAs(situation string) func(context.Context, string) ([]byte, error) {
	return func(ctx context.Context, prompt string) ([]byte, error) {
		m := knownKey(skillSituation).FindStringSubmatch(prompt)
		if m == nil {
			return nil, fmt.Errorf("the input has no known release claim:\n%s", prompt)
		}
		return skillReply(map[string]any{"situation": situation, "rule": "follow the release steps",
			"same_as": m[1]})(ctx, prompt)
	}
}

// AC #361 with the run 5 shape: R1 gives a skill claim of the agent's own release commands,
// whose candidate is a command chain. The tellings R2 and R3, each a claim of its own words with
// a same_as link to that claim, join its candidate: your text turns it into a procedure, and the
// agent's commands stay linked but do not count, as they are no tellings. Three tellings give one
// skill proposal, and no link waits for review.
func TestTellingsJoinAgentCommands(t *testing.T) {
	f := newFixture(t)
	claim := f.agentRelease()
	f.cli.reply = tellingAs(otherSituation)
	f.release(2)
	cs := f.candidates()
	if len(cs) != 1 || cs[0].ClaimID != claim || cs[0].Kind != store.CandidateProcedure || cs[0].Count != 2 {
		t.Fatalf("candidates after R2 = %+v, want the procedure of claim %s with R1 and R2", cs, claim)
	}
	if n := f.count(`SELECT COUNT(*) FROM evidence_links WHERE sender = 'tool'`); n != 6 {
		t.Errorf("tool output evidence = %d, want the 6 commands linked, but not counted as tellings", n)
	}
	f.cli.reply = tellingAs("cutting a release")
	f.release(3)
	if ls := f.links(); len(ls) != 0 {
		t.Errorf("links = %+v, want none to review", ls)
	}
	cs = f.candidates()
	if len(cs) != 1 || cs[0].Count != 3 {
		t.Fatalf("candidates after R3 = %+v, want one with the 3 tellings", cs)
	}
	ps := f.proposals()
	if len(ps) != 1 || ps[0].Status != store.ProposalPending || *ps[0].CandidateID != cs[0].ID {
		t.Fatalf("proposals = %+v, want one skill proposal of candidate %d", ps, cs[0].ID)
	}
	if sk, err := store.ParseSkill(ps[0].Text); err != nil || sk.Name != "release" {
		t.Errorf("ParseSkill(draft) = %+v, %v, want the release skill", sk, err)
	}
}

// AC #361, a store of run 5: the link of telling R2 to the claim of the agent's release
// commands waits for review, as Brain made it before the fix. The review offers the merge, and
// the merge moves the tellings: your text turns the candidate of the agent's commands into a
// procedure. R3 then joins it, and three tellings give one skill proposal.
func TestMergeIntoAgentCommands(t *testing.T) {
	f := newFixture(t)
	claim := f.agentRelease()
	f.cli.reply = skillReply(map[string]any{"situation": otherSituation})
	f.release(2)
	told := f.claimOf(otherSituation)
	f.write(func(ctx context.Context, tx *sql.Tx) error {
		_, err := store.SameAs(ctx, tx, told, claim, store.Match{})
		return err
	})
	ls := f.links()
	if len(ls) != 1 || ls[0].Stuck != "" || ls[0].OtherCount != 6 || ls[0].Candidate.Count != 2 {
		t.Fatalf("links = %+v, want the link of R2 that a merge can move", ls)
	}
	m, err := f.merge(ls[0].Candidate.ID)
	if err != nil || len(m.Candidates) != 1 {
		t.Fatalf("MergeLink = %+v, %v, want the evidence moved to one candidate", m, err)
	}
	cs := f.candidates()
	if len(cs) != 1 || cs[0].ID != m.Candidates[0] || cs[0].ClaimID != claim || cs[0].Kind != store.CandidateProcedure ||
		cs[0].Count != 2 {
		t.Fatalf("candidates after the merge = %+v, want the procedure of claim %s with R1 and R2", cs, claim)
	}
	f.cli.reply = tellingAs("cutting a release")
	f.release(3)
	if ps := f.proposals(); len(ps) != 1 || f.count(`SELECT count FROM candidates`) != 3 {
		t.Errorf("proposals = %+v, count %d, want one skill proposal after 3 tellings", ps, f.count(`SELECT MAX(count) FROM candidates`))
	}
}

// AC #361: a link from a claim of the agent's own commands to a claim of your text cannot move
// evidence: tool output cannot count for a rule. The review offers no merge and says why, and
// a merge is refused and writes nothing, so the review never says that the evidence counts
// together when none moved.
func TestStuckLink(t *testing.T) {
	f := newFixture(t)
	f.cli.reply = ruleReply(item3Situation, item3Rule, nil)
	f.session("p1", true, line{"ws-A", item3Prompt, t0})
	f.mustRun()
	f.cli.reply = func(ctx context.Context, prompt string) ([]byte, error) {
		m := knownItem3.FindStringSubmatch(prompt)
		if m == nil {
			return nil, fmt.Errorf("the input has no known claim of item 3:\n%s", prompt)
		}
		return skillReply(map[string]any{"situation": "finishing a change", "same_as": m[1],
			"evidence_refs": toolRefs(prompt)})(ctx, prompt)
	}
	f.commandSession("s2", turn{prompt: "add the route", cmds: []store.CommandRecord{command(item3Chain)}})
	f.mustRun()
	ls := f.links()
	if len(ls) != 1 || !strings.Contains(ls[0].Stuck, "the agent's own commands and tool output, which cannot count for a correction") {
		t.Fatalf("links = %+v, want one stuck link with the reason", ls)
	}
	if _, err := f.merge(ls[0].Candidate.ID); err == nil || !strings.Contains(err.Error(), "a merge moves no evidence") {
		t.Errorf("MergeLink of a stuck link = %v, want it refused", err)
	}
	if n := f.count(`SELECT COUNT(*) FROM claims WHERE same_as_status = 'confirmed'`); n != 0 || len(f.links()) != 1 {
		t.Errorf("confirmed links = %d, links %d, want none and the link still waiting", n, len(f.links()))
	}
}

// toolRefs returns the refs of the tool lines of an extractor prompt.
func toolRefs(prompt string) []string {
	refs := []string{}
	for _, m := range toolRefID.FindAllStringSubmatch(prompt, -1) {
		refs = append(refs, m[1])
	}
	return refs
}

// Sol review of #370 (AC #361): a merge from a procedure that keeps the agent's own commands
// uncounted (your telling turned its command chain) carries them to the other candidate, still
// uncounted: the claim that gets the skill keeps the evidence that the extractor made a skill of
// the chain (store.SkillEvidence), so the exact tool path holds that chain (heldChain).
func TestMergeKeepsAgentCommands(t *testing.T) {
	f := newFixture(t)
	first := f.agentRelease()
	f.cli.reply = skillReply(nil)
	f.release(2)
	f.cli.reply = skillReply(map[string]any{"situation": otherSituation})
	f.release(3)
	other := f.claimOf(otherSituation)
	f.write(func(ctx context.Context, tx *sql.Tx) error {
		_, err := store.SameAs(ctx, tx, first, other, store.Match{})
		return err
	})
	from := f.linkOf(skillSituation)
	if n := f.count(fmt.Sprintf(`SELECT COUNT(*) FROM evidence_links WHERE candidate_id = %d AND sender = 'tool'`, from)); n != 6 {
		t.Fatalf("tool links of candidate %d = %d, want the 6 commands of R1", from, n)
	}
	m, err := f.merge(from)
	if err != nil || len(m.Candidates) != 1 {
		t.Fatalf("MergeLink = %+v, %v, want one candidate", m, err)
	}
	to := m.Candidates[0]
	if n := f.count(fmt.Sprintf(`SELECT COUNT(*) FROM evidence_links WHERE candidate_id = %d AND sender = 'tool'`, to)); n != 6 {
		t.Errorf("tool links of candidate %d = %d, want the 6 commands moved", to, n)
	}
	if cs := f.candidates(); len(cs) != 1 || cs[0].ID != to || cs[0].Count != 3 {
		t.Errorf("candidates = %+v, want only candidate %d with the 3 tellings", cs, to)
	}
	if ps := f.proposals(); len(ps) != 1 {
		t.Fatalf("proposals = %+v, want the skill draft", ps)
	}
	f.write(func(ctx context.Context, tx *sql.Tx) error {
		var obs int64
		if err := tx.QueryRowContext(ctx, `SELECT observation_id FROM evidence_links WHERE candidate_id = ? AND sender = 'tool'
			LIMIT 1`, to).Scan(&obs); err != nil {
			return err
		}
		skill, err := store.SkillEvidence(ctx, tx, obs)
		if err == nil && !skill {
			err = fmt.Errorf("SkillEvidence(%d) = false, want the command of R1 held by the skill draft", obs)
		}
		return err
	})
}
