package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"unicode"

	"github.com/BurntSushi/toml"

	"github.com/aryanVijaywargia/herdr-brain/internal/fingerprint"
)

// promptSet is evals/prompts.toml, the labeled prompt set (HLD §12.1, S71).
type promptSet struct {
	Proposal []setProposal `toml:"proposal"`
	Prompt   []setCase     `toml:"prompt"`
	Near     []setCase     `toml:"near"`
}

// setProposal is a fixture proposal and its trigger prompts.
type setProposal struct {
	ID        string   `toml:"id"`
	Scope     Scope    `toml:"scope"`
	Workspace string   `toml:"workspace"`
	Triggers  []string `toml:"triggers"`
}

// setCase is a labeled prompt: a [[prompt]] or a [[near]] entry.
type setCase struct {
	Text      string  `toml:"text"`
	Workspace *string `toml:"workspace"` // nil is ws-A, "" a workspace with no Brain key
	Label     string  `toml:"label"`     // a proposal id or noMatch
	Kind      string  `toml:"kind"`
	Proposal  string  `toml:"proposal"` // the proposal that a negation or arguments case tests
	Part      string  `toml:"part"`
}

const (
	noMatch               = "no match"
	partTune, partHeldOut = "tune", "held-out"
)

// setKinds are the kinds of the [[prompt]] cases (§12.1).
var setKinds = []string{"repeat", "language", "unicode", "everyday", "negation", "arguments", "repo", "unrelated"}

func (c setCase) workspace() string {
	if c.Workspace == nil {
		return "ws-A"
	}
	return *c.Workspace
}

func loadPromptSet(t *testing.T) promptSet {
	t.Helper()
	var set promptSet
	if _, err := toml.DecodeFile(filepath.Join("..", "..", "evals", "prompts.toml"), &set); err != nil {
		t.Fatalf("read evals/prompts.toml: %v", err)
	}
	return set
}

// labeledStore is a temp store with the fixture proposals of a prompt set and the index rows
// of their trigger prompts.
type labeledStore struct {
	*promptStore
	ids map[string]int64 // proposal id by fixture id
}

func newLabeledStore(t *testing.T, set promptSet) *labeledStore {
	t.Helper()
	l := &labeledStore{promptStore: newPromptStore(t), ids: map[string]int64{}}
	ctx := context.Background()
	write(t, l.Store, func(tx *sql.Tx) error {
		for _, sp := range set.Proposal {
			claim, err := ClaimID(ctx, tx, ClaimKey{"prompt set", sp.ID}, testNow)
			if err != nil {
				return err
			}
			var cand int64
			for _, trigger := range sp.Triggers {
				o, err := l.prompt(ctx, tx, sp.Workspace, trigger)
				if err != nil {
					return err
				}
				if cand, err = Attach(ctx, tx, Candidate{ClaimID: claim, Scope: sp.Scope, WorkspaceKey: sp.Workspace,
					Kind: CandidateProcedure}, o, DefaultGates(), supportsForTest); err != nil {
					return err
				}
			}
			var key *string
			if sp.Scope == ScopeWorkspace {
				key = &sp.Workspace
			}
			item, err := InsertItem(ctx, tx, Item{ClaimID: claim, CurrentRevision: 1, Kind: KindPreference,
				Scope: sp.Scope, WorkspaceKey: key, State: StateDraft, Created: testNow})
			if err != nil {
				return err
			}
			if l.ids[sp.ID], err = ProposeCandidate(ctx, tx, cand, Proposal{ItemID: &item, BaseRevision: ptr(int64(1)),
				Text: sp.ID, Scope: sp.Scope, Target: string(sp.Scope), Diff: "+ " + sp.ID, Status: ProposalPending,
				Created: testNow}); err != nil {
				return err
			}
		}
		return nil
	})
	return l
}

// want is the result that label asks for.
func (l *labeledStore) want(t *testing.T, label string) []int64 {
	t.Helper()
	if label == noMatch {
		return nil
	}
	id, ok := l.ids[label]
	if !ok {
		t.Fatalf("label %q names no proposal", label)
	}
	return []int64{id}
}

// The set has every kind of case of §12.1 in both parts, a negation and an arguments case for
// each proposal, and labels that name a fixture proposal.
func TestPromptSetCoverage(t *testing.T) {
	set := loadPromptSet(t)
	ids := map[string]bool{}
	for _, sp := range set.Proposal {
		if ids[sp.ID] || len(sp.Triggers) == 0 || (sp.Scope != ScopeGlobal && sp.Scope != ScopeWorkspace) || sp.Workspace == "" {
			t.Errorf("bad proposal %+v", sp)
		}
		ids[sp.ID] = true
	}
	parts := map[string]map[string]bool{partTune: {}, partHeldOut: {}}
	tested := map[string]map[string]bool{"negation": {}, "arguments": {}}
	for _, c := range append(slices.Clone(set.Prompt), set.Near...) {
		if (c.Label != noMatch && !ids[c.Label]) || parts[c.Part] == nil {
			t.Errorf("bad case %+v", c)
		}
	}
	for _, c := range set.Prompt {
		if !slices.Contains(setKinds, c.Kind) {
			t.Errorf("case %q has kind %q, want one of %v", c.Text, c.Kind, setKinds)
		}
		parts[c.Part][c.Kind] = true
		if tested[c.Kind] != nil {
			if !ids[c.Proposal] || c.Label != noMatch {
				t.Errorf("%s case %q tests proposal %q with label %q", c.Kind, c.Text, c.Proposal, c.Label)
			}
			tested[c.Kind][c.Proposal] = true
		}
	}
	for part, kinds := range parts {
		for _, k := range setKinds {
			if !kinds[k] {
				t.Errorf("part %q has no %s case", part, k)
			}
		}
	}
	for kind, props := range tested {
		for id := range ids {
			if !props[id] {
				t.Errorf("proposal %q has no %s case", id, kind)
			}
		}
	}
	for _, part := range []string{partTune, partHeldOut} {
		if !slices.ContainsFunc(set.Near, func(c setCase) bool { return c.Part == part }) {
			t.Errorf("part %q has no near case", part)
		}
	}
}

// AC (S71): each prompt of the set gives its labeled result through MatchPrompt, the exact
// match path of the hook and brained, and through MatchPromptClose, with 0 model calls. AC:
// close match is off by default, so MatchPromptClose gives no match for each near repeat.
func TestPromptSet(t *testing.T) {
	set := loadPromptSet(t)
	l := newLabeledStore(t, set)
	ctx := context.Background()
	write(t, l.Store, func(tx *sql.Tx) error {
		for _, c := range set.Prompt {
			exact, err := MatchPrompt(ctx, tx, fingerprint.Prompt(c.Text), c.workspace())
			if err != nil {
				return err
			}
			got, err := MatchPromptClose(ctx, tx, c.Text, c.workspace())
			if err != nil {
				return err
			}
			if want := l.want(t, c.Label); !slices.Equal(exact, want) || !slices.Equal(got, want) {
				t.Errorf("%s %q in %q = %v exact, %v close, want %q %v", c.Kind, c.Text, c.workspace(), exact, got, c.Label, want)
			}
		}
		for _, c := range set.Near {
			got, err := MatchPromptClose(ctx, tx, c.Text, c.workspace())
			if err != nil {
				return err
			}
			if !CloseMatch && got != nil {
				t.Errorf("near %q in %q = %v, want no match: close match is off", c.Text, c.workspace(), got)
			}
		}
		var calls int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM extraction_calls`).Scan(&calls); err != nil {
			return err
		}
		if calls != 0 {
			t.Errorf("model calls = %d, want 0", calls)
		}
		return nil
	})
}

// closeRule is a close-match rule that Brain tried (E14-S2) [NEW]: no source gives one. match
// is its close match at threshold min, after no exact match.
type closeRule struct {
	name  string
	match func(ctx context.Context, tx *sql.Tx, text, ws string, min float64) ([]int64, error)
}

// closeRules are the rules that Brain tried. The first is the rule of MatchPromptClose.
var closeRules = []closeRule{
	{"character trigram Dice", matchClose},
	{"word Jaccard", matchWords},
}

// matchWords is the close match of the word Jaccard score: the words of fold, with the
// punctuation at their ends cut.
func matchWords(ctx context.Context, tx *sql.Tx, text, ws string, min float64) ([]int64, error) {
	words := func(s string) map[string]bool {
		set := map[string]bool{}
		for _, w := range strings.Fields(fold(s)) {
			if w = strings.TrimFunc(w, unicode.IsPunct); w != "" {
				set[w] = true
			}
		}
		return set
	}
	entries, err := PromptEntries(ctx, tx)
	if err != nil {
		return nil, err
	}
	wa := words(text)
	var ids []int64
	for _, e := range entries {
		wb, n := words(e.Text), 0
		for w := range wa {
			if wb[w] {
				n++
			}
		}
		u := len(wa) + len(wb) - n
		if len(e.ProposalIDs) == 0 || u == 0 || float64(n)/float64(u) < min {
			continue
		}
		got, err := MatchPrompt(ctx, tx, e.Hash, ws)
		if err != nil {
			return nil, err
		}
		ids = append(ids, got...)
	}
	slices.Sort(ids)
	return slices.Compact(ids), nil
}

// passes says whether rule r at threshold min passes part of set [NEW]: every [[prompt]] case
// of the part gives its label, no near repeat gives a wrong proposal, and at least one near
// repeat gives its label, or the rule adds nothing to exact match.
func (l *labeledStore) passes(t *testing.T, set promptSet, part string, r closeRule, min float64) bool {
	t.Helper()
	ctx := context.Background()
	ok, gained := true, false
	write(t, l.Store, func(tx *sql.Tx) error {
		match := func(c setCase) ([]int64, error) {
			ids, err := MatchPrompt(ctx, tx, fingerprint.Prompt(c.Text), c.workspace())
			if err != nil || ids != nil {
				return ids, err
			}
			return r.match(ctx, tx, c.Text, c.workspace(), min)
		}
		for _, c := range set.Prompt {
			if c.Part != part {
				continue
			}
			got, err := match(c)
			if err != nil {
				return err
			}
			ok = ok && slices.Equal(got, l.want(t, c.Label))
		}
		for _, c := range set.Near {
			if c.Part != part {
				continue
			}
			got, err := match(c)
			if err != nil {
				return err
			}
			right := slices.Equal(got, l.want(t, c.Label))
			gained = gained || right
			ok = ok && (right || got == nil)
		}
		return nil
	})
	return ok && gained
}

// tune returns the lowest threshold of 0.50, 0.51 … 1.00 at which rule r passes the tune
// part [NEW], and false when none passes.
func (l *labeledStore) tune(t *testing.T, set promptSet, r closeRule) (float64, bool) {
	t.Helper()
	for i := 50; i <= 100; i++ {
		if min := float64(i) / 100; l.passes(t, set, partTune, r, min) {
			return min, true
		}
	}
	return 0, false
}

// AC: the close-match threshold comes from the tune part and is checked on the held-out part
// (§12.1). closeThreshold is the threshold of the trigram rule from the tune part. The test
// fails when CloseMatch is on while that rule fails the held-out part. The other rules are a
// record of what Brain tried.
func TestCloseMatchHeldOut(t *testing.T) {
	set := loadPromptSet(t)
	l := newLabeledStore(t, set)
	for i, r := range closeRules {
		min, ok := l.tune(t, set, r)
		heldOut := ok && l.passes(t, set, partHeldOut, r, min)
		t.Logf("%s: tune part passes at %.2f: %v, held-out part passes: %v", r.name, min, ok, heldOut)
		if i > 0 {
			continue
		}
		if ok && min != closeThreshold {
			t.Errorf("closeThreshold = %.2f, want %.2f from the tune part", closeThreshold, min)
		}
		if CloseMatch && !heldOut {
			t.Errorf("CloseMatch is on while %s fails the held-out part", r.name)
		}
	}
}

// A near repeat that the trigram rule finds at closeThreshold keeps the scope check: a
// workspace proposal stays out of another workspace.
func TestMatchCloseScope(t *testing.T) {
	l := newLabeledStore(t, loadPromptSet(t))
	ctx := context.Background()
	write(t, l.Store, func(tx *sql.Tx) error {
		for _, tc := range []struct {
			ws   string
			want []int64
		}{{"ws-A", []int64{l.ids["pnpm"], l.ids["make-test"]}}, {"ws-B", []int64{l.ids["pnpm"]}}} {
			var got []int64
			for _, text := range []string{"use pnpm, not npm, when you install packages.", "run make test and fix the failures."} {
				ids, err := matchClose(ctx, tx, text, tc.ws, closeThreshold)
				if err != nil {
					return err
				}
				got = append(got, ids...)
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("matchClose in %q = %v, want %v", tc.ws, got, tc.want)
			}
		}
		return nil
	})
}
