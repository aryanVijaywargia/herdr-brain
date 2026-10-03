-- Tools (E17-S3, §10.1, §11): one row for each item of kind 'tool', the id is the item id. The
-- row copies the contract of the item's current revision: contract is its whole text (the
-- tool.toml and the run.sh, tool.Files), and name, cwd, timeout_s, effects and read_only (the
-- reviewed read-only restriction) come from it. contract_hash is the one hash of the whole
-- contract with its scope (tool.Hash). approved_hash is the contract hash that you approved
-- and approved its time; both are NULL while the revision is not active. A tool may run only
-- while approved_hash, contract_hash and its files agree (output.MayRun). [NEW] the column
-- names, read_only and approved_hash. Forget clears the text columns (§6.7).
CREATE TABLE tools (
	id            INTEGER PRIMARY KEY REFERENCES items (id),
	name          TEXT NOT NULL,
	scope         TEXT NOT NULL CHECK (scope IN ('global', 'workspace')),
	workspace_key TEXT REFERENCES workspaces (key),
	contract      TEXT NOT NULL,
	contract_hash TEXT NOT NULL,
	cwd           TEXT NOT NULL,
	timeout_s     INTEGER NOT NULL,
	effects       TEXT NOT NULL,
	read_only     INTEGER NOT NULL CHECK (read_only IN (0, 1)),
	approved_hash TEXT,
	approved      TEXT
) STRICT;
