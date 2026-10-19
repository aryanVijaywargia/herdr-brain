-- The git source (#283): a revert that git shows is an observation with source
-- 'git', so the extractor sees it as it sees the other signals. SQLite cannot change a CHECK,
-- so the migration builds observations again with the same ids. Seven tables point at
-- observations by name, and a rename would move their references to the old table. So the
-- rows wait in a plain copy, the old table goes, and the new table takes the name at once.
-- Deferred foreign keys let the references dangle until the rows are back; the commit checks
-- every reference.
PRAGMA defer_foreign_keys = ON;

CREATE TABLE observations_copy AS SELECT id, capture_identity, source, segment_id, event, excerpt, truncated,
	status, at, sender, counts, brain_origin, enriched_by FROM observations;

DROP TABLE observations;

CREATE TABLE observations (
	id               INTEGER PRIMARY KEY,
	capture_identity TEXT NOT NULL UNIQUE,
	source           TEXT NOT NULL CHECK (source IN ('herdr', 'hook', 'transcript', 'remember', 'git')),
	segment_id       INTEGER NOT NULL REFERENCES segments (id),
	event            TEXT NOT NULL,
	excerpt          TEXT NOT NULL,
	truncated        INTEGER NOT NULL CHECK (truncated IN (0, 1)),
	status           TEXT NOT NULL CHECK (status IN ('complete', 'partial', 'gap')),
	at               TEXT NOT NULL,
	sender           TEXT NOT NULL DEFAULT 'unconfirmed' CHECK (sender IN ('you', 'unconfirmed', 'assistant', 'tool')),
	counts           INTEGER NOT NULL DEFAULT 1 CHECK (counts IN (0, 1)),
	brain_origin     INTEGER NOT NULL DEFAULT 0 CHECK (brain_origin IN (0, 1)),
	enriched_by      INTEGER REFERENCES observations (id)
) STRICT;

INSERT INTO observations SELECT * FROM observations_copy ORDER BY id;

DROP TABLE observations_copy;

CREATE INDEX observations_segment ON observations (segment_id);
