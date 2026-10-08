package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// maxFailed is the most failed commands that the brief names of one session (#273 AC 2).
const maxFailed = 3

// Handoff is what the brief names of the last session of a workspace below its line (E20-S8,
// §8.3 "Handoff lines"). Brain reads no transcript for it (§6.6).
type Handoff struct {
	// Approved are the items of the brief whose revision in it you approved at or after the
	// start of that session, in id order.
	Approved []int64
	// Failed are the commands whose last run in that session failed, newest first, at most
	// maxFailed.
	Failed []FailedCommand
}

// FailedCommand is a command whose newest command record in a session has an exit status
// that is not 0: its filtered argv (§6.7) and that exit status.
type FailedCommand struct {
	Argv       string
	ExitStatus int
}

// ReadHandoff reads the handoff of last, the receipt that LastSession gave for workspace key,
// for a brief with the item revisions revisions.
func ReadHandoff(ctx context.Context, tx *sql.Tx, key string, last Delivery, revisions []RevisionRef) (Handoff, error) {
	start, err := sessionStart(ctx, tx, key, last)
	if err != nil {
		return Handoff{}, err
	}
	var h Handoff
	if h.Approved, err = approvedSince(ctx, tx, revisions, start); err != nil {
		return Handoff{}, err
	}
	h.Failed, err = failedCommands(ctx, tx, key, last)
	return h, err
}

// sessionStart is the time of the oldest receipt of the agent session of last in workspace key.
// A CLI can write more receipts for one agent session, as Claude does after a resume or a
// compact (§8.3), so last can be a later one. A receipt with no transcript path counts too: it
// can be the first one of the session.
func sessionStart(ctx context.Context, tx *sql.Tx, key string, last Delivery) (time.Time, error) {
	var start time.Time
	err := tx.QueryRowContext(ctx, `SELECT min(at) FROM deliveries
		WHERE workspace_key = ?1 AND (id = ?2 OR agent_session_id = ?3)`,
		key, last.ID, last.AgentSessionID).Scan(timeDest{&start})
	if err != nil {
		return time.Time{}, fmt.Errorf("read start of the session of receipt %d: %w", last.ID, err)
	}
	return start, nil
}

// approvedSince returns the items of revisions whose revision there you approved at or after
// since, in id order: a revision of an approved proposal (§7.4). A pin, an undo or a restore
// writes a revision with no proposal, so an approval that you undid does not count.
func approvedSince(ctx context.Context, tx *sql.Tx, revisions []RevisionRef, since time.Time) ([]int64, error) {
	return query(ctx, tx, `SELECT r.item_id FROM json_each(?1) j JOIN item_revisions r
		ON r.item_id = json_extract(j.value, '$.item') AND r.revision = json_extract(j.value, '$.revision')
		WHERE r.author = ?2 AND r.proposal_id IS NOT NULL AND r.created >= ?3 ORDER BY r.item_id`,
		scanID, jsonList[RevisionRef](revisions), AuthorYou, timeArg(since))
}

// failedCommands returns the commands whose newest command record in the agent session of last
// failed, newest first, at most maxFailed. The records come from the capture sessions with the
// binding, the harness and the agent session id of last, in segments of workspace key, so the
// commands after a pane move to another workspace never show (§6.2). A record of Brain text
// (§6.3) or with no argv (a secret, §6.7) does not count. With one max() aggregate, SQLite
// takes exit_status from the row of max(o.id): the newest run of the command.
func failedCommands(ctx context.Context, tx *sql.Tx, key string, last Delivery) ([]FailedCommand, error) {
	return query(ctx, tx, `SELECT argv, exit_status FROM (
		SELECT c.argv, c.exit_status, max(o.id) AS newest FROM sessions n
		JOIN segments g ON g.session_id = n.id JOIN observations o ON o.segment_id = g.id
		JOIN command_records c ON c.observation_id = o.id
		WHERE n.herdr_session = ?1 AND n.harness = ?2 AND n.agent_session_id = ?3 AND g.workspace_key = ?4
		AND o.brain_origin = 0 AND c.argv IS NOT NULL GROUP BY c.argv)
		WHERE exit_status != 0 ORDER BY newest DESC LIMIT ?5`,
		func(rows *sql.Rows) (FailedCommand, error) {
			var f FailedCommand
			err := rows.Scan(&f.Argv, &f.ExitStatus)
			return f, err
		}, last.HerdrSession, last.Profile, last.AgentSessionID, key, maxFailed)
}
