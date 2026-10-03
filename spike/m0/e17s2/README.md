# E17-S2 spike: MCP gateway facts

Results: `spike/m0/FINDINGS.md`, section "E17-S2: MCP gateway facts (§17 items 7, 15, 17)".

- `mcpspike/`: the test MCP server (Go, its own module, stdio). It logs only `HERDR_ENV`, `HERDR_SOCKET_PATH`, `HERDR_PANE_ID` and `HERDR_WORKSPACE_ID` (set or not, and the value), the parent command name and each MCP method. Without `HERDR_ENV=1` it returns an empty tool list and refuses a call. With it, it offers one tool, `spike_echo`, that returns its text and the marker `mcp-echo-7316`.
- `ht.sh`: herdr for the test session `brain-spike-e17s2`, with a clean env.
- `setup.sh`: builds `/private/tmp/e17s2` (server binary, run scripts, logs) and the Claude folder `/private/tmp/e11s1/agent/e17s2`.
- `claude-run.sh`: starts Claude with the server from a `--mcp-config` file and `--strict-mcp-config`.
- `codex-run.sh`: starts Codex with the server from `-c mcp_servers.spike.*` and folder trust on the command line.
- `reg-claude.sh`, `reg-codex.sh`: the official `mcp add`, `list`, `get` and `remove` commands with a scratch `HOME` or `CODEX_HOME`. No model call.
- `snapshot.sh`: sizes and mtimes of the 5 real Claude and Codex config files, before and after (no file is read).
- `data/`: `mcp.jsonl` (server log), `drive.log` (start and prompt times), `screens.txt` (pane reads), `reg-claude.txt`, `reg-codex.txt`, `snapshot-before.txt`, `snapshot-after.txt`. The home path, user name and host name are replaced by `~`, `<user>` and `<host>`.
