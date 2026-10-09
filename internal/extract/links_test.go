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
func (f *fixture) merge(id int64) ([]int64, error) {
	f.t.Helper()
	var got []int64
	err := f.s.Write(context.Background(), func(tx *sql.Tx) (err error) {
		got, err = MergeLink(context.Background(), tx, id, store.DefaultGates(), f.now)
		return err
	})
	return got, err
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
	if err != nil || len(got) != 1 || got[0] != other {
		t.Fatalf("MergeLink = %v, %v, want candidate %d", got, err, other)
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
		other, err := KeepApart(ctx, tx, id)
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
		if _, err := KeepApart(ctx, tx, id); err == nil {
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
