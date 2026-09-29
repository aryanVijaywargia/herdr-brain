package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// KeyKind is the form of a session key (§6.2 step 6).
type KeyKind string

// The session key forms.
const (
	KeyAgent   KeyKind = "agent"   // the agent session id from herdr
	KeyCapture KeyKind = "capture" // a local capture id: herdr had no agent session id
)

// SessionKey is the key of one agent session. Every key holds the canonical binding, the
// herdr session name (E2-S2), so the same pane id or agent session id in two bindings gives
// two keys.
type SessionKey struct {
	HerdrSession string
	Harness      string
	Kind         KeyKind
	ID           string
}

// Session is one row of sessions.
type Session struct {
	ID             int64
	Key            SessionKey
	AgentSessionID string // "" when herdr has given none
	PaneID         string
	WorkspaceKey   string // "" when the workspace has no Brain key
	Started        time.Time
	Ended          time.Time // zero while the session is open
}

// Segment is one row of segments.
type Segment struct {
	ID           int64
	SessionID    int64
	PaneID       string
	WorkspaceKey string
	ReadSource   string
	Started      time.Time
	Ended        time.Time
}

// ObservationStatus says how much of the new text an observation holds (S31).
type ObservationStatus string

// The observation statuses.
const (
	ObservationComplete ObservationStatus = "complete" // all of the new text
	ObservationPartial  ObservationStatus = "partial"  // the read may miss some of the new text
	ObservationGap      ObservationStatus = "gap"      // text is missing; the excerpt holds none
)

// SourceHerdr is the observation source of a pane read.
const SourceHerdr = "herdr"

// Observation is one row of observations.
type Observation struct {
	ID              int64
	CaptureIdentity string // unique (§6.4)
	Source          string
	SegmentID       int64
	Event           string // the agent status of the read, or the gap reason
	Excerpt         string
	Truncated       bool
	Status          ObservationStatus
	At              time.Time
}

// keyWhere is the WHERE clause of the session with key ?1…?4.
const keyWhere = `herdr_session = ?1 AND harness = ?2 AND key_kind = ?3 AND key_id = ?4`

func keyArgs(k SessionKey) []any {
	return []any{k.HerdrSession, k.Harness, k.Kind, k.ID}
}

// OpenSession opens the session with key k in pane paneID and returns its id. An agent key
// that has a row opens that row again, in the new pane and workspace. A capture key must be
// new: brained makes a new one for each agent that it sees.
func OpenSession(ctx context.Context, tx *sql.Tx, k SessionKey, paneID, workspaceKey string, at time.Time) (int64, error) {
	var agentID any
	if k.Kind == KeyAgent {
		agentID = k.ID
	}
	var id int64
	err := tx.QueryRowContext(ctx, `INSERT INTO sessions
		(herdr_session, harness, key_kind, key_id, agent_session_id, pane_id, workspace_key, started)
		VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8)
		ON CONFLICT (herdr_session, harness, key_kind, key_id) DO UPDATE
		SET pane_id = excluded.pane_id, workspace_key = excluded.workspace_key, ended = NULL
		WHERE key_kind = 'agent'
		RETURNING id`,
		k.HerdrSession, k.Harness, k.Kind, k.ID, agentID, paneID, nullText(workspaceKey), timeArg(at)).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("open session: %w", err)
	}
	return id, nil
}

// EnrichSession records agentSessionID, the id that herdr gave later, on the session with key
// k. It keeps the key, so it merges no two sessions, and it keeps an earlier id.
func EnrichSession(ctx context.Context, tx *sql.Tx, k SessionKey, agentSessionID string) error {
	_, err := tx.ExecContext(ctx, `UPDATE sessions SET agent_session_id = ?5
		WHERE `+keyWhere+` AND agent_session_id IS NULL`, append(keyArgs(k), agentSessionID)...)
	if err != nil {
		return fmt.Errorf("enrich session: %w", err)
	}
	return nil
}

// EndSession ends the session with key k and its open segments at at.
func EndSession(ctx context.Context, tx *sql.Tx, k SessionKey, at time.Time) error {
	args := append(keyArgs(k), timeArg(at))
	if _, err := tx.ExecContext(ctx, `UPDATE segments SET ended = ?5
		WHERE ended IS NULL AND session_id IN (SELECT id FROM sessions WHERE `+keyWhere+`)`, args...); err != nil {
		return fmt.Errorf("end segments: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE sessions SET ended = ?5 WHERE `+keyWhere+` AND ended IS NULL`, args...); err != nil {
		return fmt.Errorf("end session: %w", err)
	}
	return nil
}

// ForgetSession removes the session with key k, with its segments and observations.
func ForgetSession(ctx context.Context, tx *sql.Tx, k SessionKey) error {
	for _, q := range []string{
		`DELETE FROM observations WHERE segment_id IN
			(SELECT s.id FROM segments s JOIN sessions n ON n.id = s.session_id WHERE ` + keyWhereOf("n") + `)`,
		`DELETE FROM segments WHERE session_id IN (SELECT id FROM sessions WHERE ` + keyWhere + `)`,
		`DELETE FROM sessions WHERE ` + keyWhere,
	} {
		if _, err := tx.ExecContext(ctx, q, keyArgs(k)...); err != nil {
			return fmt.Errorf("forget session: %w", err)
		}
	}
	return nil
}

func keyWhereOf(alias string) string {
	return fmt.Sprintf(`%[1]s.herdr_session = ?1 AND %[1]s.harness = ?2 AND %[1]s.key_kind = ?3 AND %[1]s.key_id = ?4`, alias)
}

// StartSegment starts a segment of session sessionID in pane paneID and returns its id. It
// ends the open segments of the session first: one pane move or one new baseline gives one
// new segment, and text never joins across two.
func StartSegment(ctx context.Context, tx *sql.Tx, sessionID int64, paneID, workspaceKey, readSource string, at time.Time) (int64, error) {
	if _, err := tx.ExecContext(ctx, `UPDATE segments SET ended = ? WHERE session_id = ? AND ended IS NULL`,
		timeArg(at), sessionID); err != nil {
		return 0, fmt.Errorf("end segments: %w", err)
	}
	var id int64
	err := tx.QueryRowContext(ctx, `INSERT INTO segments (session_id, pane_id, workspace_key, read_source, started)
		VALUES (?, ?, ?, ?, ?) RETURNING id`, sessionID, paneID, nullText(workspaceKey), readSource, timeArg(at)).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("start segment: %w", err)
	}
	return id, nil
}

// AddObservation adds o. A second observation with the same capture identity is an error.
func AddObservation(ctx context.Context, tx *sql.Tx, o Observation) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO observations
		(capture_identity, source, segment_id, event, excerpt, truncated, status, at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		o.CaptureIdentity, o.Source, o.SegmentID, o.Event, o.Excerpt, o.Truncated, o.Status, timeArg(o.At))
	if err != nil {
		return fmt.Errorf("add observation: %w", err)
	}
	return nil
}

// Sessions returns every session, by id.
func Sessions(ctx context.Context, tx *sql.Tx) ([]Session, error) {
	return query(ctx, tx, `SELECT id, herdr_session, harness, key_kind, key_id, COALESCE(agent_session_id, ''),
		pane_id, COALESCE(workspace_key, ''), started, ended FROM sessions ORDER BY id`,
		func(rows *sql.Rows) (Session, error) {
			var s Session
			err := rows.Scan(&s.ID, &s.Key.HerdrSession, &s.Key.Harness, &s.Key.Kind, &s.Key.ID, &s.AgentSessionID,
				&s.PaneID, &s.WorkspaceKey, timeDest{&s.Started}, timeDest{&s.Ended})
			return s, err
		})
}

// Segments returns every segment, by id.
func Segments(ctx context.Context, tx *sql.Tx) ([]Segment, error) {
	return query(ctx, tx, `SELECT id, session_id, pane_id, COALESCE(workspace_key, ''), read_source, started, ended
		FROM segments ORDER BY id`,
		func(rows *sql.Rows) (Segment, error) {
			var s Segment
			err := rows.Scan(&s.ID, &s.SessionID, &s.PaneID, &s.WorkspaceKey, &s.ReadSource,
				timeDest{&s.Started}, timeDest{&s.Ended})
			return s, err
		})
}

// Observations returns every observation, by id.
func Observations(ctx context.Context, tx *sql.Tx) ([]Observation, error) {
	return query(ctx, tx, `SELECT id, capture_identity, source, segment_id, event, excerpt, truncated, status, at
		FROM observations ORDER BY id`,
		func(rows *sql.Rows) (Observation, error) {
			var o Observation
			err := rows.Scan(&o.ID, &o.CaptureIdentity, &o.Source, &o.SegmentID, &o.Event, &o.Excerpt,
				&o.Truncated, &o.Status, timeDest{&o.At})
			return o, err
		})
}

func query[T any](ctx context.Context, tx *sql.Tx, q string, scan func(*sql.Rows) (T, error)) ([]T, error) {
	rows, err := tx.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("read capture: %w", err)
	}
	defer rows.Close() //nolint:errcheck // read-only: rows.Err reports the read errors
	var ts []T
	for rows.Next() {
		t, err := scan(rows)
		if err != nil {
			return nil, fmt.Errorf("read capture: %w", err)
		}
		ts = append(ts, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read capture: %w", err)
	}
	return ts, nil
}

// nullText is the column value of s: NULL for "".
func nullText(s string) any {
	if s == "" {
		return nil
	}
	return s
}
