package extract

import (
	"context"
	"database/sql"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

// m2Lesson is the prompt of the M2 real-use test (2026-10-07) that made a correction with
// count 1 and no proposal (#253).
const m2Lesson = "Remember this for this project only: every exported function in src/math.js gets a one-line " +
	"JSDoc comment above it."

// kindOf is a claude result with one claim of kind, with each ref of prompt as evidence.
func kindOf(kind, rule string) func(context.Context, string) ([]byte, error) {
	return func(_ context.Context, prompt string) ([]byte, error) {
		refs := []string{}
		for _, m := range refID.FindAllStringSubmatch(prompt, -1) {
			refs = append(refs, m[1])
		}
		return claudeReply(map[string]any{"claims": []any{map[string]any{"kind": kind,
			"situation": "exported functions in src/math.js", "rule": rule, "scope_guess": "workspace",
			"evidence_refs": refs, "same_as": nil, "conflicts_with": nil, "touches_permission": false,
			"expected_benefit": "the user does not ask again"}}})
	}
}

// capturedSession stores one ended session of agent id with texts in ws-A, as herdr capture
// stores them: the sender is unconfirmed (§6.3).
func (f *fixture) capturedSession(id string, texts ...string) {
	f.t.Helper()
	lines := make([]line, len(texts))
	for i, s := range texts {
		lines[i] = line{"ws-A", s, t0.Add(time.Duration(i) * time.Minute)}
	}
	_, obs := f.session(id, true, lines...)
	f.write(func(ctx context.Context, tx *sql.Tx) error {
		for _, o := range obs {
			if _, err := tx.ExecContext(ctx, `UPDATE observations SET sender = ? WHERE id = ?`, store.SenderUnconfirmed, o); err != nil {
				return err
			}
		}
		return nil
	})
}

func (f *fixture) candidates() []store.Candidate {
	f.t.Helper()
	var cs []store.Candidate
	f.write(func(ctx context.Context, tx *sql.Tx) (err error) {
		cs, err = store.Candidates(ctx, tx)
		return err
	})
	return cs
}

// AC #253 (2, 3): the M2 prompt, through the real signal rules, the extractor with a fake model
// reply and the candidate path, gives one workspace proposal after one session. The lesson
// kind holds, the evidence sender stays unconfirmed, and the draft waits for review.
func TestLessonProposal(t *testing.T) {
	f := newFixture(t)
	f.capturedSession("a", m2Lesson)
	f.cli.reply = kindOf(kindLesson, "add a one-line JSDoc comment above each exported function")
	f.mustRun()
	if !strings.Contains(f.cli.prompts[0], lessonHelp) || !strings.Contains(f.cli.prompts[0], m2Lesson) {
		t.Errorf("extractor prompt has no lesson help or no lesson text:\n%s", f.cli.prompts[0])
	}
	cs := f.candidates()
	if len(cs) != 1 || cs[0].Kind != store.CandidateLesson || cs[0].Count != 1 || cs[0].Priority != 1 ||
		cs[0].Status != store.CandidateProposed || cs[0].Scope != store.ScopeWorkspace || cs[0].WorkspaceKey != "ws-A" {
		t.Fatalf("candidates = %+v, want one proposed workspace lesson in ws-A with count 1", cs)
	}
	ps := f.proposals()
	if len(ps) != 1 || ps[0].Status != store.ProposalPending || ps[0].Scope != store.ScopeWorkspace ||
		ps[0].Target != "workspace w1 (ws-A)" || ps[0].CandidateID == nil || *ps[0].CandidateID != cs[0].ID {
		t.Fatalf("proposals = %+v, want one pending workspace proposal of candidate %d", ps, cs[0].ID)
	}
	var sender, state, author, kind string
	f.write(func(ctx context.Context, tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `SELECT e.sender, i.state, r.author, i.kind FROM evidence_links e, items i
			JOIN item_revisions r ON r.item_id = i.id`).Scan(&sender, &state, &author, &kind)
	})
	if sender != string(store.SenderUnconfirmed) || state != string(store.StateDraft) || author != string(store.AuthorBrain) ||
		kind != string(store.KindPreference) {
		t.Errorf("evidence sender %s, item %s %s by %s, want unconfirmed, a draft preference by brain", sender, state,
			kind, author)
	}
}

// AC #253 (1, 6): the extractor drops the false positives of the wide rule, a lesson word in a
// claim of another kind gives no lesson, and a plain correction keeps the correction kind and
// gate 2, also when the model calls it a lesson.
func TestLessonNeedsRuleAndExtractor(t *testing.T) {
	tests := []struct {
		name  string
		text  string
		reply func(context.Context, string) ([]byte, error)
		kind  store.CandidateKind // "" for no candidate
	}{
		{"never mind", "never mind", noClaims, ""},
		{"always forget", "I always forget the flag", noClaims, ""},
		{"lesson word in a correction claim", "No, never use npm here", kindOf("correction", "use pnpm"), store.CandidateCorrection},
		{"plain correction", "Do not add JSDoc to the other functions", kindOf("correction", "no JSDoc on old functions"),
			store.CandidateCorrection},
		{"plain correction called a lesson", "Do not add JSDoc to the other functions",
			kindOf(kindLesson, "no JSDoc on old functions"), store.CandidateCorrection},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			f.capturedSession("a", tt.text)
			f.cli.reply = tt.reply
			f.mustRun()
			if f.cli.calls() != 1 {
				t.Fatalf("extractor calls = %d, want 1", f.cli.calls())
			}
			cs := f.candidates()
			if tt.kind == "" {
				if len(cs) != 0 {
					t.Errorf("candidates = %+v, want none", cs)
				}
				return
			}
			if len(cs) != 1 || cs[0].Kind != tt.kind || cs[0].Count != 1 || cs[0].Priority != 0 || cs[0].Status != store.CandidateOpen {
				t.Errorf("candidates = %+v, want one open %s with count 1 below the gate", cs, tt.kind)
			}
			if ps := f.proposals(); len(ps) != 0 {
				t.Errorf("proposals = %+v, want none", ps)
			}
		})
	}
}

// Sol finding on #261: a correction with a lesson word of an approved lesson keeps its
// correction mark, so it is a comeback of the item (§5.4).
func TestLessonCorrectionComeback(t *testing.T) {
	f := newFixture(t)
	item := f.revisedItem(1)
	f.cli.reply = func(_ context.Context, prompt string) ([]byte, error) {
		return claudeReply(map[string]any{"claims": []any{replyOf(map[string]any{"kind": kindLesson,
			"evidence_refs": promptRefs(prompt)})}})
	}
	f.session("a", true, line{"ws-A", "from now on use pnpm", t0})
	f.mustRun()
	if cs := f.candidates(); len(cs) != 1 || cs[0].Kind != store.CandidateLesson {
		t.Fatalf("candidates = %+v, want one lesson", cs)
	}
	f.deliver(receiptOf("default", "b", store.ResultEmitted, t0.Add(time.Hour), store.RevisionRef{Item: item, Revision: 1}))
	f.session("b", true, line{"ws-A", "No, from now on use pnpm, not npm", t0.Add(2 * time.Hour)})
	f.mustRun()
	if got, want := f.outcomes(), []string{"comeback 1"}; !slices.Equal(got, want) {
		t.Errorf("outcomes = %v, want %v", got, want)
	}
}
