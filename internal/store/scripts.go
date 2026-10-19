package store

import (
	"context"
	"database/sql"
	"fmt"
)

// ScriptTarget is the target of a script proposal (E17-S8): Brain installs no script, so
// the target is you. A script proposal has no item: the data model has no script kind, and
// approve records your decision only.
const ScriptTarget = "a script that you install yourself"

// Script reports whether p is a script proposal.
func (p Proposal) Script() bool { return p.ItemID == nil && p.Target == ScriptTarget }

// scriptsOfItem is the WHERE clause of the proposals of item ?1 and of the script proposals of
// its claim in its scope (E17-S8): a script proposal comes with the tool draft of the same
// command chain, so forget of that item erases the script text too. It does not test the
// target, as forget erases the target first: a script is the only proposal of a candidate with
// no item. It holds the merge proposals that list the item too: their text and diff hold
// its text, and forget of the item removes a waiting one from the queue.
const scriptsOfItem = `(item_id = ?1 OR item_id IS NULL AND candidate_id IN (
	SELECT c.id FROM candidates c JOIN items i ON i.id = ?1
	WHERE c.claim_id = i.claim_id AND COALESCE(c.workspace_key, '') = COALESCE(i.workspace_key, ''))
	OR EXISTS (SELECT 1 FROM json_each(merge) WHERE json_extract(value, '$.item') = ?1))`

// ScriptOffered reports whether candidate id had a script proposal, in any status, or an
// approved proposal: then you picked already, and Brain offers no new script.
func ScriptOffered(ctx context.Context, tx *sql.Tx, id int64) (bool, error) {
	var offered bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM proposals WHERE candidate_id = ?1
		AND (item_id IS NULL AND target = ?2 OR status = ?3))`, id, ScriptTarget, ProposalApproved).Scan(&offered)
	if err != nil {
		return false, fmt.Errorf("read script proposals of candidate %d: %w", id, err)
	}
	return offered, nil
}

// dropScripts erases the script proposals of candidate id when it has no evidence left, as
// recount then deletes it: their text holds the commands of that evidence, and without
// the candidate no forget and no scope check could find them. They lose the candidate id too,
// so a new candidate that gets the same id never shows them. A pending one is superseded.
func dropScripts(ctx context.Context, tx *sql.Tx, id int64) error {
	_, err := tx.ExecContext(ctx, `UPDATE proposals SET text = '', target = '', diff = '', expected_benefit = '',
		candidate_id = NULL, status = CASE status WHEN ?2 THEN ?3 ELSE status END
		WHERE candidate_id = ?1 AND item_id IS NULL
		AND NOT EXISTS (SELECT 1 FROM evidence_links WHERE candidate_id = ?1)`, id, ProposalPending, ProposalSuperseded)
	if err != nil {
		return fmt.Errorf("drop script proposals of candidate %d: %w", id, err)
	}
	return nil
}
