-- The claim of the failure that an outcome counts (E20-S5, §5.4): the claim whose evidence the
-- observation is, after a same_as merge. A skill counts the sessions of one failure, so two
-- different failures in two sessions are no repeat. It is an opaque id. NULL for the rows of
-- an older Brain [NEW]: they count for no failure of a skill.
ALTER TABLE outcomes ADD COLUMN claim_id TEXT;
