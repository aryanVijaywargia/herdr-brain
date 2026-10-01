package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// PromptEntry is one row of prompt_index (§11).
type PromptEntry struct {
	ID            int64
	Hash          string // the exact fingerprint of the full prompt (internal/fingerprint)
	Text          string // the secret-filtered prompt, ≤ 500 characters, for close matches only (E14-S2)
	WorkspaceKey  string // "" when the workspace has no Brain key
	SessionID     int64
	ObservationID int64
	Generation    int64 // the capture generation of the session at the write
	ProposalIDs   []int64
	At            time.Time
}

// IndexPrompt adds the prompt index row of prompt observation obs, with the exact fingerprint
// hash of the full prompt and its filtered text. The caller writes it in the transaction that
// stored obs, after the learning and capture generation checks, so a forget or a learn off
// before the commit leaves no row (§6.7). The row links the ready proposals that hash matches
// now: a forgotten or rejected proposal is no longer ready, so it gets no link.
func IndexPrompt(ctx context.Context, tx *sql.Tx, obs int64, hash, text string) error {
	var key string
	err := tx.QueryRowContext(ctx, `SELECT COALESCE(s.workspace_key, '') FROM observations o
		JOIN segments s ON s.id = o.segment_id WHERE o.id = ?`, obs).Scan(&key)
	if err != nil {
		return fmt.Errorf("read workspace of observation %d: %w", obs, err)
	}
	ids, err := MatchPrompt(ctx, tx, hash, key)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO prompt_index
		(hash, text, workspace_key, session_id, observation_id, generation, proposal_ids, at)
		SELECT ?1, ?2, s.workspace_key, s.session_id, o.id, se.generation, ?3, o.at FROM observations o
		JOIN segments s ON s.id = o.segment_id JOIN sessions se ON se.id = s.session_id WHERE o.id = ?4`,
		hash, text, jsonList[int64](ids), obs)
	if err != nil {
		return fmt.Errorf("index prompt of observation %d: %w", obs, err)
	}
	return nil
}

// MatchPrompt returns the ids of the ready proposals that prompt fingerprint hash matches
// exactly, in the workspace with key workspaceKey ("" for none), by id (§5.5). A row of the
// index with hash links them: the trigger prompts of a proposal and the later prompts that
// matched it. A proposal applies when it is global or its item is in that workspace, and its
// item is still global or in that workspace: another proposal can move the item (the E4 scope
// check). It makes no model call.
func MatchPrompt(ctx context.Context, tx *sql.Tx, hash, workspaceKey string) ([]int64, error) {
	return query(ctx, tx, `SELECT DISTINCT p.id FROM prompt_index x, json_each(x.proposal_ids) j
		JOIN proposals p ON p.id = j.value JOIN items i ON i.id = p.item_id
		WHERE x.hash = ?1 AND p.status = ?2 AND (p.scope = ?3 OR i.workspace_key = ?4)
		AND (i.workspace_key IS NULL OR i.workspace_key = ?4) ORDER BY p.id`,
		scanID, hash, ProposalPending, ScopeGlobal, workspaceKey)
}

// candidatePrompts is the WHERE clause of the prompt index rows of the evidence of candidate
// ?1 that is still in the store, and of the hook records that enrich it: a pane read can show
// only a part of a long prompt, and its hook record holds the full prompt (S35).
const candidatePrompts = `observation_id IN (SELECT o.id FROM evidence_links l
	JOIN observations o ON o.id = l.observation_id AND o.capture_identity = l.capture_identity
	WHERE l.candidate_id = ?1 UNION SELECT o.enriched_by FROM evidence_links l
	JOIN observations o ON o.id = l.observation_id AND o.capture_identity = l.capture_identity
	WHERE l.candidate_id = ?1 AND o.enriched_by IS NOT NULL)`

// triggerPrompts returns the fingerprints of the prompts among the evidence of candidate id,
// each once, sorted: the trigger prompts of its proposal (§11). They come from the index rows,
// so each is the fingerprint of the full prompt, not of its cut excerpt.
func triggerPrompts(ctx context.Context, tx *sql.Tx, id int64) ([]string, error) {
	hs, err := query(ctx, tx, `SELECT DISTINCT hash FROM prompt_index WHERE `+candidatePrompts+` ORDER BY hash`,
		func(rows *sql.Rows) (string, error) {
			var h string
			err := rows.Scan(&h)
			return h, err
		}, id)
	// A proposal with no prompt evidence stores an empty list.
	return append([]string{}, hs...), err
}

// linkPrompts adds proposal to the prompt index rows of the evidence of candidate id: the
// proposal used them.
func linkPrompts(ctx context.Context, tx *sql.Tx, id, proposal int64) error {
	_, err := tx.ExecContext(ctx, `UPDATE prompt_index SET proposal_ids = json_insert(proposal_ids, '$[#]', ?2)
		WHERE `+candidatePrompts, id, proposal)
	if err != nil {
		return fmt.Errorf("link prompts of candidate %d: %w", id, err)
	}
	return nil
}

// SessionPromptProposals returns the ids of the proposals that used the prompt index rows of
// session sessionID, by id: brain forget --session lists them first (S68).
func SessionPromptProposals(ctx context.Context, tx *sql.Tx, sessionID int64) ([]int64, error) {
	return query(ctx, tx, `SELECT DISTINCT j.value FROM prompt_index x, json_each(x.proposal_ids) j
		WHERE x.session_id = ? ORDER BY j.value`, scanID, sessionID)
}

// PromptEntries returns every prompt index row, by id.
func PromptEntries(ctx context.Context, tx *sql.Tx) ([]PromptEntry, error) {
	return query(ctx, tx, `SELECT id, hash, text, COALESCE(workspace_key, ''), session_id, observation_id, generation,
		proposal_ids, at FROM prompt_index ORDER BY id`,
		func(rows *sql.Rows) (PromptEntry, error) {
			var e PromptEntry
			err := rows.Scan(&e.ID, &e.Hash, &e.Text, &e.WorkspaceKey, &e.SessionID, &e.ObservationID, &e.Generation,
				listDest[int64]{&e.ProposalIDs}, timeDest{&e.At})
			return e, err
		})
}

func scanID(rows *sql.Rows) (int64, error) {
	var id int64
	err := rows.Scan(&id)
	return id, err
}
