package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"time"
)

// MergeItem is one item that the merge pass may merge (§9.5): an active plain item of a brief
// part, at its current revision.
type MergeItem struct {
	ID, Revision int64
	Kind         Kind
	Text         string
	// Permission is true when the claim of the item touches a permission or a safety rule.
	Permission bool
}

// MergeItems returns the items of the part of workspace key ("" for the global part) that the
// merge pass may merge, by id (§9.5 input): the active items that are not a skill, a tool or an
// automation, with no waiting proposal, in no waiting merge proposal, and with no exception
// link and no conflict mark at either end.
func MergeItems(ctx context.Context, tx *sql.Tx, key string) ([]MergeItem, error) {
	return query(ctx, tx, `SELECT i.id, i.current_revision, i.kind, r.text, COALESCE(k.touches_permission, 0)
		FROM items i JOIN item_revisions r ON r.item_id = i.id AND r.revision = i.current_revision
		LEFT JOIN claims k ON k.id = i.claim_id
		WHERE i.state = ?2 AND i.kind NOT IN (?3, ?4, ?5) AND COALESCE(i.workspace_key, '') = ?1
			AND r.exception_to_item IS NULL AND json_array_length(r.conflicts_with) = 0
			AND NOT EXISTS (SELECT 1 FROM proposals p WHERE p.status = ?6
				AND (p.item_id = i.id OR EXISTS (SELECT 1 FROM json_each(p.merge) WHERE json_extract(value, '$.item') = i.id)))
			AND NOT EXISTS (SELECT 1 FROM items o JOIN item_revisions q ON q.item_id = o.id AND q.revision = o.current_revision
				WHERE o.state IN (?2, ?7) AND (q.exception_to_item = i.id
					OR EXISTS (SELECT 1 FROM json_each(q.conflicts_with) WHERE value = i.id)))
		ORDER BY i.id`,
		func(rows *sql.Rows) (MergeItem, error) {
			var m MergeItem
			err := rows.Scan(&m.ID, &m.Revision, &m.Kind, &m.Text, &m.Permission)
			return m, err
		}, key, StateActive, KindSkill, KindTool, KindAutomation, ProposalPending, StateDisputed)
}

// mergesOf is the FROM and WHERE part of the merge proposals with status ?2 of the part of
// workspace key ?1.
const mergesOf = `FROM proposals p JOIN items i ON i.id = p.item_id
	WHERE p.status = ?2 AND json_array_length(p.merge) > 0 AND COALESCE(i.workspace_key, '') = ?1`

// MergeWaits reports whether a merge proposal of the part of workspace key waits for review.
func MergeWaits(ctx context.Context, tx *sql.Tx, key string) (bool, error) {
	var waits bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 `+mergesOf+`)`, key, ProposalPending).Scan(&waits); err != nil {
		return false, fmt.Errorf("read the merge proposals of part %q: %w", key, err)
	}
	return waits, nil
}

// RejectedMerges returns the item id set of each rejected merge proposal of the part of
// workspace key, each set sorted (§9.5 reject).
func RejectedMerges(ctx context.Context, tx *sql.Tx, key string) ([][]int64, error) {
	return query(ctx, tx, `SELECT p.merge `+mergesOf, func(rows *sql.Rows) ([]int64, error) {
		var refs []RevisionRef
		if err := rows.Scan(listDest[RevisionRef]{&refs}); err != nil {
			return nil, err
		}
		ids := make([]int64, len(refs))
		for i, r := range refs {
			ids[i] = r.Item
		}
		slices.Sort(ids)
		return ids, nil
	}, key, ProposalRejected)
}

// MergeHash returns the hash of the part of workspace key at its last merge pass, or "" when
// it had none.
func MergeHash(ctx context.Context, tx *sql.Tx, key string) (string, error) {
	var sum string
	err := tx.QueryRowContext(ctx, `SELECT sha256 FROM merge_passes WHERE workspace_key = ?`, key).Scan(&sum)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("read the merge pass of part %q: %w", key, err)
	}
	return sum, nil
}

// SetMergeHash records sum as the hash of the part of workspace key at its merge pass at now.
func SetMergeHash(ctx context.Context, tx *sql.Tx, key, sum string, now time.Time) error {
	if _, err := tx.ExecContext(ctx, `INSERT INTO merge_passes (workspace_key, sha256, at) VALUES (?, ?, ?)
		ON CONFLICT (workspace_key) DO UPDATE SET sha256 = excluded.sha256, at = excluded.at`,
		key, sum, timeArg(now)); err != nil {
		return fmt.Errorf("record the merge pass of part %q: %w", key, err)
	}
	return nil
}

// RecordMergeCall records one extractor call of the merge pass of the part of workspace key at
// now, under the daily call cap (§7.3 caps), and returns its id.
func RecordMergeCall(ctx context.Context, tx *sql.Tx, key string, now time.Time) (int64, error) {
	return insertRow(ctx, tx, "merge call", `INSERT INTO extraction_calls (workspace_key, at) VALUES (?, ?)`, key, timeArg(now))
}

// UncountMergeCall takes back merge call id: the CLI refused it at a usage or rate limit, so it
// ran no model (#377).
func UncountMergeCall(ctx context.Context, tx *sql.Tx, id int64) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM extraction_calls WHERE id = ?`, id); err != nil {
		return fmt.Errorf("take back merge call %d: %w", id, err)
	}
	return nil
}

// LinkMerged links each claim of retired, the claims of the items that the approval of merge
// proposal proposal retires, to claim kept as a confirmed same_as link (§9.5 approve): a later
// claim with the key of a retired item gets the claim id of the kept item (ClaimID). As in
// ConfirmLink, a link points at the claim id that kept has, and the confirmed links that
// pointed at a retired claim point there too, so a key never needs two hops. The claim of a
// brain remember item has no row yet: the link adds it, as SameAs does. Each claim keeps its
// link before the merge in merge_links, for UnlinkMerged.
func LinkMerged(ctx context.Context, tx *sql.Tx, proposal int64, kept string, retired []string) error {
	for _, id := range append([]string{kept}, retired...) {
		if _, err := tx.ExecContext(ctx, `INSERT INTO claims (id, created) SELECT claim_id, created FROM items
			WHERE claim_id = ? ORDER BY id LIMIT 1 ON CONFLICT (id) DO NOTHING`, id); err != nil {
			return fmt.Errorf("add claim %s of its item: %w", id, err)
		}
	}
	k, err := ClaimByID(ctx, tx, kept)
	if err != nil {
		return err
	}
	to := kept
	if k.SameAsStatus == SameAsConfirmed {
		to = k.SameAs
	}
	for _, id := range retired {
		if id == to {
			continue
		}
		for _, q := range []string{
			`INSERT INTO merge_links (proposal_id, claim_id, same_as, same_as_status)
				SELECT ?1, id, same_as, same_as_status FROM claims
				WHERE id = ?2 OR same_as = ?2 AND same_as_status = ?4 AND id != ?3 ON CONFLICT DO NOTHING`,
			`UPDATE claims SET same_as = ?3, same_as_status = ?4
				WHERE id = ?2 OR same_as = ?2 AND same_as_status = ?4 AND id != ?3`,
		} {
			if _, err := tx.ExecContext(ctx, q, proposal, id, to, SameAsConfirmed); err != nil {
				return fmt.Errorf("link claim %s to %s: %w", id, to, err)
			}
		}
	}
	return nil
}

// UnlinkMerged puts back the same_as links that the approval of merge proposal proposal set
// (LinkMerged), for the undo of the merge (§9.5 undo).
func UnlinkMerged(ctx context.Context, tx *sql.Tx, proposal int64) error {
	for _, q := range []string{
		`UPDATE claims SET (same_as, same_as_status) = (SELECT l.same_as, l.same_as_status FROM merge_links l
			WHERE l.proposal_id = ?1 AND l.claim_id = claims.id)
			WHERE id IN (SELECT claim_id FROM merge_links WHERE proposal_id = ?1)`,
		`DELETE FROM merge_links WHERE proposal_id = ?1`,
	} {
		if _, err := tx.ExecContext(ctx, q, proposal); err != nil {
			return fmt.Errorf("remove the links of merge proposal %d: %w", proposal, err)
		}
	}
	return nil
}
