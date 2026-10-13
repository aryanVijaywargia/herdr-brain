-- The history scan at setup (E21-S2, §6.8, §11, D16).
-- - history_scans: the one scan of the store. A yes in brain setup writes the row as 'pending';
--   it is the done mark, so the CHECK on id keeps one row for each store. binding is the herdr
--   session of the brained that claimed it, NULL before the claim. claimed is the claim time:
--   a restart picks the files again with it (§6.8 "Window"). finished is set at 'done'. The
--   JSON columns hold numbers, ids, kinds and scope keys only, never text:
--   - counts: for each harness, the sessions found and read, and the skipped ones by reason.
--   - turns: turn id -> recorded tokens, null for a turn with no usage field (never 0).
--   - findings: in finding order, each {id, kind, scope, obs: [{observation, turn}]}. scope is
--     the workspace key, '' for the global pool.
--   - progress: scope -> the last committed finding id of its scan batches.
-- - extraction_batches.scan_id: a scan batch (§6.8 "Proposals"). Its stable id is (scan, scope,
--   first and last finding id); workspace_key holds the scope. Only one scan batch is active.
--   The live active index now leaves scan batches out: a scan batch spans sessions, and its
--   session_id is only the session of its first finding.
CREATE TABLE history_scans (
	id        INTEGER PRIMARY KEY CHECK (id = 1),
	binding   TEXT,
	status    TEXT NOT NULL CHECK (status IN ('pending', 'reading', 'extracting', 'done')),
	requested TEXT NOT NULL,
	claimed   TEXT,
	finished  TEXT,
	counts    TEXT NOT NULL DEFAULT '{}' CHECK (json_type(counts) = 'object'),
	turns     TEXT NOT NULL DEFAULT '{}' CHECK (json_type(turns) = 'object'),
	findings  TEXT NOT NULL DEFAULT '[]' CHECK (json_type(findings) = 'array'),
	progress  TEXT NOT NULL DEFAULT '{}' CHECK (json_type(progress) = 'object')
) STRICT;

ALTER TABLE extraction_batches ADD COLUMN scan_id INTEGER REFERENCES history_scans (id);
ALTER TABLE extraction_batches ADD COLUMN first_finding INTEGER;
ALTER TABLE extraction_batches ADD COLUMN last_finding INTEGER;

CREATE UNIQUE INDEX extraction_batches_scan ON extraction_batches (scan_id, workspace_key, first_finding, last_finding)
	WHERE scan_id IS NOT NULL;

CREATE UNIQUE INDEX extraction_batches_scan_active ON extraction_batches (scan_id)
	WHERE scan_id IS NOT NULL AND status = 'active';

DROP INDEX extraction_batches_active;

CREATE UNIQUE INDEX extraction_batches_active ON extraction_batches (session_id) WHERE scan_id IS NULL AND status = 'active';
