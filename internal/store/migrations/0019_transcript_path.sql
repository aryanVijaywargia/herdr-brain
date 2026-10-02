-- The transcript path of a receipt (E15-S4, §8.3, §11): the transcript_path of the start hook
-- input, as the CLI gave it. NULL when the input has none, and after brain forget --session
-- (§6.7). Brain never reads the file: the brief names the path of the last session of the
-- workspace, and the agent reads it only when you ask.
ALTER TABLE deliveries ADD COLUMN transcript_path TEXT;

-- The last session lookup of the hook (store.LastSession): the newest receipt of a workspace
-- with a transcript path. [NEW] a partial index, so it holds only those receipts.
CREATE INDEX deliveries_transcript ON deliveries (workspace_key) WHERE transcript_path IS NOT NULL;
