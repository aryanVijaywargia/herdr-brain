#!/bin/sh
# Build the E17-S2 test folder under /private/tmp/e17s2. Writes nothing in ~/.claude, ~/.codex or a repo.
#   bin/mcpspike, bin/*.sh   the test MCP server and the run scripts
#   logs/                    server logs (one JSON line per event) and the drive log
# Claude runs in /private/tmp/e11s1/agent/e17s2 (E11-S1 trusted /private/tmp/e11s1/agent).
# Codex runs in /private/tmp/e17s2/codex (folder trust on the command line only).
set -eu
here=$(cd "$(dirname "$0")" && pwd)
t=/private/tmp/e17s2
mkdir -p "$t/bin" "$t/logs" "$t/cfg" "$t/codex" /private/tmp/e11s1/agent/e17s2
(cd "$here/mcpspike" && go build -o "$t/bin/mcpspike" .)
cp "$here/claude-run.sh" "$here/codex-run.sh" "$t/bin/"
chmod +x "$t/bin/"*.sh
printf '# demo\n\nA small demo folder for a CLI test.\n' > /private/tmp/e11s1/agent/e17s2/README.md
cp /private/tmp/e11s1/agent/e17s2/README.md "$t/codex/README.md"
