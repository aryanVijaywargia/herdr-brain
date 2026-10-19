-- The item revisions that brain recall returned to an agent session (E20-S2). A row
-- hangs on the receipt of that session, so the outcomes count the revisions as
-- delivered to the session from the recall time on. A command writes no hook receipt, so a
-- recall gets its own small table.
CREATE TABLE recalls (
	id             INTEGER PRIMARY KEY,
	delivery_id    INTEGER NOT NULL REFERENCES deliveries (id),
	item_revisions TEXT NOT NULL CHECK (json_type(item_revisions) = 'array'),
	at             TEXT NOT NULL
) STRICT;

CREATE INDEX recalls_delivery ON recalls (delivery_id, at);
