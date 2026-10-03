#!/bin/sh
# codex mcp add/remove with a scratch CODEX_HOME (and HOME) under /private/tmp. No model call.
set -u
t=/private/tmp/e17s2/reg-codex
rm -rf "$t"; mkdir -p "$t/home" "$t/codexhome"
cd "$t"
c() { echo "\$ codex $*"; env HOME="$t/home" CODEX_HOME="$t/codexhome" codex "$@" 2>&1; echo "(exit $?)"; }
state() { echo "-- files:"; (cd "$t" && find . -type f | sort); echo "-- codexhome/config.toml:"; cat "$t/codexhome/config.toml" 2>/dev/null || echo "(none)"; }
state
c mcp add brain -- /private/tmp/e17s2/bin/mcpspike -log /private/tmp/e17s2/logs/reg.jsonl -case reg-codex
state
c mcp add brain-env --env HERDR_X=1 -- /bin/true
state
c mcp list
c mcp get brain
c mcp remove brain
c mcp remove brain-env
state
