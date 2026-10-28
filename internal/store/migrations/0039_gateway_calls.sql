-- Gateway calls (E24-S6, #172): one row for each call of a tool of the MCP gateway that
-- reaches a store. method is the gateway tool. result is ok, refused (a check refused the
-- call, so nothing ran and nothing was drafted) or failed (the call passed the checks but did
-- not end well: the tool exited with an error, or Brain did). A run_tool call also writes its
-- tool_calls row; the other methods write none. A call that gets no store writes no row.
CREATE TABLE gateway_calls (
	id     INTEGER PRIMARY KEY,
	method TEXT NOT NULL CHECK (method IN ('find_tool', 'run_tool', 'recall', 'remember')),
	result TEXT NOT NULL CHECK (result IN ('ok', 'refused', 'failed')),
	at     TEXT NOT NULL
) STRICT;
