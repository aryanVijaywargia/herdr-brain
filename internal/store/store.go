// Package store opens the SQLite store, one file brain.db per store folder, and brings it to
// the latest schema version (HLD §7.5, §11). SQLite is the only source of truth.
package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"path/filepath"
	"strconv"
	"time"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// FileName is the name of the store file in its folder.
const FileName = "brain.db"

//go:embed migrations/*.sql
var migrations embed.FS

// ErrBusy means that another process held the store lock for longer than the busy timeout.
var ErrBusy = errors.New("store busy")

// ErrNewer means that the store has a newer schema than this binary knows.
var ErrNewer = errors.New("store has a newer version: update Brain")

// Store is an open store.
type Store struct {
	db *sql.DB
}

// Open opens dir/brain.db and migrates it to the latest version. The folder must exist; the
// file is created on the first write. busyTimeout is how long a write waits for the lock of
// another process: the hook gives a short one, CLI calls a longer one.
func Open(ctx context.Context, dir string, busyTimeout time.Duration) (*Store, error) {
	return open(ctx, dir, busyTimeout, migrations)
}

func open(ctx context.Context, dir string, busyTimeout time.Duration, fsys fs.FS) (*Store, error) {
	ms, err := loadMigrations(fsys)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", dsn(filepath.Join(dir, FileName), busyTimeout))
	if err != nil {
		return nil, fmt.Errorf("open store: %w", err)
	}
	// One connection: a Brain call is one short process, and a second connection in the same
	// process would only wait on the first one's lock.
	db.SetMaxOpenConns(1)
	if err := setup(ctx, db, ms); err != nil {
		return nil, errors.Join(busy(err), db.Close())
	}
	return &Store{db: db}, nil
}

// dsn turns on foreign keys for each connection and makes each transaction take the write
// lock at BEGIN. A deferred transaction that upgrades from read to write gets SQLITE_BUSY at
// once in WAL mode, without the busy timeout.
func dsn(path string, busyTimeout time.Duration) string {
	q := url.Values{}
	q.Add("_pragma", "busy_timeout("+strconv.FormatInt(busyTimeout.Milliseconds(), 10)+")")
	q.Add("_pragma", "foreign_keys(1)")
	q.Set("_txlock", "immediate")
	u := url.URL{Scheme: "file", Path: path, RawQuery: q.Encode()}
	return u.String()
}

// setup checks the version before it writes anything, so a store from a newer binary stays
// as it is. Then it turns on WAL, which is stored in the file, and runs the migrations.
func setup(ctx context.Context, db *sql.DB, ms []migration) error {
	v, err := userVersion(ctx, db)
	if err != nil {
		return err
	}
	if v > len(ms) {
		return fmt.Errorf("%w (store version %d, binary version %d)", ErrNewer, v, len(ms))
	}
	var mode string
	if err := db.QueryRowContext(ctx, "PRAGMA journal_mode=WAL").Scan(&mode); err != nil {
		return fmt.Errorf("set WAL mode: %w", err)
	}
	if mode != "wal" {
		return fmt.Errorf("set WAL mode: journal mode is %q", mode)
	}
	for _, m := range ms[v:] {
		if err := migrate(ctx, db, m); err != nil {
			return err
		}
	}
	return nil
}

// migrate runs one migration in its own transaction. It reads the version again inside the
// transaction, because another process can migrate between the first check and the lock.
func migrate(ctx context.Context, db *sql.DB, m migration) error {
	return inTx(ctx, db, func(tx *sql.Tx) error {
		v, err := userVersion(ctx, tx)
		if err != nil {
			return err
		}
		if v >= m.version {
			return nil
		}
		if _, err := tx.ExecContext(ctx, m.sql); err != nil {
			return fmt.Errorf("migration %s: %w", m.name, err)
		}
		// PRAGMA takes no bound parameters; m.version is an int from the file name.
		if _, err := tx.ExecContext(ctx, "PRAGMA user_version = "+strconv.Itoa(m.version)); err != nil {
			return fmt.Errorf("migration %s: set version: %w", m.name, err)
		}
		return nil
	})
}

type querier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func userVersion(ctx context.Context, q querier) (int, error) {
	var v int
	if err := q.QueryRowContext(ctx, "PRAGMA user_version").Scan(&v); err != nil {
		return 0, fmt.Errorf("read store version: %w", err)
	}
	return v, nil
}

// Write runs fn in one transaction that holds the write lock. It commits when fn returns nil.
// An error wraps ErrBusy when the lock stayed taken for the whole busy timeout.
func (s *Store) Write(ctx context.Context, fn func(tx *sql.Tx) error) error {
	return busy(inTx(ctx, s.db, fn))
}

func inTx(ctx context.Context, db *sql.DB, fn func(tx *sql.Tx) error) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	if err := fn(tx); err != nil {
		return errors.Join(err, tx.Rollback())
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// Close closes the store.
func (s *Store) Close() error {
	return s.db.Close()
}

// busy adds ErrBusy to err when SQLite says that the lock is taken.
func busy(err error) error {
	var se *sqlite.Error
	if errors.As(err, &se) && se.Code()&0xff == sqlite3.SQLITE_BUSY {
		return fmt.Errorf("%w: %w", ErrBusy, err)
	}
	return err
}
