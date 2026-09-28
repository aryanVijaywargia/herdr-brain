-- The deferred session-id check of a receipt (E7-S3, §8.3 limit, §17 item 29). A hook run in
-- a live pane records its (pane, agent session id) as 'unknown': herdr shows the agent only
-- after the hook exits. The next brain call for that pane compares it with the pane's
-- agent_session and marks 'match' or 'not this pane's agent'. NULL: the receipt holds no
-- record, as a receipt of a failed live check or of an older store.
ALTER TABLE deliveries ADD COLUMN agent_check TEXT;
