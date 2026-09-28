package store

import (
	"context"
	"database/sql"
	"testing"
	"time"
)

// hookTimeout is the hook's busy timeout (hook.go). [NEW] copy for the measure only.
const hookTimeout = 50 * time.Millisecond

// hookRun does the store work of one hook run: open, one receipt in one transaction, close.
func hookRun(ctx context.Context, dir, sync string) error {
	s, err := Open(ctx, dir, hookTimeout)
	if err != nil {
		return err
	}
	if sync != "" {
		// The store keeps one connection, so the pragma holds for the write below.
		if _, err := s.db.ExecContext(ctx, "PRAGMA synchronous = "+sync); err != nil {
			return err
		}
	}
	err = s.Write(ctx, func(tx *sql.Tx) error {
		_, err := InsertDelivery(ctx, tx, Delivery{Profile: "claude", PaneID: "p1", Result: ResultEmitted, At: time.Now()})
		return err
	})
	if cerr := s.Close(); err == nil {
		err = cerr
	}
	return err
}

// BenchmarkHookRun measures the store part of one hook run (E3-S6). "full" is the old
// synchronous mode. "normal-no-close-checkpoint" keeps a second connection open, so the close
// of the hook's connection is not the last one and does not copy the WAL into the store file:
// that is what SQLITE_DBCONFIG_NO_CKPT_ON_CLOSE would give.
func BenchmarkHookRun(b *testing.B) {
	for _, c := range []struct {
		name, sync string
		holdOpen   bool
	}{
		{"full", "FULL", false},
		{"normal", "", false},
		{"normal-no-close-checkpoint", "", true},
	} {
		b.Run(c.name, func(b *testing.B) {
			ctx := context.Background()
			dir := b.TempDir()
			if err := hookRun(ctx, dir, c.sync); err != nil {
				b.Fatal(err)
			}
			if c.holdOpen {
				other, err := Open(ctx, dir, testTimeout)
				if err != nil {
					b.Fatal(err)
				}
				// Open leaves its connection idle in the pool, so the file stays open.
				defer other.Close()
			}
			b.ResetTimer()
			for range b.N {
				if err := hookRun(ctx, dir, c.sync); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// TestReopenReadsCommits checks that NORMAL loses no commit at a clean close: a new open
// reads every row that the earlier opens committed.
func TestReopenReadsCommits(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	const runs = 20 // [NEW]
	for range runs {
		if err := hookRun(ctx, dir, ""); err != nil {
			t.Fatal(err)
		}
	}
	s, err := Open(ctx, dir, testTimeout)
	if err != nil {
		t.Fatal(err)
	}
	defer closeTest(t, s)
	if got := queryInt(t, s, "SELECT count(*) FROM deliveries"); got != runs {
		t.Errorf("deliveries = %d, want %d", got, runs)
	}
}
