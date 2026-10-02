#!/bin/sh
# Append one case to the raw log: the reply screen, the Claude tool calls (from the hook log) and the skill atimes.
# Usage: case-log.sh <case name> <pane> [lines]
set -u
t=/private/tmp/e16s1; here=$(cd "$(dirname "$0")" && pwd)
out=$t/logs/runs.txt
{
  echo "== $1 ($(date +%s))"
  "$here/ht.sh" agent read "$2" --source recent-unwrapped --lines "${3:-80}"
  echo "-- Claude hook log (event, permission_mode, tool, input)"
  [ -f $t/logs/claude-hooks.jsonl ] && python3 -c '
import json,sys
for l in open(sys.argv[1]):
    l=l.strip()
    if l:
        d=json.loads(l); print(d.get("hook_event_name"), d.get("permission_mode"), d.get("tool_name"), json.dumps(d.get("tool_input"))[:160])
' $t/logs/claude-hooks.jsonl
  echo "-- skill atimes (2020 = not read in this case)"
  stat -f '%Sa %N' -t '%Y-%m-%dT%H:%M:%S' $t/store/skills/*/r1/SKILL.md
} >> "$out" 2>&1
