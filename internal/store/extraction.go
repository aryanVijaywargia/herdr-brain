package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ExtractorCLI is the official CLI that runs the extractor (§7.3, D4).
type ExtractorCLI string

// The extractor CLIs.
const (
	ExtractorClaude ExtractorCLI = "claude"
	ExtractorCodex  ExtractorCLI = "codex"
)

// ExtractorSetting is one row of extractor_settings. WorkspaceKey "" is the store row: it
// holds the CLI and the model, and its switch applies to every workspace. E13-S4 writes it
// (brain setup, brain extract off|on).
type ExtractorSetting struct {
	WorkspaceKey string
	On           bool         // the extraction switch
	CLI          ExtractorCLI // "" when none is chosen; only on the store row
	Model        string       // "" for the default model of the CLI; only on the store row
	Changed      time.Time
}

// SetExtractor records s. It replaces the earlier setting of the same workspace key.
func SetExtractor(ctx context.Context, tx *sql.Tx, s ExtractorSetting) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO extractor_settings (workspace_key, extraction, cli, model, changed)
		VALUES (?, ?, ?, ?, ?) ON CONFLICT (workspace_key) DO UPDATE SET extraction = excluded.extraction,
		cli = excluded.cli, model = excluded.model, changed = excluded.changed`,
		s.WorkspaceKey, onOff(s.On), nullText(string(s.CLI)), nullText(s.Model), timeArg(s.Changed))
	if err != nil {
		return fmt.Errorf("write extractor setting: %w", err)
	}
	return nil
}

// Extractor returns the store row of the extractor settings: extraction on and no CLI when
// there is none.
func Extractor(ctx context.Context, tx *sql.Tx) (ExtractorSetting, error) {
	s := ExtractorSetting{On: true}
	var extraction string
	err := tx.QueryRowContext(ctx, `SELECT extraction, COALESCE(cli, ''), COALESCE(model, ''), changed
		FROM extractor_settings WHERE workspace_key = ''`).Scan(&extraction, &s.CLI, &s.Model, timeDest{&s.Changed})
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return s, nil
	case err != nil:
		return ExtractorSetting{}, fmt.Errorf("read extractor settings: %w", err)
	}
	s.On = extraction == "on"
	return s, nil
}

// ExtractionOn reports whether extraction is on for the workspace with key workspaceKey: no
// off switch of the store or of the workspace applies. Extraction is on by default (§7.3 D9).
func ExtractionOn(ctx context.Context, tx *sql.Tx, workspaceKey string) (bool, error) {
	var offs int
	err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM extractor_settings WHERE extraction = 'off'
		AND workspace_key IN ('', ?)`, workspaceKey).Scan(&offs)
	if err != nil {
		return false, fmt.Errorf("read extractor settings: %w", err)
	}
	return offs == 0, nil
}

// BatchStatus is the status of an extraction batch (§11).
type BatchStatus string

// The batch statuses.
const (
	BatchActive    BatchStatus = "active"    // the extractor call runs, or a crash stopped it
	BatchCommitted BatchStatus = "committed" // its result and the new watermark are stored
	BatchEmpty     BatchStatus = "empty"     // a valid empty result, or no signal: the watermark moved
	BatchFailed    BatchStatus = "failed"    // the call failed: the watermark stays
	BatchInvalid   BatchStatus = "invalid"   // the response was invalid: the watermark stays
	BatchDiscarded BatchStatus = "discarded" // a forget, a switch or a newer batch made the result stale
)

// Batch is one row of extraction_batches. Its stable id is (SessionID, WorkspaceKey,
// FirstCapture, LastCapture) (§7.3); ID is the row id.
type Batch struct {
	ID              int64
	SessionID       int64
	WorkspaceKey    string
	FirstCapture    string
	LastCapture     string
	StartWatermark  int64 // the observation id of the watermark at the start; 0 for none
	LastObservation int64 // the observation id of LastCapture
	Status          BatchStatus
	ModelCalls      int
	Reason          string // why it failed, was invalid or was discarded; no model text
	At              time.Time
}

// Done reports whether the batch is committed or empty: a retry adds nothing (§7.3).
func (b Batch) Done() bool { return b.Status == BatchCommitted || b.Status == BatchEmpty }

const batchColumns = `id, session_id, workspace_key, first_capture, last_capture, start_watermark, last_observation,
	status, model_calls, reason, at`

func scanBatch(rows interface{ Scan(...any) error }) (Batch, error) {
	var b Batch
	err := rows.Scan(&b.ID, &b.SessionID, &b.WorkspaceKey, &b.FirstCapture, &b.LastCapture, &b.StartWatermark,
		&b.LastObservation, &b.Status, &b.ModelCalls, &b.Reason, timeDest{&b.At})
	return b, err
}

// FindBatch returns the batch with the stable id of b, or sql.ErrNoRows.
func FindBatch(ctx context.Context, tx *sql.Tx, b Batch) (Batch, error) {
	return scanBatch(tx.QueryRowContext(ctx, `SELECT `+batchColumns+` FROM extraction_batches
		WHERE session_id = ? AND workspace_key = ? AND first_capture = ? AND last_capture = ?`,
		b.SessionID, b.WorkspaceKey, b.FirstCapture, b.LastCapture))
}

// BatchByID returns the batch with row id, or sql.ErrNoRows.
func BatchByID(ctx context.Context, tx *sql.Tx, id int64) (Batch, error) {
	return scanBatch(tx.QueryRowContext(ctx, `SELECT `+batchColumns+` FROM extraction_batches WHERE id = ?`, id))
}

// Batches returns every batch, by id.
func Batches(ctx context.Context, tx *sql.Tx) ([]Batch, error) {
	return query(ctx, tx, `SELECT `+batchColumns+` FROM extraction_batches ORDER BY id`,
		func(rows *sql.Rows) (Batch, error) { return scanBatch(rows) })
}

// FailedBatches returns the failed and invalid batches, by id: brain status shows them (§7.3).
func FailedBatches(ctx context.Context, tx *sql.Tx) ([]Batch, error) {
	return query(ctx, tx, `SELECT `+batchColumns+` FROM extraction_batches WHERE status IN (?, ?) ORDER BY id`,
		func(rows *sql.Rows) (Batch, error) { return scanBatch(rows) }, BatchFailed, BatchInvalid)
}

// StartBatch makes b the active batch of its session, adds one model call to it and returns
// it. A batch with the same stable id is used again; a done one is returned as it is, with no
// call added. Any other active batch of the session, left by a crash, is discarded, and so are
// the failed and invalid batches of the same session and workspace key: this one replaces them.
func StartBatch(ctx context.Context, tx *sql.Tx, b Batch, now time.Time) (Batch, error) {
	old, err := FindBatch(ctx, tx, b)
	switch {
	case err == nil && old.Done():
		return old, nil
	case err != nil && !errors.Is(err, sql.ErrNoRows):
		return Batch{}, fmt.Errorf("read batch: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE extraction_batches SET status = ?, reason = ?, at = ?
		WHERE (session_id = ? AND status = ? OR session_id = ? AND workspace_key = ? AND status IN (?, ?))
		AND NOT (workspace_key = ? AND first_capture = ? AND last_capture = ?)`,
		BatchDiscarded, "replaced by a newer batch", timeArg(now), b.SessionID, BatchActive, b.SessionID,
		b.WorkspaceKey, BatchFailed, BatchInvalid, b.WorkspaceKey, b.FirstCapture, b.LastCapture); err != nil {
		return Batch{}, fmt.Errorf("discard old batches: %w", err)
	}
	var id int64
	err = tx.QueryRowContext(ctx, `INSERT INTO extraction_batches (session_id, workspace_key, first_capture,
		last_capture, start_watermark, last_observation, status, model_calls, reason, at)
		VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7, 1, '', ?8)
		ON CONFLICT (session_id, workspace_key, first_capture, last_capture) DO UPDATE SET
		start_watermark = excluded.start_watermark, last_observation = excluded.last_observation,
		status = excluded.status, model_calls = model_calls + 1, reason = '', at = excluded.at RETURNING id`,
		b.SessionID, b.WorkspaceKey, b.FirstCapture, b.LastCapture, b.StartWatermark, b.LastObservation,
		BatchActive, timeArg(now)).Scan(&id)
	if err != nil {
		return Batch{}, fmt.Errorf("start batch: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO extraction_calls (batch_id, workspace_key, at) VALUES (?, ?, ?)`,
		id, b.WorkspaceKey, timeArg(now)); err != nil {
		return Batch{}, fmt.Errorf("record extractor call: %w", err)
	}
	return BatchByID(ctx, tx, id)
}

// EmptyBatch records b as an empty batch with no model call: its range has no signal, so it
// needs none. It is done at once; the caller moves the watermark in the same transaction.
func EmptyBatch(ctx context.Context, tx *sql.Tx, b Batch, now time.Time) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO extraction_batches (session_id, workspace_key, first_capture,
		last_capture, start_watermark, last_observation, status, model_calls, reason, at)
		VALUES (?, ?, ?, ?, ?, ?, ?, 0, '', ?) ON CONFLICT DO NOTHING`,
		b.SessionID, b.WorkspaceKey, b.FirstCapture, b.LastCapture, b.StartWatermark, b.LastObservation,
		BatchEmpty, timeArg(now))
	if err != nil {
		return fmt.Errorf("add empty batch: %w", err)
	}
	return nil
}

// EndBatch sets the status of the active batch id to status, with reason. It returns false
// when the batch is no longer active: a newer batch discarded it.
func EndBatch(ctx context.Context, tx *sql.Tx, id int64, status BatchStatus, reason string, now time.Time) (bool, error) {
	res, err := tx.ExecContext(ctx, `UPDATE extraction_batches SET status = ?, reason = ?, at = ?
		WHERE id = ? AND status = ?`, status, reason, timeArg(now), id, BatchActive)
	if err != nil {
		return false, fmt.Errorf("end batch %d: %w", id, err)
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// Watermark returns the observation id of the extraction watermark of session sessionID and
// workspace key workspaceKey: 0 when it has none.
func Watermark(ctx context.Context, tx *sql.Tx, sessionID int64, workspaceKey string) (int64, error) {
	var id int64
	err := tx.QueryRowContext(ctx, `SELECT observation_id FROM extraction_watermarks
		WHERE session_id = ? AND workspace_key = ?`, sessionID, workspaceKey).Scan(&id)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("read watermark: %w", err)
	}
	return id, nil
}

// Watermarks returns the watermarks of session sessionID by workspace key.
func Watermarks(ctx context.Context, tx *sql.Tx, sessionID int64) (map[string]int64, error) {
	type row struct {
		key string
		id  int64
	}
	rows, err := query(ctx, tx, `SELECT workspace_key, observation_id FROM extraction_watermarks WHERE session_id = ?`,
		func(rows *sql.Rows) (row, error) {
			var r row
			err := rows.Scan(&r.key, &r.id)
			return r, err
		}, sessionID)
	if err != nil {
		return nil, err
	}
	m := map[string]int64{}
	for _, r := range rows {
		m[r.key] = r.id
	}
	return m, nil
}

// MoveWatermark sets the watermark of the session and workspace key of b to the last
// observation of b. The caller moves it in the transaction that commits b (§7.3 G3).
func MoveWatermark(ctx context.Context, tx *sql.Tx, b Batch) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO extraction_watermarks (session_id, workspace_key, observation_id,
		capture_identity) VALUES (?, ?, ?, ?) ON CONFLICT (session_id, workspace_key) DO UPDATE SET
		observation_id = excluded.observation_id, capture_identity = excluded.capture_identity`,
		b.SessionID, b.WorkspaceKey, b.LastObservation, b.LastCapture)
	if err != nil {
		return fmt.Errorf("move watermark: %w", err)
	}
	return nil
}

// ObservationHeld reports whether observation id with capture identity is still in the
// store: a forget deletes it (§6.7), and the identity check stops a reused row id.
func ObservationHeld(ctx context.Context, tx *sql.Tx, id int64, identity string) (bool, error) {
	var n int
	err := tx.QueryRowContext(ctx, `SELECT count(*) FROM observations WHERE id = ? AND capture_identity = ?`,
		id, identity).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("read observation %d: %w", id, err)
	}
	return n > 0, nil
}

// CandidateOf returns the candidate of claim claimID in the scope with key workspaceKey (""
// for global), or sql.ErrNoRows.
func CandidateOf(ctx context.Context, tx *sql.Tx, claimID, workspaceKey string) (Candidate, error) {
	return scanCandidate(tx.QueryRowContext(ctx, `SELECT `+candidateColumns+` FROM candidates
		WHERE claim_id = ? AND COALESCE(workspace_key, '') = ?`, claimID, workspaceKey))
}

// ClaimHasItem reports whether claim claimID is known in the scope with key workspaceKey ("" for
// global): an item of the claim is in that scope now, or a revision of an item of the claim had
// that scope's key. So a new proposal would be a duplicate (§7.3). The second case keeps a
// claim known in a workspace that a link (§9.4) or a promotion moved its item out of: a queued
// extraction there must not draft the item again.
func ClaimHasItem(ctx context.Context, tx *sql.Tx, claimID, workspaceKey string) (bool, error) {
	var has bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM items WHERE claim_id = ?1
			AND COALESCE(workspace_key, '') = ?2)
		OR EXISTS (SELECT 1 FROM item_revisions r JOIN items i ON i.id = r.item_id
			WHERE i.claim_id = ?1 AND COALESCE(r.workspace_key, '') = ?2)`, claimID, workspaceKey).Scan(&has)
	if err != nil {
		return false, fmt.Errorf("read items of claim %s: %w", claimID, err)
	}
	return has, nil
}

// MarkProposed sets candidate id to proposed.
func MarkProposed(ctx context.Context, tx *sql.Tx, id int64) error {
	if _, err := tx.ExecContext(ctx, `UPDATE candidates SET status = ? WHERE id = ?`, CandidateProposed, id); err != nil {
		return fmt.Errorf("mark candidate %d proposed: %w", id, err)
	}
	return nil
}

// ClaimSourceKeys returns the capture identities of the evidence of claim claimID, sorted: the
// source keys of its tombstone, which hold no text (§6.7).
func ClaimSourceKeys(ctx context.Context, tx *sql.Tx, claimID string) ([]string, error) {
	return query(ctx, tx, `SELECT DISTINCT l.capture_identity FROM evidence_links l
		JOIN candidates c ON c.id = l.candidate_id WHERE c.claim_id = ? ORDER BY 1`,
		func(r *sql.Rows) (k string, err error) { return k, r.Scan(&k) }, claimID)
}

// Tombstoned reports whether a tombstone holds capture identity as a source key: forget
// stopped that evidence (§6.7).
func Tombstoned(ctx context.Context, tx *sql.Tx, identity string) (bool, error) {
	var n int
	err := tx.QueryRowContext(ctx, `SELECT count(*) FROM tombstones t, json_each(t.source_keys) k
		WHERE k.value = ?`, identity).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("read tombstones: %w", err)
	}
	return n > 0, nil
}

// SetExtractorCLI records the extractor CLI and model of the store row (brain setup, D4); cli ""
// clears the choice. It keeps the switch of the row; a new row has extraction on (§7.3 D9).
func SetExtractorCLI(ctx context.Context, tx *sql.Tx, cli ExtractorCLI, model string, now time.Time) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO extractor_settings (workspace_key, extraction, cli, model, changed)
		VALUES ('', 'on', ?, ?, ?) ON CONFLICT (workspace_key) DO UPDATE SET cli = excluded.cli,
		model = excluded.model, changed = excluded.changed`, nullText(string(cli)), nullText(model), timeArg(now))
	if err != nil {
		return fmt.Errorf("write extractor CLI: %w", err)
	}
	return nil
}

// SetExtraction records the extraction switch of the workspace with key workspaceKey, or of
// the store row for "" (brain extract on|off). It keeps the CLI and model of the store row.
func SetExtraction(ctx context.Context, tx *sql.Tx, workspaceKey string, on bool, now time.Time) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO extractor_settings (workspace_key, extraction, changed)
		VALUES (?, ?, ?) ON CONFLICT (workspace_key) DO UPDATE SET extraction = excluded.extraction,
		changed = excluded.changed`, workspaceKey, onOff(on), timeArg(now))
	if err != nil {
		return fmt.Errorf("write extraction switch: %w", err)
	}
	return nil
}

// ExtractionOffKeys returns the workspace keys whose extraction switch is off, sorted. The
// store switch is not in it: Extractor reads it.
func ExtractionOffKeys(ctx context.Context, tx *sql.Tx) ([]string, error) {
	return query(ctx, tx, `SELECT workspace_key FROM extractor_settings WHERE workspace_key != ''
		AND extraction = 'off' ORDER BY 1`, func(r *sql.Rows) (k string, err error) { return k, r.Scan(&k) })
}

// CallsSince returns the extractor calls since since.
func CallsSince(ctx context.Context, tx *sql.Tx, since time.Time) (int, error) {
	var n int
	err := tx.QueryRowContext(ctx, `SELECT count(*) FROM extraction_calls WHERE at >= ?`, timeArg(since)).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count extractor calls: %w", err)
	}
	return n, nil
}

// ExtractorProposalsSince returns the proposals made since since from a candidate, and the
// drafts of the change requests of item revisions (E15-S1, a skill patch of E20-S5): only the
// extractor makes them (§7.3 caps). Your own edit in review has neither.
func ExtractorProposalsSince(ctx context.Context, tx *sql.Tx, since time.Time) (int, error) {
	var n int
	err := tx.QueryRowContext(ctx, `SELECT count(*) FROM proposals WHERE created >= ? AND (candidate_id IS NOT NULL
		OR id IN (SELECT new_proposal_id FROM change_requests WHERE source = ?))`, timeArg(since), ChangeItem).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count extractor proposals: %w", err)
	}
	return n, nil
}

// LastCalls returns, by workspace key, the time of its last extractor call: the round robin of
// the queue serves the key that waited longest first (§7.3).
func LastCalls(ctx context.Context, tx *sql.Tx) (map[string]time.Time, error) {
	type row struct {
		key string
		at  time.Time
	}
	rows, err := query(ctx, tx, `SELECT workspace_key, MAX(at) FROM extraction_calls GROUP BY workspace_key`, func(r *sql.Rows) (row, error) {
		var x row
		err := r.Scan(&x.key, timeDest{&x.at})
		return x, err
	})
	if err != nil {
		return nil, fmt.Errorf("read last extractor calls: %w", err)
	}
	m := map[string]time.Time{}
	for _, r := range rows {
		m[r.key] = r.at
	}
	return m, nil
}

// LastWorkedCall returns the end time of the last extractor call that worked: a batch with a
// model call that committed or gave a valid empty result. It is the zero time when no call
// worked. A failed batch older than it is tried again at once (#263). Only batches count: a
// done batch keeps its row and time, but brain review can queue a done change request again,
// which replaces its time.
func LastWorkedCall(ctx context.Context, tx *sql.Tx) (time.Time, error) {
	var at time.Time
	err := tx.QueryRowContext(ctx, `SELECT MAX(at) FROM extraction_batches WHERE status IN (?, ?) AND model_calls > 0`,
		BatchCommitted, BatchEmpty).Scan(timeDest{&at})
	if err != nil {
		return time.Time{}, fmt.Errorf("read last worked extractor call: %w", err)
	}
	return at, nil
}
