-- Automation runs (E17-S11): one row for each claimed stop event of an automation.
-- The pair (automation_id, event_key) is unique, so a second claim of one event inserts nothing
-- (claim before run). event_key is (pane, terminal_id, agent session id, state_change_seq) from
-- the agent get answer that joined the event (E17-S1 test). pane_id and workspace_id
-- are the herdr ids of the triggering pane; the run resolves that pane again. claimed_at is the
-- claim, started_at the launch mark, finished_at the end. result starts as unknown, so a crash
-- at any point leaves unknown; the run writes ok, failed or skipped, and brain automation
-- cancel writes cancelled. pid is the process that runs the tool, from the launch mark, for
-- the cancel.
CREATE TABLE automation_runs (
	id            INTEGER PRIMARY KEY,
	automation_id INTEGER NOT NULL REFERENCES automations (id),
	event_key     TEXT NOT NULL,
	pane_id       TEXT NOT NULL,
	workspace_id  TEXT NOT NULL,
	claimed_at    TEXT NOT NULL,
	started_at    TEXT,
	finished_at   TEXT,
	result        TEXT NOT NULL CHECK (result IN ('ok', 'failed', 'skipped', 'cancelled', 'unknown')),
	pid           INTEGER,
	UNIQUE (automation_id, event_key)
) STRICT;
