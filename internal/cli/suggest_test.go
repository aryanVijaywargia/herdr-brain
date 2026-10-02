package cli

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aryanVijaywargia/herdr-brain/internal/bindings"
	"github.com/aryanVijaywargia/herdr-brain/internal/brained"
	"github.com/aryanVijaywargia/herdr-brain/internal/extract"
	"github.com/aryanVijaywargia/herdr-brain/internal/fingerprint"
	"github.com/aryanVijaywargia/herdr-brain/internal/herdr"
	"github.com/aryanVijaywargia/herdr-brain/internal/signal"
	"github.com/aryanVijaywargia/herdr-brain/internal/store"
	"github.com/aryanVijaywargia/herdr-brain/internal/workspace"
)

const suggestPrompt = "run make Foo"

// suggestFixture is the fixture with a claude agent in pane w14:p1, its capture session
// cap-1, a pending workspace proposal that the prompt "run make Foo" led to, and the reserved
// suggestion of a repeat of that prompt.
type suggestFixture struct {
	*fixture
	session, item, proposal int64
	config                  string // the Brain config file; absent until a test writes it
}

func newSuggestFixture(t *testing.T) *suggestFixture {
	t.Helper()
	f := &suggestFixture{fixture: newFixture(t), config: filepath.Join(t.TempDir(), "config.toml")}
	p := f.panes["w14:p1"]
	agent := "claude"
	p.Agent = &agent
	f.panes["w14:p1"] = p
	f.storeWrite(t, func(ctx context.Context, tx *sql.Tx) error {
		key := store.SessionKey{HerdrSession: "default", Harness: "claude", Kind: store.KeyCapture, ID: "cap-1"}
		var err error
		if f.session, err = store.OpenSession(ctx, tx, key, "w14:p1", "ws_a14", testNow); err != nil {
			return err
		}
		if _, err := store.StartSegment(ctx, tx, f.session, "w14:p1", "ws_a14", "visible", testNow); err != nil {
			return err
		}
		o, err := f.prompt(ctx, tx)
		if err != nil {
			return err
		}
		// The key in the normal form of the extractor, so a claim of the prompt is this claim.
		claim, err := store.ClaimID(ctx, tx, store.ClaimKey{Situation: "make", Rule: strings.ToLower(suggestPrompt)}, testNow)
		if err != nil {
			return err
		}
		cand, err := store.Attach(ctx, tx, store.Candidate{ClaimID: claim, Scope: store.ScopeWorkspace,
			WorkspaceKey: "ws_a14", Kind: store.CandidateCorrection}, o, store.DefaultGates(), signal.Supports)
		if err != nil {
			return err
		}
		ws := "ws_a14"
		if f.item, err = store.InsertItem(ctx, tx, store.Item{ClaimID: claim, CurrentRevision: 1, Kind: store.KindPreference,
			Scope: store.ScopeWorkspace, WorkspaceKey: &ws, State: store.StateDraft, Created: testNow}); err != nil {
			return err
		}
		base := int64(1)
		f.proposal, err = store.ProposeCandidate(ctx, tx, cand, store.Proposal{ItemID: &f.item, BaseRevision: &base,
			Text: "make: " + suggestPrompt, Scope: store.ScopeWorkspace, Target: "workspace", Diff: "+ make",
			Status: store.ProposalPending, Created: testNow})
		if err != nil {
			return err
		}
		return store.InsertItemRevision(ctx, tx, store.ItemRevision{ItemID: f.item, Revision: 1, Text: "make: " + suggestPrompt,
			Scope: store.ScopeWorkspace, WorkspaceKey: &ws, State: store.StateDraft, Author: store.AuthorBrain,
			Origin: store.OriginExtractor, ProposalID: &f.proposal, Created: testNow})
	})
	return f
}

// prompt stores the prompt in the open segment of cap-1 with its index row, in tx.
func (f *suggestFixture) prompt(ctx context.Context, tx *sql.Tx) (int64, error) {
	_, seg, err := store.HookSegment(ctx, tx, "default", "claude", "w14:p1", "")
	if err != nil {
		return 0, err
	}
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM observations`).Scan(&n); err != nil {
		return 0, err
	}
	o, err := store.AddObservation(ctx, tx, store.Observation{CaptureIdentity: fmt.Sprint("p", n+1), Source: store.SourceHook,
		SegmentID: seg, Event: "prompt", Excerpt: suggestPrompt, Status: store.ObservationComplete,
		Sender: store.SenderUnconfirmed, Counts: true, At: testNow})
	if err != nil {
		return 0, err
	}
	return o, store.IndexPrompt(ctx, tx, o, fingerprint.Prompt(suggestPrompt), suggestPrompt)
}

// reserve stores a repeat of the prompt and returns the dispatch of its suggestion.
func (f *suggestFixture) reserve(t *testing.T) brained.Dispatch {
	t.Helper()
	var d brained.Dispatch
	f.storeWrite(t, func(ctx context.Context, tx *sql.Tx) error {
		o, err := f.prompt(ctx, tx)
		if err != nil {
			return err
		}
		s, err := store.ReserveSuggestion(ctx, tx, o, fingerprint.Prompt(suggestPrompt), testNow)
		if err == nil && s.ID == 0 {
			return errors.New("the repeat reserved no suggestion")
		}
		d = brained.Dispatch{Suggestion: s.ID, Binding: s.HerdrSession, Store: f.store, Workspace: s.WorkspaceKey,
			Session: s.SessionID, Occurrence: s.ObservationID, Generation: s.Generation, Proposal: s.ProposalID}
		return err
	})
	return d
}

// suggest runs brain suggest for d from pane w14:p1, with paused between the resolve and
// the checks.
func (f *suggestFixture) suggest(t *testing.T, d brained.Dispatch, paused func()) {
	t.Helper()
	e := f.env(f.sock, "w14:p1", "w14", &bytes.Buffer{})
	e.Config = f.config
	if err := (suggester{Env: e, paused: paused}).run(context.Background(), d); err != nil {
		t.Fatalf("suggest = %v", err)
	}
}

func (f *suggestFixture) suggestions(t *testing.T) []store.Suggestion {
	t.Helper()
	var ss []store.Suggestion
	f.storeWrite(t, func(ctx context.Context, tx *sql.Tx) (err error) {
		ss, err = store.Suggestions(ctx, tx)
		return err
	})
	return ss
}

// review runs brain review args from pane w14:p1.
func (f *suggestFixture) review(t *testing.T, args ...string) {
	t.Helper()
	if err := Review(context.Background(), f.env(f.sock, "w14:p1", "w14", &bytes.Buffer{}), args); err != nil {
		t.Fatalf("review %q = %v", args, err)
	}
}

// S55: a reserved suggestion shows a herdr notification and the review pane beside the agent,
// with no focus (the client always passes --no-focus, TestOpenPluginPane), and no model call.
// The child sends no key to the agent: PaneHerdr has no call that could.
func TestSuggestShows(t *testing.T) {
	f := newSuggestFixture(t)
	d := f.reserve(t)
	f.suggest(t, d, nil)
	if want := []string{fmt.Sprintf("Brain has 1 suggestion: proposal %d opens beside pane w14:p1", f.proposal)}; !reflect.DeepEqual(f.herdr.notes, want) {
		t.Errorf("notifications = %q, want %q", f.herdr.notes, want)
	}
	want := []herdr.PluginPaneSpec{{Plugin: "brain", Entrypoint: "review", Placement: "split", TargetPane: "w14:p1",
		Env: []string{fmt.Sprint("BRAIN_PROPOSAL_ID=", f.proposal)}}}
	if !reflect.DeepEqual(f.herdr.opened, want) {
		t.Errorf("opened %+v, want %+v", f.herdr.opened, want)
	}
	ss := f.suggestions(t)
	if len(ss) != 1 || ss[0].ReviewPaneID != "w14:p5" || !ss[0].ShownAt.Equal(testNow) || ss[0].Result != "" {
		t.Errorf("suggestions = %+v, want shown in pane w14:p5", ss)
	}
	if ps := f.reviewPanes(t); len(ps) != 1 || ps[0].PaneID != "w14:p5" || ps[0].BesidePaneID != "w14:p1" {
		t.Errorf("review panes = %+v, want w14:p5 beside the agent", ps)
	}
	f.storeWrite(t, func(ctx context.Context, tx *sql.Tx) error {
		n, err := store.CallsSince(ctx, tx, testNow.AddDate(-1, 0, 0))
		if n != 0 {
			t.Errorf("model calls = %d, want 0", n)
		}
		return err
	})
}

// AC: the dispatch waits, then in turn a new agent in the pane, a pane move, learn off, a
// forget, a reject and "later" change the state. On release the suggestion shows no
// notification and no pane, and it is expired (or gone with the forget).
func TestSuggestPaused(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(t *testing.T, f *suggestFixture)
		gone   bool // a forget deleted the suggestion
	}{
		{"new agent", func(_ *testing.T, f *suggestFixture) {
			p := f.panes["w14:p1"]
			codex := "codex"
			p.Agent = &codex
			f.panes["w14:p1"] = p
		}, false},
		{"new agent session", func(t *testing.T, f *suggestFixture) {
			f.storeWrite(t, func(ctx context.Context, tx *sql.Tx) error {
				// Capture ends the session at another agent session, and opens the new one.
				old := store.SessionKey{HerdrSession: "default", Harness: "claude", Kind: store.KeyCapture, ID: "cap-1"}
				if err := store.EndSession(ctx, tx, old, testNow); err != nil {
					return err
				}
				key := store.SessionKey{HerdrSession: "default", Harness: "claude", Kind: store.KeyCapture, ID: "cap-2"}
				id, err := store.OpenSession(ctx, tx, key, "w14:p1", "ws_a14", testNow)
				if err != nil {
					return err
				}
				_, err = store.StartSegment(ctx, tx, id, "w14:p1", "ws_a14", "visible", testNow)
				return err
			})
		}, false},
		{"agent session id", func(_ *testing.T, f *suggestFixture) {
			// herdr now shows an agent session id that the capture session does not have.
			p := f.panes["w14:p1"]
			p.AgentSession = &herdr.AgentSession{Value: "other-session"}
			f.panes["w14:p1"] = p
		}, false},
		{"rebind", func(t *testing.T, f *suggestFixture) {
			bs, _, err := bindings.Read(f.file)
			if err != nil {
				t.Fatal(err)
			}
			other := filepath.Join(filepath.Dir(f.store), "other")
			if err := os.MkdirAll(other, 0o700); err != nil {
				t.Fatal(err)
			}
			for i := range bs {
				if bs[i].Session == "default" {
					bs[i].Store = other
				}
			}
			if err := bindings.Write(f.file, bs); err != nil {
				t.Fatal(err)
			}
		}, false},
		{"pane move", func(_ *testing.T, f *suggestFixture) {
			p := f.panes["w14:p1"]
			p.ID, p.WorkspaceID = "w20:p1", "w20"
			f.panes["w14:p1"] = p
		}, false},
		{"learn off", func(t *testing.T, f *suggestFixture) {
			f.storeWrite(t, func(ctx context.Context, tx *sql.Tx) error {
				return store.SetLearning(ctx, tx, store.LearningSetting{Scope: store.LearningStore, On: false, Changed: testNow})
			})
		}, false},
		{"extraction off", func(t *testing.T, f *suggestFixture) {
			f.storeWrite(t, func(ctx context.Context, tx *sql.Tx) error {
				return store.SetExtraction(ctx, tx, "ws_a14", false, testNow)
			})
		}, false},
		{"live suggestions off", func(t *testing.T, f *suggestFixture) {
			if err := os.WriteFile(f.config, []byte("live_suggestions = \"off\"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}, false},
		{"forget session", func(t *testing.T, f *suggestFixture) {
			f.storeWrite(t, func(ctx context.Context, tx *sql.Tx) error { return store.ForgetSession(ctx, tx, f.session) })
		}, true},
		{"forget item", func(t *testing.T, f *suggestFixture) {
			f.storeWrite(t, func(ctx context.Context, tx *sql.Tx) error { return store.EraseItem(ctx, tx, f.item, testNow) })
		}, true},
		{"reject", func(t *testing.T, f *suggestFixture) { f.review(t, fmt.Sprint(f.proposal), "--reject") }, false},
		{"later", func(t *testing.T, f *suggestFixture) {
			// An earlier suggestion of the proposal in the session was shown, its pane closed,
			// and you chose later in it.
			f.storeWrite(t, func(ctx context.Context, tx *sql.Tx) error {
				_, err := tx.ExecContext(ctx, `UPDATE suggestions SET shown_at = ?, review_pane_id = 'w14:p9' WHERE id = 1`,
					testNow.Format("2006-01-02T15:04:05.000Z07:00"))
				return err
			})
			f.review(t, fmt.Sprint(f.proposal), "--later")
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSuggestFixture(t)
			if tc.name == "later" {
				f.reserve(t) // the earlier suggestion
			}
			d := f.reserve(t)
			f.suggest(t, d, func() { tc.change(t, f) })
			if len(f.herdr.notes) != 0 || len(f.herdr.opened) != 0 {
				t.Errorf("notifications %q, opened %+v, want none", f.herdr.notes, f.herdr.opened)
			}
			var s *store.Suggestion
			for _, row := range f.suggestions(t) {
				if row.ID == d.Suggestion {
					s = &row
				}
			}
			switch {
			case tc.gone && s != nil:
				t.Errorf("suggestion %+v after the forget, want it gone", *s)
			case tc.name == "rebind":
				// The child no longer reaches the store of the suggestion: it shows nothing
				// and cannot mark it.
			case !tc.gone && (s == nil || s.Result != store.SuggestionExpired):
				t.Errorf("suggestion %d = %+v, want it expired", d.Suggestion, s)
			}
		})
	}
}

// One review pane for each agent at a time: a live review pane beside the agent skips the
// suggestion; a row whose pane is gone (closed with q) is marked closed, and the pane opens.
func TestSuggestOnePane(t *testing.T) {
	f := newSuggestFixture(t)
	key := "ws_a14"
	f.addReviewPane(t, store.ReviewPane{HerdrSession: "default", SessionIdentity: &key, PaneID: "w14:p7",
		BesidePaneID: "w14:p1", ProposalID: f.proposal, Opened: testNow})
	f.herdr.procs["w14:p7"] = reviewProc(f.proposal)
	d := f.reserve(t)
	f.suggest(t, d, nil)
	if len(f.herdr.notes) != 0 || len(f.herdr.opened) != 0 {
		t.Errorf("with a live pane: notifications %q, opened %+v, want none", f.herdr.notes, f.herdr.opened)
	}
	delete(f.herdr.procs, "w14:p7") // you closed it with q
	f.suggest(t, f.reserve(t), nil)
	if len(f.herdr.notes) != 1 || len(f.herdr.opened) != 1 {
		t.Errorf("after the close: notifications %q, opened %+v, want one", f.herdr.notes, f.herdr.opened)
	}
	if ps := f.reviewPanes(t); len(ps) != 2 || ps[0].Closed.IsZero() || !ps[1].Closed.IsZero() {
		t.Errorf("review panes = %+v, want the old one closed and the new one open", ps)
	}
}

// One review pane for each agent also holds for two suggestions of two prompts whose processes
// run at the same time: the suggestion lock spans the checks, the open and its record.
func TestSuggestConcurrent(t *testing.T) {
	f := newSuggestFixture(t)
	d1, d2 := f.reserve(t), f.reserve(t)
	e := f.env(f.sock, "w14:p1", "w14", &bytes.Buffer{})
	e.Config = f.config
	var mu sync.Mutex
	sync1 := e.Sync
	e.Sync = func(ctx context.Context, s workspace.Session, st *store.Store) error {
		mu.Lock()
		defer mu.Unlock()
		return sync1(ctx, s, st)
	}
	// The fake herdr shows the opened pane as Brain's review pane, as herdr does.
	f.herdr.procs["w14:p5"] = reviewProc(f.proposal)
	begin := make(chan struct{})
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i, d := range []brained.Dispatch{d1, d2} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-begin
			errs[i] = suggester{Env: e}.run(context.Background(), d)
		}()
	}
	close(begin)
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		t.Fatal(err)
	}
	if len(f.herdr.opened) != 1 || len(f.herdr.notes) != 1 {
		t.Errorf("opened %+v, notifications %q, want one pane beside the agent", f.herdr.opened, f.herdr.notes)
	}
}

// Your decision in the review pane, or in any surface, gives the shown suggestion its result
// (§12 counts).
func TestSuggestResults(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want store.SuggestionResult
	}{
		{[]string{"--later"}, store.SuggestionLater},
		{[]string{"--reject"}, store.SuggestionRejected},
		{[]string{"--change", "use make bar"}, store.SuggestionChanged},
	} {
		t.Run(string(tc.want), func(t *testing.T) {
			f := newSuggestFixture(t)
			f.suggest(t, f.reserve(t), nil)
			f.review(t, append([]string{fmt.Sprint(f.proposal)}, tc.args...)...)
			if ss := f.suggestions(t); len(ss) != 1 || ss[0].Result != tc.want {
				t.Errorf("suggestions = %+v, want result %s", ss, tc.want)
			}
		})
	}
}

func TestLiveSuggestions(t *testing.T) {
	dir := t.TempDir()
	for _, tc := range []struct {
		name, text string
		want, err  bool
	}{
		{"on", `live_suggestions = "on"`, true, false},
		{"off", `live_suggestions = "off"`, false, false},
		{"no switch", `other = 1`, true, false},
		{"bad value", `live_suggestions = "maybe"`, false, true},
		{"bad toml", `live_suggestions = `, false, true},
	} {
		path := filepath.Join(dir, tc.name+".toml")
		if err := os.WriteFile(path, []byte(tc.text+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		got, err := liveSuggestions(path)
		if got != tc.want || (err != nil) != tc.err {
			t.Errorf("%s: liveSuggestions = %v, %v, want %v, error %v", tc.name, got, err, tc.want, tc.err)
		}
	}
	// A missing file is on, the default of §5.5.
	if got, err := liveSuggestions(filepath.Join(dir, "none.toml")); !got || err != nil {
		t.Errorf("missing file: liveSuggestions = %v, %v, want true, nil", got, err)
	}
}

// promptRef is an evidence ref in the extractor prompt.
var promptRef = regexp.MustCompile(`\[(e\d+)\]`)

// claimCLI is the fake extractor CLI: no model call. Each call answers with one workspace
// claim of the suggestion's prompt, with every ref of the prompt as its evidence.
type claimCLI struct{ calls int }

func (c *claimCLI) Run(_ context.Context, cmd extract.Command) ([]byte, error) {
	c.calls++
	refs := []string{}
	for _, m := range promptRef.FindAllStringSubmatch(cmd.Argv[len(cmd.Argv)-1], -1) {
		refs = append(refs, m[1])
	}
	return json.Marshal(map[string]any{"is_error": false, "structured_output": map[string]any{"claims": []any{
		map[string]any{"kind": "correction", "situation": "make", "rule": suggestPrompt, "scope_guess": "workspace",
			"evidence_refs": refs, "same_as": nil, "conflicts_with": nil, "touches_permission": false,
			"expected_benefit": "no typed command"}}}})
}

// repeat stores the prompt again in the open segment of pane w14:p1 and returns its dispatch,
// as the hook and capture reserve it (brained.Reserve). A zero dispatch starts nothing.
func (f *suggestFixture) repeat(t *testing.T) brained.Dispatch {
	t.Helper()
	var d brained.Dispatch
	f.storeWrite(t, func(ctx context.Context, tx *sql.Tx) error {
		id, err := f.prompt(ctx, tx)
		if err != nil {
			return err
		}
		d, err = brained.Reserve(ctx, tx, store.Observation{ID: id, Source: store.SourceHook, Event: "prompt",
			Excerpt: suggestPrompt, Status: store.ObservationComplete, Sender: store.SenderUnconfirmed, Counts: true,
			At: testNow}, suggestPrompt, testNow)
		return err
	})
	return d
}

// AC (E15-S3, §5.5 "a reject is final in its scope"): you reject a live suggestion. The same
// prompt again in the same session and in a new session reserves nothing, so no notification
// and no pane. The extractor sees the repeats and gives the same claim: its evidence attaches,
// and no new proposal of the claim comes in the workspace.
func TestSuggestRejectNoNag(t *testing.T) {
	f := newSuggestFixture(t)
	f.setCLI(t, store.ExtractorClaude)
	f.suggest(t, f.reserve(t), nil)
	if len(f.herdr.notes) != 1 || len(f.herdr.opened) != 1 {
		t.Fatalf("notifications %q, opened %+v, want the one suggestion", f.herdr.notes, f.herdr.opened)
	}
	f.review(t, fmt.Sprint(f.proposal), "--reject")

	if d := f.repeat(t); d != (brained.Dispatch{}) {
		t.Errorf("same session: dispatch %+v, want none", d)
	}
	f.storeWrite(t, func(ctx context.Context, tx *sql.Tx) error {
		old := store.SessionKey{HerdrSession: "default", Harness: "claude", Kind: store.KeyCapture, ID: "cap-1"}
		if err := store.EndSession(ctx, tx, old, testNow); err != nil {
			return err
		}
		key := store.SessionKey{HerdrSession: "default", Harness: "claude", Kind: store.KeyCapture, ID: "cap-2"}
		id, err := store.OpenSession(ctx, tx, key, "w14:p1", "ws_a14", testNow)
		if err != nil {
			return err
		}
		_, err = store.StartSegment(ctx, tx, id, "w14:p1", "ws_a14", "visible", testNow)
		return err
	})
	for range 2 {
		if d := f.repeat(t); d != (brained.Dispatch{}) {
			t.Errorf("new session: dispatch %+v, want none", d)
		}
	}
	if len(f.herdr.notes) != 1 || len(f.herdr.opened) != 1 {
		t.Errorf("notifications %q, opened %+v, want only the first suggestion", f.herdr.notes, f.herdr.opened)
	}
	if ss := f.suggestions(t); len(ss) != 1 || ss[0].Result != store.SuggestionRejected {
		t.Errorf("suggestions = %+v, want the rejected one only", ss)
	}

	s := openStore(t, f.store)
	cli := &claimCLI{}
	p := extract.Pass{Store: s, Runner: cli, HerdrSession: "default", DefaultOn: true,
		Now: func() time.Time { return testNow.Add(2 * time.Hour) }}
	err := p.Run(context.Background())
	closeStore(t, s)
	if err != nil {
		t.Fatal(err)
	}
	if cli.calls == 0 {
		t.Fatal("the extractor got no batch of the repeats")
	}
	f.storeWrite(t, func(ctx context.Context, tx *sql.Tx) error {
		var claim string
		if err := tx.QueryRowContext(ctx, `SELECT claim_id FROM items WHERE id = ?`, f.item).Scan(&claim); err != nil {
			return err
		}
		c, err := store.CandidateOf(ctx, tx, claim, "ws_a14")
		if err != nil {
			return err
		}
		if c.Count < 2 {
			t.Errorf("candidate %+v, want the repeats attached", c)
		}
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM proposals WHERE id != ?`, f.proposal).Scan(&n); err != nil {
			return err
		}
		if n != 0 {
			t.Errorf("%d new proposals, want none for the rejected claim", n)
		}
		return nil
	})
}
