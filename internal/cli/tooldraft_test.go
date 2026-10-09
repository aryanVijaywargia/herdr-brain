package cli

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/aryanVijaywargia/herdr-brain/internal/extract"
	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

// commandSession adds an ended agent session in workspace key ws: you ask for the api tests,
// and the agent runs argv in folder /src/shop.
func (f *fixture) commandSession(t *testing.T, ws, argv string) {
	t.Helper()
	f.storeWrite(t, func(ctx context.Context, tx *sql.Tx) error {
		key := store.SessionKey{HerdrSession: "default", Harness: "claude", Kind: store.KeyAgent, ID: "tests"}
		id, err := store.OpenSession(ctx, tx, key, "w14:p1", ws, testNow)
		if err != nil {
			return err
		}
		seg, err := store.StartSegment(ctx, tx, id, "w14:p1", ws, "recent_unwrapped", testNow)
		if err != nil {
			return err
		}
		var obs int64
		for i, o := range []store.Observation{
			{Source: store.SourceHerdr, Event: "working", Excerpt: "run the api tests", Sender: store.SenderYou},
			{Source: store.SourceHook, Event: "tool", Excerpt: argv, Sender: store.SenderTool},
		} {
			o.CaptureIdentity, o.SegmentID, o.Status, o.Counts = fmt.Sprintf("tests-%d", i), seg, store.ObservationComplete, true
			o.At = testNow.Add(time.Duration(i) * time.Minute)
			if obs, err = store.AddObservation(ctx, tx, o); err != nil {
				return err
			}
		}
		exit := 0
		if err := store.AddCommandRecord(ctx, tx, store.CommandRecord{ObservationID: obs, Argv: argv, CWD: "/src/shop",
			ExitStatus: &exit}); err != nil {
			return err
		}
		return store.EndSession(ctx, tx, key, testNow.Add(time.Minute))
	})
}

// AC: "make this a tool" after one example gives a draft. brain review --all offers the action
// on the command candidate; the draft is the exact command, it goes through the normal review,
// and it has no approval yet.
func TestMakeToolAction(t *testing.T) {
	f := newFixture(t)
	f.commandSession(t, "ws_a14", "pnpm test --filter api")
	s := openStore(t, f.store)
	err := extract.Pass{Store: s, Runner: &skillCLI{}, HerdrSession: "default", DefaultOn: true,
		Now: func() time.Time { return testNow.Add(time.Hour) }}.Run(context.Background())
	closeStore(t, s)
	if err != nil {
		t.Fatalf("Run() = %v", err)
	}
	r := f.rows(t)
	if len(r.proposals) != 0 {
		t.Fatalf("proposals = %+v, want none after one example", r.proposals)
	}
	var cand int64
	f.storeWrite(t, func(ctx context.Context, tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `SELECT id FROM candidates WHERE kind = 'command_chain'`).Scan(&cand)
	})
	hint := fmt.Sprintf("  make this a tool: brain review --tool %d\n", cand)
	if out := f.mustReview(t, "--all"); !strings.Contains(out, hint) {
		t.Errorf("review --all =\n%s\nwant the line %q", out, hint)
	}

	out := f.mustReview(t, "--tool", fmt.Sprint(cand))
	r = f.rows(t)
	if len(r.proposals) != 1 {
		t.Fatalf("proposals = %+v, want one tool draft", r.proposals)
	}
	p := r.proposals[0]
	if !strings.HasPrefix(out, fmt.Sprintf("proposal %d: a tool draft from candidate %d", p.ID, cand)) {
		t.Errorf("review --tool = %q, want the proposal and the candidate", out)
	}
	if it, _ := r.item(t, *p.ItemID); it.Kind != store.KindTool || it.State != store.StateDraft {
		t.Errorf("item = %+v, want a draft tool", it)
	}
	show := f.mustReview(t, fmt.Sprint(p.ID))
	for _, want := range []string{`"pnpm" "test" "--filter" "api"`, "/src/shop", "source:   the exact repeat rule ("} {
		if !strings.Contains(show, want) {
			t.Errorf("review show =\n%s\nwant %q", show, want)
		}
	}
	if strings.Contains(f.mustReview(t, "--all"), hint) {
		t.Error("review --all still offers the action while the draft waits")
	}
	if _, err := f.review(t, "--tool", fmt.Sprint(cand)); err == nil {
		t.Error("a second review --tool = nil, want a refusal")
	}
	if _, err := f.review(t, "--tool", "99"); err == nil {
		t.Error("review --tool of an unknown candidate = nil, want a refusal")
	}
	if _, err := f.review(t, "3", "--tool", fmt.Sprint(cand)); err == nil {
		t.Error("review <id> --tool = nil, want a refusal")
	}
}

// #345 AC: the review of an exact tool draft names the exact repeat rule as its source: no
// model call made it. The extractor is not its source.
func TestExactToolSource(t *testing.T) {
	f := newFixture(t)
	for i := range 3 {
		f.chainSession(t, "ws_a14", fmt.Sprintf("s%d", i))
	}
	f.extractChains(t)
	_, draft := scriptAndTool(t, f.rows(t))
	out := f.mustReview(t, fmt.Sprint(draft.ID))
	want := "\nsource:   the exact repeat rule (the recorded commands of a command chain, as they ran; no model call), "
	if !strings.Contains(out, want) || strings.Contains(out, "extractor") {
		t.Errorf("review =\n%s\nwant the source line of the exact repeat rule, and no extractor", out)
	}
}
