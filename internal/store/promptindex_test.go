package store

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"sort"
	"testing"
	"time"

	"github.com/aryanVijaywargia/herdr-brain/internal/fingerprint"
)

// promptStore is a store with one capture session of each workspace key, for prompt index
// tests.
type promptStore struct {
	*Store
	segments map[string]int64 // by workspace key
	n        int
}

func newPromptStore(t *testing.T) *promptStore {
	t.Helper()
	return &promptStore{Store: openM1(t), segments: map[string]int64{}}
}

// prompt stores prompt text as a prompt observation in workspace ws with its index row, in tx,
// and returns the observation id.
func (p *promptStore) prompt(ctx context.Context, tx *sql.Tx, ws, text string) (int64, error) {
	seg, ok := p.segments[ws]
	if !ok {
		var err error
		if seg, err = p.segment(ctx, tx, ws); err != nil {
			return 0, err
		}
	}
	p.n++
	o, err := AddObservation(ctx, tx, Observation{CaptureIdentity: fmt.Sprint("p", p.n), Source: SourceHook,
		SegmentID: seg, Event: "prompt", Excerpt: text, Status: ObservationComplete, Sender: SenderUnconfirmed,
		Counts: true, At: testNow})
	if err != nil {
		return 0, err
	}
	return o, IndexPrompt(ctx, tx, o, fingerprint.Prompt(text), text)
}

// segment adds workspace ws and a capture session and segment in it, in tx, and returns the
// segment id.
func (p *promptStore) segment(ctx context.Context, tx *sql.Tx, ws string) (int64, error) {
	if err := InsertWorkspace(ctx, tx, Workspace{Key: ws, HerdrSession: "default", HerdrWorkspaceID: "w-" + ws,
		Label: ws, Status: WorkspaceLive, LastSeen: testNow}); err != nil {
		return 0, err
	}
	key := SessionKey{HerdrSession: "default", Harness: "claude", Kind: KeyCapture, ID: "cap-" + ws}
	sid, err := OpenSession(ctx, tx, key, "w1:p1", ws, testNow)
	if err != nil {
		return 0, err
	}
	seg, err := StartSegment(ctx, tx, sid, "w1:p1", ws, "visible", testNow)
	p.segments[ws] = seg
	return seg, err
}

// propose stores prompt text in workspace ws-A and a pending proposal of scope that it led to,
// and returns the proposal id and its item id.
func (p *promptStore) propose(t *testing.T, text string, scope Scope) (int64, int64) {
	t.Helper()
	ws := "ws-A"
	ctx := context.Background()
	var id, item int64
	write(t, p.Store, func(tx *sql.Tx) error {
		o, err := p.prompt(ctx, tx, ws, text)
		if err != nil {
			return err
		}
		claim, err := ClaimID(ctx, tx, ClaimKey{"prompt", text}, testNow)
		if err != nil {
			return err
		}
		cand, err := Attach(ctx, tx, Candidate{ClaimID: claim, Scope: scope, WorkspaceKey: ws, Kind: CandidateProcedure},
			o, DefaultGates(), supportsForTest)
		if err != nil {
			return err
		}
		var key *string
		if scope == ScopeWorkspace {
			key = &ws
		}
		if item, err = InsertItem(ctx, tx, Item{ClaimID: claim, CurrentRevision: 1, Kind: KindPreference, Scope: scope,
			WorkspaceKey: key, State: StateDraft, Created: testNow}); err != nil {
			return err
		}
		id, err = ProposeCandidate(ctx, tx, cand, Proposal{ItemID: &item, BaseRevision: ptr(int64(1)), Text: "prompt: " + text,
			Scope: scope, Target: string(scope), Diff: "+ prompt: " + text, Status: ProposalPending, Created: testNow})
		return err
	})
	return id, item
}

// match is MatchPrompt of the fingerprint of text in workspace ws.
func (p *promptStore) match(t *testing.T, ws, text string) []int64 {
	t.Helper()
	var ids []int64
	write(t, p.Store, func(tx *sql.Tx) (err error) {
		ids, err = MatchPrompt(context.Background(), tx, fingerprint.Prompt(text), ws)
		return err
	})
	return ids
}

// AC: in the same workspace, a change of case or of inner space is no match, and the same
// prompt again is. The Unicode fixtures are Brain's own, of the three kinds of waku
// store.py:31-64: text with no spaces, accents and command punctuation. Each has no silent
// miss and no unrelated match.
func TestMatchPromptExact(t *testing.T) {
	for _, tc := range []struct {
		name, trigger, prompt string
		match                 bool
	}{
		{"same prompt", "run make Foo", "run make Foo", true},
		{"case", "run make Foo", "run make foo", false},
		{"inner space", `run echo "a  b"`, `run echo "a b"`, false},
		{"outer space", "run make Foo", "  run make Foo\n", true},
		{"no spaces, same", "テストを実行してからコミットして", "テストを実行してからコミットして", true},
		{"no spaces, other", "テストを実行してからコミットして", "テストを実行してからプッシュして", false},
		{"no spaces, prefix", "测试然后提交", "测试然后提交代码", false},
		{"accents, NFC and NFD", "résumé the café notes", "résumé the café notes", true},
		{"accents, dropped", "résumé the café notes", "resume the cafe notes", false},
		{"punctuation, same", `git commit -m "fix: x" && git push --force-with-lease`, `git commit -m "fix: x" && git push --force-with-lease`, true},
		{"punctuation, quotes", `git commit -m "fix: x"`, `git commit -m 'fix: x'`, false},
		{"punctuation, slash", "rm -rf ./build", "rm -rf ./build/", false},
		{"punctuation, operator", "make test && make lint", "make test || make lint", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newPromptStore(t)
			id, _ := p.propose(t, tc.trigger, ScopeWorkspace)
			var want []int64
			if tc.match {
				want = []int64{id}
			}
			if got := p.match(t, "ws-A", tc.prompt); !slices.Equal(got, want) {
				t.Errorf("MatchPrompt(%q) after trigger %q = %v, want %v", tc.prompt, tc.trigger, got, want)
			}
		})
	}
}

// A workspace proposal matches only in its workspace, a global one in every workspace, and a
// rejected one nowhere (the E4 scope check). A new prompt that matches links the proposal,
// and a rejected one gets no link.
func TestMatchPromptScope(t *testing.T) {
	p := newPromptStore(t)
	ws, _ := p.propose(t, "run make Foo", ScopeWorkspace)
	global, _ := p.propose(t, "run make Bar", ScopeGlobal)
	for _, tc := range []struct {
		ws, prompt string
		want       []int64
	}{
		{"ws-A", "run make Foo", []int64{ws}},
		{"ws-B", "run make Foo", nil},
		{"", "run make Foo", nil},
		{"ws-B", "run make Bar", []int64{global}},
		{"", "run make Bar", []int64{global}},
	} {
		if got := p.match(t, tc.ws, tc.prompt); !slices.Equal(got, tc.want) {
			t.Errorf("MatchPrompt(%q in %q) = %v, want %v", tc.prompt, tc.ws, got, tc.want)
		}
	}
	ctx := context.Background()
	write(t, p.Store, func(tx *sql.Tx) error {
		if _, err := p.prompt(ctx, tx, "ws-A", "run make Foo"); err != nil {
			return err
		}
		return DecideProposal(ctx, tx, global, ProposalRejected, SurfaceCLI, testNow)
	})
	if got := p.match(t, "ws-B", "run make Bar"); got != nil {
		t.Errorf("MatchPrompt of a rejected proposal = %v, want none", got)
	}
	write(t, p.Store, func(tx *sql.Tx) error {
		_, err := p.prompt(ctx, tx, "ws-A", "run make Bar")
		return err
	})
	// A pending global proposal whose item another proposal moved into ws-A matches only there.
	moved, _ := p.propose(t, "run make Baz", ScopeGlobal)
	write(t, p.Store, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE items SET scope = 'workspace', workspace_key = 'ws-A'
			WHERE id = (SELECT item_id FROM proposals WHERE id = ?)`, moved)
		return err
	})
	if got := p.match(t, "ws-B", "run make Baz"); got != nil {
		t.Errorf("MatchPrompt of a moved item in ws-B = %v, want none", got)
	}
	if got := p.match(t, "ws-A", "run make Baz"); !slices.Equal(got, []int64{moved}) {
		t.Errorf("MatchPrompt of a moved item in ws-A = %v, want [%d]", got, moved)
	}
	var es []PromptEntry
	write(t, p.Store, func(tx *sql.Tx) (err error) {
		es, err = PromptEntries(ctx, tx)
		return err
	})
	if len(es) != 5 {
		t.Fatalf("prompt index = %+v, want 5 rows", es)
	}
	for i, want := range [][]int64{{ws}, {global}, {ws}, {}} {
		if !slices.Equal(es[i].ProposalIDs, want) {
			t.Errorf("row %d links %v, want %v", i, es[i].ProposalIDs, want)
		}
	}
	if e := es[2]; e.Hash != fingerprint.Prompt("run make Foo") || e.Text != "run make Foo" || e.WorkspaceKey != "ws-A" ||
		e.SessionID == 0 || e.ObservationID == 0 || !e.At.Equal(testNow) {
		t.Errorf("row 2 = %+v, want the prompt in ws-A", e)
	}
}

// A pane observation that a hook record enriches is the evidence, and the hook record holds
// the full prompt: the trigger prompts hold the hashes of both, and both rows link the
// proposal (S35, E14-S1).
func TestTriggerPromptsEnriched(t *testing.T) {
	p := newPromptStore(t)
	ctx := context.Background()
	const part, full = "run the long deploy", "run the long deploy and then check every log of it"
	var id int64
	write(t, p.Store, func(tx *sql.Tx) error {
		pane, err := p.prompt(ctx, tx, "ws-A", part)
		if err != nil {
			return err
		}
		hook, err := p.prompt(ctx, tx, "ws-A", full)
		if err != nil {
			return err
		}
		if err := Enrich(ctx, tx, pane, hook); err != nil {
			return err
		}
		claim, err := ClaimID(ctx, tx, ClaimKey{"deploy", "check the logs"}, testNow)
		if err != nil {
			return err
		}
		cand, err := Attach(ctx, tx, Candidate{ClaimID: claim, Scope: ScopeGlobal, Kind: CandidateProcedure},
			pane, DefaultGates(), supportsForTest)
		if err != nil {
			return err
		}
		item, err := InsertItem(ctx, tx, Item{ClaimID: claim, CurrentRevision: 1, Kind: KindPreference, Scope: ScopeGlobal,
			State: StateDraft, Created: testNow})
		if err != nil {
			return err
		}
		id, err = ProposeCandidate(ctx, tx, cand, Proposal{ItemID: &item, BaseRevision: ptr(int64(1)), Text: "deploy",
			Scope: ScopeGlobal, Target: "global", Diff: "+ deploy", Status: ProposalPending, Created: testNow})
		if err != nil {
			return err
		}
		pr, err := ProposalByID(ctx, tx, id)
		want := []string{fingerprint.Prompt(part), fingerprint.Prompt(full)}
		slices.Sort(want)
		if !slices.Equal(pr.TriggerPrompts, want) {
			t.Errorf("trigger prompts = %v, want %v", pr.TriggerPrompts, want)
		}
		return err
	})
	if got := p.match(t, "ws-B", full); !slices.Equal(got, []int64{id}) {
		t.Errorf("MatchPrompt(full prompt) = %v, want [%d]", got, id)
	}
}

// An item forget deletes the prompt index rows of its proposals, with their text and hashes,
// so its prompt matches nothing. Other rows stay.
func TestEraseItemPromptIndex(t *testing.T) {
	p := newPromptStore(t)
	_, item := p.propose(t, "run make Foo", ScopeWorkspace)
	kept, _ := p.propose(t, "run make Bar", ScopeWorkspace)
	ctx := context.Background()
	write(t, p.Store, func(tx *sql.Tx) error {
		if err := EraseItem(ctx, tx, item, testNow); err != nil {
			return err
		}
		left, err := ItemTextLeft(ctx, tx, item)
		if len(left) != 0 {
			t.Errorf("ItemTextLeft = %v, want none", left)
		}
		return err
	})
	if got := p.match(t, "ws-A", "run make Foo"); got != nil {
		t.Errorf("MatchPrompt after the forget = %v, want none", got)
	}
	if got := p.match(t, "ws-A", "run make Bar"); !slices.Equal(got, []int64{kept}) {
		t.Errorf("MatchPrompt of the other item = %v, want [%d]", got, kept)
	}
	var n int
	write(t, p.Store, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `SELECT count(*) FROM prompt_index WHERE hash = ? OR text LIKE '%Foo%'`,
			fingerprint.Prompt("run make Foo")).Scan(&n)
	})
	if n != 0 {
		t.Errorf("prompt index rows of the forgotten item = %d, want 0", n)
	}
}

// matchRows is the size of the match speed test [NEW]: 10,000 prompt index rows.
const matchRows = 10000

// fillIndex adds matchRows prompt index rows to p, one in ten of them linked to one of 100
// proposals, and returns the prompt of the last proposal.
func fillIndex(t testing.TB, p *promptStore) string {
	t.Helper()
	ctx := context.Background()
	var last string
	if err := p.Write(ctx, func(tx *sql.Tx) error {
		var props []int64
		for i := range 100 {
			last = fmt.Sprintf("run task %d", i)
			claim, err := ClaimID(ctx, tx, ClaimKey{"prompt", last}, testNow)
			if err != nil {
				return err
			}
			item, err := InsertItem(ctx, tx, Item{ClaimID: claim, CurrentRevision: 1, Kind: KindPreference,
				Scope: ScopeGlobal, State: StateDraft, Created: testNow})
			if err != nil {
				return err
			}
			id, err := InsertProposal(ctx, tx, Proposal{ItemID: &item, Text: last, Scope: ScopeGlobal, Target: "global",
				Diff: "+ " + last, TriggerPrompts: []string{fingerprint.Prompt(last)}, Status: ProposalPending, Created: testNow})
			if err != nil {
				return err
			}
			props = append(props, id)
		}
		for i := range matchRows {
			text := fmt.Sprintf("an everyday prompt %d", i)
			if i%10 == 0 {
				text = fmt.Sprintf("run task %d", i/10%100)
			}
			o, err := p.prompt(ctx, tx, fmt.Sprint("ws-", i%7), text)
			if err != nil {
				return err
			}
			if i%10 == 0 {
				if _, err := tx.ExecContext(ctx, `UPDATE prompt_index SET proposal_ids = ? WHERE observation_id = ?`,
					jsonList[int64]{props[i/10%100]}, o); err != nil {
					return err
				}
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return last
}

// AC: an exact match on 10,000 index rows takes under 50 ms [NEW]. The median of 11
// runs counts, so one slow run of a loaded machine does not fail it.
func TestMatchPromptSpeed(t *testing.T) {
	p := newPromptStore(t)
	prompt := fillIndex(t, p)
	ctx := context.Background()
	var took []time.Duration
	for range 11 {
		start := time.Now()
		var ids []int64
		err := p.Write(ctx, func(tx *sql.Tx) (err error) {
			ids, err = MatchPrompt(ctx, tx, fingerprint.Prompt(prompt), "ws-1")
			return err
		})
		took = append(took, time.Since(start))
		if err != nil || len(ids) != 1 {
			t.Fatalf("MatchPrompt = %v, %v, want 1 proposal", ids, err)
		}
	}
	sort.Slice(took, func(i, j int) bool { return took[i] < took[j] })
	if took[5] >= 50*time.Millisecond {
		t.Errorf("median match time = %v, want < 50ms (runs %v)", took[5], took)
	}
}

func BenchmarkMatchPrompt(b *testing.B) {
	s, err := Open(context.Background(), b.TempDir(), testTimeout)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = s.Close() })
	p := &promptStore{Store: s, segments: map[string]int64{}}
	hash := fingerprint.Prompt(fillIndex(b, p))
	ctx := context.Background()
	b.ResetTimer()
	for range b.N {
		if err := p.Write(ctx, func(tx *sql.Tx) error {
			_, err := MatchPrompt(ctx, tx, hash, "ws-1")
			return err
		}); err != nil {
			b.Fatal(err)
		}
	}
}
