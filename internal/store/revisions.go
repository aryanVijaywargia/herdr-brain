package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ErrConflict means that the item's current revision is not the expected one: another write
// came first (§3.1, §7.5 step 2).
var ErrConflict = errors.New("revision conflict")

// WriteRevision makes r the next revision of item r.ItemID, when the item's current revision
// is still expected (§7.5 steps 1-2). It sets r.Revision to expected+1, copies the fields that
// items also holds from r, and records a new output generation, which it returns. A stale
// expected revision returns ErrConflict before it writes anything. Run it inside Store.Write,
// so all of it is one transaction.
func WriteRevision(ctx context.Context, tx *sql.Tx, expected int64, r ItemRevision) (int64, error) {
	r.Revision = expected + 1
	// The WHERE clause is the revision check, so the check and the write cannot come apart.
	res, err := tx.ExecContext(ctx, `UPDATE items SET current_revision = ?, scope = ?,
		workspace_key = ?, state = ?, pinned = ? WHERE id = ? AND current_revision = ?`,
		r.Revision, r.Scope, r.WorkspaceKey, r.State, r.Pinned, r.ItemID, expected)
	if err != nil {
		return 0, fmt.Errorf("update item %d: %w", r.ItemID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("update item %d: %w", r.ItemID, err)
	}
	if n == 0 {
		return 0, fmt.Errorf("item %d at revision %d: %w", r.ItemID, expected, ErrConflict)
	}
	return commitRevision(ctx, tx, r)
}

// CreateItem adds it with r as its first revision, and records a new output generation. The
// item's current revision and the fields that it shares with r come from r. It returns the
// new item id and the generation. it.ID and r.ItemID are ignored.
func CreateItem(ctx context.Context, tx *sql.Tx, it Item, r ItemRevision) (id, generation int64, err error) {
	r.Revision = 1
	it.CurrentRevision = r.Revision
	it.Scope, it.WorkspaceKey, it.State, it.Pinned = r.Scope, r.WorkspaceKey, r.State, r.Pinned
	if r.ItemID, err = InsertItem(ctx, tx, it); err != nil {
		return 0, 0, err
	}
	generation, err = commitRevision(ctx, tx, r)
	return r.ItemID, generation, err
}

// commitRevision inserts r and records the next output generation.
func commitRevision(ctx context.Context, tx *sql.Tx, r ItemRevision) (int64, error) {
	if err := InsertItemRevision(ctx, tx, r); err != nil {
		return 0, err
	}
	return nextGeneration(ctx, tx)
}

// nextGeneration records the output generation after the newest one (§7.5 step 2). The
// compiler builds it later and fills in its manifest hash and build time (step 3).
func nextGeneration(ctx context.Context, tx *sql.Tx) (int64, error) {
	var g int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(generation), 0) + 1 FROM outputs`).Scan(&g); err != nil {
		return 0, fmt.Errorf("read newest output generation: %w", err)
	}
	// [NEW] the folder form: gen-<n> in the out folder (§7.5 step 3).
	o := Output{Generation: g, Folder: fmt.Sprintf("gen-%d", g)}
	if err := InsertOutput(ctx, tx, o); err != nil {
		return 0, err
	}
	return g, nil
}
