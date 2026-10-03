#!/bin/sh
# Start Claude with the test server from a --mcp-config file only (--strict-mcp-config: no other MCP server).
# Nothing is written to ~/.claude or ~/.claude.json by this route.
# Usage: claude-run.sh <case> [claude args...]
set -eu
t=/private/tmp/e17s2
c=$1; shift
cat > "$t/cfg/claude-$c.json" <<JSON
{"mcpServers":{"spike":{"type":"stdio","command":"$t/bin/mcpspike","args":["-log","$t/logs/mcp.jsonl","-case","$c"]}}}
JSON
exec claude --mcp-config "$t/cfg/claude-$c.json" --strict-mcp-config "$@"
