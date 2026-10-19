package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ErrConflict means that the item's current revision is not the expected one: another write
// came first.
var ErrConflict = errors.New("revision conflict")

// WriteRevision makes r the next revision of item r.ItemID, when the item's current revision
// is still expected. It sets r.Revision to expected+1, copies the fields that items also holds
// from r, and records a new output generation, which it returns. A stale expected revision
// returns ErrConflict before it writes anything. An active skill whose text does not parse, or
// whose name another active skill of its scope has, returns an error (CheckSkill; ErrSkillName
// for the name). A tool writes its tools row; a contract that breaks a tool form rule, or an
// active tool name that its scope has, returns an error (syncTool). An automation writes its
// automations row (syncAutomation). Run it inside Store.Write, so all of it is one transaction
// and an error writes nothing.
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
	gen, err := commitRevision(ctx, tx, r)
	if err != nil {
		return 0, err
	}
	// Every path that makes a skill or a tool active writes a revision here (approve, restore,
	// undo, promotion and link), so the name check runs in the same transaction.
	if err := CheckSkill(ctx, tx, r.ItemID); err != nil {
		return 0, err
	}
	if err := syncTool(ctx, tx, r.ItemID); err != nil {
		return 0, err
	}
	return gen, syncAutomation(ctx, tx, r.ItemID)
}

// CreateItem adds it with r as its first revision, and records a new output generation. The
// item's current revision and the fields that it shares with r come from r. It returns the
// new item id and the generation. A tool writes its tools row, with the checks of syncTool,
// and an automation its automations row.
// it.ID and r.ItemID are ignored.
func CreateItem(ctx context.Context, tx *sql.Tx, it Item, r ItemRevision) (id, generation int64, err error) {
	r.Revision = 1
	it.CurrentRevision = r.Revision
	it.Scope, it.WorkspaceKey, it.State, it.Pinned = r.Scope, r.WorkspaceKey, r.State, r.Pinned
	if r.ItemID, err = InsertItem(ctx, tx, it); err != nil {
		return 0, 0, err
	}
	if generation, err = commitRevision(ctx, tx, r); err != nil {
		return 0, 0, err
	}
	if err := syncTool(ctx, tx, r.ItemID); err != nil {
		return 0, 0, err
	}
	return r.ItemID, generation, syncAutomation(ctx, tx, r.ItemID)
}

// commitRevision inserts r and records the next output generation.
func commitRevision(ctx context.Context, tx *sql.Tx, r ItemRevision) (int64, error) {
	if err := InsertItemRevision(ctx, tx, r); err != nil {
		return 0, err
	}
	return nextGeneration(ctx, tx)
}

// nextGeneration records the output generation after the newest one. The compiler
// builds it later and fills in its manifest hash and build time (step 3).
func nextGeneration(ctx context.Context, tx *sql.Tx) (int64, error) {
	var g int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(generation), 0) + 1 FROM outputs`).Scan(&g); err != nil {
		return 0, fmt.Errorf("read newest output generation: %w", err)
	}
	// The folder form: gen-<n> in the out folder.
	o := Output{Generation: g, Folder: fmt.Sprintf("gen-%d", g)}
	if err := InsertOutput(ctx, tx, o); err != nil {
		return 0, err
	}
	return g, nil
}
