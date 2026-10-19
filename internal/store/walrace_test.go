package store

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// TestOpenNewStoreRace opens a new, empty store from several goroutines at the same time. Each
// goroutine has its own connection, as two brain processes do. SQLite does not always call the
// busy handler when it changes the journal mode, so without a retry one open fails with
// SQLITE_BUSY.
func TestOpenNewStoreRace(t *testing.T) {
	const rounds, openers = 50, 4
	for round := range rounds {
		dir := t.TempDir()
		var wg sync.WaitGroup
		stores := make([]*Store, openers)
		errs := make([]error, openers)
		start := make(chan struct{})
		for i := range openers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				stores[i], errs[i] = Open(context.Background(), dir, testTimeout)
			}()
		}
		close(start)
		wg.Wait()
		for i, s := range stores {
			if errs[i] != nil {
				t.Fatalf("round %d: Open() = %v", round, errs[i])
			}
			var mode string
			if err := s.db.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil {
				t.Fatal(err)
			}
			if mode != "wal" {
				t.Errorf("round %d: journal_mode = %q, want wal", round, mode)
			}
			closeTest(t, s)
		}
	}
}

// TestOpenBusyEndsAtTimeout checks that a short busy timeout, such as the hook's, bounds the
// wait of Open while another connection holds the lock on a new store.
func TestOpenBusyEndsAtTimeout(t *testing.T) {
	dir := t.TempDir()
	holder, err := Open(context.Background(), dir, testTimeout)
	if err != nil {
		t.Fatal(err)
	}
	defer closeTest(t, holder)
	// A rollback-journal store that another connection locks: Open cannot switch it to WAL.
	if _, err := holder.db.Exec("PRAGMA journal_mode=DELETE"); err != nil {
		t.Fatal(err)
	}
	tx, err := holder.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec("PRAGMA user_version = 0"); err != nil {
		t.Fatal(err)
	}

	const timeout = 50 * time.Millisecond // the hook's busy timeout
	start := time.Now()
	s, err := Open(context.Background(), dir, timeout)
	waited := time.Since(start)
	if err == nil {
		closeTest(t, s)
	}
	if !errors.Is(err, ErrBusy) {
		t.Errorf("Open() while locked = %v, want ErrBusy", err)
	}
	// The margin is for a slow test machine.
	if limit := timeout + 250*time.Millisecond; waited >= limit {
		t.Errorf("Open() with a 50 ms busy timeout waited %s, want less than %s", waited, limit)
	}
}

// TestOpenWALStoreWhileLocked checks that a store in WAL mode opens as before: the journal mode
// is set already, so Open does not wait for a write lock that another connection holds.
func TestOpenWALStoreWhileLocked(t *testing.T) {
	dir := t.TempDir()
	holder, err := Open(context.Background(), dir, testTimeout)
	if err != nil {
		t.Fatal(err)
	}
	defer closeTest(t, holder)
	tx, err := holder.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()

	s, err := Open(context.Background(), dir, 50*time.Millisecond) // the hook's busy timeout
	if err != nil {
		t.Fatalf("Open() of a WAL store while another connection writes = %v", err)
	}
	closeTest(t, s)
}
