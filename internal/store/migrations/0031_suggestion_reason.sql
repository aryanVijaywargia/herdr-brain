-- Why a live suggestion matched (E20-S9): a kind word, no prompt text.
-- 'exact_prompt' is an exact prompt hash match, 'lesson' a plain-words lesson (#253). NULL for
-- the rows of an older Brain: they show "unknown".
ALTER TABLE suggestions ADD COLUMN reason TEXT;
