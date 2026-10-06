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

// AnchoredItems reads the active and disputed workspace items that have anchors, in id order:
// the items of workspace key, or of every workspace when key is "".
func AnchoredItems(ctx context.Context, tx *sql.Tx, key string) ([]Item, error) {
	items, err := query(ctx, tx, `SELECT id, claim_id, current_revision, kind, scope, workspace_key, state,
		pinned, anchors, created FROM items
		WHERE state IN (?1, ?2) AND scope = ?3 AND json_array_length(anchors) > 0 AND (?4 = '' OR workspace_key = ?4)
		ORDER BY id`,
		func(rows *sql.Rows) (Item, error) {
			var it Item
			err := rows.Scan(&it.ID, &it.ClaimID, &it.CurrentRevision, &it.Kind, &it.Scope, &it.WorkspaceKey,
				&it.State, &it.Pinned, listDest[Anchor]{&it.Anchors}, timeDest{&it.Created})
			return it, err
		}, StateActive, StateDisputed, ScopeWorkspace, key)
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

// RebaseAnchors stores anchors as the anchors of item id when the item is still active at
// revision rev: a normal edit of an anchor file gives the check a new baseline (§9.1, #252).
func RebaseAnchors(ctx context.Context, tx *sql.Tx, id, rev int64, anchors []Anchor) error {
	_, err := tx.ExecContext(ctx, `UPDATE items SET anchors = ? WHERE id = ? AND current_revision = ? AND state = ?`,
		jsonList[Anchor](anchors), id, rev, StateActive)
	if err != nil {
		return fmt.Errorf("rebase the anchors of item %d: %w", id, err)
	}
	return nil
}

// Undispute makes item id active again with anchors when it is still disputed at revision rev,
// and closes its pending disputes as superseded at now: an older build disputed it for a
// normal edit of an anchor file, and every anchor path is still there (#252). An item with a
// pending dispute of a move or a delete stays disputed until you decide (§9.1), also when the
// file is back: its anchor diff has a "(moved)" or "(deleted)" line. The item keeps its
// revision. It records a new output generation, so the next build drops the mark. It reports
// whether the item changed.
func Undispute(ctx context.Context, tx *sql.Tx, id, rev int64, anchors []Anchor, now time.Time) (bool, error) {
	res, err := tx.ExecContext(ctx, `UPDATE items SET state = ?, anchors = ? WHERE id = ? AND current_revision = ? AND state = ?
		AND NOT EXISTS (SELECT 1 FROM proposals p WHERE p.item_id = items.id AND p.status = ?
			AND json_array_length(p.dispute) > 0 AND (p.diff LIKE '%(moved)%' OR p.diff LIKE '%(deleted)%'))`,
		StateActive, jsonList[Anchor](anchors), id, rev, StateDisputed, ProposalPending)
	if err != nil {
		return false, fmt.Errorf("undispute item %d: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil || n == 0 {
		return false, err
	}
	if err := SupersedeDisputes(ctx, tx, id, now); err != nil {
		return false, err
	}
	_, err = nextGeneration(ctx, tx)
	return err == nil, err
}

// Disputed is the mark of one disputed item in the brief (§9.1): the anchor paths of its
// dispute, the anchor diff of its proposal and the time of the check that found them.
type Disputed struct {
	Anchors []string
	Diff    string
	At      time.Time
}

// Disputes reads the mark of each disputed item, by item id: the newest pending dispute
// proposal of the item.
func Disputes(ctx context.Context, tx *sql.Tx) (map[int64]Disputed, error) {
	type row struct {
		item int64
		d    Disputed
	}
	rows, err := query(ctx, tx, `SELECT p.item_id, p.dispute, p.diff, p.created FROM proposals p
		JOIN items i ON i.id = p.item_id
		WHERE i.state = ? AND p.status = ? AND json_array_length(p.dispute) > 0 ORDER BY p.id`,
		func(rows *sql.Rows) (row, error) {
			var r row
			err := rows.Scan(&r.item, listDest[string]{&r.d.Anchors}, &r.d.Diff, timeDest{&r.d.At})
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

// SupersedeDisputes closes the pending dispute proposals of item id with status superseded at
// now: an approval of another proposal of the item wrote a new revision and fresh anchors, so
// the dispute no longer has a base. Superseded is not your decision (§7.4), as in forget.
func SupersedeDisputes(ctx context.Context, tx *sql.Tx, id int64, now time.Time) error {
	_, err := tx.ExecContext(ctx, `UPDATE proposals SET status = ?, decided = ?
		WHERE item_id = ? AND status = ? AND json_array_length(dispute) > 0`,
		ProposalSuperseded, timeArg(now), id, ProposalPending)
	if err != nil {
		return fmt.Errorf("supersede the disputes of item %d: %w", id, err)
	}
	return nil
}
