package output

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

// testNow is in UTC with milliseconds, the precision of the store.
var testNow = time.Date(2026, 9, 28, 10, 30, 15, 123_000_000, time.UTC)

// testTimeout is the busy timeout of the test stores. [NEW]
const testTimeout = 5 * time.Second

func openStore(t *testing.T, dir string) *store.Store {
	t.Helper()
	s, err := store.Open(context.Background(), dir, testTimeout)
	if err != nil {
		t.Fatalf("Open() = %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("Close() = %v", err)
		}
	})
	return s
}

func write(t *testing.T, s *store.Store, fn func(tx *sql.Tx) error) {
	t.Helper()
	if err := s.Write(context.Background(), fn); err != nil {
		t.Fatalf("Write() = %v", err)
	}
}

func ptr64(v int64) *int64 { return &v }

// newStore makes a store with workspace k1, and returns it and its out folder.
func newStore(t *testing.T) (*store.Store, string) {
	t.Helper()
	dir := t.TempDir()
	s := openStore(t, dir)
	write(t, s, func(tx *sql.Tx) error {
		return store.InsertWorkspace(context.Background(), tx, store.Workspace{Key: "k1", HerdrSession: "default",
			HerdrWorkspaceID: "w1", Label: "brain", Status: store.WorkspaceLive, LastSeen: testNow})
	})
	return s, filepath.Join(dir, "out")
}

// addItem commits a new active item. key "" makes a global item.
func addItem(t *testing.T, s *store.Store, key, text string) int64 {
	t.Helper()
	r := store.ItemRevision{Text: text, Scope: store.ScopeGlobal, State: store.StateActive,
		Author: store.AuthorYou, Origin: store.OriginRemember, Created: testNow}
	if key != "" {
		r.Scope, r.WorkspaceKey = store.ScopeWorkspace, &key
	}
	var id int64
	write(t, s, func(tx *sql.Tx) (err error) {
		id, _, err = store.CreateItem(context.Background(), tx,
			store.Item{ClaimID: text, Kind: store.KindFact, Created: testNow}, r)
		return err
	})
	return id
}

// revise commits a new text for item id, which is at revision expected, and returns the new
// generation.
func revise(ctx context.Context, s *store.Store, id, expected int64, text string) (int64, error) {
	var gen int64
	err := s.Write(ctx, func(tx *sql.Tx) error {
		r, err := store.ItemRevisionByKey(ctx, tx, id, expected)
		if err != nil {
			return err
		}
		r.Text = text
		gen, err = store.WriteRevision(ctx, tx, expected, r)
		return err
	})
	return gen, err
}

func mustRevise(t *testing.T, s *store.Store, id, expected int64, text string) int64 {
	t.Helper()
	gen, err := revise(context.Background(), s, id, expected, text)
	if err != nil {
		t.Fatalf("revise(%d) = %v", id, err)
	}
	return gen
}

func mustBuild(t *testing.T, s *store.Store, out string) int64 {
	t.Helper()
	gen, err := Build(context.Background(), s, out, nil, testNow)
	if err != nil {
		t.Fatalf("Build() = %v", err)
	}
	return gen
}

func mustRead(t *testing.T, out, key string) Brief {
	t.Helper()
	b, err := Read(out, key)
	if err != nil {
		t.Fatalf("Read(%q) = %v", key, err)
	}
	return b
}

func newest(t *testing.T, s *store.Store) int64 {
	t.Helper()
	var g int64
	write(t, s, func(tx *sql.Tx) (err error) {
		g, err = store.NewestGeneration(context.Background(), tx)
		return err
	})
	return g
}

// appliedEdits reads the applied edits of generation gen, in id order.
func appliedEdits(t *testing.T, s *store.Store, gen int64) []store.AppliedEdit {
	t.Helper()
	ctx := context.Background()
	var es []store.AppliedEdit
	write(t, s, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, "SELECT id FROM applied_edits WHERE generation = ? ORDER BY id", gen)
		if err != nil {
			return err
		}
		var ids []int64
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				return errors.Join(err, rows.Close())
			}
			ids = append(ids, id)
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			return err
		}
		for _, id := range ids {
			e, err := store.AppliedEditByID(ctx, tx, id)
			if err != nil {
				return err
			}
			es = append(es, e)
		}
		return nil
	})
	return es
}

func TestBuildPublishes(t *testing.T) {
	s, out := newStore(t)
	g := addItem(t, s, "", "use npm")
	w := addItem(t, s, "k1", "use pnpm")
	gen := mustBuild(t, s, out)
	if want := newest(t, s); gen != want {
		t.Errorf("Build() = %d, want the newest generation %d", gen, want)
	}

	b := mustRead(t, out, "k1")
	if !strings.Contains(b.Global, "[1] use npm") || !strings.Contains(b.Workspace, "[2] use pnpm") {
		t.Errorf("Read() parts = %q, %q, want item 1 in global and item 2 in the workspace", b.Global, b.Workspace)
	}
	wantRevs := []store.RevisionRef{{Item: g, Revision: 1}, {Item: w, Revision: 1}}
	if b.Generation != gen || !reflect.DeepEqual(b.Revisions, wantRevs) {
		t.Errorf("Read() = generation %d, revisions %v, want %d, %v", b.Generation, b.Revisions, gen, wantRevs)
	}

	var o store.Output
	write(t, s, func(tx *sql.Tx) (err error) {
		o, err = store.OutputByGeneration(context.Background(), tx, gen)
		return err
	})
	data, err := os.ReadFile(filepath.Join(out, folder(gen), manifestName))
	if err != nil {
		t.Fatal(err)
	}
	if !o.Current || o.ManifestHash == nil || *o.ManifestHash != hash(data) || !o.BuiltAt.Equal(testNow) {
		t.Errorf("outputs row = %+v, want current, the manifest hash %s and built at %v", o, hash(data), testNow)
	}

	es := appliedEdits(t, s, gen)
	var files []string
	for _, e := range es {
		files = append(files, e.File)
		if e.ContentBefore != nil || e.ContentAfter == nil || e.ContentHash != hash([]byte(*e.ContentAfter)) {
			t.Errorf("applied edit %s = %+v, want a new file with its hash", e.File, e)
		}
	}
	if want := []string{"global.md", "workspaces/k1.md"}; !slices.Equal(files, want) {
		t.Errorf("applied edit files = %v, want %v", files, want)
	}

	if again := mustBuild(t, s, out); again != gen || len(appliedEdits(t, s, gen)) != len(es) {
		t.Errorf("second Build() = %d with new edits, want %d and no new edits", again, gen)
	}
}

// AC: the store is at revision 3, the pointer still names a generation with revision 2. The
// reader returns revision 2.
func TestReadGivesManifestRevisions(t *testing.T) {
	s, out := newStore(t)
	id := addItem(t, s, "", "rev 1")
	mustRevise(t, s, id, 1, "rev 2")
	mustBuild(t, s, out)
	mustRevise(t, s, id, 2, "rev 3")

	b := mustRead(t, out, "")
	if want := []store.RevisionRef{{Item: id, Revision: 2}}; !reflect.DeepEqual(b.Revisions, want) {
		t.Errorf("Read() revisions = %v, want %v", b.Revisions, want)
	}
	if !strings.Contains(b.Global, "rev 2") || b.Workspace != "" {
		t.Errorf("Read() parts = %q, %q, want the revision 2 text and no workspace part", b.Global, b.Workspace)
	}
}

// AC S43, the stale case: a build that a newer commit overtakes stops, and the build of the
// newer generation, which waited for the lock, publishes it.
func TestOlderBuildStops(t *testing.T) {
	s, out := newStore(t)
	id := addItem(t, s, "", "one")
	mustBuild(t, s, out)
	old := mustRevise(t, s, id, 1, "two")

	reached, release := make(chan struct{}), make(chan struct{})
	older := builder{out: out, writeFile: writeFile, afterRender: func() {
		close(reached)
		<-release
	}}
	var wg sync.WaitGroup
	var olderErr, newerErr error
	var newerGen int64
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, olderErr = older.build(context.Background(), s, nil, testNow)
	}()
	<-reached
	// The older build holds the lock and has rendered generation old. A newer commit lands now.
	gen := mustRevise(t, s, id, 2, "three")
	wg.Add(1)
	go func() {
		defer wg.Done()
		newerGen, newerErr = Build(context.Background(), s, out, nil, testNow)
	}()
	close(release)
	wg.Wait()

	if !errors.Is(olderErr, ErrStale) {
		t.Errorf("build of generation %d = %v, want ErrStale", old, olderErr)
	}
	if newerErr != nil || newerGen != gen {
		t.Errorf("build of generation %d = %d, %v, want %d", gen, newerGen, newerErr, gen)
	}
	if b := mustRead(t, out, ""); b.Generation != gen || !strings.Contains(b.Global, "three") {
		t.Errorf("current generation = %d %q, want %d with the newest text", b.Generation, b.Global, gen)
	}
	if es := appliedEdits(t, s, old); len(es) != 0 {
		t.Errorf("the stopped build logged %d applied edits, want 0", len(es))
	}
}

// AC S43, under load: writers on two store handles commit and build at the same time, while a
// reader follows the pointer. The pointer never goes back, and at the end the newest
// generation is current.
func TestConcurrentBuilds(t *testing.T) {
	s, out := newStore(t)
	s2 := openStore(t, filepath.Dir(out))
	const writers, rounds = 2, 10 // [NEW] enough overlap to hit the lock and stale builds
	ids := make([]int64, writers)
	for i := range writers {
		ids[i] = addItem(t, s, "k1", "start")
	}
	mustBuild(t, s, out)

	done := make(chan struct{})
	readErr := make(chan error, 1)
	go func() { readErr <- follow(out, done) }()

	errs := make(chan error, writers*rounds*2)
	var wg sync.WaitGroup
	for i, st := range []*store.Store{s, s2} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for r := range rounds {
				ctx := context.Background()
				if _, err := revise(ctx, st, ids[i], int64(r+1), fmt.Sprintf("round %d", r)); err != nil {
					errs <- err
					return
				}
				if _, err := Build(ctx, st, out, nil, testNow); err != nil && !errors.Is(err, ErrStale) {
					errs <- err
				}
			}
		}()
	}
	wg.Wait()
	close(done)
	close(errs)
	for err := range errs {
		t.Errorf("writer: %v", err)
	}
	if err := <-readErr; err != nil {
		t.Errorf("reader: %v", err)
	}
	if b, want := mustRead(t, out, "k1"), newest(t, s); b.Generation != want {
		t.Errorf("current generation = %d, want the newest %d", b.Generation, want)
	}
}

// follow reads the brief of workspace k1 in out until done closes. It returns the first read
// error, or an error when the generation goes back.
func follow(out string, done <-chan struct{}) error {
	var last int64
	for {
		select {
		case <-done:
			return nil
		default:
		}
		b, err := Read(out, "k1")
		if err != nil {
			return err
		}
		if b.Generation < last {
			return fmt.Errorf("the pointer went back from generation %d to %d", last, b.Generation)
		}
		last = b.Generation
	}
}

// AC S65: a build that stops between two files leaves the old generation current and whole,
// and leaves only a staging folder.
func TestStoppedBuildLeavesOldGeneration(t *testing.T) {
	s, out := newStore(t)
	id := addItem(t, s, "", "old global")
	addItem(t, s, "k1", "old workspace")
	oldGen := mustBuild(t, s, out)
	before := mustRead(t, out, "k1")
	mustRevise(t, s, id, 1, "new global")

	stop := errors.New("stopped")
	n := 0
	b := builder{out: out, afterRender: func() {}, writeFile: func(path string, data []byte) error {
		if n++; n == 2 {
			return stop
		}
		return writeFile(path, data)
	}}
	if _, err := b.build(context.Background(), s, nil, testNow); !errors.Is(err, stop) {
		t.Fatalf("build() = %v, want the injected stop", err)
	}

	if after := mustRead(t, out, "k1"); !reflect.DeepEqual(after, before) {
		t.Errorf("Read() after the stop = %+v, want the old generation %+v", after, before)
	}
	entries, err := os.ReadDir(out)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	want := []string{pointerName, folder(oldGen), lockName, fmt.Sprintf("staging-%d", oldGen+1)}
	slices.Sort(want)
	if !slices.Equal(names, want) {
		t.Errorf("out folder = %v, want %v", names, want)
	}

	gen := mustBuild(t, s, out)
	if b := mustRead(t, out, "k1"); b.Generation != gen || !strings.Contains(b.Global, "new global") {
		t.Errorf("Read() after the next build = %d %q, want %d with the new text", b.Generation, b.Global, gen)
	}
	if _, err := os.Stat(filepath.Join(out, fmt.Sprintf("staging-%d", gen))); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("staging folder after the next build: %v, want it gone", err)
	}
}

// AC: with an open forget, a build writes no old item text into applied_edits.
func TestOpenForgetKeepsOldTextOut(t *testing.T) {
	for _, c := range []struct {
		name   string
		forget bool
	}{{"open forget", true}, {"no forget", false}} {
		t.Run(c.name, func(t *testing.T) {
			s, out := newStore(t)
			id := addItem(t, s, "", "secret plan")
			addItem(t, s, "k1", "use pnpm")
			old := mustBuild(t, s, out)
			if c.forget {
				// What the forget flow does: mark the generations that hold the text.
				write(t, s, func(tx *sql.Tx) error {
					_, err := tx.Exec("UPDATE outputs SET holds_forgotten_text = 1 WHERE generation = ?", old)
					return err
				})
			}
			write(t, s, func(tx *sql.Tx) error {
				r, err := store.ItemRevisionByKey(context.Background(), tx, id, 1)
				if err != nil {
					return err
				}
				r.Text, r.State = "", store.StateRetired
				_, err = store.WriteRevision(context.Background(), tx, 1, r)
				return err
			})
			gen := mustBuild(t, s, out)

			es := appliedEdits(t, s, gen)
			if len(es) != 1 || es[0].File != globalName {
				t.Fatalf("applied edits = %+v, want one for %s", es, globalName)
			}
			e := es[0]
			if want := []store.RevisionRef{{Item: id, Revision: 1}}; !reflect.DeepEqual(e.RevisionsBefore, want) {
				t.Errorf("RevisionsBefore = %v, want %v", e.RevisionsBefore, want)
			}
			holds := e.ContentBefore != nil && strings.Contains(*e.ContentBefore, "secret plan")
			if holds == c.forget {
				t.Errorf("ContentBefore = %v, want old text %v", e.ContentBefore, !c.forget)
			}
		})
	}
}

func TestHandEditIsConflict(t *testing.T) {
	s, out := newStore(t)
	id := addItem(t, s, "", "one")
	old := mustBuild(t, s, out)
	if err := os.WriteFile(filepath.Join(out, folder(old), globalName), []byte("mine"), 0o600); err != nil {
		t.Fatal(err)
	}
	mustRevise(t, s, id, 1, "two")

	_, err := Build(context.Background(), s, out, nil, testNow)
	var ce *ConflictError
	if !errors.As(err, &ce) || ce.Generation != old || !slices.Equal(ce.Files, []string{globalName}) {
		t.Fatalf("Build() = %v, want a conflict on generation %d, file %s", err, old, globalName)
	}
	if gen, err := readPointer(out); err != nil || gen != old {
		t.Errorf("pointer = %d, %v, want %d", gen, err, old)
	}
}

func TestReadChecksHashes(t *testing.T) {
	s, out := newStore(t)
	addItem(t, s, "k1", "use pnpm")
	gen := mustBuild(t, s, out)
	for _, c := range []struct {
		name, key string
		edit      func(dir string) error
	}{
		{"changed workspace part", "k1", func(dir string) error {
			return os.WriteFile(filepath.Join(dir, "workspaces", "k1.md"), []byte("x"), 0o600)
		}},
		{"missing global part", "", func(dir string) error { return os.Remove(filepath.Join(dir, globalName)) }},
		{"unknown workspace", "k9", func(string) error { return nil }},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "out")
			if err := os.CopyFS(dir, os.DirFS(out)); err != nil {
				t.Fatal(err)
			}
			if err := c.edit(filepath.Join(dir, folder(gen))); err != nil {
				t.Fatal(err)
			}
			if _, err := Read(dir, c.key); !errors.Is(err, ErrInconsistent) {
				t.Errorf("Read(%q) = %v, want ErrInconsistent", c.key, err)
			}
		})
	}
	if _, err := Read(t.TempDir(), ""); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Read() with no pointer = %v, want os.ErrNotExist", err)
	}
}

// A conflict between a global item and a workspace item shows on the workspace item only,
// because the global part goes to every workspace.
func TestConflictMarkOnWorkspaceItem(t *testing.T) {
	key := "k1"
	revs := []store.ItemRevision{
		{ItemID: 1, Text: "use npm", Scope: store.ScopeGlobal, ConflictsWith: []int64{2, 3}},
		{ItemID: 2, Text: "use pnpm", Scope: store.ScopeWorkspace, WorkspaceKey: &key, ConflictsWith: []int64{1}},
		{ItemID: 3, Text: "use yarn", Scope: store.ScopeGlobal},
		{ItemID: 4, Text: "use bun", Scope: store.ScopeWorkspace, WorkspaceKey: &key, ExceptionToItem: ptr64(3)},
	}
	global, byKey := split(revs)
	if got := global[0].ConflictsWith; !slices.Equal(got, []int64{3}) {
		t.Errorf("global item 1 conflicts = %v, want [3]", got)
	}
	ws := byKey[key]
	if got := ws[0].ConflictsWith; !slices.Equal(got, []int64{1}) {
		t.Errorf("workspace item 2 conflicts = %v, want [1] once", got)
	}
	if ws[1].ExceptionTo != 3 {
		t.Errorf("workspace item 4 exception = %d, want 3", ws[1].ExceptionTo)
	}
}
