-- Hook records and command records (E11-S5).
-- - observations.enriched_by: the hook observation that adds detail to this pane observation.
--   The two rows keep both references; the hook row then does not count, because it is
--   not a second occurrence.
-- - command_records: the command of one tool hook observation. argv is the filtered command,
--   at most 4 KB, and NULL when the secret filter found a secret in it (secret_found = 1).
--   exit_status is NULL when the hook payload has none.
ALTER TABLE observations ADD COLUMN enriched_by INTEGER REFERENCES observations (id);

CREATE TABLE command_records (
	observation_id INTEGER PRIMARY KEY REFERENCES observations (id),
	argv           TEXT,
	cwd            TEXT,
	exit_status    INTEGER,
	truncated      INTEGER NOT NULL CHECK (truncated IN (0, 1)),
	secret_found   INTEGER NOT NULL CHECK (secret_found IN (0, 1))
) STRICT;
