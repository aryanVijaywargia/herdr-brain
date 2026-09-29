-- Sender mark, counts and cursor (E11-S4, §6.3, §6.4, §6.5, §11).
-- - observations.sender: who wrote the text (§6.5). Pane text never has 'you': only a review
--   approval sets it. 'unconfirmed' is a captured message and every text with no known mark.
-- - observations.counts: 0 when the observation must not raise a support count: a gap, a
--   replay that Brain cannot tell apart from new evidence (S34), or Brain-origin text.
-- - observations.brain_origin: 1 for text that Brain itself caused. It never counts.
-- - cursors: the last capture identity of each session and source. The transaction that
--   stores an observation moves it, so a crash leaves both or neither (§6.4).
ALTER TABLE observations ADD COLUMN sender TEXT NOT NULL DEFAULT 'unconfirmed'
	CHECK (sender IN ('you', 'unconfirmed', 'assistant', 'tool'));
ALTER TABLE observations ADD COLUMN counts INTEGER NOT NULL DEFAULT 1 CHECK (counts IN (0, 1));
ALTER TABLE observations ADD COLUMN brain_origin INTEGER NOT NULL DEFAULT 0 CHECK (brain_origin IN (0, 1));

CREATE TABLE cursors (
	session_id INTEGER NOT NULL REFERENCES sessions (id),
	source     TEXT NOT NULL CHECK (source IN ('herdr', 'hook', 'transcript', 'remember')),
	position   TEXT NOT NULL,
	PRIMARY KEY (session_id, source)
) STRICT;
