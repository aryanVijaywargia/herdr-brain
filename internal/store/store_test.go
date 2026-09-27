package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

// testTimeout is the busy timeout of stores that the tests open in this process. [NEW]
const testTimeout = 5 * time.Second

func openTest(t *testing.T, dir string, fsys fstest.MapFS) *Store {
	t.Helper()
	s, err := open(context.Background(), dir, testTimeout, fsys)
	if err != nil {
		t.Fatalf("open(%s) = %v", dir, err)
	}
	return s
}

func closeTest(t *testing.T, s *Store) {
	t.Helper()
	if err := s.Close(); err != nil {
		t.Fatalf("Close() = %v", err)
	}
}

func queryInt(t *testing.T, s *Store, query string) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(query).Scan(&n); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n
}

func TestOpenEmptyFolder(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(context.Background(), dir, 1234*time.Millisecond)
	if err != nil {
		t.Fatalf("Open() = %v", err)
	}
	defer closeTest(t, s)

	ms, err := loadMigrations(migrations)
	if err != nil {
		t.Fatalf("loadMigrations() = %v", err)
	}
	if len(ms) == 0 {
		t.Fatal("no embedded migrations")
	}
	for _, c := range []struct {
		pragma string
		want   int
	}{
		{"user_version", len(ms)},
		{"busy_timeout", 1234},
		{"foreign_keys", 1},
	} {
		if got := queryInt(t, s, "PRAGMA "+c.pragma); got != c.want {
			t.Errorf("PRAGMA %s = %d, want %d", c.pragma, got, c.want)
		}
	}
	var mode string
	if err := s.db.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil {
		t.Fatal(err)
	}
	if mode != "wal" {
		t.Errorf("journal_mode = %q, want wal", mode)
	}
	if _, err := os.Stat(filepath.Join(dir, FileName)); err != nil {
		t.Errorf("store file: %v", err)
	}
}

func TestOpenMigratesOnce(t *testing.T) {
	fsys := fstest.MapFS{
		"migrations/0001_a.sql": {Data: []byte("CREATE TABLE runs (n INTEGER); INSERT INTO runs VALUES (1);")},
		"migrations/0002_b.sql": {Data: []byte("INSERT INTO runs VALUES (2);")},
	}
	dir := t.TempDir()
	closeTest(t, openTest(t, dir, fsys))
	s := openTest(t, dir, fsys)
	defer closeTest(t, s)

	if got := queryInt(t, s, "SELECT count(*) FROM runs"); got != 2 {
		t.Errorf("rows after two opens = %d, want 2", got)
	}
	if got := queryInt(t, s, "PRAGMA user_version"); got != 2 {
		t.Errorf("user_version = %d, want 2", got)
	}
}

func TestOpenRunsOnlyNewMigrations(t *testing.T) {
	fsys := fstest.MapFS{
		"migrations/0001_a.sql": {Data: []byte("CREATE TABLE runs (n INTEGER); INSERT INTO runs VALUES (1);")},
	}
	dir := t.TempDir()
	closeTest(t, openTest(t, dir, fsys))
	fsys["migrations/0002_b.sql"] = &fstest.MapFile{Data: []byte("INSERT INTO runs VALUES (2);")}
	s := openTest(t, dir, fsys)
	defer closeTest(t, s)

	if got := queryInt(t, s, "SELECT group_concat(n, '') FROM runs"); got != 12 {
		t.Errorf("runs = %d, want 12 (each migration once, in order)", got)
	}
}

func TestFailedMigrationRollsBack(t *testing.T) {
	fsys := fstest.MapFS{
		"migrations/0001_a.sql": {Data: []byte("CREATE TABLE a (n INTEGER);")},
		"migrations/0002_b.sql": {Data: []byte("CREATE TABLE b (n INTEGER); NOT SQL;")},
	}
	dir := t.TempDir()
	if _, err := open(context.Background(), dir, testTimeout, fsys); err == nil {
		t.Fatal("open() with a bad migration = nil error")
	}
	delete(fsys, "migrations/0002_b.sql")
	s := openTest(t, dir, fsys)
	defer closeTest(t, s)

	if got := queryInt(t, s, "PRAGMA user_version"); got != 1 {
		t.Errorf("user_version = %d, want 1", got)
	}
	if got := queryInt(t, s, "SELECT count(*) FROM sqlite_schema WHERE name = 'b'"); got != 0 {
		t.Errorf("table b exists after a failed migration")
	}
}

func TestLoadMigrationsBadNames(t *testing.T) {
	sql := &fstest.MapFile{Data: []byte("SELECT 1;")}
	for _, c := range []struct {
		name string
		fsys fstest.MapFS
	}{
		{"gap", fstest.MapFS{"migrations/0001_a.sql": sql, "migrations/0003_c.sql": sql}},
		{"starts at 2", fstest.MapFS{"migrations/0002_b.sql": sql}},
		{"no number", fstest.MapFS{"migrations/init.sql": sql}},
		{"duplicate", fstest.MapFS{"migrations/0001_a.sql": sql, "migrations/0001_b.sql": sql}},
	} {
		t.Run(c.name, func(t *testing.T) {
			if _, err := loadMigrations(c.fsys); err == nil {
				t.Errorf("loadMigrations(%v) = nil error", c.fsys)
			}
		})
	}
}

func TestOpenRefusesNewerStore(t *testing.T) {
	dir := t.TempDir()
	s := openTest(t, dir, fstest.MapFS(nil))
	if _, err := s.db.Exec("PRAGMA user_version = 99"); err != nil {
		t.Fatal(err)
	}
	closeTest(t, s)
	file := filepath.Join(dir, FileName)
	before, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}

	_, err = Open(context.Background(), dir, testTimeout)
	if !errors.Is(err, ErrNewer) {
		t.Fatalf("Open(newer store) = %v, want ErrNewer", err)
	}
	if !strings.Contains(err.Error(), "update Brain") {
		t.Errorf("error %q does not say \"update Brain\"", err)
	}
	after, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Error("Open(newer store) changed brain.db")
	}
	if fi, err := os.Stat(file + "-wal"); err == nil && fi.Size() > 0 {
		t.Errorf("Open(newer store) wrote %d bytes to the WAL", fi.Size())
	}
}

func TestWriteRollsBackOnError(t *testing.T) {
	s := openTest(t, t.TempDir(), fstest.MapFS{
		"migrations/0001_a.sql": {Data: []byte("CREATE TABLE t (n INTEGER);")},
	})
	defer closeTest(t, s)
	errStop := errors.New("stop")
	err := s.Write(context.Background(), func(tx *sql.Tx) error {
		if _, err := tx.Exec("INSERT INTO t VALUES (1)"); err != nil {
			return err
		}
		return errStop
	})
	if !errors.Is(err, errStop) {
		t.Fatalf("Write() = %v, want errStop", err)
	}
	if got := queryInt(t, s, "SELECT count(*) FROM t"); got != 0 {
		t.Errorf("rows after a failed Write = %d, want 0", got)
	}
}
