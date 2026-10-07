package store

import (
	"context"
	"database/sql"
)

// SignalInput is one observation as the signal rules read it (§7.1): the observation, its
// session and workspace, and its command record when it has one.
type SignalInput struct {
	Observation
	SessionID    int64
	WorkspaceKey string         // the workspace of the segment; "" when it has no Brain key
	Command      *CommandRecord // nil when the observation has no command record
}

// SignalInputs returns every observation with its session, workspace and command record, by
// time and id.
func SignalInputs(ctx context.Context, tx *sql.Tx) ([]SignalInput, error) {
	return query(ctx, tx, `SELECT o.id, o.capture_identity, o.source, o.segment_id, o.event, o.excerpt, o.truncated,
		o.status, o.sender, o.counts, o.brain_origin, o.at, COALESCE(o.enriched_by, 0), s.session_id,
		COALESCE(s.workspace_key, ''), c.observation_id IS NOT NULL, COALESCE(c.argv, ''), COALESCE(c.cwd, ''),
		c.exit_status, COALESCE(c.truncated, 0), COALESCE(c.secret_found, 0)
		FROM observations o JOIN segments s ON s.id = o.segment_id
		LEFT JOIN command_records c ON c.observation_id = o.id ORDER BY o.at, o.id`,
		func(rows *sql.Rows) (SignalInput, error) {
			var in SignalInput
			var c CommandRecord
			var hasCommand bool
			var exit sql.NullInt64
			o := &in.Observation
			err := rows.Scan(&o.ID, &o.CaptureIdentity, &o.Source, &o.SegmentID, &o.Event, &o.Excerpt, &o.Truncated,
				&o.Status, &o.Sender, &o.Counts, &o.BrainOrigin, timeDest{&o.At}, &o.EnrichedBy, &in.SessionID,
				&in.WorkspaceKey, &hasCommand, &c.Argv, &c.CWD, &exit, &c.Truncated, &c.SecretFound)
			if !hasCommand {
				return in, err
			}
			c.ObservationID = o.ID
			if exit.Valid {
				n := int(exit.Int64)
				c.ExitStatus = &n
			}
			in.Command = &c
			return in, err
		})
}

// RootedWorkspaces returns the live workspaces of binding herdrSession that have a root
// folder, by key. The revert rule reads git there (#283, §7.1).
func RootedWorkspaces(ctx context.Context, tx *sql.Tx, herdrSession string) ([]Workspace, error) {
	return query(ctx, tx, `SELECT key, herdr_session, herdr_workspace_id, label, root_folder, status, last_seen
		FROM workspaces WHERE herdr_session = ? AND root_folder IS NOT NULL AND status = ? ORDER BY key`,
		func(rows *sql.Rows) (Workspace, error) {
			var w Workspace
			err := rows.Scan(&w.Key, &w.HerdrSession, &w.HerdrWorkspaceID, &w.Label, &w.RootFolder, &w.Status,
				timeDest{&w.LastSeen})
			return w, err
		}, herdrSession, WorkspaceLive)
}

// WorkspaceSegments returns the segments of workspace key in the sessions of binding
// herdrSession, newest first.
func WorkspaceSegments(ctx context.Context, tx *sql.Tx, herdrSession, key string) ([]Segment, error) {
	return query(ctx, tx, `SELECT g.id, g.session_id, g.pane_id, COALESCE(g.workspace_key, ''), g.read_source,
		g.started, g.ended FROM segments g JOIN sessions s ON s.id = g.session_id
		WHERE s.herdr_session = ? AND g.workspace_key = ? ORDER BY g.id DESC`,
		func(rows *sql.Rows) (Segment, error) {
			var s Segment
			err := rows.Scan(&s.ID, &s.SessionID, &s.PaneID, &s.WorkspaceKey, &s.ReadSource,
				timeDest{&s.Started}, timeDest{&s.Ended})
			return s, err
		}, herdrSession, key)
}
