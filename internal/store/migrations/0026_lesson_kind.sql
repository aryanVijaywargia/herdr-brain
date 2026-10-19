-- Plain-words lessons (#253): the candidate kind 'lesson' joins the CHECK of
-- candidates.kind. SQLite cannot change a CHECK, so the migration builds candidates again, and
-- evidence_links with it: foreign keys stay on inside the migration transaction, so no row may
-- point at a dropped table at any step. The rename moves the evidence_links reference to
-- candidates_old; the new evidence_links points at the new candidates. Ids stay the same.
ALTER TABLE candidates RENAME TO candidates_old;

CREATE TABLE candidates (
	id            INTEGER PRIMARY KEY,
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
	PRIMARY KEY (candidate_id, observation_id)
) STRICT;

INSERT INTO evidence_links_new SELECT * FROM evidence_links;

DROP TABLE evidence_links;

DROP TABLE candidates_old;

ALTER TABLE evidence_links_new RENAME TO evidence_links;

CREATE UNIQUE INDEX candidates_claim_scope ON candidates (claim_id, COALESCE(workspace_key, ''));

CREATE INDEX evidence_links_session ON evidence_links (session_id);
