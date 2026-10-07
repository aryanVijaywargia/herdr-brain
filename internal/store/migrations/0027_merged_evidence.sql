-- Claim merges (#279, §7.2). A same_as link to an open claim of the same kind in the same scope
-- adds the new evidence to that claim's candidate. via_claim is the claim id whose key brought
-- the evidence, so the merge can be undone; NULL for the claim's own evidence.
ALTER TABLE evidence_links ADD COLUMN via_claim TEXT REFERENCES claims (id);

-- The claim kind of the extractor and its permission flag, for the merge check: no merge
-- across claim kinds or of a permission or safety rule. NULL kind for an older claim: it never
-- merges.
ALTER TABLE claims ADD COLUMN kind TEXT;
ALTER TABLE claims ADD COLUMN touches_permission INTEGER NOT NULL DEFAULT 0 CHECK (touches_permission IN (0, 1));

-- Claim keys lose their end punctuation (normal form, #279). A tool key keeps its exact text. A
-- key whose normal form is taken already keeps its old text.
UPDATE OR IGNORE claims SET situation = rtrim(situation, '.!?;:, '), rule = rtrim(rule, '.!?;:, ')
WHERE situation IS NOT NULL AND rule IS NOT NULL
	AND (situation <> rtrim(situation, '.!?;:, ') OR rule <> rtrim(rule, '.!?;:, '))
	AND id NOT IN (SELECT claim_id FROM candidates WHERE kind = 'command_chain')
	AND id NOT IN (SELECT claim_id FROM items WHERE kind = 'tool');
