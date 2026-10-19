-- Automations (E17-S10): one row for each item of kind 'automation', the id is the item id.
-- The row copies the binding of the item's current revision (store.Binding): the pinned
-- folder, the trigger condition, the CLI kind of the bound agent, the tool and the contract hash
-- of that tool that you approved. workspace_key is the one workspace of the binding. approved is
-- the time of your approval; it is NULL while the revision is not active. enabled is 1 (on) or
-- 0 (paused); brain automation pause and resume set it. last_result and last_run are the result
-- and the time of the newest run (E17-S11). A binding may run only while output.AutomationMayRun
-- says so. [NEW] the column names, trigger_condition and cli_kind. Forget clears the folder.
CREATE TABLE automations (
	id                INTEGER PRIMARY KEY REFERENCES items (id),
	workspace_key     TEXT NOT NULL REFERENCES workspaces (key),
	folder            TEXT NOT NULL,
	trigger_condition TEXT NOT NULL,
	cli_kind          TEXT NOT NULL,
	tool_id           INTEGER NOT NULL REFERENCES tools (id),
	contract_hash     TEXT NOT NULL,
	approved          TEXT,
	enabled           INTEGER NOT NULL CHECK (enabled IN (0, 1)),
	last_result       TEXT CHECK (last_result IN ('ok', 'failed', 'skipped', 'cancelled', 'unknown')),
	last_run          TEXT
) STRICT;
