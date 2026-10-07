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
	return func(_ context.Context, prompt string) ([]byte, error) {
		return claudeReply(map[string]any{"claims": []any{replyOf(map[string]any{"situation": "releasing a version",
			"rule": "update the changelog before the tag", "evidence_refs": promptRefs(prompt), "covered_by": skill})}})
	}
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
// makes no proposal: no fact and no second skill. Its evidence still attaches (§7.3 order).
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
// you approve (§5.4). The request asks for a patch of the steps, with the failures as input.
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
// call cap the request waits, with no call; the next day it drafts. Under the input cap, its
// input holds the newest failure only.
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
	t.Run("input cap", func(t *testing.T) {
		f := newFixture(t)
		item := f.skillItem()
		f.skillFailures(item, failed{2, 1}, failed{2, 2})
		f.cli.reply = skillReply(map[string]any{"steps": patchedSteps})
		pass := f.pass()
		pass.Caps = Caps{Calls: 10, Proposals: 5, InputBytes: len(releaseFailures[1]) + 1} // [NEW] test limits: one excerpt
		if err := pass.Run(context.Background()); err != nil {
			t.Fatal(err)
		}
		prompt := f.cli.prompts[len(f.cli.prompts)-1]
		if !strings.Contains(prompt, releaseFailures[1]) || strings.Contains(prompt, releaseFailures[0]) {
			t.Errorf("redraft prompt holds more than the newest failure:\n%s", prompt)
		}
	})
}

// A covered_by that names a known claim that is no skill, or a claim that is not in the input,
// refuses the whole result, as a bad same_as does (§7.3).
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

// failures keeps the corrections and the tool failures of a candidate that is no correction,
// and all refs of a correction candidate (E20-S5).
func TestFailures(t *testing.T) {
	refs := []ref{{id: "e1", kind: signal.Correction}, {id: "e2", kind: signal.Failure}, {id: "e3", kind: signal.Procedure}}
	ids := func(rs []ref) []string {
		var out []string
		for _, r := range rs {
			out = append(out, r.id)
		}
		return out
	}
	for kind, want := range map[store.CandidateKind][]string{
		store.CandidateCorrection: {"e1", "e2", "e3"},
		store.CandidateProcedure:  {"e1", "e2"},
	} {
		if got := ids(failures(kind, refs)); !slices.Equal(got, want) {
			t.Errorf("failures(%s) = %v, want %v", kind, got, want)
		}
	}
}
