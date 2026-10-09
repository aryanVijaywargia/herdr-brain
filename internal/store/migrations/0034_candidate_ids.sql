-- A candidate id names one candidate for ever (#362): proposals and same_as links in review
-- name candidates by id, and a merge deletes a candidate with no evidence left. With a plain
-- INTEGER PRIMARY KEY, SQLite gives the next candidate the id of a deleted newest one;
-- AUTOINCREMENT never does. SQLite cannot add AUTOINCREMENT to a table, so the migration builds
-- candidates again, and evidence_links with it, as migration 26 did: foreign keys stay on inside
-- the migration transaction. Ids stay the same.
ALTER TABLE candidates RENAME TO candidates_old;

CREATE TABLE candidates (
	id            INTEGER PRIMARY KEY AUTOINCREMENT,
	claim_id      TEXT NOT NULL REFERENCES claims (id),
	scope         TEXT NOT NULL CHECK (scope IN ('global', 'workspace')),
	workspace_key TEXT,
	kind          TEXT NOT NULL CHECK (kind IN ('correction', 'lesson', 'failure', 'procedure', 'command_chain')),
	harnesses     TEXT NOT NULL CHECK (json_type(harnesses) = 'array'),
	count         INTEGER NOT NULL,
	sessions      INTEGER NOT NULL,
	priority      INTEGER NOT NULL,
	first_seen    TEXT NOT NULL,
	last_seen     TEXT NOT NULL,
	status        TEXT NOT NULL CHECK (status IN ('open', 'proposed', 'expired'))
) STRICT;

INSERT INTO candidates SELECT * FROM candidates_old;

CREATE TABLE evidence_links_new (
	candidate_id     INTEGER NOT NULL REFERENCES candidates (id),
	observation_id   INTEGER NOT NULL REFERENCES observations (id),
	capture_identity TEXT NOT NULL,
	session_id       INTEGER NOT NULL REFERENCES sessions (id),
	workspace_key    TEXT,
	harness          TEXT NOT NULL,
	sender           TEXT NOT NULL,
	at               TEXT NOT NULL,
	via_claim        TEXT REFERENCES claims (id),
	PRIMARY KEY (candidate_id, observation_id)
) STRICT;

INSERT INTO evidence_links_new SELECT * FROM evidence_links;

DROP TABLE evidence_links;

DROP TABLE candidates_old;

ALTER TABLE evidence_links_new RENAME TO evidence_links;

CREATE UNIQUE INDEX candidates_claim_scope ON candidates (claim_id, COALESCE(workspace_key, ''));

CREATE INDEX evidence_links_session ON evidence_links (session_id);

-- The next id comes after every id that a row still names: a deleted candidate that a proposal
-- names keeps its id too.
INSERT INTO sqlite_sequence (name, seq) SELECT 'candidates', 0
	WHERE NOT EXISTS (SELECT 1 FROM sqlite_sequence WHERE name = 'candidates');

UPDATE sqlite_sequence SET seq = MAX(seq, (SELECT COALESCE(MAX(candidate_id), 0) FROM proposals))
	WHERE name = 'candidates';
