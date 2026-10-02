package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// SuggestionResult is what became of a suggestion (§11, §12).
type SuggestionResult string

// The suggestion results. A suggestion has none while it waits for you.
const (
	SuggestionApproved SuggestionResult = "approved"
	SuggestionRejected SuggestionResult = "rejected"
	SuggestionChanged  SuggestionResult = "changed"
	SuggestionLater    SuggestionResult = "later"
	// SuggestionExpired: the suggestion was not shown, as a check before the pane failed. [NEW]
	SuggestionExpired SuggestionResult = "expired"
)

// Suggestion is one row of suggestions (E14-S3, §5.5, §11). It holds ids only, no text.
type Suggestion struct {
	ID            int64
	ObservationID int64 // the prompt occurrence: the reservation key
	ProposalID    int64
	SessionID     int64 // the capture session of the prompt
	Generation    int64 // its capture generation at the reservation
	HerdrSession  string
	WorkspaceKey  string // "" when the workspace has no Brain key
	AgentPaneID   string
	ReviewPaneID  string // "" until the pane opens
	PromptHash    string
	Reserved      time.Time
	ShownAt       time.Time // zero until the pane opens
	Result        SuggestionResult
}

// ReserveSuggestion reserves the suggestion of prompt occurrence obs, a stored prompt
// observation with prompt fingerprint hash, when hash matches a ready proposal (§5.5). The
// session, its capture generation, the binding, the workspace key and the agent pane come from
// the segment of obs. It returns the new row, or a Suggestion with ID 0 when there is nothing
// to suggest:
//   - the occurrence has a suggestion already: a hook record and a pane read of one prompt are
//     one occurrence, linked by enrichment (S57);
//   - extraction is off in the workspace of obs;
//   - no ready proposal matches, or each one that matches is muted with "later" in the session.
//
// The caller runs it in the transaction that stored obs, after the learning and capture
// generation checks and after the enrichment match, so the two paths of one occurrence reserve
// one suggestion. Of several matches, the oldest proposal wins [NEW].
func ReserveSuggestion(ctx context.Context, tx *sql.Tx, obs int64, hash string, now time.Time) (Suggestion, error) {
	s := Suggestion{ObservationID: obs, PromptHash: hash, Reserved: now}
	var taken bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM suggestions WHERE observation_id IN (
		SELECT id FROM observations WHERE id = ?1 OR enriched_by = ?1
		UNION SELECT enriched_by FROM observations WHERE id = ?1)),
		g.session_id, se.generation, se.herdr_session, COALESCE(g.workspace_key, ''), g.pane_id
		FROM observations o JOIN segments g ON g.id = o.segment_id JOIN sessions se ON se.id = g.session_id
		WHERE o.id = ?1`, obs).Scan(&taken, &s.SessionID, &s.Generation, &s.HerdrSession, &s.WorkspaceKey, &s.AgentPaneID)
	if err != nil {
		return Suggestion{}, fmt.Errorf("read suggestion of observation %d: %w", obs, err)
	}
	if taken {
		return Suggestion{}, nil
	}
	on, err := ExtractionOn(ctx, tx, s.WorkspaceKey)
	if err != nil || !on {
		return Suggestion{}, err
	}
	ids, err := MatchPrompt(ctx, tx, hash, s.WorkspaceKey)
	if err != nil {
		return Suggestion{}, err
	}
	for _, id := range ids {
		muted, err := SuggestionMuted(ctx, tx, s.SessionID, id)
		if err != nil {
			return Suggestion{}, err
		}
		if muted {
			continue
		}
		s.ProposalID = id
		s.ID, err = insertRow(ctx, tx, "suggestion", `INSERT INTO suggestions (observation_id, proposal_id, session_id,
			generation, herdr_session, workspace_key, agent_pane_id, prompt_hash, reserved) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			s.ObservationID, s.ProposalID, s.SessionID, s.Generation, s.HerdrSession, nullText(s.WorkspaceKey),
			s.AgentPaneID, s.PromptHash, timeArg(s.Reserved))
		return s, err
	}
	return Suggestion{}, nil
}

// SuggestionMuted reports whether you chose "later" for a suggestion of proposal in capture
// session session: the proposal is muted for the rest of that session (§5.5).
func SuggestionMuted(ctx context.Context, tx *sql.Tx, session, proposal int64) (bool, error) {
	var muted bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM suggestions
		WHERE session_id = ? AND proposal_id = ? AND result = ?)`, session, proposal, SuggestionLater).Scan(&muted)
	if err != nil {
		return false, fmt.Errorf("read suggestions of proposal %d: %w", proposal, err)
	}
	return muted, nil
}

const suggestionColumns = `id, observation_id, proposal_id, session_id, generation, herdr_session,
	COALESCE(workspace_key, ''), agent_pane_id, COALESCE(review_pane_id, ''), prompt_hash, reserved, shown_at,
	COALESCE(result, '')`

func scanSuggestion(rows interface{ Scan(...any) error }) (Suggestion, error) {
	var s Suggestion
	err := rows.Scan(&s.ID, &s.ObservationID, &s.ProposalID, &s.SessionID, &s.Generation, &s.HerdrSession,
		&s.WorkspaceKey, &s.AgentPaneID, &s.ReviewPaneID, &s.PromptHash, timeDest{&s.Reserved}, timeDest{&s.ShownAt},
		&s.Result)
	return s, err
}

// SuggestionByID reads the suggestion with id. It returns sql.ErrNoRows when there is none,
// as after a forget.
func SuggestionByID(ctx context.Context, tx *sql.Tx, id int64) (Suggestion, error) {
	s, err := scanSuggestion(tx.QueryRowContext(ctx, `SELECT `+suggestionColumns+` FROM suggestions WHERE id = ?`, id))
	if err != nil {
		return Suggestion{}, fmt.Errorf("read suggestion %d: %w", id, err)
	}
	return s, nil
}

// Suggestions returns every suggestion, by id.
func Suggestions(ctx context.Context, tx *sql.Tx) ([]Suggestion, error) {
	return query(ctx, tx, `SELECT `+suggestionColumns+` FROM suggestions ORDER BY id`,
		func(rows *sql.Rows) (Suggestion, error) { return scanSuggestion(rows) })
}

// ShowSuggestion records that the review pane reviewPane of suggestion id opened at at.
func ShowSuggestion(ctx context.Context, tx *sql.Tx, id int64, reviewPane string, at time.Time) error {
	_, err := tx.ExecContext(ctx, `UPDATE suggestions SET review_pane_id = ?, shown_at = ? WHERE id = ?`,
		reviewPane, timeArg(at), id)
	if err != nil {
		return fmt.Errorf("show suggestion %d: %w", id, err)
	}
	return nil
}

// ExpireSuggestion marks suggestion id expired when it was never shown and has no result.
func ExpireSuggestion(ctx context.Context, tx *sql.Tx, id int64) error {
	_, err := tx.ExecContext(ctx, `UPDATE suggestions SET result = ? WHERE id = ? AND result IS NULL AND shown_at IS NULL`,
		SuggestionExpired, id)
	if err != nil {
		return fmt.Errorf("expire suggestion %d: %w", id, err)
	}
	return nil
}

// DecideSuggestions gives result to the shown suggestions of proposal that wait for you: your
// decision on the proposal decides them (§12 counts).
func DecideSuggestions(ctx context.Context, tx *sql.Tx, proposal int64, result SuggestionResult) error {
	_, err := tx.ExecContext(ctx, `UPDATE suggestions SET result = ?
		WHERE proposal_id = ? AND result IS NULL AND shown_at IS NOT NULL`, result, proposal)
	if err != nil {
		return fmt.Errorf("decide suggestions of proposal %d: %w", proposal, err)
	}
	return nil
}

// SessionByID reads the capture session with id. It returns sql.ErrNoRows when there is none.
func SessionByID(ctx context.Context, tx *sql.Tx, id int64) (Session, error) {
	ss, err := sessions(ctx, tx, `WHERE id = ?1`, id)
	switch {
	case err != nil:
		return Session{}, err
	case len(ss) == 0:
		return Session{}, fmt.Errorf("read session %d: %w", id, sql.ErrNoRows)
	}
	return ss[0], nil
}

// SessionInPane reports whether capture session session has an open segment in pane paneID:
// the pane still holds the session (§6.2).
func SessionInPane(ctx context.Context, tx *sql.Tx, session int64, paneID string) (bool, error) {
	var in bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM segments
		WHERE session_id = ? AND pane_id = ? AND ended IS NULL)`, session, paneID).Scan(&in)
	if err != nil {
		return false, fmt.Errorf("read segments of session %d: %w", session, err)
	}
	return in, nil
}
