package store

import (
	"context"
	"database/sql"
	"slices"
	"testing"
	"time"
)

// archiveStore holds workspaces A (live) and B (closed) and adds items made active at day 0.
type archiveStore struct {
	t  *testing.T
	s  *Store
	t0 time.Time
}

func newArchiveStore(t *testing.T) archiveStore {
	t.Helper()
	a := archiveStore{t: t, s: openM1(t), t0: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)}
	write(t, a.s, func(tx *sql.Tx) error {
		for _, w := range []Workspace{
			{Key: "kA", HerdrSession: "default", HerdrWorkspaceID: "w1", Label: "shop", Status: WorkspaceLive, LastSeen: a.t0},
			{Key: "kB", HerdrSession: "default", HerdrWorkspaceID: "w2", Label: "blog", Status: WorkspaceClosed, LastSeen: a.t0},
		} {
			if err := InsertWorkspace(context.Background(), tx, w); err != nil {
				return err
			}
		}
		return nil
	})
	return a
}

func (a archiveStore) day(n int) time.Time { return a.t0.Add(time.Duration(n) * 24 * time.Hour) }

// add makes an active item in workspace key (nil: global), pinned or not, at day 0.
func (a archiveStore) add(key *string, pinned bool) int64 {
	a.t.Helper()
	r := ItemRevision{Text: "x", Scope: ScopeGlobal, WorkspaceKey: key, State: StateActive, Pinned: pinned,
		Author: AuthorYou, Origin: OriginRemember, Created: a.t0}
	if key != nil {
		r.Scope = ScopeWorkspace
	}
	var id int64
	write(a.t, a.s, func(tx *sql.Tx) (err error) {
		id, _, err = CreateItem(context.Background(), tx, Item{ClaimID: "c", Kind: KindFact, Created: a.t0}, r)
		return err
	})
	return id
}

// receipt adds a receipt of result in workspace key that holds revision 1 of item, at day 89.
func (a archiveStore) receipt(key string, item int64, result Result) {
	a.t.Helper()
	write(a.t, a.s, func(tx *sql.Tx) error {
		_, err := InsertDelivery(context.Background(), tx, Delivery{Profile: "claude", PaneID: "w1:p1", WorkspaceKey: &key,
			ItemRevisions: []RevisionRef{{Item: item, Revision: 1}}, Result: result, At: a.day(89)})
		return err
	})
}

// candidates reads the ids of the candidates at day n.
func (a archiveStore) candidates(n int) []int64 {
	a.t.Helper()
	var ids []int64
	write(a.t, a.s, func(tx *sql.Tx) error {
		cs, err := ArchiveCandidates(context.Background(), tx, a.day(n).Add(-ArchiveAge))
		for _, c := range cs {
			ids = append(ids, c.ItemID)
		}
		return err
	})
	return ids
}

// AC: with a fake clock, no receipt for 91 days gives a candidate; a receipt on day 89 does
// not. A withheld receipt is no relevant session, and neither is a receipt of another workspace.
func TestArchiveCandidatesAge(t *testing.T) {
	a := newArchiveStore(t)
	none := a.add(ptr("kA"), false)
	seen := a.add(ptr("kA"), false)
	withheld := a.add(ptr("kA"), false)
	elsewhere := a.add(ptr("kA"), false)
	a.receipt("kA", seen, ResultEmitted)
	a.receipt("kA", withheld, ResultWithheld)
	a.receipt("kB", elsewhere, ResultEmitted)
	if got := a.candidates(90); len(got) != 0 {
		t.Errorf("candidates at day 90 = %v, want none", got)
	}
	if got, want := a.candidates(91), []int64{none, withheld, elsewhere}; !slices.Equal(got, want) {
		t.Errorf("candidates at day 91 = %v, want %v", got, want)
	}
	if got, want := a.candidates(180), []int64{none, withheld, elsewhere, seen}; !slices.Equal(got, want) {
		t.Errorf("candidates at day 180 = %v, want %v (the oldest first)", got, want)
	}
}

// AC (S29): a pinned item with no session for 6 months stays active and is no candidate. An
// unpin starts its age.
func TestArchiveCandidatesPinned(t *testing.T) {
	a := newArchiveStore(t)
	pinned := a.add(ptr("kA"), true)
	if got := a.candidates(183); len(got) != 0 {
		t.Errorf("candidates at day 183 = %v, want none: a pinned item never ages", got)
	}
	write(t, a.s, func(tx *sql.Tx) error {
		_, err := WriteRevision(context.Background(), tx, 1, ItemRevision{ItemID: pinned, Text: "x", Scope: ScopeWorkspace,
			WorkspaceKey: ptr("kA"), State: StateActive, Author: AuthorYou, Origin: OriginReview, Created: a.day(183)})
		return err
	})
	if got := a.candidates(273); len(got) != 0 {
		t.Errorf("candidates 90 days after the unpin = %v, want none", got)
	}
	if got := a.candidates(274); !slices.Equal(got, []int64{pinned}) {
		t.Errorf("candidates 91 days after the unpin = %v, want [%d]", got, pinned)
	}
}

// AC: a global item that only workspace B got stays active: for a global item any workspace
// counts.
func TestArchiveCandidatesGlobal(t *testing.T) {
	a := newArchiveStore(t)
	global := a.add(nil, false)
	a.receipt("kB", global, ResultEmitted)
	if got := a.candidates(91); len(got) != 0 {
		t.Errorf("candidates at day 91 = %v, want none", got)
	}
}

// A keep starts a new 90-day period; a restore starts the age again, and a retired item is no
// candidate.
func TestArchiveCandidatesKeepRestore(t *testing.T) {
	a := newArchiveStore(t)
	kept := a.add(ptr("kA"), false)
	restored := a.add(ptr("kA"), false)
	ctx := context.Background()
	rev := func(id, expected int64, state State, n int) {
		write(t, a.s, func(tx *sql.Tx) error {
			_, err := WriteRevision(ctx, tx, expected, ItemRevision{ItemID: id, Text: "x", Scope: ScopeWorkspace,
				WorkspaceKey: ptr("kA"), State: state, Author: AuthorYou, Origin: OriginReview, Created: a.day(n)})
			return err
		})
	}
	write(t, a.s, func(tx *sql.Tx) error { return KeepItem(ctx, tx, kept, a.day(100)) })
	rev(restored, 1, StateRetired, 100)
	if got := a.candidates(190); len(got) != 0 {
		t.Errorf("candidates at day 190 = %v, want none", got)
	}
	rev(restored, 2, StateActive, 150)
	if got := a.candidates(191); !slices.Equal(got, []int64{kept}) {
		t.Errorf("candidates at day 191 = %v, want [%d]", got, kept)
	}
	if got := a.candidates(241); !slices.Equal(got, []int64{kept, restored}) {
		t.Errorf("candidates at day 241 = %v, want [%d %d]", got, kept, restored)
	}
}

// ClosedWorkspaces counts the active items of each closed key, with no text.
func TestClosedWorkspaces(t *testing.T) {
	a := newArchiveStore(t)
	a.add(ptr("kA"), false)
	a.add(ptr("kB"), false)
	a.add(ptr("kB"), true)
	var ws []ClosedWorkspace
	write(t, a.s, func(tx *sql.Tx) (err error) {
		ws, err = ClosedWorkspaces(context.Background(), tx)
		return err
	})
	if want := []ClosedWorkspace{{Key: "kB", Label: "blog", Items: 2}}; !slices.Equal(ws, want) {
		t.Errorf("ClosedWorkspaces() = %v, want %v", ws, want)
	}
}

// A disputed item is still delivered (§9.1), so it ages like an active one. A candidate of a
// closed workspace says so, and ClosedWorkspaces counts its disputed items.
func TestArchiveCandidatesDisputedClosed(t *testing.T) {
	a := newArchiveStore(t)
	disputed := a.add(ptr("kB"), false)
	write(t, a.s, func(tx *sql.Tx) error {
		_, err := WriteRevision(context.Background(), tx, 1, ItemRevision{ItemID: disputed, Text: "x", Scope: ScopeWorkspace,
			WorkspaceKey: ptr("kB"), State: StateDisputed, Author: AuthorBrain, Origin: OriginReview, Created: a.day(10)})
		return err
	})
	var cs []ArchiveCandidate
	var ws []ClosedWorkspace
	write(t, a.s, func(tx *sql.Tx) (err error) {
		if cs, err = ArchiveCandidates(context.Background(), tx, a.day(91).Add(-ArchiveAge)); err != nil {
			return err
		}
		ws, err = ClosedWorkspaces(context.Background(), tx)
		return err
	})
	if len(cs) != 1 || cs[0].ItemID != disputed || !cs[0].Closed || !cs[0].Since.Equal(a.t0) {
		t.Errorf("ArchiveCandidates() = %+v, want item %d of closed kB since day 0", cs, disputed)
	}
	if want := []ClosedWorkspace{{Key: "kB", Label: "blog", Items: 1}}; !slices.Equal(ws, want) {
		t.Errorf("ClosedWorkspaces() = %v, want %v", ws, want)
	}
}

// addTool makes an active tool item name in workspace kA, pinned or not, at day 0.
func (a archiveStore) addTool(name string, pinned bool) int64 {
	a.t.Helper()
	r := ItemRevision{Text: toolText(name), Scope: ScopeWorkspace, WorkspaceKey: ptr("kA"), State: StateActive,
		Pinned: pinned, Author: AuthorYou, Origin: OriginReview, Created: a.t0}
	var id int64
	write(a.t, a.s, func(tx *sql.Tx) (err error) {
		id, _, err = CreateItem(context.Background(), tx, Item{ClaimID: name, Kind: KindTool, Created: a.t0}, r)
		return err
	})
	return id
}

// call adds n calls of tool with result at day d.
func (a archiveStore) call(tool int64, result CallResult, d, n int) {
	a.t.Helper()
	write(a.t, a.s, func(tx *sql.Tx) error {
		for range n {
			code := 0
			c := ToolCall{ToolID: tool, Origin: OriginAgent, Result: result, At: a.day(d)}
			if result == CallRan {
				c.ExitCode = &code
			}
			if _, err := AddToolCall(context.Background(), tx, c); err != nil {
				return err
			}
		}
		return nil
	})
}

// AC (E17-S5): a tool ages by its ran calls only. A tool with no call for 91 days is a
// candidate, and so is a tool with only refused calls, however many. A ran call on day 89 keeps
// a tool, and a pinned tool never ages.
func TestArchiveCandidatesTools(t *testing.T) {
	a := newArchiveStore(t)
	idle := a.addTool("idle", false)
	ran := a.addTool("ran", false)
	refused := a.addTool("refused", false)
	pinned := a.addTool("pinned", true)
	a.call(ran, CallRan, 89, 1)
	a.call(refused, CallRefused, 89, 5)
	a.call(refused, CallRefused, 90, 5)
	if got := a.candidates(90); len(got) != 0 {
		t.Errorf("candidates at day 90 = %v, want none", got)
	}
	if got, want := a.candidates(91), []int64{idle, refused}; !slices.Equal(got, want) {
		t.Errorf("candidates at day 91 = %v, want %v", got, want)
	}
	if got, want := a.candidates(180), []int64{idle, refused, ran}; !slices.Equal(got, want) {
		t.Errorf("candidates at day 180 = %v, want %v; pinned tool %d never ages", got, want, pinned)
	}
}
