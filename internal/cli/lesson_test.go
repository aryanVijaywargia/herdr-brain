package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/aryanVijaywargia/herdr-brain/internal/brained"
	"github.com/aryanVijaywargia/herdr-brain/internal/extract"
	"github.com/aryanVijaywargia/herdr-brain/internal/herdr"
	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

// lessonText is a plain-words lesson (#253).
const lessonText = "From now on, run make lint before each commit."

// lessonCLI is the fake extractor CLI: no model call. It confirms every ref of the prompt as
// one workspace lesson.
type lessonCLI struct{}

func (lessonCLI) Run(_ context.Context, cmd extract.Command) ([]byte, error) {
	refs := []string{}
	for _, m := range promptRef.FindAllStringSubmatch(cmd.Argv[len(cmd.Argv)-1], -1) {
		refs = append(refs, m[1])
	}
	return json.Marshal(map[string]any{"is_error": false, "structured_output": map[string]any{"claims": []any{
		map[string]any{"kind": "lesson", "situation": "before a commit", "rule": "run make lint", "scope_guess": "workspace",
			"evidence_refs": refs, "same_as": nil, "conflicts_with": nil, "touches_permission": false,
			"expected_benefit": "no lint failures in CI"}}}})
}

// lesson stores the lesson in the open segment of pane w14:p1, as capture does, and runs an
// extraction pass after the session went idle. It returns the dispatch that brained gets for
// the new lesson proposal (brained.LessonDispatch), and the proposal id.
func (f *suggestFixture) lesson(t *testing.T) (brained.Dispatch, int64) {
	t.Helper()
	f.setClaude(t)
	f.storeWrite(t, func(ctx context.Context, tx *sql.Tx) error {
		_, seg, err := store.HookSegment(ctx, tx, "default", "claude", "w14:p1", "")
		if err != nil {
			return err
		}
		_, err = store.AddObservation(ctx, tx, store.Observation{CaptureIdentity: "lesson-1", Source: store.SourceHerdr,
			SegmentID: seg, Event: "working", Excerpt: lessonText, Status: store.ObservationComplete,
			Sender: store.SenderUnconfirmed, Counts: true, At: testNow})
		return err
	})
	var ds []brained.Dispatch
	s := openStore(t, f.store)
	p := extract.Pass{Store: s, Runner: lessonCLI{}, HerdrSession: "default", DefaultOn: true,
		Now: func() time.Time { return testNow.Add(11 * time.Minute) },
		Suggest: func(_ context.Context, sg extract.Suggestion) error {
			if sg.AgentPaneID != "w14:p1" || sg.WorkspaceID != "w14" {
				t.Errorf("suggestion %+v, want beside pane w14:p1 in workspace w14", sg)
			}
			ds = append(ds, brained.LessonDispatch(f.store, sg))
			return nil
		}}
	err := p.Run(context.Background())
	closeStore(t, s)
	if err != nil {
		t.Fatal(err)
	}
	if len(ds) != 1 {
		t.Fatalf("dispatches = %+v, want 1", ds)
	}
	return ds[0], ds[0].Proposal
}

// AC #253 (2): the first session with a plain-words lesson makes a proposal, and the review
// pane opens beside the agent pane where the lesson was said, through the suggestion checks of
// brain suggest. The draft waits for you; the approval makes you the author (sender = you).
func TestLessonOpensReviewPane(t *testing.T) {
	f := newSuggestFixture(t)
	d, proposal := f.lesson(t)
	if proposal == f.proposal {
		t.Fatalf("dispatch of proposal %d, want the new lesson proposal", proposal)
	}
	f.suggest(t, d, nil)
	want := []herdr.PluginPaneSpec{{Plugin: "brain", Entrypoint: "review", Placement: "split", TargetPane: "w14:p1",
		Env: []string{fmt.Sprint("BRAIN_PROPOSAL_ID=", proposal)}}}
	if !reflect.DeepEqual(f.herdr.opened, want) {
		t.Errorf("opened %+v, want %+v", f.herdr.opened, want)
	}
	ss := f.suggestions(t)
	if len(ss) != 1 || ss[0].ProposalID != proposal || ss[0].ReviewPaneID != "w14:p5" || ss[0].Result != "" {
		t.Errorf("suggestions = %+v, want the lesson shown in pane w14:p5", ss)
	}

	f.decide(t, fmt.Sprint(proposal), "--approve")
	f.storeWrite(t, func(ctx context.Context, tx *sql.Tx) error {
		p, err := store.ProposalByID(ctx, tx, proposal)
		if err != nil {
			return err
		}
		var state, author string
		err = tx.QueryRowContext(ctx, `SELECT i.state, r.author FROM items i JOIN item_revisions r
			ON r.item_id = i.id AND r.revision = i.current_revision WHERE i.id = ?`, *p.ItemID).Scan(&state, &author)
		if state != string(store.StateActive) || author != string(store.AuthorYou) {
			t.Errorf("item after approval: %s by %s, want active by you", state, author)
		}
		return err
	})
}

// AC #253 (2): when a check fails, here a new agent in the pane, no pane opens. The suggestion
// expires, and the proposal waits in brain review.
func TestLessonPaneCheckFails(t *testing.T) {
	f := newSuggestFixture(t)
	d, proposal := f.lesson(t)
	p := f.panes["w14:p1"]
	codex := "codex"
	p.Agent = &codex
	f.panes["w14:p1"] = p
	f.suggest(t, d, nil)
	if len(f.herdr.notes) != 0 || len(f.herdr.opened) != 0 {
		t.Errorf("notifications %q, opened %+v, want none", f.herdr.notes, f.herdr.opened)
	}
	if ss := f.suggestions(t); len(ss) != 1 || ss[0].Result != store.SuggestionExpired {
		t.Errorf("suggestions = %+v, want the lesson expired", ss)
	}
	f.storeWrite(t, func(ctx context.Context, tx *sql.Tx) error {
		pr, err := store.ProposalByID(ctx, tx, proposal)
		if err == nil && pr.Status != store.ProposalPending {
			t.Errorf("proposal %d is %s, want pending", proposal, pr.Status)
		}
		return err
	})
}
