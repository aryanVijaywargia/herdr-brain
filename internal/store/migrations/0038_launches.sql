-- Launch records (E24-S3): one row for each `brain claude` or `brain codex` launch in a herdr
-- pane. id_hash is the SHA-256 of the 128-bit launch ID, in hex: the ID itself is only in the
-- 0600 file of the launch folder <store>/launches/<id>. socket is the herdr API socket of the
-- herdr server, and workspace_id and pane_id are the pane of the launch. cli_kind is the adapter
-- kind of the CLI. native_session_id is the CLI's own session id (Claude session_id, Codex thread
-- id); '' until the first hook record or transcript line with it. brained sets ended and
-- end_reason when the agent exits, or the pane closes or is gone. AUTOINCREMENT: an id is never
-- used again, so a launch folder never gets the name of an older one.
CREATE TABLE launches (
	id                INTEGER PRIMARY KEY AUTOINCREMENT,
	id_hash           TEXT NOT NULL UNIQUE CHECK (length(id_hash) = 64),
	socket            TEXT NOT NULL,
	workspace_id      TEXT NOT NULL,
	pane_id           TEXT NOT NULL,
	cli_kind          TEXT NOT NULL CHECK (cli_kind <> ''),
	native_session_id TEXT NOT NULL DEFAULT '',
	started           TEXT NOT NULL,
	ended             TEXT,
	end_reason        TEXT CHECK (end_reason IN ('agent exit', 'pane exited', 'pane closed', 'pane gone')),
	CHECK ((ended IS NULL) = (end_reason IS NULL))
) STRICT;

CREATE INDEX launches_open ON launches (socket, pane_id) WHERE ended IS NULL;
