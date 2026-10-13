-- The merge pass (E22-S1, §9.5).
-- - proposals.merge: the items of a merge proposal, each with its base revision, as a JSON
--   array of {"item": id, "revision": n}; the kept item is the item of the proposal and is in
--   the list too. An empty array is a proposal of another kind. A rejected merge proposal
--   keeps its id set, so a later pass drops a group with the same set. Ids only, no text.
-- - merge_passes [NEW]: the hash of each brief part at its last merge pass, so a restart does
--   not pass an unchanged part again. workspace_key '' is the global part.
-- - merge_links [NEW]: each claim whose same_as link the approval of a merge proposal set, with
--   its link before the approval, so undo of the merge puts it back. Claim ids only.
ALTER TABLE proposals ADD COLUMN merge TEXT NOT NULL DEFAULT '[]' CHECK (json_type(merge) = 'array');

CREATE TABLE merge_passes (
	workspace_key TEXT PRIMARY KEY,
	sha256        TEXT NOT NULL,
	at            TEXT NOT NULL
) STRICT;

CREATE TABLE merge_links (
	proposal_id    INTEGER NOT NULL REFERENCES proposals (id),
	claim_id       TEXT NOT NULL REFERENCES claims (id),
	same_as        TEXT REFERENCES claims (id),
	same_as_status TEXT,
	PRIMARY KEY (proposal_id, claim_id)
) STRICT;
