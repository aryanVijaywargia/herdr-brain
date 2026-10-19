package extract

import (
	"context"
	"database/sql"
	"fmt"
	"maps"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

// The claim keys of the release skill of skillReply, of the release procedure of the M2 eval
// rerun (candidate 8), and of a later telling of the same procedure in other words.
const (
	skillSituation     = "releasing"
	procedureSituation = "preparing a project release"
	otherSituation     = "shipping a new version"
)

// knownKey finds the claim id of the known claim with situation in an extractor prompt.
func knownKey(situation string) *regexp.Regexp {
	return regexp.MustCompile(`\[(cl_[0-9a-f]+)\] situation: ` + situation + `;`)
}

// sameAsSkill answers each prompt with one release skill claim of otherSituation whose same_as
// is the known claim that known finds: the same procedure in other words.
func sameAsSkill(known *regexp.Regexp) func(context.Context, string) ([]byte, error) {
	return func(ctx context.Context, prompt string) ([]byte, error) {
		m := known.FindStringSubmatch(prompt)
		if m == nil {
			return nil, fmt.Errorf("the input has no known claim for %s:\n%s", known, prompt)
		}
		return skillReply(map[string]any{"situation": otherSituation, "rule": "use the release steps",
			"same_as": m[1]})(ctx, prompt)
	}
}

// release stores the ended session of release telling n of the M2 eval rerun in ws-A, codex
// and claude in turn, with a new version each time, and runs a pass.
func (f *fixture) release(n int) {
	f.t.Helper()
	harness := []string{"codex", "claude"}[(n-1)%2]
	f.harnessSession(harness, fmt.Sprintf("r%d", n), true,
		line{"ws-A", releaseText(fmt.Sprintf("0.%d.0", n+1)), t0.Add(time.Duration(n) * time.Minute)})
	f.mustRun()
}

// approveSkill approves pending skill draft p as brain review does: its item becomes active at
// revision 2 with the text of p.
func (f *fixture) approveSkill(p store.Proposal) {
	f.t.Helper()
	key := "ws-A"
	f.write(func(ctx context.Context, tx *sql.Tx) error {
		if err := store.DecideProposal(ctx, tx, p.ID, store.ProposalApproved, store.SurfaceCLI, t0); err != nil {
			return err
		}
		_, err := store.WriteRevision(ctx, tx, *p.BaseRevision, store.ItemRevision{ItemID: *p.ItemID, Text: p.Text,
			Scope: store.ScopeWorkspace, WorkspaceKey: &key, State: store.StateActive, Author: store.AuthorYou,
			Origin: store.OriginReview, ProposalID: &p.ID, Created: t0})
		return err
	})
}

// skillDraft runs the release tellings R1 to R3 with a skill claim each time, and returns the
// one skill draft.
func (f *fixture) skillDraft() store.Proposal {
	f.t.Helper()
	f.cli.reply = skillReply(nil)
	for n := 1; n <= 3; n++ {
		f.release(n)
	}
	ps := f.proposals()
	if len(ps) != 1 {
		f.t.Fatalf("proposals = %+v, want the skill draft", ps)
	}
	return ps[0]
}

// AC 1 (#313): the release of the M2 eval rerun, one telling in each session, codex and claude
// in turn: the extractor sees one telling at a time and returns kind skill, and Brain's
// procedure gate counts the repeats. The third telling gives one workspace skill draft, though
// the model guessed global, and no preference. The fourth adds evidence and no second proposal.
func TestProcedureSkillAcrossSessions(t *testing.T) {
	f := newFixture(t)
	f.cli.reply = skillReply(nil)
	for n := 1; n <= 4; n++ {
		f.release(n)
		wantCount, wantProposals := []int{0, 2, 3, 4}[n-1], []int{0, 0, 1, 1}[n-1]
		if got := f.count(`SELECT COALESCE(MAX(count), 0) FROM candidates`); got != wantCount {
			t.Errorf("count after telling %d = %d, want %d", n, got, wantCount)
		}
		if got := len(f.proposals()); got != wantProposals {
			t.Errorf("proposals after telling %d = %d, want %d", n, got, wantProposals)
		}
	}
	ps := f.proposals()
	if len(ps) != 1 || ps[0].Status != store.ProposalPending || ps[0].Scope != store.ScopeWorkspace {
		t.Fatalf("proposals = %+v, want one pending workspace draft", ps)
	}
	if sk, err := store.ParseSkill(ps[0].Text); err != nil || sk.Name != "release" {
		t.Errorf("ParseSkill(draft) = %+v, %v, want the release skill", sk, err)
	}
	if n := f.count(`SELECT COUNT(*) FROM items WHERE kind = 'skill' AND state = 'draft' AND scope = 'workspace'
		AND workspace_key = 'ws-A'`); n != 1 {
		t.Errorf("draft skill items in ws-A = %d, want 1", n)
	}
	if n := f.count(`SELECT COUNT(*) FROM items WHERE kind != 'skill'`); n != 0 {
		t.Errorf("items of another kind = %d, want none: no preference", n)
	}
	if n := f.count(`SELECT COUNT(*) FROM candidates WHERE kind = 'procedure' AND sessions = 4
		AND harnesses = '["claude","codex"]'`); n != 1 {
		t.Errorf("procedure candidates of 4 sessions in claude and codex = %d, want 1", n)
	}
	if calls := f.cli.calls(); calls != 3 {
		t.Errorf("calls = %d, want 3: the first telling has no signal", calls)
	}
}

// AC 3 (#313): a procedure from one session stays below the gate. Told once with a correction
// mark, it is a skill claim with count 1, and no proposal. Told once with no mark (the one-off
// procedure 8c of the M2 eval rerun), it has no signal and costs no call.
func TestProcedureSkillOneSession(t *testing.T) {
	for name, tc := range map[string]struct {
		text         string
		calls, count int
	}{
		"correction mark": {text: "no, release it this way instead: " + releaseText("0.2.0"), calls: 1, count: 1},
		"one-off":         {text: releaseText("0.2.0")},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			f.cli.reply = skillReply(nil)
			f.session("r1", true, line{"ws-A", tc.text, t0})
			f.mustRun()
			if got := f.cli.calls(); got != tc.calls {
				t.Errorf("calls = %d, want %d", got, tc.calls)
			}
			if got := f.count(`SELECT COALESCE(MAX(count), 0) FROM candidates WHERE kind = 'procedure'`); got != tc.count {
				t.Errorf("procedure count = %d, want %d", got, tc.count)
			}
			if n := f.count(`SELECT COUNT(*) FROM proposals`) + f.count(`SELECT COUNT(*) FROM items`); n != 0 {
				t.Errorf("proposals and items = %d, want none", n)
			}
		})
	}
}

// AC 4 (#313): the second telling comes as kind procedure, the third as kind skill in other
// words with a same_as to the claim of the second. The skill joins the open procedure
// candidate, so the third telling reaches the gate and gives a skill draft with its fields.
func TestProcedureThenSkill(t *testing.T) {
	f := newFixture(t)
	f.cli.reply = procedureReply(procedureSituation)
	f.release(1)
	f.release(2)
	f.cli.reply = sameAsSkill(knownKey(procedureSituation))
	f.release(3)
	if n := f.count(`SELECT COUNT(*) FROM candidates`); n != 1 {
		t.Errorf("candidates = %d, want 1: the skill merges", n)
	}
	if n := f.count(`SELECT COUNT(*) FROM candidates WHERE count = 3 AND claim_id IN
		(SELECT id FROM claims WHERE situation = '` + procedureSituation + `')`); n != 1 {
		t.Errorf("procedure candidates with count 3 = %d, want 1", n)
	}
	if n := f.count(`SELECT COUNT(*) FROM evidence_links WHERE via_claim IN
		(SELECT id FROM claims WHERE situation = '` + otherSituation + `')`); n != 1 {
		t.Errorf("evidence merged from the skill claim = %d, want 1", n)
	}
	ps := f.proposals()
	if len(ps) != 1 {
		t.Fatalf("proposals = %+v, want one skill draft", ps)
	}
	if sk, err := store.ParseSkill(ps[0].Text); err != nil || sk.Name != "release" {
		t.Errorf("ParseSkill(draft) = %+v, %v, want the release skill", sk, err)
	}
	if n := f.count(`SELECT COUNT(*) FROM items WHERE kind = 'skill' AND state = 'draft'`); n != 1 {
		t.Errorf("draft skill items = %d, want 1", n)
	}
}

// AC #348 with the run 4 shape: the extractor flags each release telling as touching a
// permission, and the third telling has " in that order" added to the rule and a same_as link to
// the claim of the first two. The store keeps the model flag on that claim, as in a store from
// before the fix. Brain's own check finds no permission words in the procedure, so the link
// merges: three tellings give one skill proposal, which keeps the permission warning. A
// procedure whose text holds permission words is a real permission claim: the link stays for
// review, and the third telling makes a second candidate and no proposal.
func TestPermissionFlaggedProcedure(t *testing.T) {
	for name, tc := range map[string]struct {
		steps                        string
		candidates, count, proposals int // count: the highest count of a candidate
	}{
		"run 4 release": {steps: "1. Bump the version to <version>.\n2. Update the changelog.\n3. Run lint and tests.\n" +
			"4. Commit.\n5. Tag the release.", candidates: 1, count: 3, proposals: 1},
		"permission words": {steps: "1. Allow git push in the sandbox.\n2. Tag and push.", candidates: 2, count: 2},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			flagged := map[string]any{"touches_permission": true, "steps": tc.steps}
			f.cli.reply = skillReply(flagged)
			f.release(1)
			f.release(2)
			if n := f.count(`SELECT COUNT(*) FROM claims WHERE situation = 'releasing' AND touches_permission = 1`); n != 1 {
				t.Fatalf("claims of the first tellings with the permission flag = %d, want 1", n)
			}
			f.cli.reply = func(ctx context.Context, prompt string) ([]byte, error) {
				m := knownKey(skillSituation).FindStringSubmatch(prompt)
				if m == nil {
					return nil, fmt.Errorf("the input has no known release claim:\n%s", prompt)
				}
				set := maps.Clone(flagged)
				set["rule"], set["same_as"] = "follow the release steps in that order", m[1]
				return skillReply(set)(ctx, prompt)
			}
			f.release(3)
			if got := f.count(`SELECT COUNT(*) FROM candidates`); got != tc.candidates {
				t.Errorf("candidates = %d, want %d", got, tc.candidates)
			}
			if got := f.count(`SELECT MAX(count) FROM candidates`); got != tc.count {
				t.Errorf("highest count = %d, want %d", got, tc.count)
			}
			if n := f.count(`SELECT COUNT(*) FROM claims WHERE same_as_status = 'proposed'`); n != 1 {
				t.Errorf("proposed links = %d, want 1: Brain keeps both claim ids and the link", n)
			}
			ps := f.proposals()
			if len(ps) != tc.proposals {
				t.Fatalf("proposals = %+v, want %d", ps, tc.proposals)
			}
			for _, p := range ps {
				if sk, err := store.ParseSkill(p.Text); err != nil || !p.PermissionWarning || !strings.Contains(sk.Steps, "Tag") {
					t.Errorf("proposal %+v: ParseSkill = %+v, %v, want the release skill with the permission warning", p, sk, err)
				}
			}
		})
	}
}

// AC 5 (#313): after a skill draft of the release, the same procedure in other words (a skill
// claim with a same_as to its claim) adds evidence to the skill's candidate, while the draft
// waits and after its approval: no second candidate and no second skill. A skill is still
// another thing than a rule (#312): when a preference proposal holds the claim, the skill builds
// its own candidate and its own draft.
func TestNoSecondSkill(t *testing.T) {
	for name, tc := range map[string]struct {
		first     func(context.Context, string) ([]byte, error)
		situation string // the situation of the claim of the first tellings
		approve   bool
		// The proposals and the candidates after the three later tellings, and the count of the
		// first candidate with the evidence that the later tellings gave it, marked as merged.
		proposals, candidates, count, merged int
	}{
		"skill waits":    {first: skillReply(nil), situation: skillSituation, proposals: 1, candidates: 1, count: 6, merged: 3},
		"skill approved": {first: skillReply(nil), situation: skillSituation, approve: true, proposals: 1, candidates: 1, count: 6, merged: 3},
		"rule waits":     {first: procedureReply(procedureSituation), situation: procedureSituation, proposals: 2, candidates: 2, count: 3},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			f.cli.reply = tc.first
			for n := 1; n <= 3; n++ {
				f.release(n)
			}
			ps := f.proposals()
			if len(ps) != 1 {
				t.Fatalf("proposals = %+v, want one draft", ps)
			}
			if tc.approve {
				f.approveSkill(ps[0])
			}
			f.cli.reply = sameAsSkill(knownKey(tc.situation))
			for n := 4; n <= 6; n++ {
				f.release(n)
			}
			if got := len(f.proposals()); got != tc.proposals {
				t.Errorf("proposals = %d, want %d", got, tc.proposals)
			}
			if got := f.count(`SELECT COUNT(*) FROM candidates`); got != tc.candidates {
				t.Errorf("candidates = %d, want %d", got, tc.candidates)
			}
			if got := f.count(`SELECT COUNT(*) FROM items WHERE kind = 'skill'`); got != 1 {
				t.Errorf("skill items = %d, want 1", got)
			}
			if got := f.count(`SELECT count FROM candidates WHERE claim_id IN (SELECT id FROM claims WHERE situation = '` +
				tc.situation + `')`); got != tc.count {
				t.Errorf("count of the first candidate = %d, want %d", got, tc.count)
			}
			if got := f.count(`SELECT COUNT(*) FROM evidence_links WHERE via_claim IN (SELECT id FROM claims WHERE situation = '` +
				otherSituation + `')`); got != tc.merged {
				t.Errorf("evidence of the later tellings marked as merged = %d, want %d", got, tc.merged)
			}
		})
	}
}

// AC 6 (#313): a redraft of the new skill draft keeps kind skill: the request asks for a skill,
// a skill reply makes a new skill draft of the same item, and a procedure reply is invalid. The
// redraft prompt has the skill fields, not the rule that asks for kind skill.
func TestProcedureSkillRedraft(t *testing.T) {
	for name, tc := range map[string]struct {
		reply  func(context.Context, string) ([]byte, error)
		status store.ChangeStatus
		drafts int // the pending drafts after the redraft
	}{
		"skill reply":     {reply: skillReply(map[string]any{"steps": "1. Run go test ./...\n2. Tag.\n3. Push the tag."}), status: store.ChangeCommitted, drafts: 1},
		"procedure reply": {reply: procedureReply(skillSituation), status: store.ChangeInvalid},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			p := f.skillDraft()
			cr := f.ask(p, "name the push step")
			f.cli.reply = tc.reply
			f.now = f.now.Add(time.Minute)
			f.mustRun()
			prompt := f.cli.prompts[len(f.cli.prompts)-1]
			for _, want := range []string{"name the push step", "The proposal is a skill: return kind skill", skillFields} {
				if !strings.Contains(prompt, want) {
					t.Errorf("redraft prompt lacks %q:\n%s", want, prompt)
				}
			}
			if strings.Contains(prompt, skillHelp) {
				t.Errorf("redraft prompt has the rule for kind skill:\n%s", prompt)
			}
			if got := f.request(cr.ID).Status; got != tc.status {
				t.Errorf("request status = %s, want %s", got, tc.status)
			}
			var drafts []store.Proposal
			for _, q := range f.proposals() {
				if q.Status == store.ProposalPending {
					drafts = append(drafts, q)
				}
			}
			if len(drafts) != tc.drafts {
				t.Fatalf("pending drafts = %+v, want %d", drafts, tc.drafts)
			}
			for _, d := range drafts {
				sk, err := store.ParseSkill(d.Text)
				if err != nil || *d.ItemID != *p.ItemID || !strings.Contains(sk.Steps, "Push the tag") {
					t.Errorf("draft %+v: ParseSkill = %+v, %v, want the new skill text of item %d", d, sk, err, *p.ItemID)
				}
			}
			if n := f.count(`SELECT COUNT(*) FROM items WHERE kind != 'skill'`); n != 0 {
				t.Errorf("items of another kind = %d, want none", n)
			}
		})
	}
}

// AC 6 (#313): the redraft prompt of a preference has no line that asks for kind skill.
func TestPreferenceRedraftPrompt(t *testing.T) {
	f := newFixture(t)
	f.ask(f.proposed(), "say pnpm for every install")
	f.cli.reply = claimOf("use pnpm for every install")
	f.now = f.now.Add(time.Minute)
	f.mustRun()
	prompt := f.cli.prompts[len(f.cli.prompts)-1]
	if !strings.Contains(prompt, "Do not use kind skill or kind tool") || strings.Contains(prompt, skillHelp) {
		t.Errorf("preference redraft prompt:\n%s\nwant no line that asks for kind skill", prompt)
	}
}

// AC 7 (#313, #270): the skill of the release tellings can take a patch. After its approval,
// the same failure that it covers in two sessions that got it gives one patch of that skill.
func TestProcedureSkillPatch(t *testing.T) {
	f := newFixture(t)
	p := f.skillDraft()
	f.approveSkill(p)
	f.skillFailures(*p.ItemID, failed{2, 1}, failed{2, 1})
	f.cli.reply = skillReply(map[string]any{"steps": patchedSteps})
	f.mustRun()
	var patch []store.Proposal
	for _, q := range f.proposals() {
		if q.Status == store.ProposalPending {
			patch = append(patch, q)
		}
	}
	if len(patch) != 1 || *patch[0].ItemID != *p.ItemID || *patch[0].BaseRevision != 2 || patch[0].CandidateID != nil {
		t.Fatalf("pending proposals = %+v, want one patch of skill item %d on revision 2", patch, *p.ItemID)
	}
	if want := "@@ line 9\n- 2. Update the changelog.\n+ 2. Update the changelog before the tag."; patch[0].Diff != want {
		t.Errorf("patch diff = %q, want %q", patch[0].Diff, want)
	}
	if n := f.count(`SELECT COUNT(*) FROM items WHERE kind = 'skill'`); n != 1 {
		t.Errorf("skill items = %d, want 1: no second skill", n)
	}
}

// AC 8 (#313): the batch prompt says that Brain counts the repeats and asks for kind skill for
// a procedure told once, with general steps, and not for a one-line instruction.
func TestPromptAsksForSkillOnce(t *testing.T) {
	in := input(job{})
	for _, want := range []string{"you see one session", "use kind skill, also when the user tells it once",
		"fits in one line", "a placeholder such as <version>"} {
		if !strings.Contains(in, want) {
			t.Errorf("prompt has no %q", want)
		}
	}
}
