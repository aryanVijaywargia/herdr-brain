package extract

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

// release is a release procedure that the user explains again and again (S8), in three
// wordings that the procedure signal finds similar, and the correction signal does not.
var release = []string{
	"to release: 1. run the tests with go test ./... 2. update the changelog for the new version 3. tag the release with git tag and push the tag to origin",
	"release checklist, 1. run all the tests with go test ./... first 2. update the changelog entry for this version 3. tag the release with git tag and push it",
	"how we release: 1. run the tests with go test ./... 2. write the changelog for the new version 3. make a git tag for the release and push the tag to origin",
}

// skillReply answers each prompt with one release skill claim, with each ref of the prompt as
// evidence and set over the default fields.
func skillReply(set map[string]any) func(context.Context, string) ([]byte, error) {
	return func(_ context.Context, prompt string) ([]byte, error) {
		c := replyOf(map[string]any{"kind": "skill", "situation": "releasing", "rule": "follow the release steps",
			"scope_guess": "global", "evidence_refs": promptRefs(prompt), "name": "release",
			"description": "Release a new version.", "required_tools": "git, go", "example": "Release v1.2.0.",
			"steps": "1. Run go test ./...\n2. Update the changelog.\n3. Tag and push."})
		for k, v := range set {
			c[k] = v
		}
		return claudeReply(map[string]any{"claims": []any{c}})
	}
}

// releaseSession stores one ended session in ws-A with the three release explanations.
func (f *fixture) releaseSession() {
	f.t.Helper()
	f.session("r", true, line{"ws-A", release[0], t0}, line{"ws-A", release[1], t0.Add(time.Minute)},
		line{"ws-A", release[2], t0.Add(2 * time.Minute)})
}

// AC S8: a release procedure explained 3 times in a workspace gives one workspace skill draft,
// though the model guessed global. Its text is the whole skill file, and its diff adds it.
func TestSkillDraft(t *testing.T) {
	f := newFixture(t)
	f.cli.reply = skillReply(nil)
	f.releaseSession()
	f.mustRun()
	if !strings.Contains(f.cli.prompts[0], skillHelp) {
		t.Errorf("prompt lacks the skill help:\n%s", f.cli.prompts[0])
	}
	ps := f.proposals()
	if len(ps) != 1 {
		t.Fatalf("proposals = %+v, want one skill draft", ps)
	}
	sk, err := store.ParseSkill(ps[0].Text)
	if err != nil || sk.Name != "release" || sk.Example != "Release v1.2.0." || !strings.Contains(sk.Steps, "Tag and push") {
		t.Fatalf("ParseSkill(draft) = %+v, %v, want the release skill", sk, err)
	}
	if ps[0].Scope != store.ScopeWorkspace || ps[0].Diff != store.SkillDiff("", ps[0].Text) {
		t.Errorf("draft scope %s, diff %q, want workspace and the whole text added", ps[0].Scope, ps[0].Diff)
	}
	if n := f.count(`SELECT COUNT(*) FROM items WHERE kind = 'skill' AND state = 'draft' AND scope = 'workspace' AND workspace_key = 'ws-A'`); n != 1 {
		t.Errorf("draft skill items in ws-A = %d, want 1", n)
	}
	if n := f.count(`SELECT COUNT(*) FROM candidates WHERE kind = 'procedure' AND count = 3`); n != 1 {
		t.Errorf("procedure candidates with 3 occurrences = %d, want 1", n)
	}
}

// AC: offline tests with a fake CLI: a skill reply with no example, or with a bad name, is
// refused, and gives no proposal.
func TestSkillReplyRefused(t *testing.T) {
	for name, set := range map[string]map[string]any{
		"no example":    {"example": nil},
		"empty example": {"example": "  "},
		"bad name":      {"name": "Release Notes"},
		"long name":     {"name": strings.Repeat("a", 41)},
		"no name":       {"name": nil},
		"no steps":      {"steps": nil},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			f.cli.reply = skillReply(set)
			f.releaseSession()
			f.mustRun()
			if bs := f.batches(); len(bs) != 1 || bs[0].Status != store.BatchInvalid {
				t.Errorf("batches = %+v, want one invalid batch", bs)
			}
			if n := f.count(`SELECT COUNT(*) FROM proposals`) + f.count(`SELECT COUNT(*) FROM items`); n != 0 {
				t.Errorf("proposals and items = %d, want none", n)
			}
		})
	}
}

// The secret filter runs on the skill fields before Brain stores them.
func TestSkillSecret(t *testing.T) {
	f := newFixture(t)
	f.cli.reply = skillReply(map[string]any{"steps": "1. Export TOKEN=sk-ant-abcdefghijklmnopqrstuvwxyz\n2. Tag."})
	f.releaseSession()
	f.mustRun()
	ps := f.proposals()
	if len(ps) != 1 || strings.Contains(ps[0].Text+ps[0].Diff, "sk-ant") || !strings.Contains(ps[0].Text, "[secret]") {
		t.Errorf("proposals = %+v, want one draft with the secret replaced", ps)
	}
}

// skillItem stores an active skill item of the release claim in ws-A at revision 2 (a draft
// and its approval), and returns its id.
func (f *fixture) skillItem() int64 {
	f.t.Helper()
	_, item := f.claimItem("releasing", "follow the release steps")
	key := "ws-A"
	text := releaseSkill.Text()
	f.write(func(ctx context.Context, tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `UPDATE items SET kind = 'skill', current_revision = 2 WHERE id = ?`, item); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE item_revisions SET text = ? WHERE item_id = ?`, text, item); err != nil {
			return err
		}
		return store.InsertItemRevision(ctx, tx, store.ItemRevision{ItemID: item, Revision: 2, Text: text,
			Scope: store.ScopeWorkspace, WorkspaceKey: &key, State: store.StateActive, Author: store.AuthorYou,
			Origin: store.OriginReview, Created: t0})
	})
	return item
}

var releaseSkill = store.Skill{Name: "release", Description: "Release a new version.", RequiredTools: "git, go",
	Example: "Release v1.2.0.", Steps: "1. Run go test ./...\n2. Update the changelog.\n3. Tag and push."}

// A known skill claim: the new evidence attaches to the claim, and the duplicate proposal
// drops (§7.3). Brain makes no patch by itself.
func TestSkillKnownClaim(t *testing.T) {
	f := newFixture(t)
	f.skillItem()
	f.cli.reply = skillReply(map[string]any{"steps": "1. Run the tests.\n2. Tag."})
	f.releaseSession()
	f.mustRun()
	if n := f.count(`SELECT COUNT(*) FROM proposals`); n != 0 {
		t.Errorf("proposals = %d, want none", n)
	}
	if n := f.count(`SELECT COUNT(*) FROM candidates WHERE count = 3`); n != 1 {
		t.Errorf("candidates with the 3 occurrences = %d, want 1", n)
	}
	if n := f.count(`SELECT COUNT(*) FROM item_revisions`); n != 2 {
		t.Errorf("item revisions = %d, want 2: the skill does not change", n)
	}
}

// AC: two comebacks on one skill revision give one change request and one patch draft, whose
// diff holds only the changed line. The skill does not change until you approve.
func TestSkillComebackPatch(t *testing.T) {
	f := newFixture(t)
	item := f.skillItem()
	// The comebacks are corrections of the release claim.
	f.cli.reply = func(_ context.Context, prompt string) ([]byte, error) {
		return claudeReply(map[string]any{"claims": []any{replyOf(map[string]any{"situation": "releasing",
			"rule": "follow the release steps", "evidence_refs": promptRefs(prompt)})}})
	}
	f.comebacks(item, 2, 2)
	if n := f.count(fmt.Sprintf(`SELECT COUNT(*) FROM change_requests WHERE source = 'item' AND source_id = %d AND revision = 2`, item)); n != 1 ||
		f.count(`SELECT COUNT(*) FROM change_requests`) != 1 {
		t.Fatalf("item change requests = %d, want one", n)
	}

	f.cli.reply = skillReply(map[string]any{"steps": "1. Run go test ./...\n2. Update the changelog and the README.\n3. Tag and push."})
	f.mustRun()
	prompt := f.cli.prompts[len(f.cli.prompts)-1]
	if !strings.Contains(prompt, "The proposal is a skill") || !strings.Contains(prompt, "name: release") {
		t.Errorf("redraft prompt lacks the skill:\n%s", prompt)
	}
	ps := f.proposals()
	if len(ps) != 1 || *ps[0].ItemID != item || *ps[0].BaseRevision != 2 || ps[0].Status != store.ProposalPending {
		t.Fatalf("proposals = %+v, want one pending patch of revision 2", ps)
	}
	if want := "@@ line 9\n- 2. Update the changelog.\n+ 2. Update the changelog and the README."; ps[0].Diff != want {
		t.Errorf("patch diff = %q, want %q", ps[0].Diff, want)
	}
	if _, err := store.ParseSkill(ps[0].Text); err != nil {
		t.Errorf("patch text: %v", err)
	}
	if n := f.count(`SELECT COUNT(*) FROM items WHERE current_revision = 2 AND state = 'active'`); n != 1 ||
		f.count(`SELECT COUNT(*) FROM item_revisions`) != 2 {
		t.Error("the skill changed before an approval")
	}
}

// A redraft of a skill must be a skill: another kind is an invalid response, and gives no
// draft.
func TestSkillRedraftKind(t *testing.T) {
	f := newFixture(t)
	item := f.skillItem()
	f.cli.reply = func(_ context.Context, prompt string) ([]byte, error) {
		return claudeReply(map[string]any{"claims": []any{replyOf(map[string]any{"situation": "releasing",
			"rule": "follow the release steps", "evidence_refs": promptRefs(prompt)})}})
	}
	f.comebacks(item, 2, 2)
	f.mustRun()
	if n := f.count(`SELECT COUNT(*) FROM change_requests WHERE status = 'invalid'`); n != 1 {
		t.Errorf("invalid change requests = %d, want 1", n)
	}
	if n := f.count(`SELECT COUNT(*) FROM proposals`); n != 0 {
		t.Errorf("proposals = %d, want none", n)
	}
}
