package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// ForgetPending reports whether an output generation still holds the text of an item with an
// open forget. The forget flow marks such a generation with outputs.holds_forgotten_text, and
// forget is done only when no generation holds the text. Until then the hook withholds.
func ForgetPending(ctx context.Context, tx *sql.Tx) (bool, error) {
	var pending bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM outputs WHERE holds_forgotten_text = 1)`).
		Scan(&pending)
	if err != nil {
		return false, fmt.Errorf("read forget state: %w", err)
	}
	return pending, nil
}

// itemRows is the WHERE clause of the rows of item ?1 in each table of ItemTextFields: the
// item, its revisions, its proposals and the script proposals of its claim (scriptsOfItem), the applied edits whose file held it at any revision
// (AppliedEditsWithItem), its claim, the rejections of its claim in its scope, and the prompt
// index rows that link a proposal of the item, and its tools and automations rows. Forget finds them by id, never by a text search.
// The claim key is shared by the scopes: while another item, or a candidate in another scope,
// uses the claim, that scope keeps its copy of the claim and its key text.
// The claims rows also hold the claims whose same_as link names the claim of the item: a
// restatement (#312) or a merge (#279) keeps the key of the linked claim with no candidate of
// its own. Such a claim goes with the item while no item has it, no candidate in another scope
// has it, and no candidate in another scope has evidence that it brought (#327). A candidate of
// its own in the scope of the item does not keep it: the same telling after the forget gets a
// new claim id, not the old candidate with its evidence (#389).
var itemRows = map[string]string{
	"items":          `id = ?1`,
	"item_revisions": `item_id = ?1`,
	"proposals":      scriptsOfItem,
	"applied_edits":  appliedEditsWithItem,
	"claims": `(id = (SELECT claim_id FROM items WHERE id = ?1)
		AND NOT EXISTS (SELECT 1 FROM items o WHERE o.claim_id = claims.id AND o.id != ?1)
		AND NOT EXISTS (SELECT 1 FROM candidates c JOIN items i ON i.id = ?1
			WHERE c.claim_id = claims.id AND COALESCE(c.workspace_key, '') != COALESCE(i.workspace_key, '')))
		OR (same_as = (SELECT claim_id FROM items WHERE id = ?1)
		AND NOT EXISTS (SELECT 1 FROM items o WHERE o.claim_id = claims.id)
		AND NOT EXISTS (SELECT 1 FROM candidates c JOIN items i ON i.id = ?1
			WHERE c.claim_id = claims.id AND COALESCE(c.workspace_key, '') != COALESCE(i.workspace_key, ''))
		AND NOT EXISTS (SELECT 1 FROM evidence_links l JOIN candidates c ON c.id = l.candidate_id
			JOIN items i ON i.id = ?1
			WHERE l.via_claim = claims.id AND COALESCE(c.workspace_key, '') != COALESCE(i.workspace_key, '')))`,
	"rejections": `claim_id = (SELECT claim_id FROM items WHERE id = ?1)
		AND COALESCE(workspace_key, '') = (SELECT COALESCE(workspace_key, '') FROM items WHERE id = ?1)`,
	"prompt_index": promptRowsOfItem,
	"tools":        `id = ?1`,
	"automations":  `id = ?1`,
	// The runs of the automation item, and of each automation of the tool item.
	"automation_runs": `automation_id = ?1 OR automation_id IN (SELECT id FROM automations WHERE tool_id = ?1)`,
}

// promptRowsOfItem is the WHERE clause of the prompt index rows that link a proposal of item ?1.
const promptRowsOfItem = `EXISTS (SELECT 1 FROM json_each(proposal_ids) j JOIN proposals p ON p.id = j.value
	WHERE p.item_id = ?1)`

// blanks is the value that forget writes into a field of ItemTextFields; any other field gets
// "". A nullable field gets NULL, and a list field an empty list.
var blanks = map[Field]string{
	{"items", "anchors"}:                `'[]'`,
	{"proposals", "dispute"}:            `'[]'`,
	{"proposals", "change_request"}:     `NULL`,
	{"applied_edits", "content_before"}: `NULL`,
	{"applied_edits", "content_after"}:  `NULL`,
	{"claims", "situation"}:             `NULL`,
	{"claims", "rule"}:                  `NULL`,
	{"claims", "draft"}:                 `NULL`,
	{"automation_runs", "cause"}:        `NULL`,
}

// sideRows is the WHERE clause of the applied edits whose content field held item ?1: the
// side whose revisions list holds it. The forget build writes a row with the item in its
// revisions_before and new bytes without the item in content_after; that field holds no text
// of the item, so ItemTextLeft checks each side on its own.
var sideRows = map[Field]string{
	{"applied_edits", "content_before"}: `EXISTS (SELECT 1 FROM json_each(revisions_before) WHERE json_extract(value, '$.item') = ?1)`,
	{"applied_edits", "content_after"}:  `EXISTS (SELECT 1 FROM json_each(revisions_after) WHERE json_extract(value, '$.item') = ?1)`,
}

func blank(f Field) string {
	if b, ok := blanks[f]; ok {
		return b
	}
	return `''`
}

// rowsOf is the WHERE clause of the rows of item in the table of f.
func rowsOf(f Field) string {
	w, ok := itemRows[f.Table]
	if !ok {
		panic(fmt.Sprintf("store: forget has no rows of an item in %s", f.Table))
	}
	return w
}

// EraseItem removes the text of item and of its derivatives from every field in
// ItemTextFields, and supersedes its pending proposals, so no blank proposal waits in the
// queue. The rows and their ids stay, except the prompt index rows and the suggestions
// of its proposals: they go with their hashes, so no prompt matches the item again. Its
// outcomes go too. It raises the capture
// generation of the sessions of those rows, so a pane read or a hook record of the same session
// that collected its text before the forget stores nothing, as after brain forget --session.
// now is the decision time of those proposals.
func EraseItem(ctx context.Context, tx *sql.Tx, item int64, now time.Time) error {
	for _, q := range []string{
		`UPDATE sessions SET generation = generation + 1
			WHERE id IN (SELECT session_id FROM prompt_index WHERE ` + promptRowsOfItem + `)`,
		`DELETE FROM prompt_index WHERE ` + promptRowsOfItem,
		// A suggestion of a proposal of the item holds a prompt hash (E14-S3).
		`DELETE FROM suggestions WHERE proposal_id IN (SELECT id FROM proposals WHERE ` + scriptsOfItem + `)`,
		// The outcomes of the item hold ids only, but they name the item (E15-S1).
		`DELETE FROM outcomes WHERE item_id = ?1`,
	} {
		if _, err := tx.ExecContext(ctx, q, item); err != nil {
			return fmt.Errorf("forget prompt index rows of item %d: %w", item, err)
		}
	}
	for _, f := range ItemTextFields() {
		// The table and column names come from ItemTextFields, not from input.
		q := fmt.Sprintf(`UPDATE %s SET %s = %s WHERE %s`, f.Table, f.Column, blank(f), rowsOf(f))
		if _, err := tx.ExecContext(ctx, q, item); err != nil {
			return fmt.Errorf("erase %s.%s of item %d: %w", f.Table, f.Column, item, err)
		}
	}
	_, err := tx.ExecContext(ctx, `UPDATE proposals SET status = ?2, decided = ?3 WHERE `+scriptsOfItem+` AND status = ?4`,
		item, ProposalSuperseded, timeArg(now), ProposalPending)
	if err != nil {
		return fmt.Errorf("supersede proposals of item %d: %w", item, err)
	}
	return discardChanges(ctx, tx, `SELECT id FROM proposals WHERE item_id = ?1`, `SELECT ?1`, item, now)
}

// ItemTextLeft returns the fields of ItemTextFields that still hold text of item or of its
// derivatives, in list order. Forget is done in the store when it is empty.
func ItemTextLeft(ctx context.Context, tx *sql.Tx, item int64) ([]Field, error) {
	var left []Field
	for _, f := range ItemTextFields() {
		var has bool
		w, ok := sideRows[f]
		if !ok {
			w = rowsOf(f)
		}
		q := fmt.Sprintf(`SELECT EXISTS (SELECT 1 FROM %s WHERE (%s) AND %s IS NOT %s)`, f.Table, w, f.Column, blank(f))
		if err := tx.QueryRowContext(ctx, q, item).Scan(&has); err != nil {
			return nil, fmt.Errorf("read %s.%s of item %d: %w", f.Table, f.Column, item, err)
		}
		if has {
			left = append(left, f)
		}
	}
	return left, nil
}

// ClaimCopies reads the other items with the same claim id as item, in id order: the copies
// of the claim in other scopes, which forget lists and keeps.
func ClaimCopies(ctx context.Context, tx *sql.Tx, item int64) ([]Item, error) {
	rows, err := tx.QueryContext(ctx, `SELECT c.id FROM items c JOIN items i ON c.claim_id = i.claim_id
		WHERE i.id = ? AND c.id != i.id ORDER BY c.id`, item)
	if err != nil {
		return nil, fmt.Errorf("read copies of item %d: %w", item, err)
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("read copies of item %d: %w", item, err)
		}
		ids = append(ids, id)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("read copies of item %d: %w", item, err)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read copies of item %d: %w", item, err)
	}
	var its []Item
	for _, id := range ids {
		it, err := ItemByID(ctx, tx, id)
		if err != nil {
			return nil, err
		}
		its = append(its, it)
	}
	return its, nil
}

// MarkForgotten sets outputs.holds_forgotten_text on each of generations: their folders hold
// the text of an item that forget removed from the store.
func MarkForgotten(ctx context.Context, tx *sql.Tx, generations []int64) error {
	_, err := tx.ExecContext(ctx, `UPDATE outputs SET holds_forgotten_text = 1
		WHERE generation IN (SELECT value FROM json_each(?))`, jsonList[int64](generations))
	if err != nil {
		return fmt.Errorf("mark output generations %v: %w", generations, err)
	}
	return nil
}

// ClearForgotten clears outputs.holds_forgotten_text on every generation that is not in
// kept, the generation folders that are still on disk: a deleted folder holds no text.
func ClearForgotten(ctx context.Context, tx *sql.Tx, kept []int64) error {
	_, err := tx.ExecContext(ctx, `UPDATE outputs SET holds_forgotten_text = 0
		WHERE holds_forgotten_text = 1 AND generation NOT IN (SELECT value FROM json_each(?))`, jsonList[int64](kept))
	if err != nil {
		return fmt.Errorf("clear forget marks: %w", err)
	}
	return nil
}
