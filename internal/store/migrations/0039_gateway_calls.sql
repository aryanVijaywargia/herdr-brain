-- Gateway calls (E24-S6, #172): one row for each call of a tool of the MCP gateway. A call
-- whose pane check fails writes its refused row in the store of its herdr server; a call with
-- no identity, or of a herdr server with no store, has no store for a row. method is the
-- gateway tool. result is ok, refused (a check refused the call, so nothing ran and nothing
-- was drafted) or failed (the call passed the checks but did not end well: the tool exited
-- with an error, or Brain did). A run_tool call also writes its tool_calls row; the other
-- methods write none.
CREATE TABLE gateway_calls (
	id     INTEGER PRIMARY KEY,
	method TEXT NOT NULL CHECK (method IN ('find_tool', 'run_tool', 'recall', 'remember')),
	result TEXT NOT NULL CHECK (result IN ('ok', 'refused', 'failed')),
	at     TEXT NOT NULL
) STRICT;
