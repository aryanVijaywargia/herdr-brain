package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// CallOrigin says who called a tool (§11 tool_calls).
type CallOrigin string

// The call origins.
const (
	OriginAgent      CallOrigin = "agent"      // brain run in the agent's shell
	OriginAutomation CallOrigin = "automation" // the automation runner (E17-S11)
)

// CallResult is the result of a tool call. Only CallRan is a real call (§10.2).
type CallResult string

// The call results.
const (
	CallRan     CallResult = "ran"
	CallRefused CallResult = "refused" // a check failed, so nothing ran
)

// ToolCall is one row of tool_calls (§11).
type ToolCall struct {
	ID     int64
	ToolID int64
	// ContractHash is the hash of the checked contract that the call used; for a refused call
	// the approved hash, "" when the tool has none.
	ContractHash string
	SessionID    int64 // the sessions row of the calling pane; 0 when capture has none
	Origin       CallOrigin
	Result       CallResult
	At           time.Time
	ExitCode     *int // nil when refused
	Duration     time.Duration
}

// AddToolCall adds c and returns its id.
func AddToolCall(ctx context.Context, tx *sql.Tx, c ToolCall) (int64, error) {
	var exit, ms any
	if c.Result == CallRan {
		ms = c.Duration.Milliseconds()
	}
	if c.ExitCode != nil {
		exit = *c.ExitCode
	}
	var id int64
	err := tx.QueryRowContext(ctx, `INSERT INTO tool_calls (tool_id, contract_hash, session_id, origin, result, at,
		exit_code, duration_ms) VALUES (?, ?, ?, ?, ?, ?, ?, ?) RETURNING id`,
		c.ToolID, nullText(c.ContractHash), sql.NullInt64{Int64: c.SessionID, Valid: c.SessionID != 0}, c.Origin,
		c.Result, timeArg(c.At), exit, ms).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("add call of tool %d: %w", c.ToolID, err)
	}
	return id, nil
}

// ToolCalls returns every tool call, in id order.
func ToolCalls(ctx context.Context, tx *sql.Tx) ([]ToolCall, error) {
	return query(ctx, tx, `SELECT id, tool_id, COALESCE(contract_hash, ''), COALESCE(session_id, 0), origin, result,
		at, exit_code, COALESCE(duration_ms, 0) FROM tool_calls ORDER BY id`,
		func(rows *sql.Rows) (ToolCall, error) {
			var c ToolCall
			var exit sql.NullInt64
			var ms int64
			err := rows.Scan(&c.ID, &c.ToolID, &c.ContractHash, &c.SessionID, &c.Origin, &c.Result,
				timeDest{&c.At}, &exit, &ms)
			if exit.Valid {
				n := int(exit.Int64)
				c.ExitCode = &n
			}
			c.Duration = time.Duration(ms) * time.Millisecond
			return c, err
		})
}

// RanCalls counts the ran calls of each tool since since, by tool id (§13 M4 review prompt).
// A refused call is no real call (§10.2), so it does not count.
func RanCalls(ctx context.Context, tx *sql.Tx, since time.Time) (map[int64]int, error) {
	n := make(map[int64]int)
	err := eachRow(ctx, tx, `SELECT tool_id, count(*) FROM tool_calls WHERE result = ? AND at >= ? GROUP BY tool_id`,
		func(rows *sql.Rows) error {
			var id int64
			var c int
			if err := rows.Scan(&id, &c); err != nil {
				return err
			}
			n[id] = c
			return nil
		}, CallRan, timeArg(since))
	if err != nil {
		return nil, fmt.Errorf("count tool calls: %w", err)
	}
	return n, nil
}

// PaneSession returns the id of the newest open capture session of pane paneID in binding
// herdrSession, or 0 when capture has none. A tool call takes its session key from it.
func PaneSession(ctx context.Context, tx *sql.Tx, herdrSession, paneID string) (int64, error) {
	var id int64
	err := tx.QueryRowContext(ctx, `SELECT id FROM sessions WHERE herdr_session = ? AND pane_id = ? AND ended IS NULL
		ORDER BY id DESC LIMIT 1`, herdrSession, paneID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("find session of pane %s: %w", paneID, err)
	}
	return id, nil
}
