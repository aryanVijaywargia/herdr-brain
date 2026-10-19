-- The refusal of the last extractor call of a failed batch or change request (#377, #428): the
-- CLI refused the call before it ran a model, at a usage or rate limit ('limit') or with no
-- sign-in ('sign-in'), so the call used no try. '' for any other end. Brain read it from the
-- stored reason text before; that text is for display only now.
-- The reasons of an older Brain map once: the limit text of #377, and the bare reason
-- "claude: exit status 1" that a Brain before #428 stored for every failure. Most of those were
-- sign-in failures, so each such batch or request gets one more call each hour, also with no
-- try left, until a call stores its own refusal.
ALTER TABLE extraction_batches ADD COLUMN refusal TEXT NOT NULL DEFAULT ''
	CHECK (refusal IN ('', 'limit', 'sign-in'));
ALTER TABLE change_requests ADD COLUMN refusal TEXT NOT NULL DEFAULT ''
	CHECK (refusal IN ('', 'limit', 'sign-in'));

UPDATE extraction_batches SET refusal = 'limit'
	WHERE status = 'failed' AND reason IN ('claude is at a usage or rate limit', 'codex is at a usage or rate limit');
UPDATE extraction_batches SET refusal = 'sign-in'
	WHERE status = 'failed' AND (reason GLOB 'claude: exit status [0-9]' OR reason GLOB 'claude: exit status [0-9][0-9]'
		OR reason GLOB 'claude: exit status [0-9][0-9][0-9]' OR reason GLOB 'codex: exit status [0-9]'
		OR reason GLOB 'codex: exit status [0-9][0-9]' OR reason GLOB 'codex: exit status [0-9][0-9][0-9]');
UPDATE change_requests SET refusal = 'limit'
	WHERE status = 'failed' AND reason IN ('claude is at a usage or rate limit', 'codex is at a usage or rate limit');
UPDATE change_requests SET refusal = 'sign-in'
	WHERE status = 'failed' AND (reason GLOB 'claude: exit status [0-9]' OR reason GLOB 'claude: exit status [0-9][0-9]'
		OR reason GLOB 'claude: exit status [0-9][0-9][0-9]' OR reason GLOB 'codex: exit status [0-9]'
		OR reason GLOB 'codex: exit status [0-9][0-9]' OR reason GLOB 'codex: exit status [0-9][0-9][0-9]');
