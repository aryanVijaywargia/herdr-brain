-- The review of a same_as link that waits (#358).
-- - claims.draft: the claim of the newest telling of the claim key, as the extractor gave it
--   after the Brain checks and the secret filter (JSON) [NEW]. A merge in review proposes the
--   text of the newest telling with no model call; the skill steps of a telling are in no other
--   row. Forget removes it with the claim key text. NULL for a claim of an older Brain.
-- - claims.drafted: the time of that telling, so a merge picks the newest of the two claims.
-- - claims_apart [NEW]: the pairs of claims that you keep apart in review. The extractor link of
--   such a pair is not stored again, so the pair does not come back for review. Claim ids only.
ALTER TABLE claims ADD COLUMN draft TEXT;
ALTER TABLE claims ADD COLUMN drafted TEXT;

CREATE TABLE claims_apart (
	claim_id TEXT NOT NULL REFERENCES claims (id),
	other_id TEXT NOT NULL REFERENCES claims (id),
	PRIMARY KEY (claim_id, other_id)
) STRICT;
