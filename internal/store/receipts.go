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

// TiedReceipt reads the last receipt before observation obs that ties to its capture session,
// of any result (§5.4), with the E11-S3 join: the binding and the harness first, then the
// agent session id of the session. A session that herdr never gave an id ties by its stored
// capture row: the receipts of its pane in its binding after the previous session of that pane,
// whose agent session id no other session of the binding holds. ok is false when no receipt
// ties, when those pane receipts name more than one agent session, or when no previous session
// of the pane bounds them: an older receipt of the pane can be another agent's, so no single
// receipt ties.
// A pane id never joins across bindings.
func TiedReceipt(ctx context.Context, tx *sql.Tx, obs int64) (d Delivery, ok bool, err error) {
	var s struct {
		id                        int64
		binding, harness, agent   string
		pane, started, observedAt string
	}
	err = tx.QueryRowContext(ctx, `SELECT n.id, n.herdr_session, n.harness, COALESCE(n.agent_session_id, ''), n.pane_id,
		n.started, o.at FROM observations o JOIN segments g ON g.id = o.segment_id JOIN sessions n ON n.id = g.session_id
		WHERE o.id = ?`, obs).Scan(&s.id, &s.binding, &s.harness, &s.agent, &s.pane, &s.started, &s.observedAt)
	if err != nil {
		return Delivery{}, false, fmt.Errorf("read session of observation %d: %w", obs, err)
	}
	var rows *sql.Rows
	if s.agent != "" {
		rows, err = tx.QueryContext(ctx, `SELECT id, agent_session_id FROM deliveries
			WHERE herdr_session = ? AND profile = ? AND agent_session_id = ? AND at <= ?
			ORDER BY at DESC, id DESC LIMIT 1`, s.binding, s.harness, s.agent, s.observedAt)
	} else {
		rows, err = tx.QueryContext(ctx, `SELECT id, COALESCE(agent_session_id, '') FROM deliveries d
			WHERE herdr_session = ?1 AND profile = ?2 AND pane_id = ?3 AND at <= ?4
			AND at > (SELECT max(COALESCE(p.ended, p.started)) FROM sessions p WHERE p.herdr_session = ?1
				AND p.pane_id = ?3 AND p.id != ?5 AND p.started < ?6)
			AND NOT EXISTS (SELECT 1 FROM sessions p WHERE p.herdr_session = ?1 AND p.id != ?5
				AND p.agent_session_id = d.agent_session_id)
			ORDER BY at DESC, id DESC`, s.binding, s.harness, s.pane, s.observedAt, s.id, s.started)
	}
	if err != nil {
		return Delivery{}, false, fmt.Errorf("read receipts of observation %d: %w", obs, err)
	}
	defer rows.Close() //nolint:errcheck // read-only: rows.Err reports the read errors
	var ids []int64
	agents := map[string]bool{}
	for rows.Next() {
		var id int64
		var agent string
		if err := rows.Scan(&id, &agent); err != nil {
			return Delivery{}, false, fmt.Errorf("read receipts of observation %d: %w", obs, err)
		}
		ids = append(ids, id)
		agents[agent] = true
	}
	if err := rows.Err(); err != nil {
		return Delivery{}, false, fmt.Errorf("read receipts of observation %d: %w", obs, err)
	}
	if len(ids) == 0 || len(agents) > 1 {
		return Delivery{}, false, nil
	}
	d, err = DeliveryByID(ctx, tx, ids[0])
	return d, err == nil, err
}
