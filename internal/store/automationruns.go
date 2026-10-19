package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// RunResult is the result of an automation run.
type RunResult string

// The run results. A run starts as RunUnknown, so a crash at any point leaves it.
const (
	RunOK        RunResult = "ok"
	RunFailed    RunResult = "failed"
	RunSkipped   RunResult = "skipped"
	RunCancelled RunResult = "cancelled"
	RunUnknown   RunResult = "unknown"
)

// AutomationRun is one automation_runs row: one claimed stop event of an automation.
type AutomationRun struct {
	ID           int64
	AutomationID int64
	EventKey     string
	PaneID       string // the herdr pane of the event
	WorkspaceID  string // the herdr workspace of the event
	Claimed      time.Time
	Started      time.Time // the launch mark; zero before it
	Finished     time.Time // zero while the run is not over
	Result       RunResult
	PID          int // the process that runs the tool; 0 before the launch mark
	// Cause is why a failed run failed: the failed step and its exit code, or the refusal of
	// the call (#352). No output of the tool. "" for the other results.
	Cause string
}

// Active reports whether the run has its launch mark and no end: its tool may run now.
func (r AutomationRun) Active() bool {
	return !r.Started.IsZero() && r.Finished.IsZero()
}

// ErrRunNotActive means that a run has no launch mark, or has ended.
var ErrRunNotActive = errors.New("the run is not active")

// ClaimRun inserts the claim of run r and returns its id, or 0 when the event key of the
// automation has a row already: a duplicate event claims nothing.
func ClaimRun(ctx context.Context, tx *sql.Tx, r AutomationRun) (int64, error) {
	var id int64
	err := tx.QueryRowContext(ctx, `INSERT INTO automation_runs (automation_id, event_key, pane_id, workspace_id,
		claimed_at, result) VALUES (?, ?, ?, ?, ?, ?) ON CONFLICT (automation_id, event_key) DO NOTHING RETURNING id`,
		r.AutomationID, r.EventKey, r.PaneID, r.WorkspaceID, timeArg(r.Claimed), RunUnknown).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("claim run of automation %d: %w", r.AutomationID, err)
	}
	return id, nil
}

// StartRun writes the launch mark of run id with the process pid that runs the tool.
// The automation's last result is unknown from now until the run ends.
func StartRun(ctx context.Context, tx *sql.Tx, id int64, pid int, now time.Time) error {
	r, err := AutomationRunByID(ctx, tx, id)
	if err != nil {
		return err
	}
	if !r.Started.IsZero() || !r.Finished.IsZero() {
		return fmt.Errorf("run %d: started or ended already", id)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE automation_runs SET started_at = ?, pid = ? WHERE id = ?`,
		timeArg(now), pid, id); err != nil {
		return fmt.Errorf("start run %d: %w", id, err)
	}
	return setLastRun(ctx, tx, r.AutomationID, id, RunUnknown, now)
}

// FinishRun writes result and its cause to run id when the run has not ended, and the result
// to its automation as the last result when the run has a launch mark. It reports whether it
// wrote: a cancel that came first keeps cancelled. The cause names a program of the tool, so a
// run that ends after forget of its tool or its automation keeps no cause.
func FinishRun(ctx context.Context, tx *sql.Tx, id int64, result RunResult, cause string, now time.Time) (bool, error) {
	r, err := AutomationRunByID(ctx, tx, id)
	if err != nil || !r.Finished.IsZero() {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE automation_runs SET finished_at = ?, result = ?,
		cause = CASE WHEN EXISTS (SELECT 1 FROM automations a JOIN tools t ON t.id = a.tool_id
			WHERE a.id = automation_runs.automation_id AND a.folder != '' AND t.contract != '') THEN NULLIF(?, '') END
		WHERE id = ?`, timeArg(now), result, cause, id); err != nil {
		return false, fmt.Errorf("finish run %d: %w", id, err)
	}
	if r.Started.IsZero() {
		return true, nil // a run with no launch mark never ran: the last result stays
	}
	return true, setLastRun(ctx, tx, r.AutomationID, id, result, now)
}

// CancelRun records cancelled for active run id and returns the run. It fails with
// ErrRunNotActive when the run has no launch mark or has ended.
func CancelRun(ctx context.Context, tx *sql.Tx, id int64, now time.Time) (AutomationRun, error) {
	r, err := AutomationRunByID(ctx, tx, id)
	if err != nil {
		return AutomationRun{}, err
	}
	if !r.Active() {
		return AutomationRun{}, fmt.Errorf("run %d (%s): %w", id, r.Result, ErrRunNotActive)
	}
	if _, err := FinishRun(ctx, tx, id, RunCancelled, "", now); err != nil {
		return AutomationRun{}, err
	}
	return r, nil
}

// setLastRun writes result as the last result of automation, unless a newer run of it has a
// launch mark: an older run that ends later does not hide the result of a newer one.
func setLastRun(ctx context.Context, tx *sql.Tx, automation, run int64, result RunResult, now time.Time) error {
	if _, err := tx.ExecContext(ctx, `UPDATE automations SET last_result = ?, last_run = ? WHERE id = ?
		AND NOT EXISTS (SELECT 1 FROM automation_runs WHERE automation_id = ? AND id > ? AND started_at IS NOT NULL)`,
		result, timeArg(now), automation, automation, run); err != nil {
		return fmt.Errorf("write last run of automation %d: %w", automation, err)
	}
	return nil
}

const runColumns = `id, automation_id, event_key, pane_id, workspace_id, claimed_at, started_at, finished_at, result,
	COALESCE(pid, 0), COALESCE(cause, '')`

func scanRun(row interface{ Scan(...any) error }) (AutomationRun, error) {
	var r AutomationRun
	err := row.Scan(&r.ID, &r.AutomationID, &r.EventKey, &r.PaneID, &r.WorkspaceID, timeDest{&r.Claimed},
		timeDest{&r.Started}, timeDest{&r.Finished}, &r.Result, &r.PID, &r.Cause)
	return r, err
}

// AutomationRunByID reads run id. It returns sql.ErrNoRows when there is none.
func AutomationRunByID(ctx context.Context, tx *sql.Tx, id int64) (AutomationRun, error) {
	r, err := scanRun(tx.QueryRowContext(ctx, `SELECT `+runColumns+` FROM automation_runs WHERE id = ?`, id))
	if err != nil {
		return AutomationRun{}, fmt.Errorf("read run %d: %w", id, err)
	}
	return r, nil
}

// AutomationRuns reads every run, in id order.
func AutomationRuns(ctx context.Context, tx *sql.Tx) ([]AutomationRun, error) {
	return query(ctx, tx, `SELECT `+runColumns+` FROM automation_runs ORDER BY id`,
		func(rows *sql.Rows) (AutomationRun, error) { return scanRun(rows) })
}

// LastRun reads the newest run of automation id that has a launch mark, and false when it has
// none. A skipped run never ran, so it does not hide the result of an older run.
func LastRun(ctx context.Context, tx *sql.Tx, id int64) (AutomationRun, bool, error) {
	r, err := scanRun(tx.QueryRowContext(ctx, `SELECT `+runColumns+` FROM automation_runs WHERE automation_id = ?
		AND started_at IS NOT NULL ORDER BY id DESC LIMIT 1`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return AutomationRun{}, false, nil
	}
	if err != nil {
		return AutomationRun{}, false, fmt.Errorf("read last run of automation %d: %w", id, err)
	}
	return r, true, nil
}
