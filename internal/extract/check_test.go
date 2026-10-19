package extract

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
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
// global, and nothing of workspace B.
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
	if _, _, err := check(json.RawMessage(out), j); !errors.Is(err, ErrInvalid) {
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
		claims, _, err := check(json.RawMessage(out), j)
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
	claims, _, err := check(claim("cl_a", "cl_b"), j)
	if err != nil || claims[0].SameAs != "cl_a" || claims[0].ConflictsWith != "cl_b" {
		t.Fatalf("check(known) = %+v, %v", claims, err)
	}
	if claims, _, err := check(claim(nil, nil), j); err != nil || claims[0].SameAs != "" || claims[0].ConflictsWith != "" {
		t.Errorf("check(null) = %+v, %v", claims, err)
	}
	for name, out := range map[string]json.RawMessage{
		"unknown same_as":        claim("cl_other", nil),
		"unknown conflicts_with": claim(nil, "cl_other"),
		"bad conflicts_with":     claim(nil, 1),
	} {
		if _, _, err := check(out, j); !errors.Is(err, ErrInvalid) {
			t.Errorf("check(%s) = %v, want ErrInvalid", name, err)
		}
	}
}

var knownPnpm = regexp.MustCompile(`\[(cl_[0-9a-f]+)\] situation: installing packages; rule: (use pnpm|always allow pnpm)`)

// A known claim by the same claim id attaches its new evidence to its candidate first, then
// makes no second proposal.
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

// A same_as reply keeps a proposed link and both claim ids. A link to an open claim of the same
// kind in the same scope merges: the evidence goes to the known candidate, marked with the new
// claim id, so the merge can be undone (#279). A link of two permission rules merges nothing:
// the evidence goes to the candidate of the new claim, and the known candidate keeps its count.
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
			if f.cli.calls() != 2 {
				t.Fatalf("calls = %d, want 2", f.cli.calls())
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
			// The merged candidate reaches the gate of 2.
			if n := f.count(`SELECT COUNT(*) FROM proposals`); n != 1-wantNew {
				t.Errorf("proposals = %d, want %d", n, 1-wantNew)
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
// pass extracts again.
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

// result is an extractor result with claims.
func result(t *testing.T, claims ...map[string]any) json.RawMessage {
	t.Helper()
	return json.RawMessage(mustJSON(t, map[string]any{"claims": claims}))
}

// A claim that fails its own check is dropped, and the other claims stay (#310 AC 1, 6): the
// three shapes of the M2 eval rerun (claim 4 of 5, claim 3, claim 1 with an empty
// evidence_refs), and an evidence ref that is not in the input. dropped holds no claim text
// (AC 4).
func TestCheckDropsBadClaim(t *testing.T) {
	j := job{refs: []ref{{id: "e1"}, {id: "e2"}}}
	good := func(rule string) map[string]any {
		return replyOf(map[string]any{"rule": rule, "evidence_refs": []string{"e1"}})
	}
	noEvidence := replyOf(map[string]any{"rule": "bad rule text"})
	unknownRef := replyOf(map[string]any{"rule": "bad rule text", "evidence_refs": []string{"e1", "e9"}})
	for name, tc := range map[string]struct {
		claims  []map[string]any
		rules   []string
		dropped []string
	}{
		"claim 4 of 5": {[]map[string]any{good("r1"), good("r2"), good("r3"), noEvidence, good("r5")},
			[]string{"r1", "r2", "r3", "r5"}, []string{"claim 4: no evidence"}},
		"claim 3": {[]map[string]any{good("r1"), good("r2"), noEvidence, good("r4")},
			[]string{"r1", "r2", "r4"}, []string{"claim 3: no evidence"}},
		"claim 1": {[]map[string]any{noEvidence, good("r2")}, []string{"r2"}, []string{"claim 1: no evidence"}},
		"unknown ref": {[]map[string]any{good("r1"), unknownRef, noEvidence},
			[]string{"r1"}, []string{"claim 2: unknown evidence ref", "claim 3: no evidence"}},
	} {
		t.Run(name, func(t *testing.T) {
			claims, dropped, err := check(result(t, tc.claims...), j)
			if err != nil {
				t.Fatalf("check = %v, want no error", err)
			}
			var rules []string
			for _, c := range claims {
				rules = append(rules, c.Rule)
			}
			if !slices.Equal(rules, tc.rules) || !slices.Equal(dropped, tc.dropped) {
				t.Errorf("check = rules %q, dropped %q, want %q, %q", rules, dropped, tc.rules, tc.dropped)
			}
		})
	}
}

// A result with claims that are all dropped stays invalid, so its batch waits for the retry of
// #263 (AC 3). A result that does not match the schema stays invalid even with a good claim
// (AC 2): a field that is not in the schema, a missing field, a kind or a scope out of its enum,
// or a field of the wrong type. A valid empty result has no claim and drops none.
func TestCheckInvalidResult(t *testing.T) {
	j := job{refs: []ref{{id: "e1"}}}
	good := replyOf(map[string]any{"evidence_refs": []string{"e1"}})
	bad := func(set map[string]any) map[string]any {
		c := replyOf(map[string]any{"evidence_refs": []string{"e1"}})
		for k, v := range set {
			c[k] = v
		}
		return c
	}
	noBenefit := bad(nil)
	delete(noBenefit, "expected_benefit")
	for name, tc := range map[string]struct {
		out  json.RawMessage
		want string
	}{
		"all dropped":       {result(t, replyOf(nil), replyOf(nil)), "claim 1: no evidence; claim 2: no evidence: "},
		"field of a claim":  {result(t, good, bad(map[string]any{"x": 1})), "result does not match the schema: "},
		"type of a field":   {result(t, bad(map[string]any{"evidence_refs": "e1"}), good), "result does not match the schema: "},
		"field of a result": {json.RawMessage(`{"claims":[` + mustJSON(t, good) + `],"x":1}`), "result does not match the schema: "},
		"missing field":     {result(t, good, noBenefit), "claim 2: a field is missing: "},
		"kind":              {result(t, good, bad(map[string]any{"kind": "tip"})), "claim 2: unknown kind: "},
		"scope":             {result(t, bad(map[string]any{"scope_guess": "team"}), good), "claim 1: unknown scope: "},
		"type of same_as":   {result(t, good, bad(map[string]any{"same_as": 1})), "claim 2: same_as: not a string or null: "},
		"type of covered_by": {result(t, good, bad(map[string]any{"covered_by": true})),
			"claim 2: covered_by: not a string or null: "},
	} {
		t.Run(name, func(t *testing.T) {
			claims, dropped, err := check(tc.out, j)
			if !errors.Is(err, ErrInvalid) || !strings.HasPrefix(err.Error(), tc.want) || claims != nil || dropped != nil {
				t.Errorf("check = %v, %q, %v, want ErrInvalid %q", claims, dropped, err, tc.want)
			}
		})
	}
	claims, dropped, err := check(json.RawMessage(`{"claims":[]}`), j)
	if err != nil || len(claims) != 0 || len(dropped) != 0 {
		t.Errorf("check(no claims) = %v, %q, %v, want none and no error", claims, dropped, err)
	}
}

// A batch with a dropped claim commits its other claims and moves its watermark, and the
// brained log gets one line with the count and the reason, no claim text (#310 AC 1, 4, 6): the
// three shapes of the M2 eval rerun. A batch with claims that are all dropped is invalid: its
// watermark stays, and the log gets no line (AC 3).
func TestBatchDropsBadClaim(t *testing.T) {
	for name, tc := range map[string]struct {
		evidence []bool // for each claim: it has the refs of the prompt as evidence
		notes    []string
	}{
		"claim 4 of 5": {[]bool{true, true, true, false, true}, []string{"batch 1: dropped 1 of 5 claims: claim 4: no evidence"}},
		"claim 3":      {[]bool{true, true, false}, []string{"batch 1: dropped 1 of 3 claims: claim 3: no evidence"}},
		"claim 1":      {[]bool{false, true}, []string{"batch 1: dropped 1 of 2 claims: claim 1: no evidence"}},
		"all bad":      {[]bool{false, false}, nil},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			sid, obs := f.session("a", true, line{"ws-A", pnpm, t0})
			f.cli.reply = func(_ context.Context, prompt string) ([]byte, error) {
				var claims []any
				for i, ok := range tc.evidence {
					refs := []string{}
					if ok {
						refs = promptRefs(prompt)
					}
					claims = append(claims, replyOf(map[string]any{"rule": fmt.Sprintf("rule %d", i+1), "evidence_refs": refs}))
				}
				return claudeReply(map[string]any{"claims": claims})
			}
			var notes []string
			p := f.pass()
			p.Note = func(line string) { notes = append(notes, line) }
			if err := p.Run(context.Background()); err != nil {
				t.Fatal(err)
			}
			kept := 0
			for _, ok := range tc.evidence {
				if ok {
					kept++
				}
			}
			status, wm := store.BatchCommitted, obs[len(obs)-1]
			if kept == 0 {
				status, wm = store.BatchInvalid, 0
			}
			if bs := f.batches(); len(bs) != 1 || bs[0].Status != status {
				t.Fatalf("batches = %+v, want one %s batch", bs, status)
			}
			if !slices.Equal(notes, tc.notes) {
				t.Errorf("notes = %q, want %q", notes, tc.notes)
			}
			if n := f.count(`SELECT COUNT(*) FROM candidates`); n != kept {
				t.Errorf("candidates = %d, want %d", n, kept)
			}
			if got := f.watermark(sid, "ws-A"); got != wm {
				t.Errorf("watermark = %d, want %d", got, wm)
			}
		})
	}
}

// A dropped reason holds no model text, so it stays one line (#310 AC 4): a skill or tool name
// that its parser refuses is not quoted, in a mixed result and in an all-dropped one.
func TestDropReasonNoModelText(t *testing.T) {
	exit := 0
	j := job{refs: []ref{{id: "e1", obs: 1, cmd: &store.CommandRecord{Argv: "pnpm test", CWD: shop, ExitStatus: &exit}}}}
	good := replyOf(map[string]any{"evidence_refs": []string{"e1"}})
	skill := replyOf(map[string]any{"kind": "skill", "evidence_refs": []string{"e1"}, "name": "-private-name",
		"description": "Write the notes", "required_tools": "none", "example": "write the notes", "steps": "1. Write."})
	tool := replyOf(map[string]any{"kind": "tool", "evidence_refs": []string{"e1"}, "name": "Private\nName",
		"description": "Run the tests", "commands": [][]string{{"pnpm", "test"}}, "timeout_s": 60,
		"effects": "runs tests", "read_only": true})
	for name, tc := range map[string]struct {
		claim  map[string]any
		reason string
	}{
		"skill": {skill, "claim 2: the skill text is not valid"},
		"tool":  {tool, "claim 2: the tool contract is not valid"},
	} {
		t.Run(name, func(t *testing.T) {
			_, dropped, err := check(result(t, good, tc.claim), j)
			if err != nil || !slices.Equal(dropped, []string{tc.reason}) {
				t.Errorf("check(good, %s) = %q, %v, want dropped %q", name, dropped, err, tc.reason)
			}
			_, _, err = check(result(t, tc.claim), j)
			if !errors.Is(err, ErrInvalid) || strings.Contains(strings.ToLower(err.Error()), "private") {
				t.Errorf("check(%s) = %v, want ErrInvalid with no model text", name, err)
			}
		})
	}
}
