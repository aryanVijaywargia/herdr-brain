#!/bin/sh
# claude mcp add/remove in each scope, with a scratch HOME under /private/tmp. No model call.
# Prints the scratch files after each step (the scratch files hold no token: there is no sign-in).
set -u
t=/private/tmp/e17s2/reg-claude
rm -rf "$t"; mkdir -p "$t/home" "$t/proj"
cd "$t/proj"
c() { echo "\$ claude $*"; env -u CLAUDE_CONFIG_DIR HOME="$t/home" claude "$@" 2>&1; echo "(exit $?)"; }
files() { echo "-- files under the scratch HOME and the project:"; (cd "$t" && find . -type f | sort); }
show() { for f in "$@"; do echo "-- $f"; [ -f "$f" ] && python3 -c 'import json,sys; d=json.load(open(sys.argv[1])); print(json.dumps({k:v for k,v in d.items() if k in ("mcpServers","projects")}, indent=1))' "$f" || echo "(none)"; done; }
files
for s in user local project; do
  c mcp add -s "$s" "brain-$s" -- /private/tmp/e17s2/bin/mcpspike -log /private/tmp/e17s2/logs/reg.jsonl -case "reg-$s"
  files; show "$t/home/.claude.json" "$t/proj/.mcp.json"
done
c mcp list
c mcp get brain-user
for s in user local project; do
  c mcp remove -s "$s" "brain-$s"
done
files; show "$t/home/.claude.json" "$t/proj/.mcp.json"
c mcp add -s user brain-env -e HERDR_X=1 -- /bin/true
show "$t/home/.claude.json"
c mcp remove brain-env
