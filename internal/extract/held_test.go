package extract

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/aryanVijaywargia/herdr-brain/internal/brief"
	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

// The T2 request of M2 eval run 5, which made the tool reset-db-sh, and the rule of proposal 7
// that the extractor made of it (#365).
const (
	t2Request   = "Reset the test database with ./scripts/reset-db.sh, then run npm test, and tell me the result."
	t2Situation = "resetting the test database"
	t2Rule      = "Run exactly ./scripts/reset-db.sh and then exactly npm test"
)

// chainClaim answers each prompt with one claim of kind of the agent's full check chain, of the
// tool lines only, in global scope, with situation n and a same_as link to the claim of item 3:
// the agents of run 5 ran item 1's chain because the rule says so (#366). flagged sets the
// model's permission flag.
func chainClaim(kind string, flagged bool, n *int) func(context.Context, string) ([]byte, error) {
	return func(ctx context.Context, prompt string) ([]byte, error) {
		m := knownItem3.FindStringSubmatch(prompt)
		if m == nil {
			return nil, fmt.Errorf("the input has no known claim of item 3:\n%s", prompt)
		}
		*n++
		set := map[string]any{"kind": kind, "situation": fmt.Sprintf("finishing change %d", *n), "rule": "run " + item3Chain,
			"scope_guess": "global", "evidence_refs": toolRefs(prompt), "same_as": m[1], "touches_permission": flagged}
		if kind == kindSkill {
			return skillReply(set)(ctx, prompt)
		}
		return claudeReply(map[string]any{"claims": []any{replyOf(set)}})
	}
}

// AC #366 with the run 5 shape: item 3 (the full check rule) is active, and in 8 sessions the
// agent runs its chain because the rule says so. The extractor makes a new global claim of the
// chain each time, as a skill or a rule, linked to the claim of item 3, also with the permission
// flag. Each is held by item 3: no candidate, no link to review and no proposal. A hold merges
// nothing, so the permission flag does not stop it (Sol review of #370).
func TestAgentFollowsItem(t *testing.T) {
	for name, tc := range map[string]struct {
		kind    string
		flagged bool
	}{
		"skill":                  {kind: kindSkill},
		"procedure":              {kind: "procedure"},
		"with a permission flag": {kind: "procedure", flagged: true},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			claim := f.item3(true)
			n := 0
			f.cli.reply = chainClaim(tc.kind, tc.flagged, &n)
			for i := range 8 {
				f.commandSession(fmt.Sprintf("h%d", i), turn{prompt: fmt.Sprintf("add route %d", i),
					cmds: []store.CommandRecord{command(item3Chain)}})
			}
			f.mustRun()
			if n != 8 {
				t.Fatalf("chain claims = %d, want 8", n)
			}
			if cs := f.candidates(); len(cs) != 1 || cs[0].ClaimID != claim || cs[0].Count != 2 {
				t.Errorf("candidates = %+v, want only the candidate of item 3 with its 2 prompts", cs)
			}
			if ls := f.links(); len(ls) != 0 {
				t.Errorf("links = %+v, want none to review", ls)
			}
			if ps := f.proposals(); len(ps) != 1 {
				t.Errorf("proposals = %+v, want only the proposal of item 3", ps)
			}
		})
	}
}

// editItem writes revision 2 of active item id with text, as brain review --item <id> --edit
// and its approval do.
func (f *fixture) editItem(id int64, text string) {
	f.t.Helper()
	key := "ws-A"
	f.write(func(ctx context.Context, tx *sql.Tx) error {
		_, err := store.WriteRevision(ctx, tx, 1, store.ItemRevision{ItemID: id, Text: text, Scope: store.ScopeWorkspace,
			WorkspaceKey: &key, State: store.StateActive, Author: store.AuthorYou, Origin: store.OriginReview, Created: t0})
		return err
	})
}

// AC #364 with the run 5 shape: item 3 is edited (r2), and your reply restates the new text,
// a lesson of gate 1. The claim of the item keeps the key of its first text, so the extractor
// gives no same_as link; Brain's own check finds the current text of the item: the claim is
// held, with no candidate and no proposal, also when the new text has a permission word (Sol
// review of #370). A claim with another text still makes its own.
func TestRestatedEditedItem(t *testing.T) {
	const newRule = "run npm run lint, npm test and npm run build, and document each endpoint and its error codes in docs/API.md"
	const permRule = "always run npm run lint, npm test and npm run build, and document each endpoint in docs/API.md"
	for name, tc := range map[string]struct {
		edit, rule string
		candidates int
	}{
		"the new text":      {edit: newRule, rule: newRule + ".", candidates: 1},
		"a permission word": {edit: permRule, rule: permRule, candidates: 1},
		"another text":      {edit: newRule, rule: "document each endpoint in README.md", candidates: 2},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			claim := f.item3(true)
			f.editItem(f.onlyItem(), brief.ClaimText(item3Situation, tc.edit))
			f.cli.reply = func(_ context.Context, prompt string) ([]byte, error) {
				return claudeReply(map[string]any{"claims": []any{replyOf(map[string]any{"kind": kindLesson,
					"situation": item3Situation, "rule": tc.rule, "evidence_refs": promptRefs(prompt)})}})
			}
			f.capturedSession("h15", "Yes, change the saved rule to say docs/API.md, and remember it from now on.")
			f.mustRun()
			if got := len(f.candidates()); got != tc.candidates {
				t.Errorf("candidates = %+v, want %d", f.candidates(), tc.candidates)
			}
			if got, want := len(f.proposals()), tc.candidates; got != want {
				t.Errorf("proposals = %+v, want %d", f.proposals(), want)
			}
			links := f.count(`SELECT COUNT(*) FROM claims WHERE same_as = '` + claim + `' AND same_as_status = 'proposed'`)
			if want := 2 - tc.candidates; links != want {
				t.Errorf("proposed links to the claim of item 3 = %d, want %d", links, want)
			}
		})
	}
}

// reject rejects pending proposal p of candidate c in its scope, as brain review --reject does.
func (f *fixture) reject(p store.Proposal, c store.Candidate) {
	f.t.Helper()
	f.write(func(ctx context.Context, tx *sql.Tx) error {
		if err := store.DecideProposal(ctx, tx, p.ID, store.ProposalRejected, store.SurfaceCLI, t0); err != nil {
			return err
		}
		_, err := store.Reject(ctx, tx, c, "a one-time request", t0)
		return err
	})
}

// rejectedRewording runs the run 5 shape of proposals 7 and 8 (#365): you reject the rule of the
// T2 request, and the same request comes in other words in two sessions, with a same_as link to
// the rejected claim (an unclear match). It returns the rejected candidate and the link.
func (f *fixture) rejectedRewording() (store.Candidate, Link) {
	f.t.Helper()
	f.cli.reply = ruleReply(t2Situation, t2Rule, nil)
	f.session("s5", true, line{"ws-A", "no, " + t2Request, t0})
	f.session("s7", true, line{"ws-A", "no, " + t2Request, t0.Add(time.Minute)})
	f.mustRun()
	ps, cs := f.proposals(), f.candidates()
	if len(ps) != 1 || len(cs) != 1 {
		f.t.Fatalf("proposals %+v, candidates %+v, want proposal 7 of one candidate", ps, cs)
	}
	f.reject(ps[0], cs[0])
	f.cli.reply = ruleReply("preparing the test database", "reset it with the script before you run the tests",
		knownKey(t2Situation))
	f.session("s10", true, line{"ws-A", "again: reset the db with the script first, then the tests", t0.Add(time.Hour)})
	f.harnessSession("codex", "s11", true, line{"ws-A", "again: reset the db with the script first, then the tests",
		t0.Add(2 * time.Hour)})
	f.mustRun()
	ls := f.links()
	if len(ls) != 1 || ls[0].Candidate.Count != 2 || ls[0].OtherCandidateID != cs[0].ID || ls[0].Stuck != "" {
		f.t.Fatalf("links = %+v, want the link of the new claim at its gate to the rejected candidate %d", ls, cs[0].ID)
	}
	if got := f.proposals(); len(got) != 1 {
		f.t.Fatalf("proposals = %+v, want only the rejected one while the link waits", got)
	}
	return cs[0], ls[0]
}

// AC #365 with the run 5 shape of proposals 7 and 8: the request in other words reaches its
// gate, but the rejection holds it: no proposal while the link waits for review (§7.2: an
// unclear match goes to review, never to a silent merge). A merge keeps the rejection: the
// evidence counts with the rejected claim, as new evidence since the rejection, and no proposal
// comes. Keep apart frees the claim: its candidate at its gate gets the proposal of its newest
// telling, with no model call (Sol review of #370).
func TestRejectedRequestInOtherWords(t *testing.T) {
	t.Run("merge", func(t *testing.T) {
		f := newFixture(t)
		rejected, l := f.rejectedRewording()
		if _, err := f.merge(l.Candidate.ID); err != nil {
			t.Fatal(err)
		}
		if got := f.candidates(); len(got) != 1 || got[0].ID != rejected.ID || got[0].Count != 4 {
			t.Errorf("candidates = %+v, want only the rejected candidate %d with the 2 new tellings", got, rejected.ID)
		}
		if got := f.proposals(); len(got) != 1 || got[0].Status != store.ProposalRejected {
			t.Errorf("proposals after the merge = %+v, want only the rejected one", got)
		}
	})
	t.Run("keep apart", func(t *testing.T) {
		f := newFixture(t)
		_, l := f.rejectedRewording()
		calls := f.cli.calls()
		var freed []int64
		f.write(func(ctx context.Context, tx *sql.Tx) (err error) {
			_, freed, err = KeepApart(ctx, tx, l.Candidate.ID, store.DefaultGates(), f.now)
			return err
		})
		ps := f.proposals()
		if len(freed) != 1 || len(ps) != 2 || ps[1].ID != freed[0] || ps[1].Status != store.ProposalPending ||
			ps[1].Text != "When preparing the test database: reset it with the script before you run the tests" {
			t.Fatalf("KeepApart proposals %v, proposals %+v, want one pending proposal of the newest telling", freed, ps)
		}
		if f.cli.calls() != calls {
			t.Errorf("calls = %d, want %d: keep apart makes no model call", f.cli.calls(), calls)
		}
	})
}

// AC #365 with the run 5 shape of proposal 7: the T2 request runs the commands of tool
// reset-db-sh. After the tool draft of the exact repeats, the extractor makes a procedure claim
// of the same request: it names the commands of the tool word for word, and its session ran the
// tool's chain, so the tool holds it: no rule candidate and no rule proposal. These still count
// (Sol review of #370): a procedure of other commands, a procedure whose session ran more than
// the tool, a correction that adds a condition, and a lesson with "always".
func TestToolCoversRequest(t *testing.T) {
	reset, test, commit := command("./scripts/reset-db.sh"), command("npm test"), command("git commit -am wip")
	for name, tc := range map[string]struct {
		kind, rule, text string
		cmds             []store.CommandRecord
		held             bool
	}{
		"the tool's request": {kind: "procedure", rule: t2Rule, text: t2Request, cmds: []store.CommandRecord{reset, test}, held: true},
		"other commands": {kind: "procedure", rule: "Run exactly ./scripts/seed-db.sh and then exactly npm test",
			text: t2Request, cmds: []store.CommandRecord{reset, test}},
		"a longer run": {kind: "procedure", rule: t2Rule + ", then commit", text: t2Request,
			cmds: []store.CommandRecord{reset, test, commit}},
		"a correction": {kind: "correction", rule: "run ./scripts/reset-db.sh and then npm test before you commit",
			text: "no, " + t2Request, cmds: []store.CommandRecord{reset, test}},
		"a lesson": {kind: kindLesson, rule: "always run ./scripts/reset-db.sh and then npm test before you commit",
			text: "From now on, always run ./scripts/reset-db.sh and then npm test before you commit.",
			cmds: []store.CommandRecord{reset, test}},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			f.cli.reply = noClaims
			for i := range 3 {
				f.commandSession(fmt.Sprintf("t%d", i), turn{prompt: t2Request, cmds: []store.CommandRecord{reset, test}})
			}
			f.mustRun()
			if ps := f.toolProposals(); len(ps) != 1 {
				t.Fatalf("tool drafts = %+v, want the draft of reset-db-sh", ps)
			}
			f.cli.reply = func(_ context.Context, prompt string) ([]byte, error) {
				return claudeReply(map[string]any{"claims": []any{replyOf(map[string]any{"kind": tc.kind,
					"situation": t2Situation, "rule": tc.rule, "evidence_refs": promptRefs(prompt)})}})
			}
			f.commandSession("t3", turn{prompt: tc.text, cmds: tc.cmds})
			f.commandSession("t4", turn{prompt: tc.text, cmds: tc.cmds})
			f.mustRun()
			rules := f.count(`SELECT COUNT(*) FROM candidates c JOIN claims k ON k.id = c.claim_id
				WHERE k.situation = '` + t2Situation + `'`)
			if want := map[bool]int{true: 0, false: 1}[tc.held]; rules != want {
				t.Errorf("rule candidates = %d, want %d (candidates %+v)", rules, want, f.candidates())
			}
		})
	}
}

// AC #365: a question asked again in another session (RQ of runs 4 and 5) and a request for one
// change with the lesson word "always" (proposal 20 of run 5) still reach the extractor: the
// lesson words and the repeat signal stay (#253), and the extractor prompt says that they give
// no rule. With the extractor following it, no proposal comes. A real lesson with "always" still
// gives a proposal at gate 1.
func TestOneTimeRequests(t *testing.T) {
	const rq = "How do we ship a new version here? Do not change anything yet; just list the steps you would follow."
	for name, tc := range map[string]struct {
		texts     []string
		lesson    bool // the extractor returns a lesson claim
		proposals int
	}{
		"question asked twice": {texts: []string{rq, rq}},
		"one-time always":      {texts: []string{"make that startup error message always print"}},
		"lesson with always":   {texts: []string{"Always run npm test before you commit."}, lesson: true, proposals: 1},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			f.cli.reply = noClaims
			if tc.lesson {
				f.cli.reply = kindOf(kindLesson, "run npm test before you commit")
			}
			for i, text := range tc.texts {
				f.capturedSession(fmt.Sprintf("q%d", i), text)
			}
			f.mustRun()
			if f.cli.calls() == 0 {
				t.Fatal("calls = 0, want the signal to reach the extractor")
			}
			last := f.cli.prompts[len(f.cli.prompts)-1]
			if !strings.Contains(last, oneTimeHelp) || !strings.Contains(last, tc.texts[0]) {
				t.Errorf("extractor prompt has no one-time rule or no text:\n%s", last)
			}
			if got := len(f.proposals()); got != tc.proposals {
				t.Errorf("proposals = %+v, want %d", f.proposals(), tc.proposals)
			}
		})
	}
}

// AC #365: the extractor prompt names the run 5 shapes: a question asked again and the
// conditions of its answer, and a one-time request with "always", beside a real lesson.
func TestPromptOneTimeRule(t *testing.T) {
	in := input(job{})
	for _, want := range []string{"A question that asks for an answer gives no claim, also when the user asks it again",
		"\"do not change anything yet; just list the steps\"", "\"make that startup error message always print\"",
		"\"Always run the tests before you commit\" is a lesson"} {
		if !strings.Contains(in, want) {
			t.Errorf("prompt has no %q", want)
		}
	}
}
