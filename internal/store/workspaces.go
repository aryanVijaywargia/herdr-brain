package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// SeeWorkspace sets the label of the workspace with key and its last seen time. A `brain`
// call runs it for each workspace in `herdr workspace list`, so a rename updates the label
// and keeps the key (§9.4). It returns sql.ErrNoRows when there is no such workspace.
func SeeWorkspace(ctx context.Context, tx *sql.Tx, key, label string, seen time.Time) error {
	res, err := tx.ExecContext(ctx, `UPDATE workspaces SET label = ?, last_seen = ? WHERE key = ?`,
		label, timeArg(seen), key)
	if err != nil {
		return fmt.Errorf("update workspace %s: %w", key, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("update workspace %s: %w", key, err)
	}
	if n == 0 {
		return fmt.Errorf("update workspace %s: %w", key, sql.ErrNoRows)
	}
	return nil
}

// CloseWorkspace sets the status of the workspace with key to closed (§9.4 "Close", D4). Its
// items stay. It returns sql.ErrNoRows when there is no such workspace.
func CloseWorkspace(ctx context.Context, tx *sql.Tx, key string) error {
	res, err := tx.ExecContext(ctx, `UPDATE workspaces SET status = ? WHERE key = ?`, WorkspaceClosed, key)
	if err != nil {
		return fmt.Errorf("close workspace %s: %w", key, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("close workspace %s: %w", key, err)
	}
	if n == 0 {
		return fmt.Errorf("close workspace %s: %w", key, sql.ErrNoRows)
	}
	return nil
}
