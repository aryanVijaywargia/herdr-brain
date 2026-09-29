package store

import (
	"context"
	"database/sql"
	"errors"
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
	Generation     int64     // the capture generation; forget raises it
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

// The observation sources.
const (
	SourceHerdr = "herdr" // a pane read
	SourceHook  = "hook"  // a CLI hook (E11-S5)
)

// Sender says who wrote the text of an observation (§6.5).
type Sender string

// The senders. Capture never sets SenderYou: only a review approval does (§6.3).
const (
	SenderYou         Sender = "you"
	SenderUnconfirmed Sender = "unconfirmed" // a captured message, or text with no known mark
	SenderAssistant   Sender = "assistant"   // agent reply text
	SenderTool        Sender = "tool"        // tool output
)

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
	Sender          Sender
	Counts          bool // it can raise a support count (§6.4)
	BrainOrigin     bool // Brain caused the text; it never counts
	At              time.Time
	EnrichedBy      int64 // the hook observation that adds detail to it (S35); 0 when none
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

// ErrForgotten says that brain forget --session raised the capture generation of a session
// after a pane read or a hook record noted it: the text may be forgotten text, so the write
// stores nothing (§6.7).
var ErrForgotten = errors.New("the session was forgotten after the text was collected")

// Generation returns the capture generation of session sessionID. A pane read and a hook
// record note it before they collect text.
func Generation(ctx context.Context, tx *sql.Tx, sessionID int64) (int64, error) {
	var gen int64
	if err := tx.QueryRowContext(ctx, `SELECT generation FROM sessions WHERE id = ?`, sessionID).Scan(&gen); err != nil {
		return 0, fmt.Errorf("read capture generation: %w", err)
	}
	return gen, nil
}

// Forgets returns the sum of the capture generations of all sessions. Each brain forget
// --session raises it, and session rows are never deleted, so a changed sum says that a forget
// ran in between. A pane read notes it before a baseline, when the session is not yet known.
func Forgets(ctx context.Context, tx *sql.Tx) (int64, error) {
	var n int64
	err := tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(generation), 0) FROM sessions`).Scan(&n)
	return n, err
}

// CheckGeneration returns ErrForgotten when the capture generation of session sessionID is
// no longer gen, the value that the writer noted before it collected its text.
func CheckGeneration(ctx context.Context, tx *sql.Tx, sessionID, gen int64) error {
	cur, err := Generation(ctx, tx, sessionID)
	if err != nil {
		return err
	}
	if cur != gen {
		return fmt.Errorf("session %d: %w", sessionID, ErrForgotten)
	}
	return nil
}

// SessionsByID returns the sessions of binding herdrSession whose key id or agent session id
// is id, by row id. One agent session can have two rows: a capture key that herdr enriched
// later, and an agent key (§6.2 step 6).
func SessionsByID(ctx context.Context, tx *sql.Tx, herdrSession, id string) ([]Session, error) {
	return sessions(ctx, tx, `WHERE herdr_session = ?1 AND (key_id = ?2 OR agent_session_id = ?2)`, herdrSession, id)
}

// Evidence counts the evidence of one session that forget deletes.
type Evidence struct {
	Observations   int
	CommandRecords int
	EvidenceLinks  int // links of its observations to candidates
}

// SessionEvidence counts the observations, the command records and the evidence links of
// session sessionID.
func SessionEvidence(ctx context.Context, tx *sql.Tx, sessionID int64) (Evidence, error) {
	var e Evidence
	err := tx.QueryRowContext(ctx, `SELECT count(o.id), count(c.observation_id),
		(SELECT count(*) FROM evidence_links WHERE session_id = ?1) FROM observations o
		JOIN segments s ON s.id = o.segment_id LEFT JOIN command_records c ON c.observation_id = o.id
		WHERE s.session_id = ?1`, sessionID).Scan(&e.Observations, &e.CommandRecords, &e.EvidenceLinks)
	if err != nil {
		return Evidence{}, fmt.Errorf("count evidence of session %d: %w", sessionID, err)
	}
	return e, nil
}

// ForgetSession deletes the evidence of session sessionID: its evidence links, observations,
// command records and cursors (§6.7). The candidates that used the links are counted again,
// with the default gates until a config sets them, and one with no evidence left goes. It raises the capture generation, so a
// pane read or a hook record that noted the old value stores nothing, and it keeps the
// capture identities of the deleted observations, so a replay stores nothing again. The
// session row and its segments hold no text, and they stay: the counter must not go back,
// and new text after the forget is captured as usual.
func ForgetSession(ctx context.Context, tx *sql.Tx, sessionID int64) error {
	if err := forgetEvidence(ctx, tx, sessionID, DefaultGates()); err != nil {
		return err
	}
	const obs = `SELECT o.id FROM observations o JOIN segments s ON s.id = o.segment_id WHERE s.session_id = ?1`
	for _, q := range []string{
		`INSERT INTO forgotten_captures (capture_identity) SELECT capture_identity FROM observations
			WHERE id IN (` + obs + `) ON CONFLICT DO NOTHING`,
		`DELETE FROM command_records WHERE observation_id IN (` + obs + `)`,
		`DELETE FROM observations WHERE id IN (` + obs + `)`,
		`DELETE FROM cursors WHERE session_id = ?1`,
		`UPDATE sessions SET generation = generation + 1 WHERE id = ?1`,
	} {
		if _, err := tx.ExecContext(ctx, q, sessionID); err != nil {
			return fmt.Errorf("forget session %d: %w", sessionID, err)
		}
	}
	return nil
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

// AddObservation adds o and returns its id. An observation with a known capture identity is
// a retry or a duplicate: it adds nothing, does not count again (§6.4), and the id is 0. So is
// the identity of an observation that forget deleted: its replay stores nothing (§6.7).
func AddObservation(ctx context.Context, tx *sql.Tx, o Observation) (int64, error) {
	sender := o.Sender
	if sender == "" {
		sender = SenderUnconfirmed
	}
	var id int64
	err := tx.QueryRowContext(ctx, `INSERT INTO observations
		(capture_identity, source, segment_id, event, excerpt, truncated, status, sender, counts, brain_origin, at)
		SELECT ?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8, ?9, ?10, ?11
		WHERE NOT EXISTS (SELECT 1 FROM forgotten_captures WHERE capture_identity = ?1)
		ON CONFLICT (capture_identity) DO NOTHING RETURNING id`,
		o.CaptureIdentity, o.Source, o.SegmentID, o.Event, o.Excerpt, o.Truncated, o.Status, sender,
		o.Counts && !o.BrainOrigin, o.BrainOrigin, timeArg(o.At)).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("add observation: %w", err)
	}
	return id, nil
}

// SeenExcerpt reports whether segment segmentID holds an observation of sender with excerpt.
func SeenExcerpt(ctx context.Context, tx *sql.Tx, segmentID int64, sender Sender, excerpt string) (bool, error) {
	var n int
	err := tx.QueryRowContext(ctx, `SELECT count(*) FROM observations
		WHERE segment_id = ? AND sender = ? AND excerpt = ? AND status != 'gap'`, segmentID, sender, excerpt).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("find excerpt: %w", err)
	}
	return n > 0, nil
}

// MoveCursor sets the cursor of session sessionID and source to position, the capture
// identity of its last observation. The caller moves it in the transaction that stores the
// observation (§6.4).
func MoveCursor(ctx context.Context, tx *sql.Tx, sessionID int64, source, position string) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO cursors (session_id, source, position) VALUES (?, ?, ?)
		ON CONFLICT (session_id, source) DO UPDATE SET position = excluded.position`, sessionID, source, position)
	if err != nil {
		return fmt.Errorf("move cursor: %w", err)
	}
	return nil
}

// Cursor returns the cursor position of session sessionID and source, or "" when it has none.
func Cursor(ctx context.Context, tx *sql.Tx, sessionID int64, source string) (string, error) {
	var pos string
	err := tx.QueryRowContext(ctx, `SELECT position FROM cursors WHERE session_id = ? AND source = ?`,
		sessionID, source).Scan(&pos)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read cursor: %w", err)
	}
	return pos, nil
}

// Sessions returns every session, by id.
func Sessions(ctx context.Context, tx *sql.Tx) ([]Session, error) {
	return sessions(ctx, tx, "")
}

// sessions returns the sessions that match the clause where with args, by id.
func sessions(ctx context.Context, tx *sql.Tx, where string, args ...any) ([]Session, error) {
	return query(ctx, tx, `SELECT id, herdr_session, harness, key_kind, key_id, COALESCE(agent_session_id, ''),
		pane_id, COALESCE(workspace_key, ''), started, ended, generation FROM sessions `+where+` ORDER BY id`,
		func(rows *sql.Rows) (Session, error) {
			var s Session
			err := rows.Scan(&s.ID, &s.Key.HerdrSession, &s.Key.Harness, &s.Key.Kind, &s.Key.ID, &s.AgentSessionID,
				&s.PaneID, &s.WorkspaceKey, timeDest{&s.Started}, timeDest{&s.Ended}, &s.Generation)
			return s, err
		}, args...)
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
	return query(ctx, tx, `SELECT id, capture_identity, source, segment_id, event, excerpt, truncated, status,
		sender, counts, brain_origin, at, COALESCE(enriched_by, 0) FROM observations ORDER BY id`,
		func(rows *sql.Rows) (Observation, error) {
			var o Observation
			err := rows.Scan(&o.ID, &o.CaptureIdentity, &o.Source, &o.SegmentID, &o.Event, &o.Excerpt,
				&o.Truncated, &o.Status, &o.Sender, &o.Counts, &o.BrainOrigin, timeDest{&o.At}, &o.EnrichedBy)
			return o, err
		})
}

func query[T any](ctx context.Context, tx *sql.Tx, q string, scan func(*sql.Rows) (T, error), args ...any) ([]T, error) {
	rows, err := tx.QueryContext(ctx, q, args...)
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
