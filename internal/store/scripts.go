package store

import (
	"context"
	"database/sql"
	"fmt"
)

// ScriptTarget is the target of a script proposal (§10.4 suggestions, E17-S8) [NEW]: Brain
// installs no script, so the target is you. A script proposal has no item: §11 has no script
// kind, and approve records your decision only.
const ScriptTarget = "a script that you install yourself"

// Script reports whether p is a script proposal.
func (p Proposal) Script() bool { return p.ItemID == nil && p.Target == ScriptTarget }

// scriptsOfItem is the WHERE clause of the proposals of item ?1 and of the script proposals of
// its claim in its scope (E17-S8): a script proposal comes with the tool draft of the same
// command chain, so forget of that item erases the script text too (§6.7). It does not test the
// target, as forget erases the target first: a script is the only proposal of a candidate with
// no item.
const scriptsOfItem = `(item_id = ?1 OR item_id IS NULL AND candidate_id IN (
	SELECT c.id FROM candidates c JOIN items i ON i.id = ?1
	WHERE c.claim_id = i.claim_id AND COALESCE(c.workspace_key, '') = COALESCE(i.workspace_key, '')))`

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
