-- Extraction batches, watermarks and extractor settings (E13-S2, §7.3, §11).
-- - extraction_batches: one extractor batch. Its stable id is (session, workspace key, first
--   and last capture identity) (§7.3): the UNIQUE index. A batch holds the observations of one
--   run of one workspace key in one session [NEW]; workspace_key is never NULL, because a
--   session with no Brain key has no batch [NEW]. start_watermark is the observation id of the
--   watermark at the start (0 for none) and last_observation the id of its last observation
--   [NEW]. model_calls counts the extractor calls of the batch. reason says why a batch failed,
--   was invalid or was discarded [NEW]; it holds no model text. at is the time of the last
--   status change. Only one batch of a session is active (§7.3).
-- - extraction_watermarks [NEW, §11 puts the watermark in cursors]: the extraction watermark of
--   one session and one workspace key, the last observation that a committed or empty batch
--   covered. It is a table of its own: a cursors row is one (session, source), and a CHECK
--   fixes its sources, but a session has one watermark for each workspace key.
-- - extractor_settings [NEW]: the extractor CLI and model that you chose, and the extraction
--   switch. workspace_key '' is the store row: it holds the CLI and model, and its switch
--   applies to every workspace. A workspace row holds only its switch. No row means
--   extraction on (§7.3 D9) with no CLI chosen, so no call.
CREATE TABLE extraction_batches (
	id               INTEGER PRIMARY KEY,
	session_id       INTEGER NOT NULL REFERENCES sessions (id),
	workspace_key    TEXT NOT NULL,
	first_capture    TEXT NOT NULL,
	last_capture     TEXT NOT NULL,
	start_watermark  INTEGER NOT NULL,
	last_observation INTEGER NOT NULL,
	status           TEXT NOT NULL CHECK (status IN ('active', 'committed', 'empty', 'failed', 'invalid', 'discarded')),
	model_calls      INTEGER NOT NULL,
	reason           TEXT NOT NULL,
	at               TEXT NOT NULL
) STRICT;

CREATE UNIQUE INDEX extraction_batches_id ON extraction_batches (session_id, workspace_key, first_capture, last_capture);

CREATE UNIQUE INDEX extraction_batches_active ON extraction_batches (session_id) WHERE status = 'active';

CREATE TABLE extraction_watermarks (
	session_id       INTEGER NOT NULL REFERENCES sessions (id),
	workspace_key    TEXT NOT NULL,
	observation_id   INTEGER NOT NULL,
	capture_identity TEXT NOT NULL,
	PRIMARY KEY (session_id, workspace_key)
) STRICT;

CREATE TABLE extractor_settings (
	workspace_key TEXT PRIMARY KEY,
	extraction    TEXT NOT NULL CHECK (extraction IN ('on', 'off')),
	cli           TEXT CHECK (cli IN ('claude', 'codex')),
	model         TEXT,
	changed       TEXT NOT NULL
) STRICT;
