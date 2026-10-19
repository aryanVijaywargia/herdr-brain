package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Rejection is one row of rejections: you rejected one claim in one scope.
type Rejection struct {
	ID            int64
	ClaimID       string
	Scope         Scope
	WorkspaceKey  string // "" for global
	Reason        string
	EvidenceCount int // the evidence count of the claim in the scope at the rejection or the last reset
	Evidence      int // the evidence count now: the count of its candidate, 0 with no candidate
	New           int // the evidence observed after At: the new evidence since the rejection or the last reset
	At            time.Time
	Reopened      time.Time // zero while the rejection holds
}

// ErrRejected is the error of a proposal of a claim that you rejected in its scope.
var ErrRejected = errors.New("rejected")

// Reject records that you rejected the claim of candidate c in the scope of c, with reason,
// and returns the id of the rejection. A reworded key keeps the claim id, so it stays
// rejected too. A rejection that already holds for the claim and scope stays as it is.
func Reject(ctx context.Context, tx *sql.Tx, c Candidate, reason string, at time.Time) (int64, error) {
	_, err := tx.ExecContext(ctx, `INSERT INTO rejections (claim_id, scope, workspace_key, reason, evidence_count, at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (claim_id, COALESCE(workspace_key, '')) WHERE reopened IS NULL DO NOTHING`,
		c.ClaimID, c.Scope, nullText(c.WorkspaceKey), reason, c.Count, timeArg(at))
	if err != nil {
		return 0, fmt.Errorf("reject claim %s: %w", c.ClaimID, err)
	}
	var id int64
	err = tx.QueryRowContext(ctx, `SELECT id FROM rejections WHERE claim_id = ? AND COALESCE(workspace_key, '') = ?
		AND reopened IS NULL`, c.ClaimID, c.WorkspaceKey).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("read rejection of claim %s: %w", c.ClaimID, err)
	}
	return id, nil
}

// Rejected reports whether a rejection holds for claim claimID in the scope of workspaceKey
// ("" for global). Every place that makes a proposal from a candidate calls it first
// (ProposeCandidate): new evidence still attaches to the candidate, but no proposal comes.
func Rejected(ctx context.Context, tx *sql.Tx, claimID, workspaceKey string) (bool, error) {
	var held bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM rejections WHERE claim_id = ?
		AND COALESCE(workspace_key, '') = ? AND reopened IS NULL)`, claimID, workspaceKey).Scan(&held)
	if err != nil {
		return false, fmt.Errorf("read rejection of claim %s: %w", claimID, err)
	}
	return held, nil
}

// ProposeCandidate adds p as a proposal of candidate id and returns its id. It refuses with
// ErrRejected when you rejected the claim of the candidate in its scope. E13 makes its
// proposals through it. The trigger prompts of p are the fingerprints of the prompt index rows
// of the evidence of the candidate, and those rows link the new proposal (E14-S1).
func ProposeCandidate(ctx context.Context, tx *sql.Tx, id int64, p Proposal) (int64, error) {
	c, err := CandidateByID(ctx, tx, id)
	if err != nil {
		return 0, err
	}
	held, err := Rejected(ctx, tx, c.ClaimID, c.WorkspaceKey)
	if err != nil {
		return 0, err
	}
	if held {
		return 0, fmt.Errorf("candidate %d: claim %s is %w in its scope", id, c.ClaimID, ErrRejected)
	}
	p.CandidateID = &id
	if p.TriggerPrompts, err = triggerPrompts(ctx, tx, id); err != nil {
		return 0, err
	}
	pid, err := InsertProposal(ctx, tx, p)
	if err != nil {
		return 0, err
	}
	return pid, linkPrompts(ctx, tx, id, pid)
}

// rejectionColumns are the columns that scanRejection reads, in order. The evidence count now
// is the count of the candidate of the claim in the scope. The new evidence is the evidence
// links of that candidate observed after the rejection: a forget of older evidence does not
// hide it, as a difference of two counts would.
const rejectionColumns = `r.id, r.claim_id, r.scope, COALESCE(r.workspace_key, ''), r.reason, r.evidence_count,
	COALESCE((SELECT c.count FROM candidates c WHERE c.claim_id = r.claim_id
		AND COALESCE(c.workspace_key, '') = COALESCE(r.workspace_key, '')), 0),
	(SELECT count(*) FROM evidence_links l JOIN candidates c ON c.id = l.candidate_id WHERE c.claim_id = r.claim_id
		AND COALESCE(c.workspace_key, '') = COALESCE(r.workspace_key, '') AND l.at > r.at), r.at, r.reopened`

func scanRejection(rows interface{ Scan(...any) error }) (Rejection, error) {
	var r Rejection
	err := rows.Scan(&r.ID, &r.ClaimID, &r.Scope, &r.WorkspaceKey, &r.Reason, &r.EvidenceCount, &r.Evidence, &r.New,
		timeDest{&r.At}, timeDest{&r.Reopened})
	return r, err
}

// RejectionByID reads rejection id. It returns sql.ErrNoRows when there is none.
func RejectionByID(ctx context.Context, tx *sql.Tx, id int64) (Rejection, error) {
	r, err := scanRejection(tx.QueryRowContext(ctx, `SELECT `+rejectionColumns+` FROM rejections r WHERE r.id = ?`, id))
	if err != nil {
		return Rejection{}, fmt.Errorf("read rejection %d: %w", id, err)
	}
	return r, nil
}

// Rejections returns the rejections that hold, the newest first.
func Rejections(ctx context.Context, tx *sql.Tx) ([]Rejection, error) {
	return query(ctx, tx, `SELECT `+rejectionColumns+` FROM rejections r WHERE r.reopened IS NULL ORDER BY r.at DESC, r.id DESC`,
		func(rows *sql.Rows) (Rejection, error) { return scanRejection(rows) })
}

// ReopenRejection ends rejection id at now: the claim can get proposals in its scope again.
// Only you reopen a rejection; Brain never does.
func ReopenRejection(ctx context.Context, tx *sql.Tx, id int64, now time.Time) error {
	return updateRejection(ctx, tx, id, `reopened = ?2`, timeArg(now))
}

// ResetRejection keeps rejection id and counts its new evidence from now: the evidence count
// of the rejection becomes the count now. [NEW] reset is the "reset" of a rejection.
func ResetRejection(ctx context.Context, tx *sql.Tx, id int64, now time.Time) error {
	return updateRejection(ctx, tx, id, `at = ?2, evidence_count = COALESCE((SELECT c.count FROM candidates c
		WHERE c.claim_id = rejections.claim_id
		AND COALESCE(c.workspace_key, '') = COALESCE(rejections.workspace_key, '')), 0)`, timeArg(now))
}

// updateRejection sets the columns of set in rejection id while it holds.
func updateRejection(ctx context.Context, tx *sql.Tx, id int64, set string, arg any) error {
	// set is one of the constant clauses of this file, not input.
	res, err := tx.ExecContext(ctx, `UPDATE rejections SET `+set+` WHERE id = ?1 AND reopened IS NULL`, id, arg)
	if err != nil {
		return fmt.Errorf("change rejection %d: %w", id, err)
	}
	if n, err := res.RowsAffected(); err != nil || n == 0 {
		return errors.Join(err, fmt.Errorf("rejection %d does not hold: %w", id, sql.ErrNoRows))
	}
	return nil
}
