package extract

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aryanVijaywargia/herdr-brain/internal/signal"
	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

// releaseFailures are corrections of a release step that the release skill covers.
var releaseFailures = []string{
	"no, you forgot the changelog again, update it before the tag",
	"wrong: the changelog entry must come before the git tag",
}

// coveredReply answers each prompt with one correction of a release step, with each ref of the
// prompt as evidence, that skill (a claim id, or nil) covers.
func coveredReply(skill any) func(context.Context, string) ([]byte, error) {
	return coveredRule(skill, "update the changelog before the tag")
}

// coveredRule is coveredReply with the rule of the failure.
func coveredRule(skill any, rule string) func(context.Context, string) ([]byte, error) {
	return func(_ context.Context, prompt string) ([]byte, error) {
		return claudeReply(map[string]any{"claims": []any{replyOf(map[string]any{"situation": "releasing a version",
			"rule": rule, "evidence_refs": promptRefs(prompt), "covered_by": skill})}})
	}
}

// skillSession stores ended session agent in ws-A at t0 plus i hours, with a receipt of
// revision 2 of skill item and the one line text, and returns the observation id.
func (f *fixture) skillSession(item int64, agent string, i int, text string) int64 {
	f.t.Helper()
	at := t0.Add(time.Duration(i) * time.Hour)
	f.deliver(receiptOf("default", agent, store.ResultEmitted, at.Add(-time.Minute), store.RevisionRef{Item: item, Revision: 2}))
	_, obs := f.session(agent, true, line{"ws-A", text, at})
	f.now = at.Add(time.Hour)
	return obs[0]
}

// claimOfItem returns the claim id of item.
func (f *fixture) claimOfItem(item int64) string {
	f.t.Helper()
	var claim string
	f.write(func(ctx context.Context, tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `SELECT claim_id FROM items WHERE id = ?`, item).Scan(&claim)
	})
	return claim
}

// failed is one session of skillFailures: the skill revision that its receipt names (0 for a
// receipt without the skill: the session started before it), and its count of flags.
type failed struct{ rev, flags int64 }

// skillFailures stores one ended session in ws-A for each of ss, with its receipt and its
// release failures, and runs a pass in which the extractor links each failure to the skill item.
func (f *fixture) skillFailures(item int64, ss ...failed) {
	f.t.Helper()
	f.cli.reply = coveredReply(f.claimOfItem(item))
	for i, s := range ss {
		agent := fmt.Sprintf("s%d", i)
		at := t0.Add(time.Duration(i) * time.Hour)
		var revs []store.RevisionRef
		if s.rev > 0 {
			revs = append(revs, store.RevisionRef{Item: item, Revision: s.rev})
		}
		f.deliver(receiptOf("default", agent, store.ResultEmitted, at.Add(-time.Minute), revs...))
		var lines []line
		for n := range s.flags {
			lines = append(lines, line{"ws-A", releaseFailures[n], at.Add(time.Duration(n) * time.Minute)})
		}
		f.session(agent, true, lines...)
	}
	f.now = t0.Add(time.Duration(len(ss)+1) * time.Hour)
	f.mustRun()
}

// patchedSteps are the steps of the release skill with the failure fixed.
const patchedSteps = "1. Run go test ./...\n2. Update the changelog before the tag.\n3. Tag and push."

// AC E20-S5 #270 1: Brain links a repeat failure to the active skill that covers it. The input
// marks the claim of the skill with its name. A correction that the extractor links to the
// skill (covered_by) is a comeback of the skill revision that the session got, and its claim
// makes no proposal: no fact and no second skill. Its evidence still attaches.
// With no link, the skill gets no outcome.
func TestSkillFailureLink(t *testing.T) {
	for _, linked := range []bool{true, false} {
		t.Run(fmt.Sprintf("linked=%v", linked), func(t *testing.T) {
			f := newFixture(t)
			item := f.skillItem()
			claim := f.claimOfItem(item)
			f.deliver(receiptOf("default", "s0", store.ResultEmitted, t0.Add(-time.Minute),
				store.RevisionRef{Item: item, Revision: 2}))
			f.session("s0", true, line{"ws-A", releaseFailures[0], t0})
			f.cli.reply = coveredReply(nil)
			if linked {
				f.cli.reply = coveredReply(claim)
			}
			f.mustRun()
			prompt := f.cli.prompts[0]
			for _, want := range []string{"[" + claim + "] situation: releasing; rule: follow the release steps; skill: release\n",
				coverHelp} {
				if !strings.Contains(prompt, want) {
					t.Errorf("prompt lacks %q:\n%s", want, prompt)
				}
			}
			var want []string
			if linked {
				want = []string{"comeback 2"}
			}
			if got := f.outcomes(); !slices.Equal(got, want) {
				t.Errorf("outcomes = %v, want %v", got, want)
			}
			if !linked {
				return
			}
			if n := f.count(`SELECT COUNT(*) FROM proposals`); n != 0 {
				t.Errorf("proposals = %d, want none: the failure goes to the skill", n)
			}
			if n := f.count(`SELECT COUNT(*) FROM items`); n != 1 {
				t.Errorf("items = %d, want the skill only", n)
			}
			if n := f.count(`SELECT COUNT(*) FROM candidates c JOIN claims l ON l.id = c.claim_id
				WHERE l.rule = 'update the changelog before the tag' AND c.count = 1`); n != 1 {
				t.Errorf("candidates of the failure with its evidence = %d, want 1", n)
			}
		})
	}
}

// AC E20-S5 #270 2: Brain counts distinct sessions, not signal flags. Two flags in one session
// make no proposal. The same failure in 2 distinct sessions after the skill revision makes one
// proposal, and a third session no second one. A failure of a session that got an older
// revision, or no revision of the skill, does not count for the current revision.
func TestSkillComebackSessions(t *testing.T) {
	for _, c := range []struct {
		name      string
		sessions  []failed
		outcomes  []string
		proposals int
	}{
		{"two flags in one session", []failed{{2, 2}}, []string{"comeback 2", "comeback 2"}, 0},
		{"two sessions after the revision", []failed{{2, 1}, {2, 1}}, []string{"comeback 2", "comeback 2"}, 1},
		{"three sessions", []failed{{2, 1}, {2, 1}, {2, 1}}, []string{"comeback 2", "comeback 2", "comeback 2"}, 1},
		{"a failure before the skill revision", []failed{{1, 1}, {2, 1}}, []string{"comeback 1", "comeback 2"}, 0},
		{"a session before the skill", []failed{{0, 1}, {2, 1}}, []string{"comeback 2"}, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			item := f.skillItem()
			f.skillFailures(item, c.sessions...)
			if got := f.outcomes(); !slices.Equal(got, c.outcomes) {
				t.Fatalf("outcomes = %v, want %v", got, c.outcomes)
			}
			if n := f.count(fmt.Sprintf(`SELECT COUNT(*) FROM change_requests WHERE source = 'item' AND source_id = %d
				AND revision = 2`, item)); n != c.proposals || f.count(`SELECT COUNT(*) FROM change_requests`) != c.proposals {
				t.Fatalf("change requests of revision 2 = %d, want %d and no other", n, c.proposals)
			}
			f.cli.reply = skillReply(map[string]any{"steps": patchedSteps})
			f.mustRun()
			if ps := f.proposals(); len(ps) != c.proposals {
				t.Errorf("proposals = %+v, want %d", ps, c.proposals)
			}
		})
	}
}

// AC E20-S5 #270 3: the proposal is a new revision of the existing skill, with a diff of the
// changed lines, through the change-request queue: a pending patch of the skill item on
// revision 2, with no candidate. It is not a second skill, and the skill does not change until
// you approve. The request asks for a patch of the steps, with the failures as input.
func TestSkillFailurePatch(t *testing.T) {
	f := newFixture(t)
	item := f.skillItem()
	f.skillFailures(item, failed{2, 1}, failed{2, 1})
	calls := f.cli.calls()
	f.cli.reply = skillReply(map[string]any{"steps": patchedSteps})
	f.mustRun()
	if f.cli.calls() != calls+1 {
		t.Fatalf("calls = %d, want %d: one redraft", f.cli.calls(), calls+1)
	}
	prompt := f.cli.prompts[len(f.cli.prompts)-1]
	for _, want := range []string{skillRequest, "The proposal is a skill", "name: release", releaseFailures[0]} {
		if !strings.Contains(prompt, want) {
			t.Errorf("redraft prompt lacks %q:\n%s", want, prompt)
		}
	}
	ps := f.proposals()
	if len(ps) != 1 || *ps[0].ItemID != item || *ps[0].BaseRevision != 2 || ps[0].Status != store.ProposalPending ||
		ps[0].CandidateID != nil {
		t.Fatalf("proposals = %+v, want one pending patch of the skill on revision 2", ps)
	}
	if want := "@@ line 9\n- 2. Update the changelog.\n+ 2. Update the changelog before the tag."; ps[0].Diff != want {
		t.Errorf("patch diff = %q, want %q", ps[0].Diff, want)
	}
	if n := f.count(fmt.Sprintf(`SELECT COUNT(*) FROM change_requests WHERE status = 'committed' AND new_proposal_id = %d`,
		ps[0].ID)); n != 1 {
		t.Errorf("committed requests with the patch = %d, want 1", n)
	}
	if n := f.count(`SELECT COUNT(*) FROM items WHERE kind = 'skill'`); n != 1 {
		t.Errorf("skill items = %d, want 1: no second skill", n)
	}
	if n := f.count(`SELECT COUNT(*) FROM item_revisions`); n != 2 ||
		f.count(`SELECT COUNT(*) FROM items WHERE current_revision = 2 AND state = 'active'`) != 1 {
		t.Error("the skill changed before an approval")
	}
}

// AC E20-S5 #270 5: the extractor caps still apply to the request of a skill. Under a full
// call cap or proposal cap the request waits, with no call; the next day it drafts. Under the
// input cap, its input holds the newest failure only.
func TestSkillComebackCaps(t *testing.T) {
	t.Run("call cap", func(t *testing.T) {
		f := newFixture(t)
		item := f.skillItem()
		f.skillFailures(item, failed{2, 1}, failed{2, 1})
		calls := f.cli.calls()
		f.cli.reply = skillReply(map[string]any{"steps": patchedSteps})
		pass := f.pass()
		pass.Caps = Caps{Calls: calls, Proposals: 5, InputBytes: 32 << 10}
		if err := pass.Run(context.Background()); err != nil {
			t.Fatal(err)
		}
		if f.cli.calls() != calls || f.count(`SELECT COUNT(*) FROM change_requests WHERE status = 'queued'`) != 1 ||
			len(f.proposals()) != 0 {
			t.Fatalf("calls = %d, want %d, and the request queued with no proposal", f.cli.calls(), calls)
		}
		f.now = f.now.Add(day)
		f.mustRun()
		if f.cli.calls() != calls+1 || len(f.proposals()) != 1 {
			t.Errorf("the next day: calls = %d, proposals = %d, want %d and 1", f.cli.calls(), len(f.proposals()), calls+1)
		}
	})
	// Review of PR #303: a skill patch counts under the daily proposal cap. You ask for a
	// change of the first patch: under a cap of 1 proposal its redraft waits, then drafts the
	// next day.
	t.Run("proposal cap", func(t *testing.T) {
		f := newFixture(t)
		item := f.skillItem()
		f.skillFailures(item, failed{2, 1}, failed{2, 1})
		f.cli.reply = skillReply(map[string]any{"steps": patchedSteps})
		f.mustRun()
		ps := f.proposals()
		if len(ps) != 1 {
			t.Fatalf("proposals = %+v, want the first patch", ps)
		}
		f.write(func(ctx context.Context, tx *sql.Tx) error {
			_, err := store.AskItemChange(ctx, tx, ps[0].ID, "keep the tag step", store.SurfaceCLI, f.now)
			return err
		})
		calls := f.cli.calls()
		pass := f.pass()
		pass.Caps = Caps{Calls: 10, Proposals: 1, InputBytes: 32 << 10} // test limits: a cap of 1 proposal
		if err := pass.Run(context.Background()); err != nil {
			t.Fatal(err)
		}
		if f.cli.calls() != calls || len(f.proposals()) != 1 {
			t.Fatalf("calls = %d, proposals = %d, want %d and 1 under the full proposal cap", f.cli.calls(), len(f.proposals()), calls)
		}
		f.now = f.now.Add(day + time.Minute)
		if err := pass.Run(context.Background()); err != nil {
			t.Fatal(err)
		}
		if f.cli.calls() != calls+1 || len(f.proposals()) != 2 {
			t.Errorf("the next day: calls = %d, proposals = %d, want %d and 2", f.cli.calls(), len(f.proposals()), calls+1)
		}
	})
	t.Run("input cap", func(t *testing.T) {
		f := newFixture(t)
		item := f.skillItem()
		f.skillFailures(item, failed{2, 1}, failed{2, 2})
		f.cli.reply = skillReply(map[string]any{"steps": patchedSteps})
		pass := f.pass()
		pass.Caps = Caps{Calls: 10, Proposals: 5, InputBytes: len(releaseFailures[1]) + 1} // test limits: one excerpt
		if err := pass.Run(context.Background()); err != nil {
			t.Fatal(err)
		}
		prompt := f.cli.prompts[len(f.cli.prompts)-1]
		if !strings.Contains(prompt, releaseFailures[1]) || strings.Contains(prompt, releaseFailures[0]) {
			t.Errorf("redraft prompt holds more than the newest failure:\n%s", prompt)
		}
	})
}

// Review of PR #303: a tool failure reaches the skill, though the correction candidate of its
// claim refuses tool output. The first session corrects the agent; the second shows the
// same failure as tool output. The two sessions queue one request.
func TestSkillToolFailure(t *testing.T) {
	f := newFixture(t)
	item := f.skillItem()
	f.cli.reply = coveredReply(f.claimOfItem(item))
	f.skillSession(item, "s0", 0, releaseFailures[0])
	f.mustRun()
	tool := f.skillSession(item, "s1", 1, "error: tag v1.2.0 has no changelog entry")
	f.write(func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE observations SET sender = 'tool' WHERE id = ?`, tool)
		return err
	})
	f.mustRun()
	if got, want := f.outcomes(), []string{"comeback 2", "comeback 2"}; !slices.Equal(got, want) {
		t.Fatalf("outcomes = %v, want %v", got, want)
	}
	if n := f.count(`SELECT COUNT(*) FROM evidence_links WHERE observation_id = ` + fmt.Sprint(tool)); n != 0 {
		t.Errorf("evidence links of the tool output = %d, want 0: the correction refuses it", n)
	}
	if n := f.count(`SELECT COUNT(*) FROM change_requests WHERE source = 'item' AND revision = 2`); n != 1 {
		t.Errorf("change requests = %d, want 1", n)
	}
}

// AC E20-S5 #270 2, review of PR #303: a skill counts the sessions of the same failure. Two
// different failures in two sessions queue nothing; the first failure again in a third session
// queues one request.
func TestSkillDifferentFailures(t *testing.T) {
	f := newFixture(t)
	item := f.skillItem()
	skill := f.claimOfItem(item)
	for i, rule := range []string{"update the changelog before the tag", "run the tests before the tag"} {
		f.cli.reply = coveredRule(skill, rule)
		f.skillSession(item, fmt.Sprintf("s%d", i), i, releaseFailures[i])
		f.mustRun()
	}
	if got, want := f.outcomes(), []string{"comeback 2", "comeback 2"}; !slices.Equal(got, want) {
		t.Fatalf("outcomes = %v, want %v", got, want)
	}
	if n := f.count(`SELECT COUNT(*) FROM change_requests`); n != 0 {
		t.Fatalf("change requests = %d, want 0: no failure came back", n)
	}
	f.cli.reply = coveredReply(skill)
	f.skillSession(item, "s2", 2, releaseFailures[0])
	f.mustRun()
	if n := f.count(`SELECT COUNT(*) FROM change_requests WHERE source = 'item' AND revision = 2`); n != 1 {
		t.Errorf("change requests = %d, want 1", n)
	}
}

// A covered_by that names a known claim that is no skill, or a claim that is not in the input,
// refuses the whole result, as a bad same_as does.
func TestSkillCoveredByRefused(t *testing.T) {
	for _, c := range []struct {
		name  string
		claim func(f *fixture) string
	}{
		{"not a skill", func(f *fixture) string { c, _ := f.claimItem("installing packages", "use pnpm"); return c }},
		{"not in the input", func(*fixture) string { return "cl_unknown" }},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			f.skillItem()
			f.cli.reply = coveredReply(c.claim(f))
			f.session("s0", true, line{"ws-A", releaseFailures[0], t0})
			f.mustRun()
			if bs := f.batches(); len(bs) != 1 || bs[0].Status != store.BatchInvalid {
				t.Errorf("batches = %+v, want one invalid batch", bs)
			}
		})
	}
}

// failures keeps the accepted corrections of a claim and each tool failure of its refs, also
// one that its correction candidate refused, once (E20-S5).
func TestFailures(t *testing.T) {
	e1 := ref{id: "e1", obs: 1, kind: signal.Correction}
	e2 := ref{id: "e2", obs: 2, kind: signal.Failure}
	e3 := ref{id: "e3", obs: 3, kind: signal.Procedure}
	refs := []ref{e1, e2, e3}
	ids := func(rs []ref) []string {
		var out []string
		for _, r := range rs {
			out = append(out, r.id)
		}
		return out
	}
	for _, c := range []struct {
		kind     store.CandidateKind
		accepted []ref
		want     []string
	}{
		{store.CandidateCorrection, []ref{e1, e3}, []string{"e1", "e3", "e2"}},
		{store.CandidateProcedure, []ref{e1, e3}, []string{"e1", "e2"}},
		{store.CandidateFailure, []ref{e2}, []string{"e2"}},
	} {
		if got := ids(failures(c.kind, c.accepted, refs)); !slices.Equal(got, c.want) {
			t.Errorf("failures(%s, %v) = %v, want %v", c.kind, ids(c.accepted), got, c.want)
		}
	}
}
