package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

// noInputTool is a tool contract named name with no input, in folder ~/src/shop.
func noInputTool(name string) string {
	return "name = \"" + name + "\"\ndescription = \"Run the tests\"\ncwd = \"~/src/shop\"\ntimeout_s = 600\n" +
		"effects = \"runs tests\"\nread_only = true\nargv = [\"go\", \"test\", \"./...\"]\n"
}

// bindingStore is a tool store with an active tool with no input in k1; it returns the
// store, the tool row and a function that adds an automation item of binding b in k1.
func bindingStore(t *testing.T) (*Store, Tool, func(state State, b Binding) (int64, error)) {
	t.Helper()
	s, add := toolStore(t)
	k1 := "k1"
	id, err := add(&k1, StateActive, noInputTool("go-test"))
	if err != nil {
		t.Fatal(err)
	}
	tl := readTool(t, s, id)
	now := time.Date(2026, 10, 4, 10, 0, 0, 0, time.UTC)
	bind := func(state State, b Binding) (int64, error) {
		var id int64
		err := s.Write(context.Background(), func(tx *sql.Tx) (err error) {
			id, _, err = CreateItem(context.Background(), tx, Item{ClaimID: "a" + b.Folder, Kind: KindAutomation, Created: now},
				ItemRevision{Text: b.Text(), Scope: ScopeWorkspace, WorkspaceKey: &k1, State: state, Author: AuthorYou,
					Origin: OriginReview, Created: now})
			return err
		})
		return id, err
	}
	return s, tl, bind
}

func goTestBinding(tl Tool) Binding {
	return Binding{Tool: tl.Name, ToolID: tl.ID, ContractHash: tl.ContractHash, Folder: "~/src/shop", CLI: "claude",
		Trigger: stopTrigger}
}

func TestBindingText(t *testing.T) {
	b := Binding{Tool: "go-test", ToolID: 3, ContractHash: "h", Folder: "/src/shop", CLI: "codex", Trigger: stopTrigger}
	got, err := ParseBinding(b.Text())
	if err != nil || got != b {
		t.Errorf("ParseBinding(Text()) = %+v, %v, want %+v", got, err, b)
	}
	for _, text := range []string{"", b.Text() + "extra = 1\n", strings.Replace(b.Text(), "cli", "x", 1), "tool = 3\n"} {
		if _, err := ParseBinding(text); err == nil {
			t.Errorf("ParseBinding(%q) = nil, want an error", text)
		}
	}
	if _, ok := StopTrigger("agy"); ok {
		t.Error("StopTrigger(agy) = ok, want no tested condition")
	}
}

// An active automation item writes its row, enabled, with the approval time. A pause stays at
// a new approval; a retired revision clears the approval.
func TestAutomationRow(t *testing.T) {
	s, tl, bind := bindingStore(t)
	id, err := bind(StateActive, goTestBinding(tl))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	read := func() Automation {
		var a Automation
		write(t, s, func(tx *sql.Tx) (err error) {
			a, err = AutomationByID(ctx, tx, id)
			return err
		})
		return a
	}
	a := read()
	if !a.Enabled || a.Approved.IsZero() || a.WorkspaceKey != "k1" || a.Folder != "~/src/shop" || a.ToolID != tl.ID ||
		a.ContractHash != tl.ContractHash || a.CLI != "claude" || a.Trigger != stopTrigger || a.LastResult != "" {
		t.Errorf("automation = %+v", a)
	}
	write(t, s, func(tx *sql.Tx) error { return SetAutomationEnabled(ctx, tx, id, false) })
	write(t, s, func(tx *sql.Tx) error {
		cur, err := ItemRevisionByKey(ctx, tx, id, 1)
		if err != nil {
			return err
		}
		b := goTestBinding(tl)
		b.ContractHash = "h2"
		cur.Text = b.Text()
		_, err = WriteRevision(ctx, tx, 1, cur)
		return err
	})
	if a := read(); a.Enabled || a.ContractHash != "h2" {
		t.Errorf("automation = %+v, want it paused with the new hash", a)
	}
	write(t, s, func(tx *sql.Tx) error {
		cur, err := ItemRevisionByKey(ctx, tx, id, 2)
		if err != nil {
			return err
		}
		cur.State = StateRetired
		_, err = WriteRevision(ctx, tx, 2, cur)
		return err
	})
	if a := read(); !a.Approved.IsZero() {
		t.Errorf("approved = %v, want none after the retirement", a.Approved)
	}
	write(t, s, func(tx *sql.Tx) error {
		if as, err := ActiveAutomations(ctx, tx); err != nil || len(as) != 0 {
			t.Errorf("ActiveAutomations() = %+v, %v, want none", as, err)
		}
		if err := SetAutomationEnabled(ctx, tx, 99, true); !errors.Is(err, sql.ErrNoRows) {
			t.Errorf("SetAutomationEnabled(99) = %v, want sql.ErrNoRows", err)
		}
		return nil
	})
}

func TestCheckBinding(t *testing.T) {
	s, tl, _ := bindingStore(t)
	k2 := "k2"
	now := time.Date(2026, 10, 4, 10, 0, 0, 0, time.UTC)
	add := func(key *string, text string) Tool {
		t.Helper()
		var id int64
		write(t, s, func(tx *sql.Tx) (err error) {
			id, _, err = CreateItem(context.Background(), tx, Item{ClaimID: text + *key, Kind: KindTool, Created: now},
				ItemRevision{Text: text, Scope: ScopeWorkspace, WorkspaceKey: key, State: StateActive, Author: AuthorYou,
					Origin: OriginReview, Created: now})
			return err
		})
		return readTool(t, s, id)
	}
	k1 := "k1"
	withInput := add(&k1, toolText("pkg-test"))
	other := add(&k2, noInputTool("go-test"))
	ok := goTestBinding(tl)
	for _, tc := range []struct {
		name string
		b    func() Binding
		want string // "" passes
	}{
		{"home form", func() Binding { return ok }, ""},
		{"absolute form", func() Binding { b := ok; b.Folder = "/home/u/src/shop"; return b }, ""},
		{"codex", func() Binding { b := ok; b.CLI = "codex"; return b }, ""},
		{"agy", func() Binding { b := ok; b.CLI = "agy"; return b }, "no tested stop condition"},
		{"trigger", func() Binding { b := ok; b.Trigger = "working → idle"; return b }, "is not the stop condition"},
		{"folder", func() Binding { b := ok; b.Folder = "/home/u/src/blog"; return b }, "not the pinned folder"},
		{"hash", func() Binding { b := ok; b.ContractHash = "old"; return b }, "not approved at the pinned contract hash"},
		{"input", func() Binding {
			return Binding{Tool: withInput.Name, ToolID: withInput.ID, ContractHash: withInput.ContractHash,
				Folder: "~/src/shop", CLI: "claude", Trigger: stopTrigger}
		}, "required input"},
		{"other workspace", func() Binding {
			b := ok
			b.ToolID, b.ContractHash = other.ID, other.ContractHash
			return b
		}, "a tool of another workspace"},
		{"no tool", func() Binding { b := ok; b.ToolID = 99; return b }, "does not exist"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			write(t, s, func(tx *sql.Tx) error {
				err := CheckBinding(context.Background(), tx, tc.b(), "k1", "/home/u")
				switch {
				case tc.want == "" && err != nil:
					t.Errorf("CheckBinding() = %v, want nil", err)
				case tc.want != "" && (!errors.Is(err, ErrBindingRefused) || !strings.Contains(err.Error(), tc.want)):
					t.Errorf("CheckBinding() = %v, want the refusal %q", err, tc.want)
				}
				return nil
			})
		})
	}
}

// StopCalls finds a ran agent call whose newest observation of its session is a hook stop, or
// an idle or done pane read right after a working one (M0 E17-S1); not a lone idle read, a
// denied approval (blocked → idle), a call during work, a refused call or an automation call.
func TestStopCalls(t *testing.T) {
	s, tl, _ := bindingStore(t)
	ctx := context.Background()
	at := time.Date(2026, 10, 4, 11, 0, 0, 0, time.UTC)
	type obs struct{ source, event string }
	var want []int64
	write(t, s, func(tx *sql.Tx) error {
		for i, tc := range []struct {
			obs    []obs
			origin CallOrigin
			result CallResult
			found  bool
		}{
			{[]obs{{SourceHook, "stop"}}, OriginAgent, CallRan, true},
			{[]obs{{SourceHerdr, "working"}, {SourceHerdr, "idle"}}, OriginAgent, CallRan, true},
			{[]obs{{SourceHerdr, "working"}, {SourceHerdr, "done"}}, OriginAgent, CallRan, true},
			{[]obs{{SourceHerdr, "idle"}}, OriginAgent, CallRan, false},
			{[]obs{{SourceHerdr, "working"}, {SourceHerdr, "blocked"}, {SourceHerdr, "idle"}}, OriginAgent, CallRan, false},
			{[]obs{{SourceHerdr, "working"}}, OriginAgent, CallRan, false},
			{[]obs{{SourceHook, "tool"}}, OriginAgent, CallRan, false},
			{[]obs{{SourceHook, "stop"}}, OriginAutomation, CallRan, false},
			{[]obs{{SourceHook, "stop"}}, OriginAgent, CallRefused, false},
		} {
			key := SessionKey{HerdrSession: "default", Harness: "claude", Kind: KeyAgent, ID: string(rune('a' + i))}
			sid, err := OpenSession(ctx, tx, key, "w1:p1", "k1", at)
			if err != nil {
				return err
			}
			seg, err := StartSegment(ctx, tx, sid, "w1:p1", "k1", "recent_unwrapped", at)
			if err != nil {
				return err
			}
			var last int64
			for j, o := range tc.obs {
				if last, err = AddObservation(ctx, tx, Observation{CaptureIdentity: fmt.Sprintf("%s-%d", key.ID, j),
					Source: o.source, SegmentID: seg, Event: o.event, Excerpt: "x", Status: ObservationComplete,
					Sender: SenderAssistant, Counts: true, At: at.Add(time.Duration(j) * time.Second)}); err != nil {
					return err
				}
			}
			if tc.found {
				want = append(want, last)
			}
			if _, err := AddToolCall(ctx, tx, ToolCall{ToolID: tl.ID, ContractHash: "h", SessionID: sid, Origin: tc.origin,
				Result: tc.result, At: at.Add(time.Minute)}); err != nil {
				return err
			}
			if err := EndSession(ctx, tx, key, at.Add(time.Minute)); err != nil {
				return err
			}
		}
		calls, err := StopCalls(ctx, tx, "default")
		if err != nil {
			return err
		}
		var got []int64
		for _, c := range calls {
			if c.ToolID != tl.ID || c.ContractHash != "h" || c.Harness != "claude" || c.WorkspaceKey != "k1" || c.PaneID != "w1:p1" {
				t.Errorf("StopCall = %+v", c)
			}
			got = append(got, c.Stop)
		}
		if !slices.Equal(got, want) {
			t.Errorf("StopCalls() stops = %v, want %v", got, want)
		}
		if calls, err := StopCalls(ctx, tx, "other"); err != nil || len(calls) != 0 {
			t.Errorf("StopCalls(other) = %+v, %v, want none", calls, err)
		}
		return nil
	})
}
