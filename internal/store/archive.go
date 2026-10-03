package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// ArchiveAge is how long an item can go with no relevant session before it is an archive
// candidate (§9.2, D7).
const ArchiveAge = 90 * 24 * time.Hour

// ArchiveCandidate is an active item with no relevant session since Since (§9.2).
type ArchiveCandidate struct {
	ItemID       int64
	WorkspaceKey *string // nil for a global item
	// Since is the start of the current idle period: the newest of the last relevant session,
	// the last keep (brain archive --keep), and the time the item began to age.
	Since time.Time
}

// ArchiveCandidates reads the archive candidates whose idle period started before before, the
// oldest first, of every workspace (§9.2). A relevant session is an emitted receipt that holds
// a revision of the item [NEW rule], in the item's workspace, or in any workspace for a global
// item. A pinned item never ages (S29), so it is no candidate. An item begins to age at the
// revision that made it an active, unpinned item: an approval, a restore or an unpin. The
// caller checks the scope of each candidate (E4).
func ArchiveCandidates(ctx context.Context, tx *sql.Tx, before time.Time) ([]ArchiveCandidate, error) {
	var cs []ArchiveCandidate
	err := eachRow(ctx, tx, `WITH seen AS (SELECT json_extract(j.value, '$.item') AS item_id, d.workspace_key AS key,
			max(d.at) AS at FROM deliveries d, json_each(d.item_revisions) j WHERE d.result = ?1 GROUP BY 1, 2),
		idle AS (SELECT i.id, i.workspace_key, max(
			(SELECT max(r.created) FROM item_revisions r WHERE r.item_id = i.id AND r.state = ?2 AND r.pinned = 0
				AND NOT EXISTS (SELECT 1 FROM item_revisions p WHERE p.item_id = r.item_id
					AND p.revision = r.revision - 1 AND p.state = ?2 AND p.pinned = 0)),
			COALESCE(i.kept_at, ''),
			COALESCE((SELECT max(s.at) FROM seen s WHERE s.item_id = i.id
				AND (i.workspace_key IS NULL OR s.key = i.workspace_key)), '')) AS since
			FROM items i WHERE i.state = ?2 AND i.pinned = 0)
		SELECT id, workspace_key, since FROM idle WHERE since < ?3 ORDER BY since, id`,
		func(rows *sql.Rows) error {
			var c ArchiveCandidate
			if err := rows.Scan(&c.ItemID, &c.WorkspaceKey, timeDest{&c.Since}); err != nil {
				return err
			}
			cs = append(cs, c)
			return nil
		}, ResultEmitted, StateActive, timeArg(before))
	if err != nil {
		return nil, fmt.Errorf("read archive candidates: %w", err)
	}
	return cs, nil
}

// KeepItem sets the keep time of item id to at, so a new idle period starts there (brain
// archive --keep, §9.2). It writes no revision: the item and its text do not change.
func KeepItem(ctx context.Context, tx *sql.Tx, id int64, at time.Time) error {
	if _, err := tx.ExecContext(ctx, `UPDATE items SET kept_at = ? WHERE id = ?`, timeArg(at), id); err != nil {
		return fmt.Errorf("keep item %d: %w", id, err)
	}
	return nil
}

// ClosedWorkspace is a closed workspace key and the count of its active items (§9.4).
type ClosedWorkspace struct {
	Key, Label string
	Items      int
}

// ClosedWorkspaces reads the closed workspaces that hold active items, in key order (§9.4). It
// reads keys, labels and counts only, never item text.
func ClosedWorkspaces(ctx context.Context, tx *sql.Tx) ([]ClosedWorkspace, error) {
	var ws []ClosedWorkspace
	err := eachRow(ctx, tx, `SELECT w.key, w.label, count(*) FROM workspaces w JOIN items i ON i.workspace_key = w.key
		WHERE w.status = ? AND i.state = ? GROUP BY w.key ORDER BY w.key`, func(rows *sql.Rows) error {
		var w ClosedWorkspace
		if err := rows.Scan(&w.Key, &w.Label, &w.Items); err != nil {
			return err
		}
		ws = append(ws, w)
		return nil
	}, WorkspaceClosed, StateActive)
	if err != nil {
		return nil, fmt.Errorf("read closed workspaces: %w", err)
	}
	return ws, nil
}
