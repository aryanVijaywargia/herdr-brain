package brained

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/aryanVijaywargia/herdr-brain/internal/adapter"
	"github.com/aryanVijaywargia/herdr-brain/internal/fingerprint"
	"github.com/aryanVijaywargia/herdr-brain/internal/signal"
	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

// s40Prompt is the prompt of the claude-s40 reads: the second read repeats it.
const s40Prompt = "Reply with only the word ready."

// started is one start of a suggestion process by Capture.
type started struct {
	environ []string
	d       Dispatch
}

// suggestHarness is a Capture of pane w1:p1 with a claude agent after the claude-s40 reads,
// and a ready proposal that their prompt led to. The next Change step stores the prompt a
// third time (claude-e14s3-3-idle.txt).
type suggestHarness struct {
	*capHarness
	proposal int64
	mu       sync.Mutex
	starts   []started
}

func newSuggestHarness(t *testing.T) *suggestHarness {
	t.Helper()
	k := &suggestHarness{capHarness: newCapture(t, "s1", t.TempDir())}
	k.c.environ = []string{"HOME=/home/me", "HERDR_PANE_ID=plugin-pane", "HERDR_BIN_PATH=/bin/herdr"}
	k.c.start = func(_ context.Context, environ []string, d Dispatch) error {
		k.mu.Lock()
		defer k.mu.Unlock()
		k.starts = append(k.starts, started{environ, d})
		return nil
	}
	k.pane("w1:p1", "claude", "")
	k.reads(t, "w1:p1", "claude-s40-0-base.txt", "claude-s40-1-idle.txt", "claude-s40-2-idle.txt",
		"claude-e14s3-3-idle.txt")
	k.must(t, Baseline, "w1:p1", "", GapNone)
	k.must(t, Change, "w1:p1", "idle", GapNone)
	k.must(t, Change, "w1:p1", "idle", GapNone)
	k.proposal = k.propose(t)
	// The third prompt comes after the match window: a hook record of it must not enrich the
	// stored second prompt, which had no hook record of its own.
	k.clock = k.clock.Add(2 * matchWindow)
	return k
}

// propose makes a pending global proposal whose trigger prompt is the stored s40 prompt.
func (k *suggestHarness) propose(t *testing.T) int64 {
	t.Helper()
	ctx := context.Background()
	var id int64
	k.tx(t, func(tx *sql.Tx) error {
		es, err := store.PromptEntries(ctx, tx)
		if err != nil {
			return err
		}
		i := slices.IndexFunc(es, func(e store.PromptEntry) bool { return e.Hash == fingerprint.Prompt(s40Prompt) })
		if i < 0 {
			return fmt.Errorf("prompt index = %+v, want the s40 prompt", es)
		}
		claim, err := store.ClaimID(ctx, tx, store.ClaimKey{Situation: "ready", Rule: s40Prompt}, k.clock)
		if err != nil {
			return err
		}
		cand, err := store.Attach(ctx, tx, store.Candidate{ClaimID: claim, Scope: store.ScopeGlobal,
			Kind: store.CandidateProcedure}, es[i].ObservationID, store.DefaultGates(), signal.Supports)
		if err != nil {
			return err
		}
		item, err := store.InsertItem(ctx, tx, store.Item{ClaimID: claim, CurrentRevision: 1, Kind: store.KindPreference,
			Scope: store.ScopeGlobal, State: store.StateDraft, Created: k.clock})
		if err != nil {
			return err
		}
		base := int64(1)
		id, err = store.ProposeCandidate(ctx, tx, cand, store.Proposal{ItemID: &item, BaseRevision: &base,
			Text: "ready: " + s40Prompt, Scope: store.ScopeGlobal, Target: "global", Diff: "+ ready", Status: store.ProposalPending,
			Created: k.clock})
		return err
	})
	return id
}

// change is the Change step of pane w1:p1 that stores the repeat. It is made in the test
// goroutine, as it reads the store.
func (k *suggestHarness) change(t *testing.T) Step {
	t.Helper()
	return Step{Kind: Change, PaneID: "w1:p1", WorkspaceID: "w1", Gate: k.gate, Learning: k.learning(t, "w1:p1", "w1")}
}

// hookPrompt stores the hook record of the s40 prompt in pane w1:p1 at at, as hook.write does
// it in one transaction, and returns its reservation.
func (k *suggestHarness) hookPrompt(t *testing.T, at time.Time) Dispatch {
	t.Helper()
	d, err := k.hookWrite(at)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// hookWrite is hookPrompt for another goroutine: it returns its error.
func (k *suggestHarness) hookWrite(at time.Time) (d Dispatch, err error) {
	ctx := context.Background()
	st, err := store.Open(ctx, k.gate.Binding.Store, busyTimeout)
	if err != nil {
		return Dispatch{}, err
	}
	defer func() { err = errors.Join(err, st.Close()) }()
	err = st.Write(ctx, func(tx *sql.Tx) error {
		session, segment, err := store.HookSegment(ctx, tx, k.gate.Binding.Session, "claude", "w1:p1", "")
		if err != nil {
			return err
		}
		o := store.Observation{CaptureIdentity: fmt.Sprintf("s1/%d/hook/prompt/%s", segment, at.Format(time.RFC3339Nano)),
			Source: store.SourceHook, SegmentID: segment, Event: adapter.EventPrompt, Status: store.ObservationComplete,
			Sender: store.SenderUnconfirmed, Counts: true, At: at}
		o.Excerpt, o.Truncated, o.BrainOrigin = HookText("claude", s40Prompt)
		full := HookPrompt("claude", s40Prompt)
		if o.ID, err = store.AddObservation(ctx, tx, o); err != nil {
			return err
		}
		if err := IndexPrompt(ctx, tx, o, full); err != nil {
			return err
		}
		if err := Match(ctx, tx, "claude", session, o); err != nil {
			return err
		}
		d, err = Reserve(ctx, tx, o, full, at)
		return err
	})
	return d, err
}

func (k *suggestHarness) suggestions(t *testing.T) []store.Suggestion {
	t.Helper()
	var ss []store.Suggestion
	k.tx(t, func(tx *sql.Tx) (err error) {
		ss, err = store.Suggestions(context.Background(), tx)
		return err
	})
	return ss
}

// E14-S3 fallback: a pane read that stores a prompt that matches a ready proposal starts one
// suggestion process after its commit, with ids only and the herdr variables of the pane in
// place of brained's own.
func TestCaptureSuggestion(t *testing.T) {
	k := newSuggestHarness(t)
	k.must(t, Change, "w1:p1", "idle", GapNone)
	ss := k.suggestions(t)
	if len(ss) != 1 || len(k.starts) != 1 {
		t.Fatalf("suggestions %+v, starts %+v, want one", ss, k.starts)
	}
	s := ss[0]
	want := Dispatch{Suggestion: s.ID, Binding: "s1", Store: k.gate.Binding.Store, Workspace: "", Session: s.SessionID,
		Occurrence: s.ObservationID, Generation: 0, Proposal: k.proposal}
	if got := k.starts[0]; got.d != want {
		t.Errorf("dispatch = %+v, want %+v", got.d, want)
	}
	wantEnv := []string{"HOME=/home/me", "HERDR_BIN_PATH=/bin/herdr", "HERDR_ENV=1", "HERDR_SESSION=s1",
		"HERDR_SOCKET_PATH=" + k.f.path, "HERDR_PANE_ID=w1:p1", "HERDR_WORKSPACE_ID=w1"}
	if !slices.Equal(k.starts[0].environ, wantEnv) {
		t.Errorf("environment = %q, want %q", k.starts[0].environ, wantEnv)
	}
	if s.AgentPaneID != "w1:p1" || s.ProposalID != k.proposal {
		t.Errorf("suggestion = %+v, want pane w1:p1 and proposal %d", s, k.proposal)
	}
}

// A hook record and a pane read of the same prompt show one suggestion, in either order.
func TestSuggestionOnce(t *testing.T) {
	t.Run("hook first", func(t *testing.T) {
		k := newSuggestHarness(t)
		if d := k.hookPrompt(t, k.clock); d.Suggestion == 0 {
			t.Fatal("the hook record reserved nothing")
		}
		k.must(t, Change, "w1:p1", "idle", GapNone)
		if ss := k.suggestions(t); len(ss) != 1 || len(k.starts) != 0 {
			t.Errorf("suggestions %+v, capture starts %+v, want the hook's one only", ss, k.starts)
		}
	})
	t.Run("pane first", func(t *testing.T) {
		k := newSuggestHarness(t)
		k.must(t, Change, "w1:p1", "idle", GapNone)
		if d := k.hookPrompt(t, k.clock); d.Suggestion != 0 {
			t.Errorf("the hook record reserved %+v after the pane read, want nothing", d)
		}
		if ss := k.suggestions(t); len(ss) != 1 || len(k.starts) != 1 {
			t.Errorf("suggestions %+v, capture starts %+v, want the pane read's one only", ss, k.starts)
		}
	})
}

// AC: a hook dispatch and a capture dispatch of one occurrence at the same time give one
// suggestion: the store serializes the two writes, and each reserves after the enrichment.
func TestSuggestionConcurrent(t *testing.T) {
	for i := range 5 { // 5 rounds: the order of the two writes is up to the scheduler
		k := newSuggestHarness(t)
		step := k.change(t)
		at := k.clock.Add(500 * time.Millisecond) // the hook record comes between two reads
		begin := make(chan struct{})
		var wg sync.WaitGroup
		var hook Dispatch
		var capErr, hookErr error
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-begin
			capErr = k.c.Handle(context.Background(), step)
		}()
		go func() {
			defer wg.Done()
			<-begin
			hook, hookErr = k.hookWrite(at)
		}()
		close(begin)
		wg.Wait()
		if err := errors.Join(capErr, hookErr); err != nil {
			t.Fatalf("round %d: %v", i, err)
		}
		shown := len(k.starts)
		if hook.Suggestion != 0 {
			shown++
		}
		if ss := k.suggestions(t); len(ss) != 1 || shown != 1 {
			t.Errorf("round %d: suggestions %+v, dispatches %d (hook %+v), want one", i, ss, shown, hook)
		}
	}
}

// The dispatch arguments carry the ids and nothing else, and parse back.
func TestDispatchArgs(t *testing.T) {
	d := Dispatch{Suggestion: 3, Binding: "default", Store: "/s/main", Workspace: "ws_a1", Session: 4, Occurrence: 9,
		Generation: 2, Proposal: 7}
	args := d.Args()
	if args[0] != "suggest" {
		t.Fatalf("Args() = %q, want the suggest command", args)
	}
	got, err := ParseDispatch(args[1:])
	if err != nil || got != d {
		t.Errorf("ParseDispatch(Args()) = %+v, %v, want %+v", got, err, d)
	}
	d.Workspace = ""
	if got, err := ParseDispatch(d.Args()[1:]); err != nil || got != d {
		t.Errorf("ParseDispatch with no workspace = %+v, %v, want %+v", got, err, d)
	}
	for _, bad := range [][]string{nil, {"--suggestion", "0"}, append(d.Args()[1:], "extra")} {
		if _, err := ParseDispatch(bad); err == nil {
			t.Errorf("ParseDispatch(%q) = nil error, want an error", bad)
		}
	}
}
