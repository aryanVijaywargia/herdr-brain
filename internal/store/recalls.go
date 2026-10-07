package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Recall is the record of one brain recall call (E20-S2, §8.6): the receipt of the agent
// session that called it, and the item revisions that it returned.
type Recall struct {
	DeliveryID    int64
	ItemRevisions []RevisionRef
	At            time.Time
}

// InsertRecall adds r and returns its new id.
func InsertRecall(ctx context.Context, tx *sql.Tx, r Recall) (int64, error) {
	return insertRow(ctx, tx, "recall", `INSERT INTO recalls (delivery_id, item_revisions, at) VALUES (?, ?, ?)`,
		r.DeliveryID, jsonList[RevisionRef](r.ItemRevisions), timeArg(r.At))
}

// RecallReceipt reads the id of the newest receipt of pane paneID in the store binding of herdr
// session session: the receipt of the agent session in the pane, that a recall hangs on. When
// herdr shows the agent session agentSession of the pane, the receipt must name it, so a
// recall never hangs on the receipt of an older agent of the pane. ok is false when there is
// none.
func RecallReceipt(ctx context.Context, tx *sql.Tx, session, paneID, agentSession string) (id int64, ok bool, err error) {
	err = tx.QueryRowContext(ctx, `SELECT id FROM deliveries
		WHERE herdr_session = ?1 AND pane_id = ?2 AND (?3 = '' OR agent_session_id = ?3) ORDER BY id DESC LIMIT 1`,
		session, paneID, agentSession).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("read receipt of pane %s: %w", paneID, err)
	}
	return id, true, nil
}

// recalled returns the item revisions that the recalls on receipt delivery returned at or
// before at, the newest recall first.
func recalled(ctx context.Context, tx *sql.Tx, delivery int64, at string) ([]RevisionRef, error) {
	var refs []RevisionRef
	err := eachRow(ctx, tx, `SELECT item_revisions FROM recalls WHERE delivery_id = ? AND at <= ?
		ORDER BY at DESC, id DESC`, func(rows *sql.Rows) error {
		var list []RevisionRef
		err := rows.Scan(listDest[RevisionRef]{&list})
		refs = append(refs, list...)
		return err
	}, delivery, at)
	if err != nil {
		return nil, fmt.Errorf("read recalls of receipt %d: %w", delivery, err)
	}
	return refs, nil
}
