-- Claims and candidates (E12-S2, §7.2, §11).
-- - claims: one claim key (situation, rule) from the extractor, in normal form. id is the claim
--   id: random, with no text (§7.2). situation and rule are the claim key text; forget of an
--   item sets them to NULL (§6.7), so they are nullable, and two forgotten keys never clash on
--   the UNIQUE. same_as is the claim id that this key means the same as; same_as_status
--   'proposed' waits for review, 'confirmed' gives this key that claim id (§7.2).
-- - candidates: one claim in one scope (a workspace, or global; workspace_key is NULL for
--   global). Brain makes one with its first evidence link. kind is the candidate kind
--   of the §7.2 gate table. harnesses, count and sessions come from the evidence links; count
--   is the number of occurrences, sessions the number of distinct capture sessions [NEW].
--   priority is 1 when the gate of the kind is reached, else 0 [NEW]: it orders the queue and
--   sets the label (§7.2). last_seen is the time of the newest occurrence; expiry reads it.
-- - evidence_links [NEW, not in §11]: one observation that supports one candidate. It records
--   the workspace, harness, sender and capture identity of the observation (§7.2), and its
--   session for the session gates and forget. It holds no text.
CREATE TABLE claims (
	id             TEXT PRIMARY KEY,
	situation      TEXT,
	rule           TEXT,
	same_as        TEXT REFERENCES claims (id),
	same_as_status TEXT CHECK (same_as_status IN ('proposed', 'confirmed')),
	created        TEXT NOT NULL,
	UNIQUE (situation, rule)
) STRICT;

CREATE TABLE candidates (
	id            INTEGER PRIMARY KEY,
	claim_id      TEXT NOT NULL REFERENCES claims (id),
	scope         TEXT NOT NULL CHECK (scope IN ('global', 'workspace')),
	workspace_key TEXT,
	kind          TEXT NOT NULL CHECK (kind IN ('correction', 'failure', 'procedure', 'command_chain')),
	harnesses     TEXT NOT NULL CHECK (json_type(harnesses) = 'array'),
	count         INTEGER NOT NULL,
	sessions      INTEGER NOT NULL,
	priority      INTEGER NOT NULL,
	first_seen    TEXT NOT NULL,
	last_seen     TEXT NOT NULL,
	status        TEXT NOT NULL CHECK (status IN ('open', 'proposed', 'expired'))
) STRICT;

CREATE UNIQUE INDEX candidates_claim_scope ON candidates (claim_id, COALESCE(workspace_key, ''));

CREATE TABLE evidence_links (
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

CREATE INDEX evidence_links_session ON evidence_links (session_id);
