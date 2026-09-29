-- Extractor calls (E13-S4, §7.3 caps). One row for each extractor call, at its dispatch time,
-- so the daily call cap counts each call once, also the calls of a batch that is retried on a
-- later day (extraction_batches keeps only the count and the last change) [NEW]. The queue
-- serves the workspace key whose last call is oldest first.
CREATE TABLE extraction_calls (
	id            INTEGER PRIMARY KEY,
	batch_id      INTEGER NOT NULL REFERENCES extraction_batches (id),
	workspace_key TEXT NOT NULL,
	at            TEXT NOT NULL
) STRICT;

CREATE INDEX extraction_calls_at ON extraction_calls (at);
