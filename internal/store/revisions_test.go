package store

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"sync"
	"testing"
)

// newItem adds a workspace, a global item and a workspace draft item, the way remember does.
func newItem(t *testing.T, s *Store) (global, item int64) {
	t.Helper()
	ctx := context.Background()
	write(t, s, func(tx *sql.Tx) error {
		err := InsertWorkspace(ctx, tx, Workspace{Key: "k1", HerdrSession: "default",
			HerdrWorkspaceID: "w1", Label: "brain", Status: WorkspaceLive, LastSeen: testNow})
		if err != nil {
			return err
		}
		global, _, err = CreateItem(ctx, tx, Item{ClaimID: "c0", Kind: KindPreference, Created: testNow},
			ItemRevision{Text: "use npm", Scope: ScopeGlobal, State: StateActive, Author: AuthorYou,
				Origin: OriginRemember, Created: testNow})
		if err != nil {
			return err
		}
		item, _, err = CreateItem(ctx, tx, Item{ClaimID: "c1", Kind: KindFact, Created: testNow},
			ItemRevision{Text: "use pnpm", Scope: ScopeWorkspace, WorkspaceKey: ptr("k1"),
				State: StateDraft, Author: AuthorYou, Origin: OriginRemember, Created: testNow})
		return err
	})
	return global, item
}

// storeState is what a revision write can change.
type storeState struct {
	item        Item
	revisions   int
	generations int
}

func readState(t *testing.T, s *Store, id int64) storeState {
	t.Helper()
	var st storeState
	write(t, s, func(tx *sql.Tx) error {
		var err error
		st.item, err = ItemByID(context.Background(), tx, id)
		return err
	})
	st.revisions = queryInt(t, s, "SELECT count(*) FROM item_revisions")
	st.generations = queryInt(t, s, "SELECT count(*) FROM outputs")
	return st
}

// checkItemMatchesRevision checks that the items row copies its current revision.
func checkItemMatchesRevision(t *testing.T, s *Store, id int64) ItemRevision {
	t.Helper()
	var it Item
	var r ItemRevision
	write(t, s, func(tx *sql.Tx) error {
		var err error
		if it, err = ItemByID(context.Background(), tx, id); err != nil {
			return err
		}
		r, err = ItemRevisionByKey(context.Background(), tx, id, it.CurrentRevision)
		return err
	})
	got := []any{it.Scope, it.WorkspaceKey, it.State, it.Pinned}
	want := []any{r.Scope, r.WorkspaceKey, r.State, r.Pinned}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("item %d (scope, key, state, pinned) = %v, want %v from revision %d", id, got, want, r.Revision)
	}
	return r
}

func TestCreateItem(t *testing.T) {
	s := openM1(t)
	_, id := newItem(t, s)
	r := checkItemMatchesRevision(t, s, id)
	if r.Revision != 1 {
		t.Errorf("first revision = %d, want 1", r.Revision)
	}
	if got := queryInt(t, s, "SELECT MAX(generation) FROM outputs"); got != 2 {
		t.Errorf("newest generation after two creations = %d, want 2", got)
	}
}

// Approval, scope change, retirement and undo, as E4 writes them: after each one the items
// row equals its current revision.
func TestFlowsKeepItemEqualToRevision(t *testing.T) {
	ctx := context.Background()
	s := openM1(t)
	_, id := newItem(t, s)
	var proposal int64
	write(t, s, func(tx *sql.Tx) (err error) {
		proposal, err = InsertProposal(ctx, tx, Proposal{ItemID: &id, BaseRevision: ptr[int64](1),
			Text: "use pnpm", Scope: ScopeGlobal, Target: "fact", Diff: "+use pnpm",
			ExpectedBenefit: "one lockfile", Status: ProposalApproved, Created: testNow})
		return err
	})
	for _, c := range []struct {
		name   string
		change func(r *ItemRevision)
	}{
		{"approval", func(r *ItemRevision) { r.State, r.Origin, r.ProposalID = StateActive, OriginReview, &proposal }},
		{"scope change", func(r *ItemRevision) { r.Scope, r.WorkspaceKey, r.Pinned = ScopeGlobal, nil, true }},
		{"retirement", func(r *ItemRevision) { r.State = StateRetired }},
	} {
		t.Run(c.name, func(t *testing.T) {
			cur := checkItemMatchesRevision(t, s, id)
			next := cur
			c.change(&next)
			write(t, s, func(tx *sql.Tx) error {
				_, err := WriteRevision(ctx, tx, cur.Revision, next)
				return err
			})
			if r := checkItemMatchesRevision(t, s, id); r.Revision != cur.Revision+1 {
				t.Errorf("revision after %s = %d, want %d", c.name, r.Revision, cur.Revision+1)
			}
		})
	}
	t.Run("undo", func(t *testing.T) {
		retired := checkItemMatchesRevision(t, s, id)
		var prev ItemRevision
		write(t, s, func(tx *sql.Tx) (err error) {
			prev, err = ItemRevisionByKey(ctx, tx, id, retired.Revision-1)
			return err
		})
		undo := prev
		undo.UndoesRevision = &retired.Revision
		write(t, s, func(tx *sql.Tx) error {
			_, err := WriteRevision(ctx, tx, retired.Revision, undo)
			return err
		})
		if r := checkItemMatchesRevision(t, s, id); r.State != StateActive || r.Scope != ScopeGlobal {
			t.Errorf("undo of the retirement: (state, scope) = (%s, %s), want (active, global)", r.State, r.Scope)
		}
	})
}

// Undo is a copy of the previous row: every undoable field comes back.
func TestRevisionHoldsEveryUndoableField(t *testing.T) {
	ctx := context.Background()
	s := openM1(t)
	global, id := newItem(t, s)
	full := ItemRevision{ItemID: id, Text: "use pnpm here", Scope: ScopeWorkspace, WorkspaceKey: ptr("k1"),
		State: StateDisputed, Pinned: true, Author: AuthorBrain, Origin: OriginExtractor,
		ExceptionToItem: &global, ExceptionToRevision: ptr[int64](1), ConflictsWith: []int64{global},
		Created: testNow}
	write(t, s, func(tx *sql.Tx) error {
		_, err := WriteRevision(ctx, tx, 1, full)
		return err
	})
	later := ItemRevision{ItemID: id, Text: "use yarn", Scope: ScopeGlobal, State: StateActive,
		Author: AuthorYou, Origin: OriginReview, Created: testNow}
	write(t, s, func(tx *sql.Tx) error {
		_, err := WriteRevision(ctx, tx, 2, later)
		return err
	})

	var got ItemRevision
	write(t, s, func(tx *sql.Tx) error {
		prev, err := ItemRevisionByKey(ctx, tx, id, 2)
		if err != nil {
			return err
		}
		prev.UndoesRevision = ptr[int64](3)
		if _, err := WriteRevision(ctx, tx, 3, prev); err != nil {
			return err
		}
		got, err = ItemRevisionByKey(ctx, tx, id, 4)
		return err
	})
	want := full
	want.Revision, want.UndoesRevision = 4, ptr[int64](3)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("undo revision = %+v, want %+v", got, want)
	}
	checkItemMatchesRevision(t, s, id)
}

func TestStaleRevisionWritesNothing(t *testing.T) {
	ctx := context.Background()
	s := openM1(t)
	_, id := newItem(t, s)
	next := ItemRevision{ItemID: id, Text: "use pnpm", Scope: ScopeWorkspace, WorkspaceKey: ptr("k1"),
		State: StateActive, Author: AuthorYou, Origin: OriginReview, Created: testNow}
	write(t, s, func(tx *sql.Tx) error {
		_, err := WriteRevision(ctx, tx, 1, next)
		return err
	})
	before := readState(t, s, id)

	stale := next
	stale.State, stale.Pinned = StateRetired, true
	var err error
	// fn returns nil, so the transaction commits: WriteRevision itself must write nothing.
	write(t, s, func(tx *sql.Tx) error {
		_, err = WriteRevision(ctx, tx, 1, stale)
		return nil
	})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("WriteRevision(expected 1, current 2) = %v, want ErrConflict", err)
	}
	if after := readState(t, s, id); !reflect.DeepEqual(after, before) {
		t.Errorf("state after a conflict = %+v, want %+v", after, before)
	}
}

func TestGenerationMovesWithRevision(t *testing.T) {
	ctx := context.Background()
	s := openM1(t)
	_, id := newItem(t, s)
	before := readState(t, s, id)
	next := ItemRevision{ItemID: id, Text: "use pnpm", Scope: ScopeWorkspace, WorkspaceKey: ptr("k1"),
		State: StateActive, Author: AuthorYou, Origin: OriginReview, Created: testNow}

	// A rolled-back write takes the generation with it.
	errStop := errors.New("stop")
	err := s.Write(ctx, func(tx *sql.Tx) error {
		if _, err := WriteRevision(ctx, tx, 1, next); err != nil {
			return err
		}
		return errStop
	})
	if !errors.Is(err, errStop) {
		t.Fatalf("Write() = %v, want errStop", err)
	}
	if after := readState(t, s, id); !reflect.DeepEqual(after, before) {
		t.Errorf("state after a rollback = %+v, want %+v", after, before)
	}

	var gen int64
	write(t, s, func(tx *sql.Tx) (err error) {
		gen, err = WriteRevision(ctx, tx, 1, next)
		return err
	})
	if want := int64(before.generations) + 1; gen != want {
		t.Errorf("WriteRevision() generation = %d, want %d", gen, want)
	}
	var o Output
	write(t, s, func(tx *sql.Tx) (err error) {
		o, err = OutputByGeneration(ctx, tx, gen)
		return err
	})
	want := Output{Generation: gen, Folder: "gen-3"}
	if !reflect.DeepEqual(o, want) {
		t.Errorf("output = %+v, want %+v", o, want)
	}
}

// Two writers on two connections to one store file, with the same expected revision.
func TestTwoWritersOneWins(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	stores := make([]*Store, 2)
	for i := range stores {
		s, err := Open(ctx, dir, testTimeout)
		if err != nil {
			t.Fatalf("Open() = %v", err)
		}
		t.Cleanup(func() { closeTest(t, s) })
		stores[i] = s
	}
	_, id := newItem(t, stores[0])
	before := readState(t, stores[0], id)

	start := make(chan struct{})
	errs := make([]error, len(stores))
	var wg sync.WaitGroup
	for i, s := range stores {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errs[i] = s.Write(ctx, func(tx *sql.Tx) error {
				_, err := WriteRevision(ctx, tx, 1, ItemRevision{ItemID: id, Text: "writer", Scope: ScopeGlobal,
					State: StateActive, Author: AuthorYou, Origin: OriginReview, Created: testNow})
				return err
			})
		}()
	}
	close(start)
	wg.Wait()

	var wins, conflicts int
	for _, err := range errs {
		switch {
		case err == nil:
			wins++
		case errors.Is(err, ErrConflict):
			conflicts++
		default:
			t.Errorf("Write() = %v, want nil or ErrConflict", err)
		}
	}
	if wins != 1 || conflicts != 1 {
		t.Errorf("wins, conflicts = %d, %d, want 1, 1", wins, conflicts)
	}
	after := readState(t, stores[1], id)
	if after.revisions != before.revisions+1 || after.generations != before.generations+1 {
		t.Errorf("revisions, generations = %d, %d, want %d, %d",
			after.revisions, after.generations, before.revisions+1, before.generations+1)
	}
	if after.item.CurrentRevision != 2 {
		t.Errorf("current revision = %d, want 2", after.item.CurrentRevision)
	}
}
