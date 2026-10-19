-- Pane capture (E11-S3). brained reads the panes of one herdr session and keeps the new part of
-- each read.
-- - sessions: one agent session in one binding. Every key holds the canonical binding
--   (herdr_session, E2-S2), so two herdr sessions with the same pane ids and the same agent
--   session ids never share a row. key_kind 'agent': key_id is the agent session id from
--   herdr. key_kind 'capture': key_id is a local capture id that brained makes when it first
--   sees the agent in a pane, before any evidence. agent_session_id is the id from herdr: for
--   a capture key it can come later, and it never merges the row with an 'agent' key.
-- - segments: one run of reads of a session in one pane and one workspace. A pane move, a
--   reconnect or a learning switch starts a new one; text never joins across two.
--   read_source is the pane.read source: 'visible' has a lower coverage.
-- - observations: one new part of a read, or one gap. capture_identity is unique.
--   E11-S4 adds the filters and the cursor, E11-S5 the hook records.
-- workspace_key is NULL when the workspace has no Brain key.
CREATE TABLE sessions (
	id               INTEGER PRIMARY KEY,
	herdr_session    TEXT NOT NULL,
	harness          TEXT NOT NULL,
	key_kind         TEXT NOT NULL CHECK (key_kind IN ('agent', 'capture')),
	key_id           TEXT NOT NULL,
	agent_session_id TEXT,
	pane_id          TEXT NOT NULL,
	workspace_key    TEXT,
	started          TEXT NOT NULL,
	ended            TEXT,
	UNIQUE (herdr_session, harness, key_kind, key_id)
) STRICT;

CREATE TABLE segments (
	id            INTEGER PRIMARY KEY,
	session_id    INTEGER NOT NULL REFERENCES sessions (id),
	pane_id       TEXT NOT NULL,
	workspace_key TEXT,
	read_source   TEXT NOT NULL CHECK (read_source IN ('visible', 'recent_unwrapped')),
	started       TEXT NOT NULL,
	ended         TEXT
) STRICT;

CREATE TABLE observations (
	id               INTEGER PRIMARY KEY,
	capture_identity TEXT NOT NULL UNIQUE,
	source           TEXT NOT NULL CHECK (source IN ('herdr', 'hook', 'transcript', 'remember')),
	segment_id       INTEGER NOT NULL REFERENCES segments (id),
	event            TEXT NOT NULL,
	excerpt          TEXT NOT NULL,
	truncated        INTEGER NOT NULL CHECK (truncated IN (0, 1)),
	status           TEXT NOT NULL CHECK (status IN ('complete', 'partial', 'gap')),
	at               TEXT NOT NULL
) STRICT;

CREATE INDEX segments_session ON segments (session_id);
CREATE INDEX observations_segment ON observations (segment_id);
