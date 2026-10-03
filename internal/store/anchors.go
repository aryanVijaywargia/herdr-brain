package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// The queries of the anchors and the stale check (E16-S6, §9.1).

// SetAnchors replaces the anchors of item id.
func SetAnchors(ctx context.Context, tx *sql.Tx, id int64, anchors []Anchor) error {
	if _, err := tx.ExecContext(ctx, `UPDATE items SET anchors = ? WHERE id = ?`, jsonList[Anchor](anchors), id); err != nil {
		return fmt.Errorf("set anchors of item %d: %w", id, err)
	}
	return nil
}

// AnchoredItems reads the active workspace items that have anchors, in id order: the items of
// workspace key, or of every workspace when key is "".
func AnchoredItems(ctx context.Context, tx *sql.Tx, key string) ([]Item, error) {
	items, err := query(ctx, tx, `SELECT id, claim_id, current_revision, kind, scope, workspace_key, state,
		pinned, anchors, created FROM items
		WHERE state = ?1 AND scope = ?2 AND json_array_length(anchors) > 0 AND (?3 = '' OR workspace_key = ?3)
		ORDER BY id`,
		func(rows *sql.Rows) (Item, error) {
			var it Item
			err := rows.Scan(&it.ID, &it.ClaimID, &it.CurrentRevision, &it.Kind, &it.Scope, &it.WorkspaceKey,
				&it.State, &it.Pinned, listDest[Anchor]{&it.Anchors}, timeDest{&it.Created})
			return it, err
		}, StateActive, ScopeWorkspace, key)
	if err != nil {
		return nil, fmt.Errorf("read anchored items: %w", err)
	}
	return items, nil
}

// AnchorPaths reads the anchor paths of each delivered item that has anchors, by item id: the
// brief keeps room for their dispute mark (§9.1).
func AnchorPaths(ctx context.Context, tx *sql.Tx) (map[int64][]string, error) {
	type row struct {
		id      int64
		anchors []Anchor
	}
	rows, err := query(ctx, tx, `SELECT id, anchors FROM items
		WHERE state IN (?, ?) AND json_array_length(anchors) > 0 ORDER BY id`,
		func(rows *sql.Rows) (row, error) {
			var r row
			err := rows.Scan(&r.id, listDest[Anchor]{&r.anchors})
			return r, err
		}, StateActive, StateDisputed)
	if err != nil {
		return nil, fmt.Errorf("read anchor paths: %w", err)
	}
	m := make(map[int64][]string, len(rows))
	for _, r := range rows {
		for _, a := range r.anchors {
			m[r.id] = append(m[r.id], a.Path)
		}
	}
	return m, nil
}

// Dispute makes item p.ItemID disputed and adds p, its review in the queue (§9.1), when the
// item is still active at revision *p.BaseRevision. The item keeps its revision and its text:
// Brain does not change the item itself. It records a new output generation, so the next
// build marks the item in the brief. It returns the new proposal id, or 0 when the item
// changed since the check read it.
func Dispute(ctx context.Context, tx *sql.Tx, p Proposal) (int64, error) {
	res, err := tx.ExecContext(ctx, `UPDATE items SET state = ? WHERE id = ? AND current_revision = ? AND state = ?`,
		StateDisputed, *p.ItemID, *p.BaseRevision, StateActive)
	if err != nil {
		return 0, fmt.Errorf("dispute item %d: %w", *p.ItemID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("dispute item %d: %w", *p.ItemID, err)
	}
	if n == 0 {
		return 0, nil
	}
	id, err := InsertProposal(ctx, tx, p)
	if err != nil {
		return 0, err
	}
	_, err = nextGeneration(ctx, tx)
	return id, err
}

// Disputed is the mark of one disputed item in the brief (§9.1): the anchor paths of its
// dispute and the time of the check that found them.
type Disputed struct {
	Anchors []string
	At      time.Time
}

// Disputes reads the mark of each disputed item, by item id: the newest pending dispute
// proposal of the item.
func Disputes(ctx context.Context, tx *sql.Tx) (map[int64]Disputed, error) {
	type row struct {
		item int64
		d    Disputed
	}
	rows, err := query(ctx, tx, `SELECT p.item_id, p.dispute, p.created FROM proposals p
		JOIN items i ON i.id = p.item_id
		WHERE i.state = ? AND p.status = ? AND json_array_length(p.dispute) > 0 ORDER BY p.id`,
		func(rows *sql.Rows) (row, error) {
			var r row
			err := rows.Scan(&r.item, listDest[string]{&r.d.Anchors}, timeDest{&r.d.At})
			return r, err
		}, StateDisputed, ProposalPending)
	if err != nil {
		return nil, fmt.Errorf("read disputes: %w", err)
	}
	m := make(map[int64]Disputed, len(rows))
	for _, r := range rows {
		m[r.item] = r.d
	}
	return m, nil
}
