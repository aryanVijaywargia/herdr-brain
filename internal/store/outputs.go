package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// The queries of the compiler (E6-S2, §7.5 steps 3-9).

// NewestGeneration returns the newest output generation that a commit recorded, or 0 when
// there is none.
func NewestGeneration(ctx context.Context, tx *sql.Tx) (int64, error) {
	var g int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(generation), 0) FROM outputs`).Scan(&g); err != nil {
		return 0, fmt.Errorf("read newest output generation: %w", err)
	}
	return g, nil
}

// ActiveRevisions reads the current revision of each active item, in item id order.
func ActiveRevisions(ctx context.Context, tx *sql.Tx) ([]ItemRevision, error) {
	rows, err := tx.QueryContext(ctx, `SELECT `+revisionColumns+` FROM item_revisions r
		JOIN items i ON r.item_id = i.id AND r.revision = i.current_revision
		WHERE i.state = ? ORDER BY i.id`, StateActive)
	if err != nil {
		return nil, fmt.Errorf("read active items: %w", err)
	}
	defer rows.Close() //nolint:errcheck // read-only: rows.Err reports the read errors
	var rs []ItemRevision
	for rows.Next() {
		r, err := scanRevision(rows)
		if err != nil {
			return nil, fmt.Errorf("read active items: %w", err)
		}
		rs = append(rs, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read active items: %w", err)
	}
	return rs, nil
}

// WorkspaceKeys reads the keys of all workspaces, in key order.
func WorkspaceKeys(ctx context.Context, tx *sql.Tx) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT key FROM workspaces ORDER BY key`)
	if err != nil {
		return nil, fmt.Errorf("read workspace keys: %w", err)
	}
	defer rows.Close() //nolint:errcheck // read-only: rows.Err reports the read errors
	var keys []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, fmt.Errorf("read workspace keys: %w", err)
		}
		keys = append(keys, k)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read workspace keys: %w", err)
	}
	return keys, nil
}

// PublishOutput records that generation is built and current. The pointer file is the
// authority; this row is its copy (§7.5 step 9), so every other row stops being current.
func PublishOutput(ctx context.Context, tx *sql.Tx, generation int64, manifestHash string, builtAt time.Time) error {
	if _, err := tx.ExecContext(ctx, `UPDATE outputs SET current = (generation = ?)`, generation); err != nil {
		return fmt.Errorf("publish output generation %d: %w", generation, err)
	}
	_, err := tx.ExecContext(ctx, `UPDATE outputs SET manifest_hash = ?, built_at = ? WHERE generation = ?`,
		manifestHash, timeArg(builtAt), generation)
	if err != nil {
		return fmt.Errorf("publish output generation %d: %w", generation, err)
	}
	return nil
}
