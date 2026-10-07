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

	"github.com/aryanVijaywargia/herdr-brain/internal/signal"
	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

// The C1 and C2 claim keys of the M2 eval run (2026-10-07, B4).
const (
	c1Situation = "Writing or updating source code in the notes-api project."
	c2Situation = "writing or updating source code in notes-api"
	logRule     = "Use src/log.js for logs, not console.log."
	logText     = "no, do not use console.log, use src/log.js instead"
)

var knownLog = regexp.MustCompile(`\[(cl_[0-9a-f]+)\] situation: writing or updating source code in the notes-api project;`)

// logReply is a claude result with one correction claim of situation, with each ref of
// prompt as evidence; sameAs names the C1 claim of the input when it is true.
func logReply(situation string, sameAs bool) func(context.Context, string) ([]byte, error) {
	return func(_ context.Context, prompt string) ([]byte, error) {
		var id any
		if sameAs {
			m := knownLog.FindStringSubmatch(prompt)
			if m == nil {
				return nil, fmt.Errorf("the input has no C1 claim:\n%s", prompt)
			}
			id = m[1]
		}
		return claudeReply(map[string]any{"claims": []any{replyOf(map[string]any{"situation": situation,
			"rule": logRule, "evidence_refs": promptRefs(prompt), "same_as": id})}})
	}
}

// A correction in a claude session and a codex session with the C1 and C2 keys of the M2 eval
// run reaches one candidate with count 2, so the gate of 2 makes a proposal (#279, §7.2). The
// C2 claim keeps its own id and a proposed link, and its evidence is marked as merged.
func TestSameLessonAddsUpAcrossSessions(t *testing.T) {
	f := newFixture(t)
	f.cli.reply = logReply(c1Situation, false)
	f.harnessSession("claude", "c1", true, line{"ws-A", logText, t0})
	f.mustRun()
	f.cli.reply = logReply(c2Situation, true)
	f.harnessSession("codex", "c2", true, line{"ws-A", logText, t0.Add(time.Minute)})
	f.mustRun()
	if f.cli.calls() != 2 {
		t.Fatalf("calls = %d, want 2", f.cli.calls())
	}
	if n := f.count(`SELECT COUNT(*) FROM candidates`); n != 1 {
		t.Errorf("candidates = %d, want 1", n)
	}
	if n := f.count(`SELECT count FROM candidates WHERE harnesses = '["claude","codex"]' AND sessions = 2
		AND kind = 'correction'`); n != 2 {
		t.Errorf("candidate count = %d, want 2 from claude and codex", n)
	}
	if n := f.count(`SELECT COUNT(*) FROM claims WHERE same_as IS NOT NULL AND same_as_status = 'proposed'`); n != 1 {
		t.Errorf("proposed links = %d, want 1", n)
	}
	if n := f.count(`SELECT COUNT(*) FROM evidence_links e JOIN claims k ON k.id = e.via_claim
		WHERE k.situation = '` + normal(c2Situation) + `'`); n != 1 {
		t.Errorf("merged evidence = %d, want 1", n)
	}
	var text string
	f.write(func(ctx context.Context, tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `SELECT text FROM proposals`).Scan(&text)
	})
	if want := "When writing or updating source code in notes-api: " + logRule; text != want {
		t.Errorf("proposal text = %q, want %q", text, want)
	}
}

// The same claim key in normal form (case, spaces, end punctuation) in a later batch gets the
// same claim id, with no same_as (#279).
func TestNormalKeySameClaim(t *testing.T) {
	f := newFixture(t)
	for i, situation := range []string{"installing   packages", "Installing packages."} {
		rule := []string{"use pnpm", "Use pnpm."}[i]
		f.cli.reply = func(_ context.Context, prompt string) ([]byte, error) {
			return claudeReply(map[string]any{"claims": []any{replyOf(map[string]any{"situation": situation,
				"rule": rule, "evidence_refs": promptRefs(prompt)})}})
		}
		f.session(fmt.Sprintf("s%d", i), true, line{"ws-A", pnpm, t0.Add(time.Duration(i) * time.Minute)})
		f.mustRun()
	}
	if n := f.count(`SELECT COUNT(*) FROM claims`); n != 1 {
		t.Errorf("claims = %d, want 1", n)
	}
	if n := f.count(`SELECT count FROM candidates`); n != 2 {
		t.Errorf("candidate count = %d, want 2", n)
	}
	// The item text of a situation that ends in "." reads clean (#284).
	var text string
	f.write(func(ctx context.Context, tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `SELECT text FROM item_revisions`).Scan(&text)
	})
	if want := "When installing packages: Use pnpm."; text != want {
		t.Errorf("item revision text = %q, want %q", text, want)
	}
}

// A same_as to a claim of another kind stays a proposed link and merges nothing (§7.2).
func TestSameAsOtherKindIsNotMerged(t *testing.T) {
	f := newFixture(t)
	f.cli.reply = logReply(c1Situation, false)
	f.session("c1", true, line{"ws-A", logText, t0})
	f.mustRun()
	f.cli.reply = func(_ context.Context, prompt string) ([]byte, error) {
		m := knownLog.FindStringSubmatch(prompt)
		if m == nil {
			return nil, fmt.Errorf("the input has no C1 claim:\n%s", prompt)
		}
		return claudeReply(map[string]any{"claims": []any{replyOf(map[string]any{"kind": "procedure",
			"situation": c2Situation, "rule": logRule, "evidence_refs": promptRefs(prompt), "same_as": m[1]})}})
	}
	f.session("c2", true, line{"ws-A", logText, t0.Add(time.Minute)})
	f.mustRun()
	if n := f.count(`SELECT COUNT(*) FROM candidates WHERE count = 1`); n != 2 {
		t.Errorf("candidates with count 1 = %d, want 2", n)
	}
	if n := f.count(`SELECT COUNT(*) FROM evidence_links WHERE via_claim IS NOT NULL`); n != 0 {
		t.Errorf("merged evidence = %d, want none", n)
	}
	if n := f.count(`SELECT COUNT(*) FROM claims WHERE same_as_status = 'proposed'`); n != 1 {
		t.Errorf("proposed links = %d, want 1", n)
	}
}

// The extractor prompt asks for a general situation with no session details (#279).
func TestPromptAsksForGeneralSituation(t *testing.T) {
	in := input(job{})
	for _, want := range []string{"no task names, version numbers, dates or ids",
		"Keep a project name only when the rule is about that project"} {
		if !strings.Contains(in, want) {
			t.Errorf("prompt has no %q", want)
		}
	}
}

// The candidate kind follows the claim, not the first signal (#280). The cases 5, 6 and 12 of
// the M2 eval run: a "From now on" lesson and a task text whose first signal was a failure,
// and a correction whose first signal was a procedure.
func TestKindFollowsClaim(t *testing.T) {
	failure := ref{kind: signal.Failure, sender: store.SenderTool}
	procedure := ref{kind: signal.Procedure, sender: store.SenderUnconfirmed}
	correction := ref{kind: signal.Correction, sender: store.SenderUnconfirmed}
	lesson := ref{kind: signal.Correction, sender: store.SenderUnconfirmed, lesson: true}
	chain := ref{kind: signal.CommandChain, sender: store.SenderTool}
	for name, tc := range map[string]struct {
		claim Claim
		want  store.CandidateKind
	}{
		"candidate 5: lesson, failure first":        {Claim{Kind: kindLesson, Refs: []ref{failure, lesson}}, store.CandidateLesson},
		"candidate 6: task text, failure first":     {Claim{Kind: "preference", Refs: []ref{failure, correction}}, store.CandidateCorrection},
		"candidate 12: correction, procedure first": {Claim{Kind: "correction", Refs: []ref{procedure}}, store.CandidateCorrection},
		"lesson with no lesson evidence":            {Claim{Kind: kindLesson, Refs: []ref{procedure}}, store.CandidateCorrection},
		"lesson evidence of a correction claim":     {Claim{Kind: "correction", Refs: []ref{lesson}}, store.CandidateCorrection},
		"procedure, correction first":               {Claim{Kind: "procedure", Refs: []ref{correction, procedure}}, store.CandidateProcedure},
		"skill, correction first":                   {Claim{Kind: kindSkill, Refs: []ref{correction}}, store.CandidateProcedure},
		"skill of a command chain":                  {Claim{Kind: kindSkill, Refs: []ref{chain}}, store.CandidateCommandChain},
		"preference from failure output alone":      {Claim{Kind: "preference", Refs: []ref{failure}}, store.CandidateFailure},
		"fact from your text":                       {Claim{Kind: "fact", Refs: []ref{procedure}}, store.CandidateCorrection},
		"tool":                                      {Claim{Kind: kindTool, Tool: "name = \"t\"", Refs: []ref{correction}}, store.CandidateCommandChain},
	} {
		if got := candidateKind(tc.claim); got != tc.want {
			t.Errorf("%s: kind = %s, want %s", name, got, tc.want)
		}
	}
}

// A same_as link merges nothing when the known claim is flagged as a permission rule by the
// model, is a claim of another claim kind with the same gate, has an item in the scope, or when
// the new claim is rejected there (#279 review). Each keeps a proposed link and two candidates.
func TestSameAsMergeStops(t *testing.T) {
	for name, tc := range map[string]struct {
		first  map[string]any
		second map[string]any
		setup  func(f *fixture)
	}{
		"known claim flagged as a permission rule": {first: map[string]any{"touches_permission": true}},
		"fact and preference":                      {first: map[string]any{"kind": "fact"}, second: map[string]any{"kind": "preference"}},
		"known claim has an item": {setup: func(f *fixture) {
			f.write(func(ctx context.Context, tx *sql.Tx) error {
				var claim string
				if err := tx.QueryRowContext(ctx, `SELECT id FROM claims`).Scan(&claim); err != nil {
					return err
				}
				key := "ws-A"
				_, err := store.InsertItem(ctx, tx, store.Item{ClaimID: claim, CurrentRevision: 1, Kind: store.KindPreference,
					Scope: store.ScopeWorkspace, WorkspaceKey: &key, State: store.StateActive, Created: t0})
				return err
			})
		}},
		"new claim rejected": {setup: func(f *fixture) {
			f.write(func(ctx context.Context, tx *sql.Tx) error {
				claim, err := store.ClaimID(ctx, tx, store.ClaimKey{Situation: normal(c2Situation), Rule: normal(logRule)}, t0)
				if err != nil {
					return err
				}
				_, err = store.Reject(ctx, tx, store.Candidate{ClaimID: claim, Scope: store.ScopeWorkspace, WorkspaceKey: "ws-A"}, "no", t0)
				return err
			})
		}},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			f.cli.reply = func(_ context.Context, prompt string) ([]byte, error) {
				set := map[string]any{"situation": c1Situation, "rule": logRule, "evidence_refs": promptRefs(prompt)}
				maps.Copy(set, tc.first)
				return claudeReply(map[string]any{"claims": []any{replyOf(set)}})
			}
			f.session("c1", true, line{"ws-A", logText, t0})
			f.mustRun()
			if tc.setup != nil {
				tc.setup(f)
			}
			f.cli.reply = func(_ context.Context, prompt string) ([]byte, error) {
				m := knownLog.FindStringSubmatch(prompt)
				if m == nil {
					return nil, fmt.Errorf("the input has no C1 claim:\n%s", prompt)
				}
				set := map[string]any{"situation": c2Situation, "rule": logRule, "evidence_refs": promptRefs(prompt), "same_as": m[1]}
				maps.Copy(set, tc.second)
				return claudeReply(map[string]any{"claims": []any{replyOf(set)}})
			}
			f.harnessSession("codex", "c2", true, line{"ws-A", logText, t0.Add(time.Minute)})
			f.mustRun()
			if n := f.count(`SELECT COUNT(*) FROM evidence_links WHERE via_claim IS NOT NULL`); n != 0 {
				t.Errorf("merged evidence = %d, want none", n)
			}
			if n := f.count(`SELECT COUNT(*) FROM candidates WHERE count = 2`); n != 0 {
				t.Errorf("candidates with count 2 = %d, want none", n)
			}
			if n := f.count(`SELECT COUNT(*) FROM claims WHERE same_as_status = 'proposed'`); n != 1 {
				t.Errorf("proposed links = %d, want 1", n)
			}
		})
	}
}

// An explicit lesson turns a candidate of tool output alone into a lesson, and the lesson
// proposal comes (#280, #253).
func TestLessonOverridesOldFailureKind(t *testing.T) {
	f := newFixture(t)
	f.cli.reply = kindOf("lesson", "every exported function gets a one-line JSDoc comment")
	f.write(func(ctx context.Context, tx *sql.Tx) error {
		claim, err := store.ClaimID(ctx, tx, store.ClaimKey{Situation: "exported functions in src/math.js",
			Rule: "every exported function gets a one-line jsdoc comment"}, t0)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO candidates (claim_id, scope, workspace_key, kind, harnesses, count,
			sessions, priority, first_seen, last_seen, status) VALUES (?, 'workspace', 'ws-A', 'failure', '[]', 0, 0, 0, ?, ?, 'open')`,
			claim, t0.Format(time.RFC3339Nano), t0.Format(time.RFC3339Nano))
		return err
	})
	f.capturedSession("s1", m2Lesson)
	f.mustRun()
	if cs := f.candidates(); len(cs) != 1 || cs[0].Kind != store.CandidateLesson || cs[0].Count != 1 {
		t.Errorf("candidates = %+v, want one lesson with count 1", cs)
	}
	if n := f.count(`SELECT COUNT(*) FROM proposals`); n != 1 {
		t.Errorf("proposals = %d, want 1", n)
	}
}
