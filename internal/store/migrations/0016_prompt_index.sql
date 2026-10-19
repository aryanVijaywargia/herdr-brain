-- The prompt index (E14-S1).
-- - prompt_index: one row for each stored prompt observation. hash is the exact fingerprint
--   of the full prompt (internal/fingerprint), before the excerpt cut. text is the
--   secret-filtered prompt, ≤ 500 characters, for close matches only (E14-S2). session_id is
--   the sessions row, which holds the session key, and generation its capture generation at
--   the write. proposal_ids are the proposals that the prompt led to or matched: a capture
--   links the ready proposals that it matches, and a new proposal links the rows of its
--   trigger prompts. The row is written in the transaction of its observation, and
--   brain forget --session deletes it.
CREATE TABLE prompt_index (
	id             INTEGER PRIMARY KEY,
	hash           TEXT NOT NULL,
	text           TEXT NOT NULL,
	workspace_key  TEXT,
	session_id     INTEGER NOT NULL REFERENCES sessions (id),
	observation_id INTEGER NOT NULL UNIQUE REFERENCES observations (id),
	generation     INTEGER NOT NULL,
	proposal_ids   TEXT NOT NULL DEFAULT '[]' CHECK (json_type(proposal_ids) = 'array'),
	at             TEXT NOT NULL
) STRICT;

CREATE INDEX prompt_index_hash ON prompt_index (hash);

CREATE INDEX prompt_index_session ON prompt_index (session_id);
