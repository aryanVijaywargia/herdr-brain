-- The deferred session-id check of a receipt (E7-S3). A hook run in a live pane records its
-- (pane, agent session id) as 'pending': herdr shows the agent only after the hook
-- exits. The next brain call for that pane compares it with the
-- pane's agent_session and marks 'match' or 'not this pane's agent', or 'unknown' when the
-- pane shows no agent_session. NULL: the receipt holds no record, as a receipt of a failed
-- live check or of an older store.
ALTER TABLE deliveries ADD COLUMN agent_check TEXT;
