-- Rejections (E12-S3).
-- - rejections: you rejected one claim in one scope (a workspace, or global; workspace_key is
--   NULL for global), with your reason. id is [NEW]: brain review --reopen and --reset name
--   the row by it. evidence_count is the evidence count of the candidate of the claim in that
--   scope at the rejection (or at the last --reset [NEW]); brain review --rejected shows the
--   evidence since. reopened is NULL while the rejection holds; Brain never sets it by itself.
--   A reopened row stays as history, so only one row of a claim and scope holds at a time.
--   reason is text of the item: forget removes it.
CREATE TABLE rejections (
	id             INTEGER PRIMARY KEY,
	claim_id       TEXT NOT NULL REFERENCES claims (id),
	scope          TEXT NOT NULL CHECK (scope IN ('global', 'workspace')),
	workspace_key  TEXT,
	reason         TEXT NOT NULL,
	evidence_count INTEGER NOT NULL,
	at             TEXT NOT NULL,
	reopened       TEXT
) STRICT;

CREATE UNIQUE INDEX rejections_claim_scope ON rejections (claim_id, COALESCE(workspace_key, ''))
	WHERE reopened IS NULL;
