package store

import (
	"context"
	"database/sql"
	"errors"
	"maps"
	"testing"
	"time"
)

// ToolByName finds a tool of the caller's workspace first, then a global one; never a tool of
// another workspace, and never a retired one.
func TestToolByName(t *testing.T) {
	s, add := toolStore(t)
	k1, k2 := "k1", "k2"
	ws, err := add(&k1, StateActive, toolText("test-package"))
	if err != nil {
		t.Fatal(err)
	}
	global, err := add(nil, StateActive, toolText("test-package"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := add(&k2, StateRetired, toolText("lint")); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, tc := range []struct {
		name, key string
		want      int64 // 0: none
	}{
		{"test-package", "k1", ws},
		{"test-package", "k2", global},
		{"test-package", "", global},
		{"lint", "k2", 0},
		{"none", "k1", 0},
	} {
		write(t, s, func(tx *sql.Tx) error {
			got, err := ToolByName(ctx, tx, tc.name, tc.key)
			switch {
			case tc.want == 0 && !errors.Is(err, sql.ErrNoRows):
				t.Errorf("ToolByName(%s, %q) = %d, %v, want none", tc.name, tc.key, got.ID, err)
			case tc.want != 0 && (err != nil || got.ID != tc.want):
				t.Errorf("ToolByName(%s, %q) = %d, %v, want %d", tc.name, tc.key, got.ID, err, tc.want)
			}
			return nil
		})
	}
}

// A ran row keeps its exit code and duration; a refused row has neither. PaneSession finds the
// open session of the pane only.
func TestToolCalls(t *testing.T) {
	s, add := toolStore(t)
	id, err := add(nil, StateActive, toolText("test-package"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	at := time.Date(2026, 10, 3, 11, 0, 0, 0, time.UTC)
	code := 2
	write(t, s, func(tx *sql.Tx) error {
		if got, err := PaneSession(ctx, tx, "default", "w1:p1"); err != nil || got != 0 {
			t.Errorf("PaneSession() = %d, %v, want none", got, err)
		}
		session, err := OpenSession(ctx, tx, SessionKey{HerdrSession: "default", Harness: "claude", Kind: KeyAgent, ID: "a1"},
			"w1:p1", "", at)
		if err != nil {
			return err
		}
		if got, err := PaneSession(ctx, tx, "default", "w1:p1"); err != nil || got != session {
			t.Errorf("PaneSession() = %d, %v, want %d", got, err, session)
		}
		if got, err := PaneSession(ctx, tx, "other", "w1:p1"); err != nil || got != 0 {
			t.Errorf("PaneSession(other) = %d, %v, want none", got, err)
		}
		for _, c := range []ToolCall{
			{ToolID: id, ContractHash: "h1", SessionID: session, Origin: OriginAgent, Result: CallRan, At: at, ExitCode: &code,
				Duration: 1500 * time.Millisecond},
			{ToolID: id, ContractHash: "h1", Origin: OriginAutomation, Result: CallRefused, At: at, Duration: time.Second},
		} {
			if _, err := AddToolCall(ctx, tx, c); err != nil {
				return err
			}
		}
		cs, err := ToolCalls(ctx, tx)
		if err != nil {
			return err
		}
		if len(cs) != 2 || cs[0].SessionID != session || *cs[0].ExitCode != 2 || cs[0].Duration != 1500*time.Millisecond ||
			cs[1].SessionID != 0 || cs[1].ExitCode != nil || cs[1].Duration != 0 || cs[1].Result != CallRefused {
			t.Errorf("ToolCalls() = %+v", cs)
		}
		return nil
	})
}

// RanCalls counts only the ran calls since the time it gets (§13 M4 review prompt).
func TestRanCalls(t *testing.T) {
	s, add := toolStore(t)
	a, err := add(nil, StateActive, toolText("a"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := add(nil, StateActive, toolText("b"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	since := time.Date(2026, 9, 5, 0, 0, 0, 0, time.UTC)
	code := 0
	write(t, s, func(tx *sql.Tx) error {
		for _, c := range []ToolCall{
			{ToolID: a, Result: CallRan, At: since, ExitCode: &code},
			{ToolID: a, Result: CallRan, At: since.Add(time.Hour), ExitCode: &code},
			{ToolID: a, Result: CallRan, At: since.Add(-time.Second), ExitCode: &code},
			{ToolID: a, Result: CallRefused, At: since.Add(time.Hour)},
			{ToolID: b, Result: CallRefused, At: since.Add(time.Hour)},
		} {
			c.Origin = OriginAgent
			if _, err := AddToolCall(ctx, tx, c); err != nil {
				return err
			}
		}
		got, err := RanCalls(ctx, tx, since)
		if err != nil {
			return err
		}
		if want := map[int64]int{a: 2}; !maps.Equal(got, want) {
			t.Errorf("RanCalls() = %v, want %v", got, want)
		}
		return nil
	})
}
