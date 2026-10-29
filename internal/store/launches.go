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
	EndAgentExit  EndReason = "agent exit"  // herdr saw the agent leave the pane
	EndPaneExited EndReason = "pane exited" // the pane's process exited
	// EndPaneClosed: the pane closed. A move to another workspace closes the old pane id too:
	// herdr gives the moved pane a new id there.
	EndPaneClosed EndReason = "pane closed"
	EndPaneGone   EndReason = "pane gone" // a reconcile did not find the pane, for example after a brained restart
)

// Launch is one launch record: a `brain claude` or `brain codex` run in a herdr pane (E24-S3).
type Launch struct {
	ID              int64
	IDHash          string // the hex SHA-256 of the launch ID; the store never holds the ID
	Socket          string // the herdr API socket of the herdr server
	WorkspaceID     string
	PaneID          string
	CLI             string // the adapter kind, for example "claude"
	NativeSessionID string // "" until the first hook record or transcript line with it
	Started         time.Time
	Ended           time.Time // zero while the launch runs
	EndReason       EndReason // "" while the launch runs
}

const launchColumns = `id, id_hash, socket, workspace_id, pane_id, cli_kind, native_session_id, started, ended,
	COALESCE(end_reason, '')`

func scanLaunch(row interface{ Scan(...any) error }) (Launch, error) {
	var l Launch
	err := row.Scan(&l.ID, &l.IDHash, &l.Socket, &l.WorkspaceID, &l.PaneID, &l.CLI, &l.NativeSessionID,
		timeDest{&l.Started}, timeDest{&l.Ended}, &l.EndReason)
	return l, err
}

// InsertLaunch adds the running launch l and returns its new id. Its ID, Ended and EndReason
// are not used.
func InsertLaunch(ctx context.Context, tx *sql.Tx, l Launch) (int64, error) {
	return insertRow(ctx, tx, "launch", `INSERT INTO launches (id_hash, socket, workspace_id, pane_id, cli_kind, started)
		VALUES (?, ?, ?, ?, ?, ?)`, l.IDHash, l.Socket, l.WorkspaceID, l.PaneID, l.CLI, timeArg(l.Started))
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

// SetLaunchSession adds the native session id session to launch record id, when the record
// has none yet: the first hook record or transcript line with it decides. It returns the native
// session id of the record after the call, so a caller with another session id sees that it
// is not the launched session.
func SetLaunchSession(ctx context.Context, tx *sql.Tx, id int64, session string) (string, error) {
	if session == "" {
		return "", errors.New("set launch session: empty session id")
	}
	if _, err := tx.ExecContext(ctx, `UPDATE launches SET native_session_id = ? WHERE id = ? AND native_session_id = ''`,
		session, id); err != nil {
		return "", fmt.Errorf("set session of launch %d: %w", id, err)
	}
	var got string
	if err := tx.QueryRowContext(ctx, `SELECT native_session_id FROM launches WHERE id = ?`, id).Scan(&got); err != nil {
		return "", fmt.Errorf("read session of launch %d: %w", id, err)
	}
	return got, nil
}
