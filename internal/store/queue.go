package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// PendingProposalIDs reads the ids of the proposals that wait for your decision, in id order.
// The caller reads each one and checks its scope.
func PendingProposalIDs(ctx context.Context, tx *sql.Tx) ([]int64, error) {
	return ids(ctx, tx, "pending proposals", `SELECT id FROM proposals WHERE status = ? ORDER BY id`, ProposalPending)
}

// ItemIDsInState reads the ids of the items in state, in id order.
func ItemIDsInState(ctx context.Context, tx *sql.Tx, state State) ([]int64, error) {
	return ids(ctx, tx, string(state)+" items", `SELECT id FROM items WHERE state = ? ORDER BY id`, state)
}

func ids(ctx context.Context, tx *sql.Tx, what, query string, arg any) ([]int64, error) {
	rows, err := tx.QueryContext(ctx, query, arg)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", what, err)
	}
	defer rows.Close() //nolint:errcheck // read-only: rows.Err reports the read errors
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("read %s: %w", what, err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read %s: %w", what, err)
	}
	return ids, nil
}

// DecideProposal records your decision on proposal id: its new status, the surface that you
// used and the time.
func DecideProposal(ctx context.Context, tx *sql.Tx, id int64, status ProposalStatus, surface Surface, decided time.Time) error {
	_, err := tx.ExecContext(ctx, `UPDATE proposals SET status = ?, surface = ?, decided = ? WHERE id = ?`,
		status, surface, timeArg(decided), id)
	if err != nil {
		return fmt.Errorf("decide proposal %d: %w", id, err)
	}
	return nil
}
