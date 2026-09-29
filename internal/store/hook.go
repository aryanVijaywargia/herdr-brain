package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// CommandRecord is one row of command_records: the command of one tool hook observation
// (§6.7, §11).
type CommandRecord struct {
	ObservationID int64
	Argv          string // the filtered command, at most 4 KB; "" when SecretFound
	CWD           string
	ExitStatus    *int // nil when the hook payload has none
	Truncated     bool // Argv was cut
	SecretFound   bool // the secret filter found a secret, so Argv is not stored
}

// HookSegment returns the open session and the open segment of pane paneID for a hook record
// of harness in binding herdrSession. A session with another agent session id is another run:
// it is not a match. A session with no agent session id yet (a capture key) matches. It returns
// zeros when capture holds no open segment of the pane: the hook then stores nothing.
func HookSegment(ctx context.Context, tx *sql.Tx, herdrSession, harness, paneID, agentSessionID string) (session, segment int64, err error) {
	err = tx.QueryRowContext(ctx, `SELECT n.id, s.id FROM sessions n JOIN segments s ON s.session_id = n.id
		WHERE n.herdr_session = ?1 AND n.harness = ?2 AND n.pane_id = ?3 AND n.ended IS NULL
		AND s.pane_id = ?3 AND s.ended IS NULL
		AND (?4 = '' OR n.agent_session_id IS NULL OR n.agent_session_id = ?4)
		ORDER BY s.id DESC LIMIT 1`, herdrSession, harness, paneID, agentSessionID).Scan(&session, &segment)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, 0, nil
	}
	if err != nil {
		return 0, 0, fmt.Errorf("find hook segment: %w", err)
	}
	return session, segment, nil
}

// AddCommandRecord adds c.
func AddCommandRecord(ctx context.Context, tx *sql.Tx, c CommandRecord) error {
	var exit any
	if c.ExitStatus != nil {
		exit = *c.ExitStatus
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO command_records
		(observation_id, argv, cwd, exit_status, truncated, secret_found) VALUES (?, ?, ?, ?, ?, ?)`,
		c.ObservationID, nullText(c.Argv), nullText(c.CWD), exit, c.Truncated, c.SecretFound)
	if err != nil {
		return fmt.Errorf("add command record: %w", err)
	}
	return nil
}

// CommandRecords returns every command record, by observation id.
func CommandRecords(ctx context.Context, tx *sql.Tx) ([]CommandRecord, error) {
	return query(ctx, tx, `SELECT observation_id, COALESCE(argv, ''), COALESCE(cwd, ''), exit_status, truncated,
		secret_found FROM command_records ORDER BY observation_id`,
		func(rows *sql.Rows) (CommandRecord, error) {
			var c CommandRecord
			var exit sql.NullInt64
			err := rows.Scan(&c.ObservationID, &c.Argv, &c.CWD, &exit, &c.Truncated, &c.SecretFound)
			if exit.Valid {
				n := int(exit.Int64)
				c.ExitStatus = &n
			}
			return c, err
		})
}

// Counterparts returns the observations of session sessionID from source, with text, sender
// and event, that no other observation enriches and that enrich none, at a time in
// [at-window, at+window]. The nearest in time comes first (S35).
func Counterparts(ctx context.Context, tx *sql.Tx, sessionID int64, source string, at time.Time, window time.Duration) ([]Observation, error) {
	rows, err := tx.QueryContext(ctx, `SELECT o.id, o.excerpt, o.sender, o.event FROM observations o JOIN segments s ON s.id = o.segment_id
		WHERE s.session_id = ?1 AND o.source = ?2 AND o.status != 'gap' AND o.excerpt != ''
		AND o.at BETWEEN ?3 AND ?4 AND o.enriched_by IS NULL
		AND NOT EXISTS (SELECT 1 FROM observations p WHERE p.enriched_by = o.id)
		ORDER BY abs(julianday(o.at) - julianday(?5)), o.id`,
		sessionID, source, timeArg(at.Add(-window)), timeArg(at.Add(window)), timeArg(at))
	if err != nil {
		return nil, fmt.Errorf("find counterparts: %w", err)
	}
	defer rows.Close() //nolint:errcheck // read-only: rows.Err reports the read errors
	var obs []Observation
	for rows.Next() {
		o := Observation{Source: source}
		if err := rows.Scan(&o.ID, &o.Excerpt, &o.Sender, &o.Event); err != nil {
			return nil, fmt.Errorf("find counterparts: %w", err)
		}
		obs = append(obs, o)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("find counterparts: %w", err)
	}
	return obs, nil
}

// Enrich links hook observation hook to pane observation pane: the hook record adds detail to
// the pane text and is not a second occurrence, so it does not count (§6.4, S35). Both rows
// stay, so both references are kept.
func Enrich(ctx context.Context, tx *sql.Tx, pane, hook int64) error {
	if _, err := tx.ExecContext(ctx, `UPDATE observations SET enriched_by = ? WHERE id = ?`, hook, pane); err != nil {
		return fmt.Errorf("enrich observation: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE observations SET counts = 0 WHERE id = ?`, hook); err != nil {
		return fmt.Errorf("enrich observation: %w", err)
	}
	return nil
}
