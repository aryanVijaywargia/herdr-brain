-- Tool calls (E17-S4, §10.1, §11): one row for each brain run call of a known tool. The tool
-- is a tools row, and contract_hash is the hash of the checked contract that the call used: the
-- contract hash of a ran call, the approved hash of a refused call (NULL when the tool has
-- none). session_id is the open capture session of the calling pane, which holds the session
-- key, or NULL when capture has none. origin says who called: the agent's shell or an
-- automation. result is ran or refused; only ran rows are real calls (§10.2, E17-S5). exit_code
-- and duration_ms are NULL when refused. [NEW] the column names, session_id and duration_ms.
CREATE TABLE tool_calls (
	id            INTEGER PRIMARY KEY,
	tool_id       INTEGER NOT NULL REFERENCES tools (id),
	contract_hash TEXT,
	session_id    INTEGER REFERENCES sessions (id),
	origin        TEXT NOT NULL CHECK (origin IN ('agent', 'automation')),
	result        TEXT NOT NULL CHECK (result IN ('ran', 'refused')),
	at            TEXT NOT NULL,
	exit_code     INTEGER,
	duration_ms   INTEGER
) STRICT;

CREATE INDEX tool_calls_tool ON tool_calls (tool_id);
