-- Outcomes of the items (E15-S1).
-- - outcomes: one row for each new correction that is evidence for the claim of an active
--   item: the item, the observation and its capture session. session_id is the sessions row,
--   which holds the session key. kind is what the receipt of that session says:
--   'comeback' (the receipt names a revision of the item), 'uncertain' (the same, but the
--   match rests only on an unconfirmed same_as link), 'delivery_gap' (a withheld receipt) or
--   'delivery_unknown' (no receipt, or no single receipt ties to the session). revision is the
--   item revision that the receipt names, never the current revision; it is NULL for a
--   gap or an unknown delivery, as no receipt names a revision then. The row holds ids
--   only. UNIQUE (item_id, observation_id): one observation is one outcome of an item,
--   so a retry adds nothing. The rows are written in the batch commit. brain forget
--   --session deletes the rows of the session, and an item forget the rows of the item.
CREATE TABLE outcomes (
	id             INTEGER PRIMARY KEY,
	item_id        INTEGER NOT NULL REFERENCES items (id),
	revision       INTEGER,
	session_id     INTEGER NOT NULL REFERENCES sessions (id),
	kind           TEXT NOT NULL CHECK (kind IN ('comeback', 'uncertain', 'delivery_gap', 'delivery_unknown')),
	observation_id INTEGER NOT NULL REFERENCES observations (id),
	at             TEXT NOT NULL,
	UNIQUE (item_id, observation_id)
) STRICT;

CREATE INDEX outcomes_revision ON outcomes (item_id, revision);

CREATE INDEX outcomes_session ON outcomes (session_id);
