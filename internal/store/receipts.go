package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// LatestReceipt reads the newest receipt of agent session agentSession of harness profile in
// the store binding of herdr session session (E9-S2). ok is false when there is none. Only the
// agent session id joins a receipt to a live agent: the receipt of an older agent in the same
// pane has another id, and a pane that moved keeps its agent session id.
func LatestReceipt(ctx context.Context, tx *sql.Tx, session, profile, agentSession string) (d Delivery, ok bool, err error) {
	var id int64
	err = tx.QueryRowContext(ctx, `SELECT id FROM deliveries
		WHERE herdr_session = ? AND profile = ? AND agent_session_id = ? ORDER BY id DESC LIMIT 1`,
		session, profile, agentSession).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return Delivery{}, false, nil
	}
	if err != nil {
		return Delivery{}, false, fmt.Errorf("read receipts of agent session %s: %w", agentSession, err)
	}
	d, err = DeliveryByID(ctx, tx, id)
	return d, err == nil, err
}

// WithheldCount is the count of the withheld receipts with one reason.
type WithheldCount struct {
	Reason string // "" for a receipt with no reason
	Count  int
}

// WithheldCounts counts the withheld receipts of harness profile in the store binding of herdr
// session session, by reason, in reason order (E9-S2).
func WithheldCounts(ctx context.Context, tx *sql.Tx, session, profile string) ([]WithheldCount, error) {
	rows, err := tx.QueryContext(ctx, `SELECT COALESCE(withheld_reason, ''), COUNT(*) FROM deliveries
		WHERE herdr_session = ? AND profile = ? AND result = ?
		GROUP BY 1 ORDER BY 1`, session, profile, ResultWithheld)
	if err != nil {
		return nil, fmt.Errorf("read withheld receipts: %w", err)
	}
	defer rows.Close() //nolint:errcheck // read-only: rows.Err reports the read errors
	var cs []WithheldCount
	for rows.Next() {
		var c WithheldCount
		if err := rows.Scan(&c.Reason, &c.Count); err != nil {
			return nil, fmt.Errorf("read withheld receipts: %w", err)
		}
		cs = append(cs, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read withheld receipts: %w", err)
	}
	return cs, nil
}

// LatestPaneReceipt reads the newest receipt of pane paneID of harness profile in the store
// binding of herdr session session (E10-S2). ok is false when there is none. The eval runner
// opens a fresh pane for each probe in a fresh store, so the pane id names one agent there.
func LatestPaneReceipt(ctx context.Context, tx *sql.Tx, session, profile, paneID string) (d Delivery, ok bool, err error) {
	var id int64
	err = tx.QueryRowContext(ctx, `SELECT id FROM deliveries
		WHERE herdr_session = ? AND profile = ? AND pane_id = ? ORDER BY id DESC LIMIT 1`,
		session, profile, paneID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return Delivery{}, false, nil
	}
	if err != nil {
		return Delivery{}, false, fmt.Errorf("read receipts of pane %s: %w", paneID, err)
	}
	d, err = DeliveryByID(ctx, tx, id)
	return d, err == nil, err
}
