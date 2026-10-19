// Package store opens the SQLite store, one file brain.db per store folder, and brings it to
// the latest schema version. SQLite is the only source of truth.
package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
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
	db          *sql.DB
	busyTimeout time.Duration
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
	if err := setup(ctx, db, busyTimeout, ms); err != nil {
		return nil, errors.Join(busy(err), db.Close())
	}
	return &Store{db: db, busyTimeout: busyTimeout}, nil
}

// dsn turns on foreign keys for each connection and makes each transaction take the write
// lock at BEGIN. A deferred transaction that upgrades from read to write gets SQLITE_BUSY at
// once in WAL mode, without the busy timeout. synchronous NORMAL lets a commit return without
// a wait for the disk; in WAL mode a power loss can lose the last commits but cannot corrupt
// the store.
func dsn(path string, busyTimeout time.Duration) string {
	q := url.Values{}
	q.Add("_pragma", "busy_timeout("+strconv.FormatInt(busyTimeout.Milliseconds(), 10)+")")
	q.Add("_pragma", "foreign_keys(1)")
	q.Add("_pragma", "synchronous(NORMAL)")
	q.Set("_txlock", "immediate")
	u := url.URL{Scheme: "file", Path: path, RawQuery: q.Encode()}
	return u.String()
}

// setup checks the version before it writes anything, so a store from a newer binary stays
// as it is. Then it turns on WAL, which is stored in the file, and runs the migrations.
func setup(ctx context.Context, db *sql.DB, busyTimeout time.Duration, ms []migration) error {
	v, err := userVersion(ctx, db)
	if err != nil {
		return err
	}
	if err := checkVersion(v, len(ms)); err != nil {
		return err
	}
	if err := setWAL(ctx, db, busyTimeout); err != nil {
		return err
	}
	for _, m := range ms[v:] {
		if err := migrate(ctx, db, busyTimeout, m, len(ms)); err != nil {
			return err
		}
	}
	return nil
}

// walRetry is the pause between two tries to switch a new store to WAL. [NEW]
const walRetry = 5 * time.Millisecond

// setWAL switches the store to WAL mode. SQLite does not always call the busy handler for a
// journal mode change, so two processes that open a new store at the same time can get
// SQLITE_BUSY at once. setWAL tries again until the busy timeout ends, or the deadline of ctx
// when that comes first. Each try waits only for the time that is left.
func setWAL(ctx context.Context, db *sql.DB, busyTimeout time.Duration) (err error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("get connection: %w", err)
	}
	defer func() {
		// Later reads on this connection get the full busy timeout again.
		err = errors.Join(err, setBusyTimeout(context.WithoutCancel(ctx), conn, busyTimeout), conn.Close())
	}()
	end := time.Now().Add(busyTimeout)
	for {
		if err := setBusyTimeout(ctx, conn, max(0, time.Until(end))); err != nil {
			return err
		}
		var mode string
		err := conn.QueryRowContext(ctx, "PRAGMA journal_mode=WAL").Scan(&mode)
		if err == nil {
			if mode != "wal" {
				return fmt.Errorf("set WAL mode: journal mode is %q", mode)
			}
			return nil
		}
		left := time.Until(end)
		if !errors.Is(busy(err), ErrBusy) || left <= 0 {
			return errors.Join(ctx.Err(), fmt.Errorf("set WAL mode: %w", err))
		}
		select {
		case <-ctx.Done():
			return errors.Join(ctx.Err(), fmt.Errorf("set WAL mode: %w", err))
		case <-time.After(min(walRetry, left)):
		}
	}
}

// checkVersion refuses a store with a newer version than latest, the binary's version.
func checkVersion(v, latest int) error {
	if v > latest {
		return fmt.Errorf("%w (store version %d, binary version %d)", ErrNewer, v, latest)
	}
	return nil
}

// migrate runs one migration in its own transaction. It reads the version again inside the
// transaction, because another process, maybe a newer binary, can migrate between the first
// check and the lock.
func migrate(ctx context.Context, db *sql.DB, busyTimeout time.Duration, m migration, latest int) error {
	return inTx(ctx, db, busyTimeout, func(tx *sql.Tx) error {
		v, err := userVersion(ctx, tx)
		if err != nil {
			return err
		}
		if err := checkVersion(v, latest); err != nil {
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
// An error wraps ErrBusy when the lock stayed taken for the whole busy timeout, or until the
// deadline of ctx when that comes first.
func (s *Store) Write(ctx context.Context, fn func(tx *sql.Tx) error) error {
	return busy(inTx(ctx, s.db, s.busyTimeout, fn))
}

func inTx(ctx context.Context, db *sql.DB, busyTimeout time.Duration, fn func(tx *sql.Tx) error) (err error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("get connection: %w", err)
	}
	defer func() {
		// After discard the connection is closed already, and Close says ErrConnDone.
		if cerr := conn.Close(); !errors.Is(cerr, sql.ErrConnDone) {
			err = errors.Join(err, cerr)
		}
	}()
	// SQLite's busy wait does not see ctx, so the wait ends at the deadline of ctx when that
	// comes before the busy timeout.
	if err := setBusyTimeout(ctx, conn, busyTimeout); err != nil {
		return err
	}
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return errors.Join(ctx.Err(), fmt.Errorf("begin: %w", err))
	}
	if err := fn(tx); err != nil {
		return errors.Join(err, tx.Rollback())
	}
	if err := tx.Commit(); err != nil {
		// A failed COMMIT, such as a deferred foreign key, can leave SQLite inside the
		// transaction while database/sql counts it as done. Closing the connection rolls it
		// back, and the next call opens a new one.
		return errors.Join(fmt.Errorf("commit: %w", err), discard(conn))
	}
	return nil
}

func setBusyTimeout(ctx context.Context, conn *sql.Conn, busyTimeout time.Duration) error {
	if deadline, ok := ctx.Deadline(); ok {
		busyTimeout = max(0, min(busyTimeout, time.Until(deadline)))
	}
	// PRAGMA takes no bound parameters; the value is an int.
	q := "PRAGMA busy_timeout = " + strconv.FormatInt(busyTimeout.Milliseconds(), 10)
	if _, err := conn.ExecContext(ctx, q); err != nil {
		return fmt.Errorf("set busy timeout: %w", err)
	}
	return nil
}

// discard closes the connection under conn instead of putting it back in the pool.
func discard(conn *sql.Conn) error {
	err := conn.Raw(func(any) error { return driver.ErrBadConn })
	if errors.Is(err, driver.ErrBadConn) {
		return nil
	}
	return err
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
