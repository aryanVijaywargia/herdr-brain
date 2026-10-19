-- Proposals, change requests and conflict marks (E13-S5).
-- - proposals.conflicts_with: an M2 column, the item ids that the proposal conflicts with. The
--   extractor names a claim id (conflicts_with); Brain stores the ids of the active items of
--   that claim in the scope of the proposal. Ids only, no text.
-- - change_requests [NEW]: one change-request entry of the extraction queue ("ask for a
--   change"). Its key is (source, source_id, revision): a request of you on a proposal is
--   ('proposal', proposal id, the item revision that the proposal is for), and
--   E15-S1 adds ('item', item id, item revision). The request text of a proposal is
--   proposals.change_request; this row holds no text. workspace_key is the scope of the
--   entry, '' for global. status follows extraction_batches, with 'queued' before the first
--   call [NEW]. new_proposal_id is the new draft that its commit made. reason says why it
--   failed, was invalid or was discarded; it holds no model text.
-- - extraction_calls gets change_request_id: a change-request call counts under the daily
--   call cap as a batch call does. Each row has a batch or a change request. The table is built
--   again, as SQLite cannot drop the NOT NULL of batch_id.
ALTER TABLE proposals ADD COLUMN conflicts_with TEXT NOT NULL DEFAULT '[]' CHECK (json_type(conflicts_with) = 'array');

CREATE TABLE change_requests (
	id              INTEGER PRIMARY KEY,
	source          TEXT NOT NULL CHECK (source IN ('proposal', 'item')),
	source_id       INTEGER NOT NULL,
	revision        INTEGER NOT NULL,
	workspace_key   TEXT NOT NULL,
	status          TEXT NOT NULL CHECK (status IN ('queued', 'active', 'committed', 'empty', 'failed', 'invalid', 'discarded')),
	model_calls     INTEGER NOT NULL,
	reason          TEXT NOT NULL,
	new_proposal_id INTEGER REFERENCES proposals (id),
	created         TEXT NOT NULL,
	at              TEXT NOT NULL,
	UNIQUE (source, source_id, revision)
) STRICT;

CREATE TABLE extraction_calls_new (
	id                INTEGER PRIMARY KEY,
	batch_id          INTEGER REFERENCES extraction_batches (id),
	change_request_id INTEGER REFERENCES change_requests (id),
	workspace_key     TEXT NOT NULL,
	at                TEXT NOT NULL
) STRICT;

INSERT INTO extraction_calls_new (id, batch_id, workspace_key, at) SELECT id, batch_id, workspace_key, at FROM extraction_calls;

DROP TABLE extraction_calls;

ALTER TABLE extraction_calls_new RENAME TO extraction_calls;

CREATE INDEX extraction_calls_at ON extraction_calls (at);
