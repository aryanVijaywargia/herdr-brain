-- The learning off switch (E9-S4): `brain learn off|on` records one row for each scope and
-- target, and E11 reads it before it captures. Delivery and `brain remember` never read it.
-- - scope 'store': the whole store; herdr_session and target are ''.
-- - scope 'workspace': target is the Brain workspace key (E2-S4); herdr_session is ''.
-- - scope 'pane': the herdr session of the binding and the pane id, because two herdr
--   sessions can both have w1:p1.
-- A later switch of the same scope and target replaces its row. No store row means the
-- default: on for the main store, off for a test store.
CREATE TABLE learning_settings (
	scope         TEXT NOT NULL CHECK (scope IN ('store', 'workspace', 'pane')),
	herdr_session TEXT NOT NULL,
	target        TEXT NOT NULL,
	learning      TEXT NOT NULL CHECK (learning IN ('on', 'off')),
	changed       TEXT NOT NULL,
	PRIMARY KEY (scope, herdr_session, target)
) STRICT;
