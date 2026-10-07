package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// OutcomeKind says what the receipt of a session says about an item that came back (§5.4).
type OutcomeKind string

// The outcome kinds.
const (
	OutcomeComeback  OutcomeKind = "comeback"         // the session got the revision, and the mistake came back
	OutcomeUncertain OutcomeKind = "uncertain"        // the same, but the match is an unconfirmed same_as link
	OutcomeGap       OutcomeKind = "delivery_gap"     // the receipt is withheld
	OutcomeUnknown   OutcomeKind = "delivery_unknown" // no single receipt ties to the session
)

// Outcome is one row of outcomes: ids only (§11).
type Outcome struct {
	ItemID        int64
	Revision      int64 // the revision that the receipt names; 0 for a gap or an unknown delivery
	SessionID     int64 // the sessions row, which holds the session key
	Kind          OutcomeKind
	ObservationID int64
	At            time.Time
}

// AddOutcome adds o. It returns false when the observation already has an outcome for the
// item: a retry adds nothing.
func AddOutcome(ctx context.Context, tx *sql.Tx, o Outcome) (bool, error) {
	res, err := tx.ExecContext(ctx, `INSERT INTO outcomes (item_id, revision, session_id, kind, observation_id, at)
		VALUES (?, ?, ?, ?, ?, ?) ON CONFLICT (item_id, observation_id) DO NOTHING`,
		o.ItemID, sql.NullInt64{Int64: o.Revision, Valid: o.Revision != 0}, o.SessionID, o.Kind, o.ObservationID,
		timeArg(o.At))
	if err != nil {
		return false, fmt.Errorf("add outcome of item %d: %w", o.ItemID, err)
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// Comebacks counts the comebacks on revision of item: one for each observation.
func Comebacks(ctx context.Context, tx *sql.Tx, item, revision int64) (int, error) {
	var n int
	err := tx.QueryRowContext(ctx, `SELECT count(*) FROM outcomes
		WHERE item_id = ? AND revision = ? AND kind = ?`, item, revision, OutcomeComeback).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count comebacks of item %d revision %d: %w", item, revision, err)
	}
	return n, nil
}

// ComebackSessions counts the sessions with a comeback on revision of item: two flags in one
// session are one. A skill counts sessions, not flags (E20-S5).
func ComebackSessions(ctx context.Context, tx *sql.Tx, item, revision int64) (int, error) {
	var n int
	err := tx.QueryRowContext(ctx, `SELECT count(DISTINCT session_id) FROM outcomes
		WHERE item_id = ? AND revision = ? AND kind = ?`, item, revision, OutcomeComeback).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count comeback sessions of item %d revision %d: %w", item, revision, err)
	}
	return n, nil
}

// ComebackExcerpts returns the observations of the comebacks on revision of item that are still
// in the store, the newest first: the input of the change request of that revision.
func ComebackExcerpts(ctx context.Context, tx *sql.Tx, item, revision int64) ([]Excerpt, error) {
	return query(ctx, tx, `SELECT o.id, o.capture_identity, o.excerpt, o.sender, s.id, s.herdr_session, s.harness,
		s.pane_id, COALESCE(g.workspace_key, ''), o.at FROM outcomes c
		JOIN observations o ON o.id = c.observation_id JOIN segments g ON g.id = o.segment_id
		JOIN sessions s ON s.id = g.session_id
		WHERE c.item_id = ? AND c.revision = ? AND c.kind = ? ORDER BY o.at DESC, o.id DESC`,
		scanExcerpt, item, revision, OutcomeComeback)
}
