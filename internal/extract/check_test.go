package extract

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

// replyOf is a claude result with one claim, with the fields of claimOf and set on top.
func replyOf(set map[string]any) map[string]any {
	c := map[string]any{"kind": "correction", "situation": "installing packages", "rule": "use pnpm",
		"scope_guess": "workspace", "evidence_refs": []string{}, "same_as": nil, "conflicts_with": nil,
		"touches_permission": false, "expected_benefit": "no npm lockfile"}
	for k, v := range set {
		c[k] = v
	}
	return c
}

// promptRefs returns the ref ids of prompt.
func promptRefs(prompt string) []string {
	refs := []string{}
	for _, m := range refID.FindAllStringSubmatch(prompt, -1) {
		refs = append(refs, m[1])
	}
	return refs
}

// activeItem adds an active item of text with a claim of its own, global when key is nil.
func (f *fixture) activeItem(key *string, text string) {
	f.t.Helper()
	scope := store.ScopeGlobal
	if key != nil {
		scope = store.ScopeWorkspace
	}
	f.write(func(ctx context.Context, tx *sql.Tx) error {
		claim, err := store.ClaimID(ctx, tx, store.ClaimKey{Situation: "item", Rule: text}, t0)
		if err != nil {
			return err
		}
		id, err := store.InsertItem(ctx, tx, store.Item{ClaimID: claim, CurrentRevision: 1, Kind: store.KindFact,
			Scope: scope, WorkspaceKey: key, State: store.StateActive, Created: t0})
		if err != nil {
			return err
		}
		return store.InsertItemRevision(ctx, tx, store.ItemRevision{ItemID: id, Revision: 1, Text: text, Scope: scope,
			WorkspaceKey: key, State: store.StateActive, Author: store.AuthorYou, Origin: store.OriginRemember,
			ConflictsWith: []int64{}, Created: t0})
	})
}

// rejected adds a claim with rule, rejected in the scope of key ("" for global).
func (f *fixture) rejected(key, rule string) {
	f.t.Helper()
	scope := store.ScopeWorkspace
	if key == "" {
		scope = store.ScopeGlobal
	}
	f.write(func(ctx context.Context, tx *sql.Tx) error {
		claim, err := store.ClaimID(ctx, tx, store.ClaimKey{Situation: "rejected", Rule: rule}, t0)
		if err != nil {
			return err
		}
		_, err = store.Reject(ctx, tx, store.Candidate{ClaimID: claim, Scope: scope, WorkspaceKey: key}, "no", t0)
		return err
	})
}

// The input of a batch in workspace A holds the claim keys, brief items and excerpts of A and
// global, and nothing of workspace B (§7.3).
func TestInputScope(t *testing.T) {
	f := newFixture(t)
	f.cli.reply = func(ctx context.Context, prompt string) ([]byte, error) {
		if strings.Contains(prompt, "tabs in go files") {
			return claimOf("use tabs")(ctx, prompt)
		}
		// A global candidate from B is not approved global context.
		return claudeReply(map[string]any{"claims": []any{replyOf(map[string]any{"rule": "run make lint",
			"scope_guess": "global", "evidence_refs": promptRefs(prompt)})}})
	}
	f.session("a", true, line{"ws-A", "no, use tabs in go files", t0},
		line{"ws-B", "wrong, run make lint first", t0.Add(time.Minute)})
	f.mustRun()
	if n := f.count(`SELECT COUNT(*) FROM candidates WHERE scope = 'global'`); n != 1 {
		t.Fatalf("global candidates = %d, want 1", n)
	}
	a, b := "ws-A", "ws-B"
	f.activeItem(&a, "brief item of A")
	f.activeItem(&b, "brief item of B")
	f.activeItem(nil, "global brief item")
	f.rejected("ws-A", "rejected in A")
	f.rejected("ws-B", "rejected in B")
	f.rejected("", "rejected globally")
	f.session("b", true, line{"ws-A", "no, use tabs in go files again", t0.Add(2 * time.Minute)},
		line{"ws-B", "wrong, run make lint first again", t0.Add(3 * time.Minute)})
	f.mustRun()
	if f.cli.calls() != 4 {
		t.Fatalf("calls = %d, want 4", f.cli.calls())
	}
	var inA string
	for _, p := range f.cli.prompts[2:] {
		if strings.Contains(p, "tabs in go files again") {
			inA = p
		}
	}
	for _, want := range []string{"rule: use tabs", "brief item of A", "global brief item", "rule: rejected in A",
		"rule: rejected globally", "rule: brief item of A", "rule: global brief item"} {
		if !strings.Contains(inA, want) {
			t.Errorf("the A input has no %q:\n%s", want, inA)
		}
	}
	for _, not := range []string{"lint", "of B", "in B"} {
		if strings.Contains(inA, not) {
			t.Errorf("the A input has %q of workspace B:\n%s", not, inA)
		}
	}
}

// A reply with an evidence ref from another workspace is refused: the batch is invalid, it adds
// no evidence and keeps its watermark (AC 1). The fake CLI counts the model calls (AC 3).
func TestEvidenceFromAnotherWorkspace(t *testing.T) {
	f := newFixture(t)
	_, inB := f.session("b", true, line{"ws-B", "wrong, run make lint first", t0})
	sid, _ := f.session("a", true, line{"ws-A", pnpm, t0.Add(time.Minute)})
	f.cli.reply = func(_ context.Context, prompt string) ([]byte, error) {
		if !strings.Contains(prompt, "pnpm") {
			return claudeReply(map[string]any{"claims": []any{}})
		}
		refs := append(promptRefs(prompt), fmt.Sprintf("e%d", inB[0]))
		return claudeReply(map[string]any{"claims": []any{replyOf(map[string]any{"evidence_refs": refs})}})
	}
	f.mustRun()
	var bad store.Batch
	for _, b := range f.batches() {
		if b.SessionID == sid {
			bad = b
		}
	}
	if f.cli.calls() != 2 || bad.Status != store.BatchInvalid || bad.ModelCalls != 1 {
		t.Fatalf("calls %d, batch of A %+v, want 2 calls and an invalid batch with 1 model call", f.cli.calls(), bad)
	}
	if n := f.count(`SELECT COUNT(*) FROM evidence_links`); n != 0 {
		t.Errorf("evidence links = %d, want 0", n)
	}
	if wm := f.watermark(sid, "ws-A"); wm != 0 {
		t.Errorf("watermark A = %d, want 0", wm)
	}

	// A ref of the input that is from another workspace is refused too.
	j := job{batch: store.Batch{WorkspaceKey: "ws-A"}, refs: []ref{{id: "e1", workspace: "ws-A"}, {id: "e2", workspace: "ws-B"}}}
	out := fmt.Sprintf(`{"claims":[%s]}`, mustJSON(t, replyOf(map[string]any{"evidence_refs": []string{"e1", "e2"}})))
	if _, err := check(json.RawMessage(out), j); !errors.Is(err, ErrInvalid) {
		t.Errorf("check(ref of ws-B) = %v, want ErrInvalid", err)
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// A reply that says touches_permission = false for "always allow push" still gets the warning:
// Brain checks the claim text with the E4 words (AC 2).
func TestPermissionKeywordCheck(t *testing.T) {
	f := newFixture(t)
	f.session("z", true, line{"ws-A", pnpm, t0})
	f.session("a", true, line{"ws-A", pnpm, t0.Add(time.Minute)})
	f.cli.reply = claimOf("always allow push")
	f.mustRun()
	if f.cli.calls() != 2 {
		t.Fatalf("calls = %d, want 2", f.cli.calls())
	}
	if n := f.count(`SELECT COUNT(*) FROM proposals WHERE permission_warning = 1`); n != 1 {
		t.Errorf("proposals with the warning = %d, want 1", n)
	}

	j := job{refs: []ref{{id: "e1"}}}
	for rule, want := range map[string]bool{"always allow push": true, "use pnpm": false} {
		out := fmt.Sprintf(`{"claims":[%s]}`, mustJSON(t, replyOf(map[string]any{"rule": rule, "evidence_refs": []string{"e1"}})))
		claims, err := check(json.RawMessage(out), j)
		if err != nil || claims[0].TouchesPermission != want {
			t.Errorf("check(%q) = %+v, %v, want the permission flag %v", rule, claims, err, want)
		}
	}
}

// same_as and conflicts_with must name a claim id of the input, or be null.
func TestCheckKnownClaims(t *testing.T) {
	j := job{refs: []ref{{id: "e1"}}, known: []store.Claim{{ID: "cl_a"}, {ID: "cl_b"}}}
	claim := func(sameAs, conflicts any) json.RawMessage {
		return json.RawMessage(fmt.Sprintf(`{"claims":[%s]}`, mustJSON(t, replyOf(map[string]any{
			"evidence_refs": []string{"e1"}, "same_as": sameAs, "conflicts_with": conflicts}))))
	}
	claims, err := check(claim("cl_a", "cl_b"), j)
	if err != nil || claims[0].SameAs != "cl_a" || claims[0].ConflictsWith != "cl_b" {
		t.Fatalf("check(known) = %+v, %v", claims, err)
	}
	if claims, err := check(claim(nil, nil), j); err != nil || claims[0].SameAs != "" || claims[0].ConflictsWith != "" {
		t.Errorf("check(null) = %+v, %v", claims, err)
	}
	for name, out := range map[string]json.RawMessage{
		"unknown same_as":        claim("cl_other", nil),
		"unknown conflicts_with": claim(nil, "cl_other"),
		"bad conflicts_with":     claim(nil, 1),
	} {
		if _, err := check(out, j); !errors.Is(err, ErrInvalid) {
			t.Errorf("check(%s) = %v, want ErrInvalid", name, err)
		}
	}
}

var knownPnpm = regexp.MustCompile(`\[(cl_[0-9a-f]+)\] situation: installing packages; rule: (use pnpm|always allow pnpm)`)

// A known claim by the same claim id attaches its new evidence to its candidate first, then
// makes no second proposal (§7.3).
func TestKnownClaim(t *testing.T) {
	f := newFixture(t)
	f.session("z", true, line{"ws-A", pnpm, t0})
	f.session("a", true, line{"ws-A", pnpm, t0.Add(time.Minute)})
	f.mustRun()
	if n := f.count(`SELECT COUNT(*) FROM proposals`); n != 1 {
		t.Fatalf("proposals = %d, want 1", n)
	}
	f.session("c", true, line{"ws-A", pnpm, t0.Add(3 * time.Minute)})
	f.mustRun()
	if n := f.count(`SELECT count FROM candidates`); n != 3 || f.cli.calls() != 3 {
		t.Errorf("evidence %d, calls %d, want 3, 3", n, f.cli.calls())
	}
	if n := f.count(`SELECT COUNT(*) FROM proposals`); n != 1 {
		t.Errorf("proposals = %d, want 1", n)
	}
}

// A same_as reply keeps a proposed link and both claim ids (§5.4, §7.2, S23). A link to an
// open claim of the same kind in the same scope merges: the evidence goes to the known
// candidate, marked with the new claim id, so the merge can be undone (#279). A link of two
// permission rules merges nothing: the evidence goes to the candidate of the new claim, and
// the known candidate keeps its count.
func TestSameAsIsProposed(t *testing.T) {
	for name, tc := range map[string]struct {
		rules  [2]string
		merged bool
	}{
		"reworded":         {[2]string{"use pnpm", "always pick pnpm"}, true},
		"permission rules": {[2]string{"always allow pnpm", "always allow pnpm installs"}, false},
	} {
		t.Run(name, func(t *testing.T) {
			rules := tc.rules
			f := newFixture(t)
			f.cli.reply = claimOf(rules[0])
			f.session("z", true, line{"ws-A", pnpm, t0})
			f.session("a", true, line{"ws-A", pnpm, t0.Add(time.Minute)})
			f.mustRun()
			known := f.count(`SELECT count FROM candidates`)
			f.cli.reply = func(_ context.Context, prompt string) ([]byte, error) {
				m := knownPnpm.FindStringSubmatch(prompt)
				if m == nil {
					return nil, fmt.Errorf("the input has no known claim:\n%s", prompt)
				}
				return claudeReply(map[string]any{"claims": []any{replyOf(map[string]any{"rule": rules[1],
					"evidence_refs": promptRefs(prompt), "same_as": m[1]})}})
			}
			f.session("b", true, line{"ws-A", pnpm, t0.Add(2 * time.Minute)})
			f.mustRun()
			if f.cli.calls() != 3 {
				t.Fatalf("calls = %d, want 3", f.cli.calls())
			}
			q := `SELECT COUNT(*) FROM claims n JOIN claims o ON o.id = n.same_as WHERE n.rule = '` + rules[1] +
				`' AND n.same_as_status = 'proposed' AND o.rule = '` + rules[0] + `'`
			if n := f.count(q); n != 1 {
				t.Errorf("proposed links = %d, want 1", n)
			}
			wantKnown, wantNew := known, 1
			if tc.merged {
				wantKnown, wantNew = known+1, 0
			}
			if n := f.count(`SELECT c.count FROM candidates c JOIN claims k ON k.id = c.claim_id WHERE k.rule = '` +
				rules[0] + `'`); n != wantKnown {
				t.Errorf("known candidate count = %d, want %d", n, wantKnown)
			}
			if n := f.count(`SELECT COALESCE(SUM(c.count), 0) FROM candidates c JOIN claims k ON k.id = c.claim_id
				WHERE k.rule = '` + rules[1] + `'`); n != wantNew {
				t.Errorf("new candidate count = %d, want %d", n, wantNew)
			}
			if n := f.count(`SELECT COUNT(*) FROM evidence_links e JOIN claims k ON k.id = e.via_claim WHERE k.rule = '` +
				rules[1] + `'`); n != 1-wantNew {
				t.Errorf("merged evidence = %d, want %d", n, 1-wantNew)
			}
			if n := f.count(`SELECT COUNT(*) FROM proposals`); n != 1 {
				t.Errorf("proposals = %d, want 1", n)
			}
		})
	}
}

// Every model text goes through the secret filter before it is stored.
func TestModelTextFiltered(t *testing.T) {
	f := newFixture(t)
	f.session("z", true, line{"ws-A", pnpm, t0})
	f.session("a", true, line{"ws-A", pnpm, t0.Add(time.Minute)})
	secret := "sk-ant-abcdefghijklmnopqrstuvwxyz"
	f.cli.reply = func(_ context.Context, prompt string) ([]byte, error) {
		return claudeReply(map[string]any{"claims": []any{replyOf(map[string]any{"situation": "token " + secret,
			"expected_benefit": "keeps " + secret, "evidence_refs": promptRefs(prompt)})}})
	}
	f.mustRun()
	if n := f.count(`SELECT COUNT(*) FROM proposals`); n != 1 {
		t.Fatalf("proposals = %d, want 1", n)
	}
	for _, q := range []string{`SELECT COUNT(*) FROM claims WHERE situation LIKE '%sk-ant%'`,
		`SELECT COUNT(*) FROM proposals WHERE text LIKE '%sk-ant%' OR expected_benefit LIKE '%sk-ant%' OR diff LIKE '%sk-ant%'`,
		`SELECT COUNT(*) FROM item_revisions WHERE text LIKE '%sk-ant%'`,
		`SELECT COUNT(*) FROM extraction_batches WHERE reason LIKE '%sk-ant%'`} {
		if n := f.count(q); n != 0 {
			t.Errorf("%s = %d, want 0", q, n)
		}
	}
}

// A forget of a brief item of the input during the call discards the whole result; the next
// pass extracts again (§6.7, §7.3).
func TestForgetOfInputItem(t *testing.T) {
	f := newFixture(t)
	a := "ws-A"
	f.activeItem(&a, "brief item of A")
	sid, obs := f.session("a", true, line{"ws-A", pnpm, t0})
	f.cli.reply = func(ctx context.Context, prompt string) ([]byte, error) {
		if !strings.Contains(prompt, "brief item of A") {
			t.Errorf("the input has no brief item:\n%s", prompt)
		}
		f.write(func(ctx context.Context, tx *sql.Tx) error { return store.EraseItem(ctx, tx, 1, t0) })
		return claimOf("use pnpm")(ctx, prompt)
	}
	f.mustRun()
	if bs := f.batches(); len(bs) != 1 || bs[0].Status != store.BatchDiscarded || f.watermark(sid, "ws-A") != 0 {
		t.Fatalf("batches %+v, want one discarded batch and no watermark", bs)
	}
	if n := f.count(`SELECT COUNT(*) FROM evidence_links`); n != 0 {
		t.Errorf("evidence links = %d, want 0", n)
	}
	f.cli.reply = claimOf("use pnpm")
	f.mustRun()
	if bs := f.batches(); f.cli.calls() != 2 || bs[0].Status != store.BatchCommitted || f.watermark(sid, "ws-A") != obs[0] {
		t.Errorf("calls %d, batches %+v, want 2 calls and a committed batch", f.cli.calls(), bs)
	}
}
