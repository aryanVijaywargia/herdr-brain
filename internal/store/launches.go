package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// EndReason is launches.end_reason: why brained ended a launch record (E24-S3).
type EndReason string

// The end reasons.
const (
	// EndAgentExit: herdr saw the agent leave the pane, or the launch process is gone, as after
	// a herdr restart that restores the pane with its old id.
	EndAgentExit  EndReason = "agent exit"
	EndPaneExited EndReason = "pane exited" // the pane's process exited
	// EndPaneClosed: the pane closed. A move to another workspace closes the old pane id too:
	// herdr gives the moved pane a new id there.
	EndPaneClosed EndReason = "pane closed"
	EndPaneGone   EndReason = "pane gone" // a reconcile did not find the pane, for example after a brained restart
)

// Launch is one launch record: a `brain claude` or `brain codex` run in a herdr pane (E24-S3).
type Launch struct {
	ID          int64
	IDHash      string // the hex SHA-256 of the launch ID; the store never holds the ID
	Socket      string // the herdr API socket of the herdr server
	WorkspaceID string
	PaneID      string
	CLI         string // the adapter kind, for example "claude"
	// NativeSessionID is the current native session of the launch: the last one that
	// SetLaunchSession set; "" until the first.
	NativeSessionID string
	Started         time.Time
	Ended           time.Time // zero while the launch runs
	EndReason       EndReason // "" while the launch runs
	// CodexHome is the absolute Codex home of a Codex launch, where its rollouts are; "" for
	// another CLI (E24-S5).
	CodexHome string
	Captured  time.Time // when brained finished the capture sources of the ended launch; zero until then
	// PID is the process id of the launch: the process of `brain <kind>`, which runs the CLI
	// in its place (exec). 0 for a record from before the pid column.
	PID int
	// Server is the identity of the herdr API socket file when the launch started: herdr makes
	// a new one at each server start. "" for a record from before the server column.
	Server string
}

const launchColumns = `id, id_hash, socket, workspace_id, pane_id, cli_kind, native_session_id, started, ended,
	COALESCE(end_reason, ''), codex_home, captured, pid, server`

func scanLaunch(row interface{ Scan(...any) error }) (Launch, error) {
	var l Launch
	err := row.Scan(&l.ID, &l.IDHash, &l.Socket, &l.WorkspaceID, &l.PaneID, &l.CLI, &l.NativeSessionID,
		timeDest{&l.Started}, timeDest{&l.Ended}, &l.EndReason, &l.CodexHome, timeDest{&l.Captured}, &l.PID, &l.Server)
	return l, err
}

// InsertLaunch adds the running launch l and returns its new id. Its ID, Ended, EndReason and
// Captured are not used.
func InsertLaunch(ctx context.Context, tx *sql.Tx, l Launch) (int64, error) {
	return insertRow(ctx, tx, "launch", `INSERT INTO launches (id_hash, socket, workspace_id, pane_id, cli_kind, started, codex_home, pid, server)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, l.IDHash, l.Socket, l.WorkspaceID, l.PaneID, l.CLI, timeArg(l.Started), l.CodexHome, l.PID, l.Server)
}

// LaunchByHash reads the launch record with the ID hash hash. ok is false when there is none.
func LaunchByHash(ctx context.Context, tx *sql.Tx, hash string) (l Launch, ok bool, err error) {
	l, err = scanLaunch(tx.QueryRowContext(ctx, `SELECT `+launchColumns+` FROM launches WHERE id_hash = ?`, hash))
	if errors.Is(err, sql.ErrNoRows) {
		return Launch{}, false, nil
	}
	if err != nil {
		return Launch{}, false, fmt.Errorf("read launch: %w", err)
	}
	return l, true, nil
}

// LaunchByID reads launch record id. ok is false when there is none.
func LaunchByID(ctx context.Context, tx *sql.Tx, id int64) (l Launch, ok bool, err error) {
	l, err = scanLaunch(tx.QueryRowContext(ctx, `SELECT `+launchColumns+` FROM launches WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Launch{}, false, nil
	}
	if err != nil {
		return Launch{}, false, fmt.Errorf("read launch %d: %w", id, err)
	}
	return l, true, nil
}

// RunningLaunches returns the launch records of the herdr server socket that have no end, by
// id.
func RunningLaunches(ctx context.Context, tx *sql.Tx, socket string) ([]Launch, error) {
	var ls []Launch
	err := eachRow(ctx, tx, `SELECT `+launchColumns+` FROM launches WHERE socket = ? AND ended IS NULL ORDER BY id`,
		func(rows *sql.Rows) error {
			l, err := scanLaunch(rows)
			ls = append(ls, l)
			return err
		}, socket)
	if err != nil {
		return nil, fmt.Errorf("read running launches: %w", err)
	}
	return ls, nil
}

// EndLaunch ends launch record id at at with reason r. A record with an end keeps its first
// end.
func EndLaunch(ctx context.Context, tx *sql.Tx, id int64, r EndReason, at time.Time) error {
	if _, err := tx.ExecContext(ctx, `UPDATE launches SET ended = ?, end_reason = ? WHERE id = ? AND ended IS NULL`,
		timeArg(at), string(r), id); err != nil {
		return fmt.Errorf("end launch %d: %w", id, err)
	}
	return nil
}

// CaptureSource is launch_sessions.source: how Brain captures one native session of a launch
// (E24-S5).
type CaptureSource string

// The capture sources, from the weakest to the strongest.
const (
	CaptureScreen     CaptureSource = "screen"     // only the pane reads
	CaptureTranscript CaptureSource = "transcript" // the exact Codex rollout of the thread
	CaptureHooks      CaptureSource = "hooks"      // Brain's private hooks of the launch
)

// sourceRank is the SQL rank of the capture source in column col: a source only rises.
func sourceRank(col string) string {
	return `CASE ` + col + ` WHEN 'hooks' THEN 2 WHEN 'transcript' THEN 1 ELSE 0 END`
}

// LaunchSession is one native session of a launch.
type LaunchSession struct {
	ID        string // the CLI's session id
	Source    CaptureSource
	FirstSeen time.Time
}

// SetLaunchSession makes session the current native session of launch record id, and adds it
// to the sessions of the launch with capture source src, first seen at at (AddLaunchSession).
// Each new session is accepted: /clear and an in-session resume start a new session in the
// same CLI process.
func SetLaunchSession(ctx context.Context, tx *sql.Tx, id int64, session string, src CaptureSource, at time.Time) error {
	if session == "" {
		return errors.New("set launch session: empty session id")
	}
	res, err := tx.ExecContext(ctx, `UPDATE launches SET native_session_id = ? WHERE id = ?`, session, id)
	if err != nil {
		return fmt.Errorf("set session of launch %d: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("set session of launch %d: %w", id, err)
	}
	if n == 0 {
		return fmt.Errorf("set session of launch %d: no such launch", id)
	}
	return AddLaunchSession(ctx, tx, id, session, src, at)
}

// AddLaunchSession adds session to the sessions of launch record id with capture source src,
// first seen at at, and leaves the current session of the launch as it is. A known session
// keeps its first time, and its source only rises: screen, transcript, hooks.
func AddLaunchSession(ctx context.Context, tx *sql.Tx, id int64, session string, src CaptureSource, at time.Time) error {
	if session == "" {
		return errors.New("add launch session: empty session id")
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO launch_sessions (launch_id, native_session_id, source, first_seen)
		VALUES (?, ?, ?, ?) ON CONFLICT (launch_id, native_session_id) DO UPDATE SET source = excluded.source
		WHERE `+sourceRank("excluded.source")+` > `+sourceRank("launch_sessions.source"),
		id, session, string(src), timeArg(at))
	if err != nil {
		return fmt.Errorf("add session of launch %d: %w", id, err)
	}
	return nil
}

// LaunchesToCapture returns the launch records of the herdr server socket that ended at or
// after since and whose capture sources brained has not finished, by id.
func LaunchesToCapture(ctx context.Context, tx *sql.Tx, socket string, since time.Time) ([]Launch, error) {
	var ls []Launch
	err := eachRow(ctx, tx, `SELECT `+launchColumns+` FROM launches WHERE socket = ? AND ended >= ? AND captured IS NULL
		ORDER BY id`, func(rows *sql.Rows) error {
		l, err := scanLaunch(rows)
		ls = append(ls, l)
		return err
	}, socket, timeArg(since))
	if err != nil {
		return nil, fmt.Errorf("read launches to capture: %w", err)
	}
	return ls, nil
}

// SetLaunchCaptured notes that brained finished the capture sources of launch record id at at.
func SetLaunchCaptured(ctx context.Context, tx *sql.Tx, id int64, at time.Time) error {
	if _, err := tx.ExecContext(ctx, `UPDATE launches SET captured = ? WHERE id = ?`, timeArg(at), id); err != nil {
		return fmt.Errorf("set capture of launch %d: %w", id, err)
	}
	return nil
}

// LaunchCapture is one capture session that herdr named in the pane of a launch.
type LaunchCapture struct {
	SessionID      int64
	AgentSessionID string
	Generation     int64
	First          time.Time // the start of its first segment in the pane
}

// LaunchCaptures returns the capture sessions of launch l in binding herdrSession, by id: the
// sessions of the CLI of l with an agent session id from herdr and a segment in the pane of l
// that started at or after the launch. A resumed session can be older than the launch, so its
// segments count, not its start.
func LaunchCaptures(ctx context.Context, tx *sql.Tx, herdrSession string, l Launch) ([]LaunchCapture, error) {
	var cs []LaunchCapture
	err := eachRow(ctx, tx, `SELECT n.id, n.agent_session_id, n.generation, min(s.started)
		FROM sessions n JOIN segments s ON s.session_id = n.id
		WHERE n.herdr_session = ? AND n.harness = ? AND n.agent_session_id IS NOT NULL AND s.pane_id = ? AND s.started >= ?
		GROUP BY n.id ORDER BY n.id`, func(rows *sql.Rows) error {
		var c LaunchCapture
		err := rows.Scan(&c.SessionID, &c.AgentSessionID, &c.Generation, timeDest{&c.First})
		cs = append(cs, c)
		return err
	}, herdrSession, l.CLI, l.PaneID, timeArg(l.Started))
	if err != nil {
		return nil, fmt.Errorf("read capture sessions of launch %d: %w", l.ID, err)
	}
	return cs, nil
}

// PaneSegments returns the segments of session sessionID in pane paneID that started at or
// after since, by id.
func PaneSegments(ctx context.Context, tx *sql.Tx, sessionID int64, paneID string, since time.Time) ([]Segment, error) {
	return query(ctx, tx, `SELECT `+segmentColumns+` FROM segments WHERE session_id = ? AND pane_id = ? AND started >= ?
		ORDER BY id`, scanSegment, sessionID, paneID, timeArg(since))
}

// LaunchSessions returns the native sessions of launch record id, in the order Brain first saw
// them.
func LaunchSessions(ctx context.Context, tx *sql.Tx, id int64) ([]LaunchSession, error) {
	var ss []LaunchSession
	err := eachRow(ctx, tx, `SELECT native_session_id, source, first_seen FROM launch_sessions WHERE launch_id = ?
		ORDER BY first_seen, native_session_id`, func(rows *sql.Rows) error {
		var s LaunchSession
		err := rows.Scan(&s.ID, &s.Source, timeDest{&s.FirstSeen})
		ss = append(ss, s)
		return err
	}, id)
	if err != nil {
		return nil, fmt.Errorf("read sessions of launch %d: %w", id, err)
	}
	return ss, nil
}
