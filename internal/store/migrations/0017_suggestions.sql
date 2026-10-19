-- Live suggestions (E14-S3).
-- - suggestions: one row for each suggestion that Brain reserved at a prompt. observation_id
--   is the reservation key: the prompt occurrence. A hook record and a pane read of the same
--   prompt are one occurrence (S57): the pane observation is enriched by the hook observation,
--   and a reservation of one of them blocks the other. The row holds ids only: the proposal,
--   the capture session and its generation at the reservation, the binding, the workspace key
--   and the agent pane. prompt_hash is the fingerprint of the prompt; forget deletes the row
--   with the prompt index rows. review_pane_id and shown_at are set when the pane
--   opens. result is NULL while you have not decided: approved, rejected, changed or later
--   after a decision, expired when the suggestion was not shown [NEW].
CREATE TABLE suggestions (
	id             INTEGER PRIMARY KEY,
	observation_id INTEGER NOT NULL UNIQUE REFERENCES observations (id),
	proposal_id    INTEGER NOT NULL REFERENCES proposals (id),
	session_id     INTEGER NOT NULL REFERENCES sessions (id),
	generation     INTEGER NOT NULL,
	herdr_session  TEXT NOT NULL,
	workspace_key  TEXT,
	agent_pane_id  TEXT NOT NULL,
	review_pane_id TEXT,
	prompt_hash    TEXT NOT NULL,
	reserved       TEXT NOT NULL,
	shown_at       TEXT,
	result         TEXT CHECK (result IN ('approved', 'rejected', 'changed', 'later', 'expired'))
) STRICT;

CREATE INDEX suggestions_proposal ON suggestions (proposal_id);

CREATE INDEX suggestions_session ON suggestions (session_id);
