-- Forget of one session (E11-S6, §6.7, §11).
-- - sessions.generation: the capture generation of the session. A pane read and a hook record
--   note it before they collect text, and their write refuses an older value. brain forget
--   --session raises it in the transaction that deletes the evidence, so a read or a record
--   of the forgotten text that waits for its commit stores nothing. The row stays after a
--   forget, so the counter never goes back.
-- - forgotten_captures: the capture identities of the deleted observations. They hold no
--   text. A replay of such an identity stores nothing (§6.4).
ALTER TABLE sessions ADD COLUMN generation INTEGER NOT NULL DEFAULT 0;

CREATE TABLE forgotten_captures (
	capture_identity TEXT PRIMARY KEY
) STRICT;
