-- The M1 tables (HLD §11, E3-S2). adapters and file_snapshots live in <data home>/install.json
-- (E8), not here. Columns that §11 marks M2 come with a later migration.
--
-- Conventions, all [NEW]:
-- - STRICT tables, so SQLite refuses a value of the wrong type.
-- - Times are UTC RFC 3339 text with milliseconds, for example 2026-09-28T10:00:00.000Z.
-- - Yes/no columns are INTEGER 0 or 1.
-- - A list of ids or hashes is a JSON array in one TEXT column.
-- - Row ids are INTEGER PRIMARY KEY. Claim ids and workspace keys are opaque text.

CREATE TABLE workspaces (
	key                TEXT PRIMARY KEY,
	herdr_session      TEXT NOT NULL,
	herdr_workspace_id TEXT NOT NULL,
	label              TEXT NOT NULL,
	root_folder        TEXT,
	status             TEXT NOT NULL CHECK (status IN ('live', 'closed')),
	last_seen          TEXT NOT NULL
) STRICT;

CREATE TABLE items (
	id               INTEGER PRIMARY KEY,
	claim_id         TEXT NOT NULL,
	current_revision INTEGER NOT NULL CHECK (current_revision >= 1),
	kind             TEXT NOT NULL CHECK (kind IN ('fact', 'preference', 'decision', 'handoff', 'skill', 'tool', 'automation')),
	scope            TEXT NOT NULL CHECK (scope IN ('global', 'workspace')),
	workspace_key    TEXT REFERENCES workspaces (key),
	state            TEXT NOT NULL CHECK (state IN ('draft', 'active', 'disputed', 'retired')),
	pinned           INTEGER NOT NULL CHECK (pinned IN (0, 1)),
	anchors          TEXT NOT NULL CHECK (json_type(anchors) = 'array'),
	created          TEXT NOT NULL
) STRICT;

-- M1 has no status values for a proposal in §11. The [NEW] set: pending until you decide,
-- then approved, rejected, or changed (you asked for a change, §7.4). "Later" only mutes a
-- proposal for one session (§5.5), so it stays pending.
CREATE TABLE proposals (
	id                 INTEGER PRIMARY KEY,
	candidate_id       INTEGER,
	item_id            INTEGER REFERENCES items (id),
	base_revision      INTEGER,
	text               TEXT NOT NULL,
	scope              TEXT NOT NULL CHECK (scope IN ('global', 'workspace')),
	target             TEXT NOT NULL,
	diff               TEXT NOT NULL,
	expected_benefit   TEXT NOT NULL,
	permission_warning INTEGER NOT NULL CHECK (permission_warning IN (0, 1)),
	trigger_prompts    TEXT NOT NULL CHECK (json_type(trigger_prompts) = 'array'),
	change_request     TEXT,
	status             TEXT NOT NULL CHECK (status IN ('pending', 'approved', 'rejected', 'changed')),
	surface            TEXT CHECK (surface IN ('cli', 'popup', 'pane')),
	created            TEXT NOT NULL,
	decided            TEXT,
	review_seconds     INTEGER
) STRICT;

CREATE TABLE item_revisions (
	item_id               INTEGER NOT NULL REFERENCES items (id),
	revision              INTEGER NOT NULL CHECK (revision >= 1),
	text                  TEXT NOT NULL,
	scope                 TEXT NOT NULL CHECK (scope IN ('global', 'workspace')),
	workspace_key         TEXT REFERENCES workspaces (key),
	state                 TEXT NOT NULL CHECK (state IN ('draft', 'active', 'disputed', 'retired')),
	pinned                INTEGER NOT NULL CHECK (pinned IN (0, 1)),
	author                TEXT NOT NULL CHECK (author IN ('you', 'brain')),
	origin                TEXT NOT NULL CHECK (origin IN ('remember', 'review', 'extractor')),
	proposal_id           INTEGER REFERENCES proposals (id),
	undoes_revision       INTEGER,
	exception_to_item     INTEGER REFERENCES items (id),
	exception_to_revision INTEGER,
	conflicts_with        TEXT NOT NULL CHECK (json_type(conflicts_with) = 'array'),
	created               TEXT NOT NULL,
	PRIMARY KEY (item_id, revision),
	-- The exception link is a global item and its revision (§7.4): both or neither.
	CHECK ((exception_to_item IS NULL) = (exception_to_revision IS NULL))
) STRICT;

-- manifest_hash and built_at stay NULL from the commit that records a generation until the
-- compiler builds it (§7.5 steps 2-3).
CREATE TABLE outputs (
	generation           INTEGER PRIMARY KEY CHECK (generation >= 1),
	folder               TEXT NOT NULL,
	manifest_hash        TEXT,
	built_at             TEXT,
	current              INTEGER NOT NULL CHECK (current IN (0, 1)),
	holds_forgotten_text INTEGER NOT NULL CHECK (holds_forgotten_text IN (0, 1))
) STRICT;

-- content_before is NULL when the edit created the file, content_after when it removed it.
CREATE TABLE applied_edits (
	id             INTEGER PRIMARY KEY,
	proposal_id    INTEGER REFERENCES proposals (id),
	file           TEXT NOT NULL,
	generation     INTEGER NOT NULL,
	content_before TEXT,
	content_after  TEXT,
	content_hash   TEXT NOT NULL,
	applied        TEXT NOT NULL,
	rolled_back    TEXT
) STRICT;

-- item_revisions is a JSON array of {"item": id, "revision": n} objects. [NEW] the form.
CREATE TABLE deliveries (
	id                INTEGER PRIMARY KEY,
	profile           TEXT NOT NULL,
	pane_id           TEXT NOT NULL,
	agent_session_id  TEXT,
	workspace_key     TEXT REFERENCES workspaces (key),
	brief_hash        TEXT,
	output_generation INTEGER,
	item_revisions    TEXT NOT NULL CHECK (json_type(item_revisions) = 'array'),
	cli_version       TEXT,
	result            TEXT NOT NULL CHECK (result IN ('emitted', 'withheld', 'unmapped')),
	withheld_reason   TEXT,
	at                TEXT NOT NULL
) STRICT;

-- id is [NEW]: §11 names no key, and herdr can give a pane id again after a pane closes.
-- The binding is the herdr session and its continuity identity. herdr 0.9.1 has no
-- persistent session identity (§4.2), so session_identity is NULL there.
CREATE TABLE review_panes (
	id               INTEGER PRIMARY KEY,
	herdr_session    TEXT NOT NULL,
	session_identity TEXT,
	pane_id          TEXT NOT NULL,
	beside_pane_id   TEXT NOT NULL,
	proposal_id      INTEGER NOT NULL REFERENCES proposals (id),
	opened           TEXT NOT NULL,
	closed           TEXT
) STRICT;

-- No text (§11): only the claim id, the source keys and the time. A claim can have more than
-- one tombstone, because items in two scopes can share a claim id (§7.2).
CREATE TABLE tombstones (
	claim_id     TEXT NOT NULL,
	source_keys  TEXT NOT NULL CHECK (json_type(source_keys) = 'array'),
	forgotten_at TEXT NOT NULL
) STRICT;

CREATE INDEX tombstones_claim ON tombstones (claim_id);
