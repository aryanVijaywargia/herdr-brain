-- Merged evidence (#279, §7.2): a same_as link to an open claim of the same kind in the same
-- scope adds the new evidence to that claim's candidate. via_claim is the claim id whose key
-- brought the evidence, so the merge can be undone; NULL for the claim's own evidence.
ALTER TABLE evidence_links ADD COLUMN via_claim TEXT REFERENCES claims (id);
