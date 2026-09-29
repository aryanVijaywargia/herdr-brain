package store

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"slices"
	"testing"
	"time"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// testNow is in UTC with milliseconds, the precision of the store.
var testNow = time.Date(2026, 9, 28, 10, 30, 15, 123_000_000, time.UTC)

func openM1(t *testing.T) *Store {
	t.Helper()
	s, err := Open(context.Background(), t.TempDir(), testTimeout)
	if err != nil {
		t.Fatalf("Open() = %v", err)
	}
	t.Cleanup(func() { closeTest(t, s) })
	return s
}

func write(t *testing.T, s *Store, fn func(tx *sql.Tx) error) {
	t.Helper()
	if err := s.Write(context.Background(), fn); err != nil {
		t.Fatalf("Write() = %v", err)
	}
}

func ptr[T any](v T) *T { return &v }

// fixture holds one row of each table that other rows point to.
type fixture struct {
	ws       Workspace
	item     Item
	proposal Proposal
}

func addFixture(t *testing.T, s *Store) fixture {
	t.Helper()
	f := fixture{
		ws: Workspace{Key: "k1", HerdrSession: "default", HerdrWorkspaceID: "w1", Label: "brain",
			RootFolder: ptr("/src/brain"), Status: WorkspaceLive, LastSeen: testNow},
		item: Item{ClaimID: "c1", CurrentRevision: 1, Kind: KindFact, Scope: ScopeWorkspace,
			WorkspaceKey: ptr("k1"), State: StateActive, Anchors: []string{"a.go"}, Created: testNow},
		proposal: Proposal{Text: "use pnpm", Scope: ScopeWorkspace, Target: "fact", Diff: "+use pnpm",
			ExpectedBenefit: "stops the npm lockfile mistake", TriggerPrompts: []string{"h1"},
			Status: ProposalPending, Created: testNow},
	}
	write(t, s, func(tx *sql.Tx) error {
		ctx := context.Background()
		if err := InsertWorkspace(ctx, tx, f.ws); err != nil {
			return err
		}
		id, err := InsertItem(ctx, tx, f.item)
		if err != nil {
			return err
		}
		f.item.ID = id
		f.proposal.ItemID = &id
		f.proposal.ID, err = InsertProposal(ctx, tx, f.proposal)
		return err
	})
	return f
}

func tableNames(t *testing.T, s *Store) []string {
	t.Helper()
	rows, err := s.db.Query(`SELECT name FROM sqlite_schema
		WHERE type = 'table' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close() //nolint:errcheck // read-only: rows.Err reports the read errors
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		names = append(names, n)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return names
}

type column struct {
	name, typ string
}

func columns(t *testing.T, s *Store, table string) []column {
	t.Helper()
	rows, err := s.db.Query("SELECT name, type FROM pragma_table_info(?) ORDER BY cid", table)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close() //nolint:errcheck // read-only: rows.Err reports the read errors
	var cs []column
	for rows.Next() {
		var c column
		if err := rows.Scan(&c.name, &c.typ); err != nil {
			t.Fatal(err)
		}
		cs = append(cs, c)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return cs
}

// AC: the migration creates the M1 tables and no others. The issue's AC says 11; the
// coordinator's brief says the 9 of the issue's table list (adapters and file_snapshots move
// to install.json, E8). E9-S4 adds learning_settings; E11-S3 and E11-S4 add the capture tables; E11-S5 adds command_records;
// E11-S6 adds forgotten_captures.
func TestM1Tables(t *testing.T) {
	s := openM1(t)
	want := []string{"applied_edits", "command_records", "cursors", "deliveries", "forgotten_captures", "item_revisions", "items", "learning_settings",
		"observations", "outputs", "proposals", "review_panes", "segments", "sessions", "tombstones", "workspaces"}
	if got := tableNames(t, s); !slices.Equal(got, want) {
		t.Errorf("tables = %v, want %v", got, want)
	}
}

func TestNoM2Columns(t *testing.T) {
	s := openM1(t)
	for _, c := range columns(t, s, "proposals") {
		if c.name == "conflicts_with" {
			t.Errorf("proposals has the M2 column conflicts_with")
		}
	}
}

// AC: tombstones has no text column (§11). Its columns are the claim id, the source keys and
// the time; none of them holds item text.
func TestTombstonesHoldNoText(t *testing.T) {
	s := openM1(t)
	want := []column{{"claim_id", "TEXT"}, {"source_keys", "TEXT"}, {"forgotten_at", "TEXT"}}
	if got := columns(t, s, "tombstones"); !slices.Equal(got, want) {
		t.Errorf("tombstones columns = %v, want %v", got, want)
	}
}

// AC: a bad enum value fails at insert.
func TestBadEnumFailsAtInsert(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		name   string
		insert func(tx *sql.Tx, f fixture) error
	}{
		{"workspaces.status", func(tx *sql.Tx, f fixture) error {
			w := f.ws
			w.Key, w.Status = "k2", "open"
			return InsertWorkspace(ctx, tx, w)
		}},
		{"items.kind", func(tx *sql.Tx, f fixture) error {
			it := f.item
			it.Kind = "rule"
			_, err := InsertItem(ctx, tx, it)
			return err
		}},
		{"items.scope", func(tx *sql.Tx, f fixture) error {
			it := f.item
			it.Scope = "team"
			_, err := InsertItem(ctx, tx, it)
			return err
		}},
		{"items.state", func(tx *sql.Tx, f fixture) error {
			it := f.item
			it.State = "deleted"
			_, err := InsertItem(ctx, tx, it)
			return err
		}},
		{"item_revisions.scope", func(tx *sql.Tx, f fixture) error {
			r := revision(f)
			r.Scope = "Global"
			return InsertItemRevision(ctx, tx, r)
		}},
		{"item_revisions.state", func(tx *sql.Tx, f fixture) error {
			r := revision(f)
			r.State = ""
			return InsertItemRevision(ctx, tx, r)
		}},
		{"item_revisions.author", func(tx *sql.Tx, f fixture) error {
			r := revision(f)
			r.Author = "agent"
			return InsertItemRevision(ctx, tx, r)
		}},
		{"item_revisions.origin", func(tx *sql.Tx, f fixture) error {
			r := revision(f)
			r.Origin = "hook"
			return InsertItemRevision(ctx, tx, r)
		}},
		{"proposals.scope", func(tx *sql.Tx, f fixture) error {
			p := f.proposal
			p.Scope = "all"
			_, err := InsertProposal(ctx, tx, p)
			return err
		}},
		{"proposals.status", func(tx *sql.Tx, f fixture) error {
			p := f.proposal
			p.Status = "later"
			_, err := InsertProposal(ctx, tx, p)
			return err
		}},
		{"proposals.surface", func(tx *sql.Tx, f fixture) error {
			p := f.proposal
			p.Surface = ptr[Surface]("web")
			_, err := InsertProposal(ctx, tx, p)
			return err
		}},
		{"deliveries.result", func(tx *sql.Tx, _ fixture) error {
			_, err := InsertDelivery(ctx, tx, Delivery{Profile: "claude", PaneID: "p1", Result: "sent", At: testNow})
			return err
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := openM1(t)
			f := addFixture(t, s)
			err := s.Write(ctx, func(tx *sql.Tx) error { return c.insert(tx, f) })
			if !isConstraint(err, sqlite3.SQLITE_CONSTRAINT_CHECK) {
				t.Errorf("insert with a bad %s = %v, want a CHECK constraint error", c.name, err)
			}
		})
	}
}

func revision(f fixture) ItemRevision {
	return ItemRevision{ItemID: f.item.ID, Revision: 1, Text: "use pnpm", Scope: ScopeWorkspace,
		WorkspaceKey: ptr("k1"), State: StateActive, Author: AuthorYou, Origin: OriginRemember,
		Created: testNow}
}

func isConstraint(err error, code int) bool {
	var se *sqlite.Error
	return errors.As(err, &se) && se.Code() == code
}

// A global item has no workspace key, and a workspace item has one.
func TestScopeMatchesWorkspaceKey(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		name   string
		scope  Scope
		key    *string
		wantOK bool
	}{
		{"global with a key", ScopeGlobal, ptr("k1"), false},
		{"workspace with no key", ScopeWorkspace, nil, false},
		{"global with no key", ScopeGlobal, nil, true},
		{"workspace with a key", ScopeWorkspace, ptr("k1"), true},
	} {
		for _, table := range []struct {
			name   string
			insert func(tx *sql.Tx, f fixture) error
		}{
			{"items", func(tx *sql.Tx, f fixture) error {
				it := f.item
				it.Scope, it.WorkspaceKey = c.scope, c.key
				_, err := InsertItem(ctx, tx, it)
				return err
			}},
			{"item_revisions", func(tx *sql.Tx, f fixture) error {
				r := revision(f)
				r.Scope, r.WorkspaceKey = c.scope, c.key
				return InsertItemRevision(ctx, tx, r)
			}},
		} {
			t.Run(table.name+"/"+c.name, func(t *testing.T) {
				s := openM1(t)
				f := addFixture(t, s)
				err := s.Write(ctx, func(tx *sql.Tx) error { return table.insert(tx, f) })
				if c.wantOK && err != nil {
					t.Errorf("insert = %v, want nil", err)
				}
				if !c.wantOK && !isConstraint(err, sqlite3.SQLITE_CONSTRAINT_CHECK) {
					t.Errorf("insert = %v, want a CHECK constraint error", err)
				}
			})
		}
	}
}

func TestExceptionLinkNeedsBothFields(t *testing.T) {
	s := openM1(t)
	f := addFixture(t, s)
	r := revision(f)
	r.ExceptionToItem = &f.item.ID
	err := s.Write(context.Background(), func(tx *sql.Tx) error { return InsertItemRevision(context.Background(), tx, r) })
	if !isConstraint(err, sqlite3.SQLITE_CONSTRAINT_CHECK) {
		t.Errorf("insert with an exception item and no revision = %v, want a CHECK constraint error", err)
	}
}

func TestForeignKeys(t *testing.T) {
	s := openM1(t)
	it := Item{ClaimID: "c1", CurrentRevision: 1, Kind: KindFact, Scope: ScopeWorkspace,
		WorkspaceKey: ptr("no such workspace"), State: StateActive, Created: testNow}
	err := s.Write(context.Background(), func(tx *sql.Tx) error {
		_, err := InsertItem(context.Background(), tx, it)
		return err
	})
	if !isConstraint(err, sqlite3.SQLITE_CONSTRAINT_FOREIGNKEY) {
		t.Errorf("insert item with an unknown workspace = %v, want a foreign key error", err)
	}
}

func TestTimesAreUTCText(t *testing.T) {
	s := openM1(t)
	at := time.Date(2026, 9, 28, 12, 30, 15, 123_456_789, time.FixedZone("CEST", 2*3600))
	write(t, s, func(tx *sql.Tx) error {
		return InsertTombstone(context.Background(), tx, Tombstone{ClaimID: "c1", ForgottenAt: at})
	})
	var got string
	if err := s.db.QueryRow("SELECT forgotten_at FROM tombstones").Scan(&got); err != nil {
		t.Fatal(err)
	}
	if want := "2026-09-28T10:30:15.123Z"; got != want {
		t.Errorf("forgotten_at = %q, want %q", got, want)
	}
}

func TestNilListIsEmptyArray(t *testing.T) {
	s := openM1(t)
	write(t, s, func(tx *sql.Tx) error {
		return InsertTombstone(context.Background(), tx, Tombstone{ClaimID: "c1", ForgottenAt: testNow})
	})
	var got string
	if err := s.db.QueryRow("SELECT source_keys FROM tombstones").Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != "[]" {
		t.Errorf("source_keys of a nil list = %q, want []", got)
	}
}

// TestRoundTrip checks that every table reads back what its insert wrote.
func TestRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := openM1(t)
	f := addFixture(t, s)
	later := testNow.Add(time.Minute)
	for _, c := range []struct {
		name string
		// run inserts a row, reads it back by its key and returns the want and got rows.
		run func(tx *sql.Tx) (want, got any, err error)
	}{
		{"workspace", func(tx *sql.Tx) (any, any, error) {
			w := Workspace{Key: "k2", HerdrSession: "test", HerdrWorkspaceID: "w2", Status: WorkspaceClosed, LastSeen: testNow}
			if err := InsertWorkspace(ctx, tx, w); err != nil {
				return nil, nil, err
			}
			got, err := WorkspaceByKey(ctx, tx, w.Key)
			return w, got, err
		}},
		{"item", func(tx *sql.Tx) (any, any, error) {
			it := Item{ClaimID: "c2", CurrentRevision: 3, Kind: KindHandoff, Scope: ScopeGlobal,
				State: StateDraft, Pinned: true, Anchors: []string{"x", "y"}, Created: testNow}
			id, err := InsertItem(ctx, tx, it)
			if err != nil {
				return nil, nil, err
			}
			it.ID = id
			got, err := ItemByID(ctx, tx, id)
			return it, got, err
		}},
		{"item revision", func(tx *sql.Tx) (any, any, error) {
			r := revision(f)
			r.Revision, r.Pinned, r.Author, r.Origin = 2, true, AuthorBrain, OriginReview
			r.ProposalID, r.UndoesRevision = &f.proposal.ID, ptr[int64](1)
			r.ExceptionToItem, r.ExceptionToRevision = &f.item.ID, ptr[int64](1)
			r.ConflictsWith = []int64{f.item.ID}
			if err := InsertItemRevision(ctx, tx, r); err != nil {
				return nil, nil, err
			}
			got, err := ItemRevisionByKey(ctx, tx, r.ItemID, r.Revision)
			return r, got, err
		}},
		{"proposal", func(tx *sql.Tx) (any, any, error) {
			p := f.proposal
			p.CandidateID, p.BaseRevision, p.PermissionWarning = ptr[int64](7), ptr[int64](1), true
			p.ChangeRequest, p.Status, p.Surface = ptr("shorter"), ProposalChanged, ptr(SurfacePane)
			p.Decided, p.ReviewSeconds = later, ptr[int64](12)
			id, err := InsertProposal(ctx, tx, p)
			if err != nil {
				return nil, nil, err
			}
			p.ID = id
			got, err := ProposalByID(ctx, tx, id)
			return p, got, err
		}},
		{"output", func(tx *sql.Tx) (any, any, error) {
			o := Output{Generation: 4, Folder: "out/gen-4", ManifestHash: ptr("m"), BuiltAt: testNow, Current: true}
			if err := InsertOutput(ctx, tx, o); err != nil {
				return nil, nil, err
			}
			got, err := OutputByGeneration(ctx, tx, o.Generation)
			return o, got, err
		}},
		{"applied edit", func(tx *sql.Tx) (any, any, error) {
			e := AppliedEdit{ProposalID: &f.proposal.ID, File: "global.md", Generation: 4,
				ContentBefore: ptr(""), ContentHash: "h", Applied: testNow, RolledBack: later,
				RevisionsBefore: []RevisionRef{}, RevisionsAfter: []RevisionRef{{Item: f.item.ID, Revision: 1}}}
			id, err := InsertAppliedEdit(ctx, tx, e)
			if err != nil {
				return nil, nil, err
			}
			e.ID = id
			got, err := AppliedEditByID(ctx, tx, id)
			return e, got, err
		}},
		{"delivery", func(tx *sql.Tx) (any, any, error) {
			d := Delivery{Profile: "claude", HerdrSession: ptr("default"), PaneID: "p1", AgentSessionID: ptr("s1"), WorkspaceKey: ptr("k1"),
				BriefHash: ptr("b"), OutputGeneration: ptr[int64](4),
				ItemRevisions: []RevisionRef{{Item: f.item.ID, Revision: 1}}, CLIVersion: ptr("2.1.0"),
				Result: ResultEmitted, At: testNow, AgentCheck: ptr("unknown")}
			id, err := InsertDelivery(ctx, tx, d)
			if err != nil {
				return nil, nil, err
			}
			d.ID = id
			got, err := DeliveryByID(ctx, tx, id)
			return d, got, err
		}},
		{"review pane", func(tx *sql.Tx) (any, any, error) {
			p := ReviewPane{HerdrSession: "default", PaneID: "p2", BesidePaneID: "p1",
				ProposalID: f.proposal.ID, Opened: testNow}
			id, err := InsertReviewPane(ctx, tx, p)
			if err != nil {
				return nil, nil, err
			}
			p.ID = id
			got, err := ReviewPaneByID(ctx, tx, id)
			return p, got, err
		}},
		{"open review panes", func(tx *sql.Tx) (any, any, error) {
			// The open panes of the proposal, in every herdr session.
			want, err := OpenReviewPanes(ctx, tx, f.proposal.ID) // the row of "review pane"
			if err != nil {
				return nil, nil, err
			}
			n := len(want)
			for _, p := range []ReviewPane{
				{HerdrSession: "b", SessionIdentity: ptr("ws_b"), PaneID: "w1:p2", BesidePaneID: "w1:p1", ProposalID: f.proposal.ID, Opened: testNow},
				{HerdrSession: "a", PaneID: "w1:p2", BesidePaneID: "w1:p1", ProposalID: f.proposal.ID, Opened: testNow},
				{HerdrSession: "b", PaneID: "w1:p3", BesidePaneID: "w1:p1", ProposalID: f.proposal.ID, Opened: testNow},
			} {
				id, err := InsertReviewPane(ctx, tx, p)
				if err != nil {
					return nil, nil, err
				}
				p.ID = id
				want = append(want, p)
			}
			if err := CloseReviewPane(ctx, tx, want[n+2].ID, testNow); err != nil {
				return nil, nil, err
			}
			got, err := OpenReviewPanes(ctx, tx, f.proposal.ID)
			return want[:n+2], got, err
		}},
		{"tombstones", func(tx *sql.Tx) (any, any, error) {
			want := []Tombstone{
				{ClaimID: "c9", SourceKeys: []string{"s1"}, ForgottenAt: testNow},
				{ClaimID: "c9", SourceKeys: []string{"s2", "s3"}, ForgottenAt: later},
			}
			for _, ts := range []Tombstone{want[1], want[0], {ClaimID: "other", SourceKeys: []string{"s4"}, ForgottenAt: testNow}} {
				if err := InsertTombstone(ctx, tx, ts); err != nil {
					return nil, nil, err
				}
			}
			got, err := TombstonesByClaim(ctx, tx, "c9")
			return want, got, err
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			var want, got any
			write(t, s, func(tx *sql.Tx) (err error) {
				want, got, err = c.run(tx)
				return err
			})
			if !reflect.DeepEqual(got, want) {
				t.Errorf("read back = %+v, want %+v", got, want)
			}
		})
	}
}

func TestReadMissingRow(t *testing.T) {
	s := openM1(t)
	err := s.Write(context.Background(), func(tx *sql.Tx) error {
		_, err := ItemByID(context.Background(), tx, 42)
		return err
	})
	if !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("ItemByID(42) = %v, want sql.ErrNoRows", err)
	}
}
