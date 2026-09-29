package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// LearningScope is where a learning switch applies (§6.7 "Exclusion").
type LearningScope string

// The learning scopes.
const (
	LearningStore     LearningScope = "store"
	LearningWorkspace LearningScope = "workspace"
	LearningPane      LearningScope = "pane"
)

// LearningSetting is one row of learning_settings: the last `brain learn on|off` of one scope
// and target (E9-S4).
type LearningSetting struct {
	Scope        LearningScope
	HerdrSession string // the herdr session of the binding, for a pane; else ""
	Target       string // the workspace key or the pane id; "" for the store
	On           bool
	Changed      time.Time
}

// SetLearning records s. It replaces the earlier setting of the same scope and target.
func SetLearning(ctx context.Context, tx *sql.Tx, s LearningSetting) error {
	// Each change time is later than every stored one, also within one millisecond of the
	// store's time precision: a reader of LearningChanged sees each off time (E11-S2).
	var last time.Time
	if err := tx.QueryRowContext(ctx, `SELECT MAX(changed) FROM learning_settings`).Scan(timeDest{&last}); err != nil {
		return fmt.Errorf("read learning settings: %w", err)
	}
	if !last.IsZero() && !s.Changed.Truncate(time.Millisecond).After(last) {
		s.Changed = last.Add(time.Millisecond)
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO learning_settings (scope, herdr_session, target, learning, changed)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (scope, herdr_session, target) DO UPDATE SET learning = excluded.learning, changed = excluded.changed`,
		s.Scope, s.HerdrSession, s.Target, onOff(s.On), timeArg(s.Changed))
	if err != nil {
		return fmt.Errorf("write learning setting: %w", err)
	}
	return nil
}

// LearningSettings returns every learning setting, the store first, then by workspace key,
// herdr session and pane id.
func LearningSettings(ctx context.Context, tx *sql.Tx) ([]LearningSetting, error) {
	rows, err := tx.QueryContext(ctx, `SELECT scope, herdr_session, target, learning, changed FROM learning_settings
		ORDER BY CASE scope WHEN 'store' THEN 0 WHEN 'workspace' THEN 1 ELSE 2 END, herdr_session, target`)
	if err != nil {
		return nil, fmt.Errorf("read learning settings: %w", err)
	}
	defer rows.Close() //nolint:errcheck // read-only: rows.Err reports the read errors
	var ss []LearningSetting
	for rows.Next() {
		var s LearningSetting
		var learning string
		if err := rows.Scan(&s.Scope, &s.HerdrSession, &s.Target, &learning, timeDest{&s.Changed}); err != nil {
			return nil, fmt.Errorf("read learning settings: %w", err)
		}
		s.On = learning == "on"
		ss = append(ss, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read learning settings: %w", err)
	}
	return ss, nil
}

// LearningOnWholeStore reports whether learning is on for the whole store: its store setting, or
// defaultOn when there is none. defaultOn is false for a test store (§12.1).
func LearningOnWholeStore(ctx context.Context, tx *sql.Tx, defaultOn bool) (bool, error) {
	var learning string
	err := tx.QueryRowContext(ctx, `SELECT learning FROM learning_settings WHERE scope = ?`, LearningStore).Scan(&learning)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return defaultOn, nil
	case err != nil:
		return false, fmt.Errorf("read learning setting: %w", err)
	}
	return learning == "on", nil
}

// LearningOn reports whether learning is on for pane paneID of the binding of herdr session
// session, in the workspace with key workspaceKey ("" when it has none). defaultOn is the store
// setting when there is no store row: false for a test store (§12.1). Any off setting that
// applies wins over a narrower on setting [NEW, design choice]. E11 calls it before it captures.
func LearningOn(ctx context.Context, tx *sql.Tx, defaultOn bool, session, workspaceKey, paneID string) (bool, error) {
	on, err := LearningOnWholeStore(ctx, tx, defaultOn)
	if err != nil || !on {
		return false, err
	}
	var offs int
	err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM learning_settings WHERE learning = 'off' AND (
		scope = ? AND target = ? AND ? != '' OR
		scope = ? AND herdr_session = ? AND target = ?)`,
		LearningWorkspace, workspaceKey, workspaceKey, LearningPane, session, paneID).Scan(&offs)
	if err != nil {
		return false, fmt.Errorf("read learning settings: %w", err)
	}
	return offs == 0, nil
}

// LearningChanged returns the latest change time of the learning settings that apply to pane
// paneID of herdr session session, in the workspace with key workspaceKey: the zero time when
// none applies. A newer time than a reader saw means that learning may have been off in
// between, so the reader starts again from a fresh baseline (E11-S2).
func LearningChanged(ctx context.Context, tx *sql.Tx, session, workspaceKey, paneID string) (time.Time, error) {
	var changed time.Time
	err := tx.QueryRowContext(ctx, `SELECT MAX(changed) FROM learning_settings WHERE scope = ? OR
		scope = ? AND target = ? AND ? != '' OR
		scope = ? AND herdr_session = ? AND target = ?`,
		LearningStore, LearningWorkspace, workspaceKey, workspaceKey, LearningPane, session, paneID).Scan(timeDest{&changed})
	if err != nil {
		return time.Time{}, fmt.Errorf("read learning settings: %w", err)
	}
	return changed, nil
}

func onOff(on bool) string {
	if on {
		return "on"
	}
	return "off"
}
