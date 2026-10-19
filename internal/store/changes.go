package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ChangeSource says what a change request asks to draft again.
type ChangeSource string

// The change request sources.
const (
	ChangeProposal ChangeSource = "proposal" // you asked for a change of a proposal
	ChangeItem     ChangeSource = "item"     // two comebacks on an item revision (E15-S1)
)

// ChangeStatus is the status of a change request entry of the extraction queue. It follows
// the batch statuses, with queued before the first call.
type ChangeStatus string

// The change request statuses.
const (
	ChangeQueued    ChangeStatus = "queued"
	ChangeActive    ChangeStatus = "active"    // the extractor call runs, or a crash stopped it
	ChangeCommitted ChangeStatus = "committed" // its new proposal is stored
	ChangeEmpty     ChangeStatus = "empty"     // the extractor gave no new draft
	ChangeFailed    ChangeStatus = "failed"    // the call failed: it waits for a retry
	ChangeInvalid   ChangeStatus = "invalid"   // the response was invalid: it waits for a retry
	ChangeDiscarded ChangeStatus = "discarded" // a forget, a reject or a newer revision made it stale
)

// ChangeRequest is one change request entry of the extraction queue. Its key is (Source,
// SourceID, Revision): for a proposal, the proposal id and the item revision that the proposal
// is for. The request text of a proposal is proposals.change_request.
type ChangeRequest struct {
	ID            int64
	Source        ChangeSource
	SourceID      int64
	Revision      int64
	WorkspaceKey  string // the scope of the request; "" for global
	Status        ChangeStatus
	ModelCalls    int
	Reason        string // why it failed, was invalid or was discarded; no model text; for display only
	Refusal       Refusal
	NewProposalID *int64
	Created       time.Time
	At            time.Time // the time of the last status change
}

// Open reports whether the request still waits for a draft.
func (c ChangeRequest) Open() bool {
	switch c.Status {
	case ChangeQueued, ChangeActive, ChangeFailed, ChangeInvalid:
		return true
	}
	return false
}

const changeColumns = `id, source, source_id, revision, workspace_key, status, model_calls, reason, refusal,
	new_proposal_id, created, at`

func scanChange(rows interface{ Scan(...any) error }) (ChangeRequest, error) {
	var c ChangeRequest
	err := rows.Scan(&c.ID, &c.Source, &c.SourceID, &c.Revision, &c.WorkspaceKey, &c.Status, &c.ModelCalls, &c.Reason,
		&c.Refusal, &c.NewProposalID, timeDest{&c.Created}, timeDest{&c.At})
	return c, err
}

// openChanges is the SQL list of the open change request statuses.
const openChanges = `('queued', 'active', 'failed', 'invalid')`

// AskChange records your change request text on pending proposal id of candidate cand, sets
// the proposal to changed, so it is not published, and queues the request. The queue
// key is the proposal id and its base revision. A proposal that its last request gave back
// with no try left (ReturnChanged) queues that key again, as a new request with new tries
// (#359). The caller filters text for secrets.
func AskChange(ctx context.Context, tx *sql.Tx, id int64, cand Candidate, text string, surface Surface, now time.Time) (ChangeRequest, error) {
	if err := markChanged(ctx, tx, id, text, surface, now); err != nil {
		return ChangeRequest{}, err
	}
	var cr int64
	err := tx.QueryRowContext(ctx, `INSERT INTO change_requests (source, source_id, revision, workspace_key, status,
		model_calls, reason, created, at) SELECT ?1, id, base_revision, ?2, ?3, 0, '', ?4, ?4 FROM proposals WHERE id = ?5
		ON CONFLICT (source, source_id, revision) DO UPDATE SET workspace_key = ?2, status = ?3, model_calls = 0,
			reason = '', new_proposal_id = NULL, created = ?4, at = ?4
		RETURNING id`, ChangeProposal, cand.WorkspaceKey, ChangeQueued, timeArg(now), id).Scan(&cr)
	if err != nil {
		return ChangeRequest{}, fmt.Errorf("queue the change request of proposal %d: %w", id, err)
	}
	return ChangeRequestByID(ctx, tx, cr)
}

// QueueItemChange queues the change request of revision of item: two comebacks on that revision.
// The queue key is the item id and the revision, so a request is queued once, and a retry adds
// nothing. Its scope is the scope of the item. The request holds no text.
func QueueItemChange(ctx context.Context, tx *sql.Tx, item, revision int64, now time.Time) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO change_requests (source, source_id, revision, workspace_key, status,
		model_calls, reason, created, at) SELECT ?, id, ?, COALESCE(workspace_key, ''), ?, 0, '', ?, ? FROM items WHERE id = ?
		ON CONFLICT (source, source_id, revision) DO NOTHING`,
		ChangeItem, revision, ChangeQueued, timeArg(now), timeArg(now), item)
	if err != nil {
		return fmt.Errorf("queue the change request of item %d revision %d: %w", item, revision, err)
	}
	return nil
}

// markChanged records your change request text on pending proposal id and sets it to
// changed, so it is not published.
func markChanged(ctx context.Context, tx *sql.Tx, id int64, text string, surface Surface, now time.Time) error {
	res, err := tx.ExecContext(ctx, `UPDATE proposals SET status = ?, change_request = ?, surface = ?, decided = ?
		WHERE id = ? AND status = ? AND base_revision IS NOT NULL`,
		ProposalChanged, text, surface, timeArg(now), id, ProposalPending)
	if err != nil {
		return fmt.Errorf("ask a change of proposal %d: %w", id, err)
	}
	if n, err := res.RowsAffected(); err != nil || n == 0 {
		return errors.Join(err, fmt.Errorf("proposal %d is not pending: %w", id, sql.ErrNoRows))
	}
	return nil
}

// ItemChangeOf returns the change request of an item revision whose draft is proposal id
// (E15-S1). ok is false when the proposal is no such draft.
func ItemChangeOf(ctx context.Context, tx *sql.Tx, id int64) (c ChangeRequest, ok bool, err error) {
	c, err = scanChange(tx.QueryRowContext(ctx, `SELECT `+changeColumns+` FROM change_requests
		WHERE source = ? AND new_proposal_id = ?`, ChangeItem, id))
	if errors.Is(err, sql.ErrNoRows) {
		return ChangeRequest{}, false, nil
	}
	if err != nil {
		return ChangeRequest{}, false, fmt.Errorf("read the item change request of proposal %d: %w", id, err)
	}
	return c, true, nil
}

// AskItemChange records your change request text on pending proposal id, the draft of the
// change request of an item revision, and queues that request again on its key (item id,
// revision): its next draft redrafts proposal id with your text. The request keeps the
// proposal as its new_proposal_id until that draft. It is a new request: it starts with new
// tries (#359), also after a redraft that gave the proposal back with no try left. The caller
// filters text for secrets.
func AskItemChange(ctx context.Context, tx *sql.Tx, id int64, text string, surface Surface, now time.Time) (ChangeRequest, error) {
	if err := markChanged(ctx, tx, id, text, surface, now); err != nil {
		return ChangeRequest{}, err
	}
	var cr int64
	err := tx.QueryRowContext(ctx, `UPDATE change_requests SET status = ?, model_calls = 0, reason = '', refusal = '', at = ?
		WHERE source = ? AND new_proposal_id = ? AND status IN (?, ?, ?) RETURNING id`,
		ChangeQueued, timeArg(now), ChangeItem, id, ChangeCommitted, ChangeFailed, ChangeInvalid).Scan(&cr)
	if err != nil {
		return ChangeRequest{}, fmt.Errorf("queue the item change request of proposal %d: %w", id, err)
	}
	return ChangeRequestByID(ctx, tx, cr)
}

// ReturnChanged gives changed proposal id back to review: its change request has no try left
// (#359). It waits for your decision again, with your request text; nothing is published. You
// approve it, edit it, reject it, or ask for a change again.
func ReturnChanged(ctx context.Context, tx *sql.Tx, id int64) error {
	if _, err := tx.ExecContext(ctx, `UPDATE proposals SET status = ?, surface = NULL, decided = NULL
		WHERE id = ? AND status = ?`, ProposalPending, id, ProposalChanged); err != nil {
		return fmt.Errorf("give proposal %d back to review: %w", id, err)
	}
	return nil
}

// RetakeChanged takes proposal id back from review at now, the opposite of ReturnChanged: its
// change request gets a call again (#428). Only a pending proposal with its request text is
// taken back; one that you decided stays as it is.
func RetakeChanged(ctx context.Context, tx *sql.Tx, id int64, now time.Time) error {
	if _, err := tx.ExecContext(ctx, `UPDATE proposals SET status = ?, decided = ?
		WHERE id = ? AND status = ? AND change_request IS NOT NULL`, ProposalChanged, timeArg(now), id,
		ProposalPending); err != nil {
		return fmt.Errorf("take proposal %d back from review: %w", id, err)
	}
	return nil
}

// ProposalChangeOf returns the change request of proposal id: the request that you asked on it,
// or that redrafts it (AskItemChange). ok is false for none.
func ProposalChangeOf(ctx context.Context, tx *sql.Tx, id int64) (c ChangeRequest, ok bool, err error) {
	c, err = scanChange(tx.QueryRowContext(ctx, `SELECT `+changeColumns+` FROM change_requests
		WHERE source = ?1 AND source_id = ?2 OR source = ?3 AND new_proposal_id = ?2 ORDER BY id DESC LIMIT 1`,
		ChangeProposal, id, ChangeItem))
	if errors.Is(err, sql.ErrNoRows) {
		return ChangeRequest{}, false, nil
	}
	if err != nil {
		return ChangeRequest{}, false, fmt.Errorf("read the change request of proposal %d: %w", id, err)
	}
	return c, true, nil
}

// ChangeRequestByID returns change request id, or sql.ErrNoRows.
func ChangeRequestByID(ctx context.Context, tx *sql.Tx, id int64) (ChangeRequest, error) {
	return scanChange(tx.QueryRowContext(ctx, `SELECT `+changeColumns+` FROM change_requests WHERE id = ?`, id))
}

// ChangeRequests returns every change request, by id.
func ChangeRequests(ctx context.Context, tx *sql.Tx) ([]ChangeRequest, error) {
	return query(ctx, tx, `SELECT `+changeColumns+` FROM change_requests ORDER BY id`,
		func(rows *sql.Rows) (ChangeRequest, error) { return scanChange(rows) })
}

// OpenChangeRequests returns the change requests that wait for a draft, by id.
func OpenChangeRequests(ctx context.Context, tx *sql.Tx) ([]ChangeRequest, error) {
	return query(ctx, tx, `SELECT `+changeColumns+` FROM change_requests WHERE status IN `+openChanges+` ORDER BY id`,
		func(rows *sql.Rows) (ChangeRequest, error) { return scanChange(rows) })
}

// StartChange makes change request id active, records one extractor call for it under the
// daily call cap, and returns its model call count: the attempt that the commit must match. An
// active request is started again only when it became active before stale: a crash stopped it.
// Else it returns sql.ErrNoRows: another pass runs it.
func StartChange(ctx context.Context, tx *sql.Tx, id int64, now, stale time.Time) (int, error) {
	var key string
	var calls int
	err := tx.QueryRowContext(ctx, `UPDATE change_requests SET status = ?1, model_calls = model_calls + 1, reason = '', refusal = '',
		at = ?2 WHERE id = ?3 AND (status IN ('queued', 'failed', 'invalid') OR status = ?1 AND at < ?4)
		RETURNING workspace_key, model_calls`, ChangeActive, timeArg(now), id, timeArg(stale)).Scan(&key, &calls)
	if err != nil {
		return 0, fmt.Errorf("start change request %d: %w", id, err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO extraction_calls (change_request_id, workspace_key, at) VALUES (?, ?, ?)`,
		id, key, timeArg(now)); err != nil {
		return 0, fmt.Errorf("record extractor call: %w", err)
	}
	return calls, nil
}

// EndChange sets the status of open change request id to status, with reason and the new
// proposal. A nil proposal keeps the one that the request has: a failed redraft of a draft
// (AskItemChange) still redrafts that draft at its retry. It returns false when the request is
// not active: a forget discarded it, or its commit is done. Only an active request commits, so
// a retry makes no second draft.
func EndChange(ctx context.Context, tx *sql.Tx, id int64, status ChangeStatus, reason string, proposal *int64, now time.Time) (bool, error) {
	res, err := tx.ExecContext(ctx, `UPDATE change_requests SET status = ?, reason = ?, refusal = '',
		new_proposal_id = COALESCE(?, new_proposal_id), at = ? WHERE id = ? AND status IN `+openChanges,
		status, reason, proposal, timeArg(now), id)
	if err != nil {
		return false, fmt.Errorf("end change request %d: %w", id, err)
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// discardChanges discards the open change requests of the proposals that where selects by
// id ?1, and of the items that items selects, and removes the request text of those
// proposals.
func discardChanges(ctx context.Context, tx *sql.Tx, proposals, items string, id int64, now time.Time) error {
	if _, err := tx.ExecContext(ctx, `UPDATE change_requests SET status = ?2, reason = ?3, refusal = '', at = ?4
		WHERE status IN `+openChanges+` AND (source = 'proposal' AND source_id IN (`+proposals+`)
		OR source = 'item' AND source_id IN (`+items+`))`,
		id, ChangeDiscarded, "a forget removed its input", timeArg(now)); err != nil {
		return fmt.Errorf("discard change requests: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE proposals SET change_request = NULL WHERE id IN (`+proposals+`)`, id); err != nil {
		return fmt.Errorf("erase change requests: %w", err)
	}
	return nil
}

// Excerpt is one evidence observation of a candidate, as a proposal shows it: its excerpt,
// sender, session, workspace and date. It is read at show time from the evidence
// links; a proposal stores no copy of it.
type Excerpt struct {
	ObservationID   int64
	CaptureIdentity string
	Excerpt         string
	Sender          Sender
	Source          string
	Event           string
	SessionID       int64
	HerdrSession    string
	Harness         string
	PaneID          string
	WorkspaceKey    string // "" when the observation has no Brain key
	At              time.Time
}

// CandidateExcerpts returns the evidence observations of candidate id that are still in the
// store, the newest first.
func CandidateExcerpts(ctx context.Context, tx *sql.Tx, id int64) ([]Excerpt, error) {
	return query(ctx, tx, `SELECT o.id, o.capture_identity, o.excerpt, o.sender, o.source, o.event, s.id,
		s.herdr_session, s.harness, s.pane_id, COALESCE(l.workspace_key, ''), o.at FROM evidence_links l
		JOIN observations o ON o.id = l.observation_id AND o.capture_identity = l.capture_identity
		JOIN sessions s ON s.id = l.session_id WHERE l.candidate_id = ? ORDER BY o.at DESC, o.id DESC`,
		scanExcerpt, id)
}

func scanExcerpt(rows *sql.Rows) (Excerpt, error) {
	var e Excerpt
	err := rows.Scan(&e.ObservationID, &e.CaptureIdentity, &e.Excerpt, &e.Sender, &e.Source, &e.Event, &e.SessionID,
		&e.HerdrSession, &e.Harness, &e.PaneID, &e.WorkspaceKey, timeDest{&e.At})
	return e, err
}

// ActiveClaimItems returns the ids of the active items of claim claimID in the scope with key
// workspaceKey: the global items and the items of that workspace ("" for global items only).
// A conflicts_with of the extractor names a claim; the proposal stores these ids.
func ActiveClaimItems(ctx context.Context, tx *sql.Tx, claimID, workspaceKey string) ([]int64, error) {
	return query(ctx, tx, `SELECT id FROM items WHERE claim_id = ? AND state = ?
		AND (workspace_key IS NULL OR workspace_key = ?) ORDER BY id`,
		func(rows *sql.Rows) (int64, error) {
			var id int64
			err := rows.Scan(&id)
			return id, err
		}, claimID, StateActive, workspaceKey)
}
