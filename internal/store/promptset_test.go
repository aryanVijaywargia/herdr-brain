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
	"golang.org/x/text/unicode/norm"

	"github.com/aryanVijaywargia/herdr-brain/internal/fingerprint"
)

// promptSet is evals/prompts.toml, the labeled prompt set.
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

// setKinds are the kinds of the [[prompt]] cases.
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

// The set has every kind of case (setKinds) in both parts, a negation and an arguments case for
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

// AC: each prompt of the set gives its labeled result through MatchPrompt, the exact
// match path of the hook and brained, with 0 model calls. AC: close match is off, so each
// near repeat gives no match.
func TestPromptSet(t *testing.T) {
	set := loadPromptSet(t)
	l := newLabeledStore(t, set)
	ctx := context.Background()
	write(t, l.Store, func(tx *sql.Tx) error {
		for _, c := range append(slices.Clone(set.Prompt), set.Near...) {
			got, err := MatchPrompt(ctx, tx, fingerprint.Prompt(c.Text), c.workspace())
			if err != nil {
				return err
			}
			want := l.want(t, c.Label)
			if c.Kind == "" { // a near repeat
				want = nil
			}
			if !slices.Equal(got, want) {
				t.Errorf("%s %q in %q = %v, want %v", c.Kind, c.Text, c.workspace(), got, want)
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

// closeRule is a close-match rule that Brain tried (E14-S2): no source gives one. score
// is the similarity of two prompts, from 0 to 1.
type closeRule struct {
	name  string
	score func(a, b string) float64
}

// closeRules are the rules that Brain tried.
var closeRules = []closeRule{
	{"character trigram Dice", trigramDice},
	{"word Jaccard", wordJaccard},
}

// fold is s in NFKC and in lower case, with its white space cut to one space between words.
func fold(s string) string {
	return strings.Join(strings.Fields(strings.ToLower(norm.NFKC.String(s))), " ")
}

// trigramDice is the Dice score of the sets of runs of three characters of a and b,
// after fold. A text of fewer than three characters is its own run.
func trigramDice(a, b string) float64 {
	trigrams := func(s string) map[string]bool {
		r := []rune(fold(s))
		set := map[string]bool{}
		for i := 0; i+3 <= len(r); i++ {
			set[string(r[i:i+3])] = true
		}
		if len(r) < 3 {
			set[string(r)] = true
		}
		return set
	}
	return similarity(trigrams(a), trigrams(b), true)
}

// wordJaccard is the Jaccard score of the words of a and b after fold, with the punctuation
// at their ends cut.
func wordJaccard(a, b string) float64 {
	words := func(s string) map[string]bool {
		set := map[string]bool{}
		for _, w := range strings.Fields(fold(s)) {
			if w = strings.TrimFunc(w, unicode.IsPunct); w != "" {
				set[w] = true
			}
		}
		return set
	}
	return similarity(words(a), words(b), false)
}

// similarity is the Dice score of sets a and b, or their Jaccard score when dice is false.
func similarity(a, b map[string]bool, dice bool) float64 {
	n := 0
	for k := range a {
		if b[k] {
			n++
		}
	}
	if dice {
		return 2 * float64(n) / float64(len(a)+len(b))
	}
	if u := len(a) + len(b) - n; u > 0 {
		return float64(n) / float64(u)
	}
	return 1
}

// closeMatch is the close match of rule r at threshold after no exact match: the proposals of
// the index rows whose text scores at least threshold against text, with the scope check of
// MatchPrompt.
func closeMatch(ctx context.Context, tx *sql.Tx, entries []PromptEntry, r closeRule, threshold float64, text, ws string) ([]int64, error) {
	var ids []int64
	for _, e := range entries {
		if len(e.ProposalIDs) == 0 || r.score(text, e.Text) < threshold {
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

// passes says whether rule r at threshold passes part of set: every case of the part, a
// [[prompt]] or a near repeat, gives its label.
func (l *labeledStore) passes(t *testing.T, set promptSet, part string, r closeRule, threshold float64) bool {
	t.Helper()
	ctx := context.Background()
	ok := true
	write(t, l.Store, func(tx *sql.Tx) error {
		entries, err := PromptEntries(ctx, tx)
		if err != nil {
			return err
		}
		for _, c := range append(slices.Clone(set.Prompt), set.Near...) {
			if c.Part != part {
				continue
			}
			got, err := MatchPrompt(ctx, tx, fingerprint.Prompt(c.Text), c.workspace())
			if err == nil && got == nil {
				got, err = closeMatch(ctx, tx, entries, r, threshold, c.Text, c.workspace())
			}
			if err != nil {
				return err
			}
			ok = ok && slices.Equal(got, l.want(t, c.Label))
		}
		return nil
	})
	return ok
}

// tune returns the lowest threshold of 0.50, 0.51 … 1.00 at which rule r passes the tune
// part, and false when none passes.
func (l *labeledStore) tune(t *testing.T, set promptSet, r closeRule) (float64, bool) {
	t.Helper()
	for i := 50; i <= 100; i++ {
		if threshold := float64(i) / 100; l.passes(t, set, partTune, r, threshold) {
			return threshold, true
		}
	}
	return 0, false
}

// AC: a close-match threshold comes from the tune part, the lowest that passes it, and is
// checked on the held-out part. Close match may turn on only with a rule that passes
// both. No rule that Brain tried passes: a threshold that keeps negation and other arguments
// out also misses near repeats. So Brain has no close matcher and no switch, and MatchPrompt
// stays exact. A rule that passes fails this test: build its matcher then, behind a store
// switch that is off by default.
func TestCloseMatchHeldOut(t *testing.T) {
	set := loadPromptSet(t)
	l := newLabeledStore(t, set)
	for _, r := range closeRules {
		threshold, ok := l.tune(t, set, r)
		if !ok {
			t.Logf("%s: no threshold passes the tune part", r.name)
			continue
		}
		if l.passes(t, set, partHeldOut, r, threshold) {
			t.Errorf("%s at %.2f passes the held-out part: build its close matcher behind an off switch", r.name, threshold)
			continue
		}
		t.Logf("%s: %.2f from the tune part fails the held-out part", r.name, threshold)
	}
}
