#!/bin/sh
# Start Codex with the test server from -c overrides only. Folder trust on the command line only.
# Nothing is written to ~/.codex/config.toml by this route.
# Usage: codex-run.sh <case> [codex args...]
set -eu
t=/private/tmp/e17s2
c=$1; shift
exec codex -c 'projects={"/private/tmp/e17s2/codex"={trust_level="trusted"}}' -c check_for_update_on_startup=false \
  -c model_reasoning_effort='"low"' \
  -c "mcp_servers.spike.command=\"$t/bin/mcpspike\"" \
  -c "mcp_servers.spike.args=[\"-log\",\"$t/logs/mcp.jsonl\",\"-case\",\"$c\"]" "$@"
