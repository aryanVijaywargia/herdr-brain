package store

import (
	"context"
	"database/sql"
	"testing"

	"github.com/aryanVijaywargia/herdr-brain/internal/fingerprint"
)

// reserve stores prompt text in workspace ws and reserves its suggestion, and returns the
// observation and the reservation.
func (p *promptStore) reserve(t *testing.T, ws, text string) (int64, Suggestion) {
	t.Helper()
	ctx := context.Background()
	var o int64
	var s Suggestion
	write(t, p.Store, func(tx *sql.Tx) (err error) {
		if o, err = p.prompt(ctx, tx, ws, text); err != nil {
			return err
		}
		s, err = ReserveSuggestion(ctx, tx, o, fingerprint.Prompt(text), testNow)
		return err
	})
	return o, s
}

// reserveObs reserves the suggestion of stored observation o with the prompt text.
func (p *promptStore) reserveObs(t *testing.T, o int64, text string) Suggestion {
	t.Helper()
	var s Suggestion
	write(t, p.Store, func(tx *sql.Tx) (err error) {
		s, err = ReserveSuggestion(context.Background(), tx, o, fingerprint.Prompt(text), testNow)
		return err
	})
	return s
}

// E14-S3: a prompt that matches a ready proposal reserves one suggestion, with the ids of its
// session, segment and binding. The same occurrence never reserves twice (S57), a prompt with
// no match reserves nothing, and each new prompt reserves its own (no rate cap).
func TestReserveSuggestion(t *testing.T) {
	p := newPromptStore(t)
	id, _ := p.propose(t, "run make Foo", ScopeGlobal)
	o, s := p.reserve(t, "ws-A", "run make Foo")
	want := Suggestion{ID: 1, ObservationID: o, ProposalID: id, SessionID: 1, Generation: 0, HerdrSession: "default",
		WorkspaceKey: "ws-A", AgentPaneID: "w1:p1", PromptHash: fingerprint.Prompt("run make Foo"), Reserved: testNow}
	if s != want {
		t.Errorf("ReserveSuggestion() = %+v, want %+v", s, want)
	}
	if again := p.reserveObs(t, o, "run make Foo"); again.ID != 0 {
		t.Errorf("second reservation of observation %d = %+v, want none", o, again)
	}
	if _, none := p.reserve(t, "ws-A", "run make Bar"); none.ID != 0 {
		t.Errorf("reservation of a prompt with no match = %+v, want none", none)
	}
	if _, next := p.reserve(t, "ws-A", "run make Foo"); next.ID != 2 {
		t.Errorf("reservation of the next prompt = %+v, want suggestion 2", next)
	}
}

// S57: a hook record and a pane read of one prompt are one occurrence, linked by enrichment:
// whichever path reserves first, the other reserves nothing.
func TestReserveSuggestionOneOccurrence(t *testing.T) {
	for _, hookFirst := range []bool{true, false} {
		p := newPromptStore(t)
		p.propose(t, "run make Foo", ScopeGlobal)
		first, s := p.reserve(t, "ws-A", "run make Foo")
		var second int64
		write(t, p.Store, func(tx *sql.Tx) (err error) {
			ctx := context.Background()
			if second, err = p.prompt(ctx, tx, "ws-A", "run make Foo"); err != nil {
				return err
			}
			if hookFirst {
				return Enrich(ctx, tx, second, first) // the later pane read enriches with the hook record
			}
			return Enrich(ctx, tx, first, second) // the later hook record enriches the pane read
		})
		if s.ID == 0 {
			t.Fatal("first path reserved nothing")
		}
		if got := p.reserveObs(t, second, "run make Foo"); got.ID != 0 {
			t.Errorf("hook first %v: the other path reserved %+v, want none", hookFirst, got)
		}
	}
}

// "Later" mutes a proposal for the rest of its session only (§5.5); extraction off and a
// rejected proposal stop new suggestions.
func TestReserveSuggestionRules(t *testing.T) {
	ctx := context.Background()
	p := newPromptStore(t)
	older, _ := p.propose(t, "run make Foo", ScopeGlobal)
	newer, _ := p.propose(t, "run make Foo", ScopeGlobal)
	_, s := p.reserve(t, "ws-A", "run make Foo")
	if s.ProposalID != older {
		t.Fatalf("reservation = %+v, want the oldest proposal %d", s, older)
	}
	write(t, p.Store, func(tx *sql.Tx) error {
		if err := ShowSuggestion(ctx, tx, s.ID, "w1:p9", testNow); err != nil {
			return err
		}
		return DecideSuggestions(ctx, tx, older, SuggestionLater)
	})
	if _, s = p.reserve(t, "ws-A", "run make Foo"); s.ProposalID != newer {
		t.Errorf("after later: reservation = %+v, want proposal %d: %d is muted in the session", s, newer, older)
	}
	write(t, p.Store, func(tx *sql.Tx) error { return SetExtraction(ctx, tx, "ws-B", false, testNow) })
	if _, s = p.reserve(t, "ws-B", "run make Foo"); s.ID != 0 {
		t.Errorf("extraction off: reservation = %+v, want none", s)
	}
	write(t, p.Store, func(tx *sql.Tx) error { return SetExtraction(ctx, tx, "ws-B", true, testNow) })
	if _, s = p.reserve(t, "ws-B", "run make Foo"); s.ProposalID != older {
		t.Errorf("another session: reservation = %+v, want proposal %d: later mutes one session", s, older)
	}
	write(t, p.Store, func(tx *sql.Tx) error {
		for _, id := range []int64{older, newer} {
			if err := DecideProposal(ctx, tx, id, ProposalRejected, SurfacePane, testNow); err != nil {
				return err
			}
		}
		return nil
	})
	if _, s = p.reserve(t, "ws-B", "run make Foo"); s.ID != 0 {
		t.Errorf("after the rejects: reservation = %+v, want none", s)
	}
}

// "Later" in one pane mutes the proposal in the session of that pane's suggestion only: a
// shown suggestion of the same global proposal in another session waits on (§5.5).
func TestLaterSuggestions(t *testing.T) {
	ctx := context.Background()
	p := newPromptStore(t)
	id, _ := p.propose(t, "run make Foo", ScopeGlobal)
	_, a := p.reserve(t, "ws-A", "run make Foo")
	write(t, p.Store, func(tx *sql.Tx) error {
		// ws-B has its own session; move its agent to another pane, as another agent.
		if _, err := p.segment(ctx, tx, "ws-B"); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `UPDATE segments SET pane_id = 'w2:p1' WHERE workspace_key = 'ws-B'`)
		return err
	})
	_, b := p.reserve(t, "ws-B", "run make Foo")
	write(t, p.Store, func(tx *sql.Tx) error {
		for _, s := range []struct {
			id   int64
			pane string
		}{{a.ID, "w1:p9"}, {b.ID, "w2:p9"}} {
			if err := ShowSuggestion(ctx, tx, s.id, s.pane, testNow); err != nil {
				return err
			}
		}
		// Later in the review pane of a.
		if err := LaterSuggestions(ctx, tx, id, "default", "w1:p9"); err != nil {
			return err
		}
		ss, err := Suggestions(ctx, tx)
		if len(ss) != 2 || ss[0].Result != SuggestionLater || ss[1].Result != "" {
			t.Errorf("suggestions = %+v, want a later and b waiting", ss)
		}
		return err
	})
}

// §6.7: brain forget --session and the item forget delete the suggestions, with their prompt
// hashes, as they delete the prompt index rows.
func TestForgetSuggestions(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		fn   func(tx *sql.Tx, s Suggestion, item int64) error
	}{
		{"session", func(tx *sql.Tx, s Suggestion, _ int64) error { return ForgetSession(ctx, tx, s.SessionID) }},
		{"item", func(tx *sql.Tx, _ Suggestion, item int64) error { return EraseItem(ctx, tx, item, testNow) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newPromptStore(t)
			_, item := p.propose(t, "run make Foo", ScopeGlobal)
			_, s := p.reserve(t, "ws-A", "run make Foo")
			write(t, p.Store, func(tx *sql.Tx) error {
				if err := tc.fn(tx, s, item); err != nil {
					return err
				}
				ss, err := Suggestions(ctx, tx)
				if len(ss) != 0 {
					t.Errorf("suggestions after the forget = %+v, want none", ss)
				}
				return err
			})
		})
	}
}

// Your decision gives its result to the shown suggestions of the proposal; an unshown one is
// expired only while it has no result.
func TestDecideSuggestions(t *testing.T) {
	ctx := context.Background()
	p := newPromptStore(t)
	id, _ := p.propose(t, "run make Foo", ScopeGlobal)
	_, shown := p.reserve(t, "ws-A", "run make Foo")
	_, unshown := p.reserve(t, "ws-A", "run make Foo")
	write(t, p.Store, func(tx *sql.Tx) error {
		if err := ShowSuggestion(ctx, tx, shown.ID, "w1:p9", testNow); err != nil {
			return err
		}
		if err := DecideSuggestions(ctx, tx, id, SuggestionApproved); err != nil {
			return err
		}
		if err := ExpireSuggestion(ctx, tx, shown.ID); err != nil {
			return err
		}
		if err := ExpireSuggestion(ctx, tx, unshown.ID); err != nil {
			return err
		}
		ss, err := Suggestions(ctx, tx)
		if len(ss) != 2 || ss[0].Result != SuggestionApproved || ss[0].ReviewPaneID != "w1:p9" || !ss[0].ShownAt.Equal(testNow) ||
			ss[1].Result != SuggestionExpired || !ss[1].ShownAt.IsZero() {
			t.Errorf("suggestions = %+v, want approved and shown, then expired", ss)
		}
		return err
	})
}

// AC #253: a lesson proposal reserves its suggestion beside the pane of the lesson, with the
// prompt hash of the lesson. The same occurrence never reserves twice, and "later" mutes the
// proposal in the session.
func TestReserveLessonSuggestion(t *testing.T) {
	ctx := context.Background()
	p := newPromptStore(t)
	id, _ := p.propose(t, "from now on run make lint", ScopeWorkspace)
	var o int64
	var s Suggestion
	lesson := func() Suggestion {
		t.Helper()
		var got Suggestion
		write(t, p.Store, func(tx *sql.Tx) (err error) {
			got, err = ReserveLessonSuggestion(ctx, tx, o, id, testNow)
			return err
		})
		return got
	}
	write(t, p.Store, func(tx *sql.Tx) (err error) {
		o, err = p.prompt(ctx, tx, "ws-A", "from now on run make lint")
		return err
	})
	s = lesson()
	want := Suggestion{ID: 1, ObservationID: o, ProposalID: id, SessionID: 1, HerdrSession: "default", WorkspaceKey: "ws-A",
		AgentPaneID: "w1:p1", PromptHash: fingerprint.Prompt("from now on run make lint"), Reserved: testNow}
	if s != want {
		t.Errorf("ReserveLessonSuggestion() = %+v, want %+v", s, want)
	}
	if again := lesson(); again.ID != 0 {
		t.Errorf("second reservation = %+v, want none", again)
	}
	write(t, p.Store, func(tx *sql.Tx) (err error) {
		if err := ShowSuggestion(ctx, tx, s.ID, "w1:p9", testNow); err != nil {
			return err
		}
		if err := DecideSuggestions(ctx, tx, id, SuggestionLater); err != nil {
			return err
		}
		o, err = p.prompt(ctx, tx, "ws-A", "always run make lint")
		return err
	})
	if muted := lesson(); muted.ID != 0 {
		t.Errorf("reservation after later = %+v, want none", muted)
	}
}
