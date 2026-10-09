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
// the agents of run 5 ran item 1's chain because the rule says so (#366).
func chainClaim(kind string, n *int) func(context.Context, string) ([]byte, error) {
	return func(ctx context.Context, prompt string) ([]byte, error) {
		m := knownItem3.FindStringSubmatch(prompt)
		if m == nil {
			return nil, fmt.Errorf("the input has no known claim of item 3:\n%s", prompt)
		}
		*n++
		set := map[string]any{"kind": kind, "situation": fmt.Sprintf("finishing change %d", *n), "rule": "run " + item3Chain,
			"scope_guess": "global", "evidence_refs": toolRefs(prompt), "same_as": m[1]}
		if kind == kindSkill {
			return skillReply(set)(ctx, prompt)
		}
		return claudeReply(map[string]any{"claims": []any{replyOf(set)}})
	}
}

// AC #366 with the run 5 shape: item 3 (the full check rule) is active, and in 8 sessions the
// agent runs its chain because the rule says so. The extractor makes a new global claim of the
// chain each time, as a skill or a rule, linked to the claim of item 3. Each is held by item 3:
// no candidate, no link to review and no proposal.
func TestAgentFollowsItem(t *testing.T) {
	for _, kind := range []string{kindSkill, "procedure"} {
		t.Run(kind, func(t *testing.T) {
			f := newFixture(t)
			claim := f.item3(true)
			n := 0
			f.cli.reply = chainClaim(kind, &n)
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
// held, with no candidate and no proposal. A claim with another text still makes its own.
func TestRestatedEditedItem(t *testing.T) {
	const newRule = "run npm run lint, npm test and npm run build, and document each endpoint and its error codes in docs/API.md"
	for name, tc := range map[string]struct {
		rule       string
		candidates int
	}{
		"the new text": {rule: newRule + ".", candidates: 1},
		"another text": {rule: "document each endpoint in README.md", candidates: 2},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			claim := f.item3(true)
			f.editItem(f.onlyItem(), brief.ClaimText(item3Situation, newRule))
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

// AC #365 with the run 5 shape of proposals 7 and 8: you reject the rule of the T2 request,
// and the same request comes in other words in two sessions, with a same_as link to the
// rejected claim (an unclear match). It stays rejected: no candidate of its own, no link to
// review and no proposal. Its evidence goes to the rejected candidate, as new evidence since the
// rejection (brain review --rejected).
func TestRejectedRequestInOtherWords(t *testing.T) {
	f := newFixture(t)
	f.cli.reply = ruleReply(t2Situation, t2Rule, nil)
	f.session("s5", true, line{"ws-A", "no, " + t2Request, t0})
	f.session("s7", true, line{"ws-A", "no, " + t2Request, t0.Add(time.Minute)})
	f.mustRun()
	ps, cs := f.proposals(), f.candidates()
	if len(ps) != 1 || len(cs) != 1 {
		t.Fatalf("proposals %+v, candidates %+v, want proposal 7 of one candidate", ps, cs)
	}
	f.reject(ps[0], cs[0])
	f.cli.reply = ruleReply("preparing the test database", "reset it with the script before you run the tests",
		knownKey(t2Situation))
	f.session("s10", true, line{"ws-A", "again: reset the db with the script first, then the tests", t0.Add(time.Hour)})
	f.harnessSession("codex", "s11", true, line{"ws-A", "again: reset the db with the script first, then the tests",
		t0.Add(2 * time.Hour)})
	f.mustRun()
	if got := f.candidates(); len(got) != 1 || got[0].ID != cs[0].ID || got[0].Count != 4 {
		t.Errorf("candidates = %+v, want only the rejected candidate %d with the 2 new tellings", got, cs[0].ID)
	}
	if ls := f.links(); len(ls) != 0 {
		t.Errorf("links = %+v, want none to review", ls)
	}
	if got := f.proposals(); len(got) != 1 || got[0].Status != store.ProposalRejected {
		t.Errorf("proposals = %+v, want only the rejected one", got)
	}
}

// AC #365 with the run 5 shape of proposal 7: the T2 request runs the commands of tool
// reset-db-sh. After the tool draft of the exact repeats, the extractor makes a rule of the same
// request: Brain finds the commands of the tool in the rule, word for word, so the tool holds
// it: no rule candidate and no rule proposal. A rule that names other commands still counts.
func TestToolCoversRequest(t *testing.T) {
	for name, tc := range map[string]struct {
		rule       string
		candidates int
	}{
		"the tool's commands": {rule: t2Rule, candidates: 1},
		"other commands":      {rule: "Run exactly ./scripts/seed-db.sh and then exactly npm test", candidates: 2},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			cmds := []store.CommandRecord{command("./scripts/reset-db.sh"), command("npm test")}
			f.cli.reply = noClaims
			for i := range 3 {
				f.commandSession(fmt.Sprintf("t%d", i), turn{prompt: t2Request, cmds: cmds})
			}
			f.mustRun()
			if ps := f.toolProposals(); len(ps) != 1 {
				t.Fatalf("tool drafts = %+v, want the draft of reset-db-sh", ps)
			}
			f.cli.reply = ruleReply(t2Situation, tc.rule, nil)
			f.commandSession("t3", turn{prompt: t2Request, cmds: cmds})
			f.commandSession("t4", turn{prompt: t2Request, cmds: cmds})
			f.mustRun()
			if got := len(f.candidates()); got != tc.candidates {
				t.Errorf("candidates = %+v, want %d", f.candidates(), tc.candidates)
			}
			if n := f.count(`SELECT COUNT(*) FROM items WHERE kind = 'preference'`); n != tc.candidates-1 {
				t.Errorf("rule items = %d, want %d", n, tc.candidates-1)
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
