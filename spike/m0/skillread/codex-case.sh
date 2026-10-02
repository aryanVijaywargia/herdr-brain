#!/bin/sh
# One Codex case in a fresh Codex session in pane w2:p1: reset the skill atimes, start Codex, answer
# "Continue without trusting" at "Hooks need review", send the prompt, wait, then log the case.
# Usage: codex-case.sh <case name> <prompt> [extra codex args...]
set -u
here=$(cd "$(dirname "$0")" && pwd); h=$here/ht.sh; p=w2:p1
name=$1; prompt=$2; shift 2
sh "$here/reset-atime.sh" >/dev/null
"$h" pane send-keys $p ctrl+c; sleep 0.5; "$h" pane send-keys $p ctrl+c; sleep 2
"$h" pane send-text $p 'clear' >/dev/null 2>&1; "$h" pane send-keys $p enter; sleep 1
"$h" agent start "cx$(date +%s)" --kind codex --pane $p --timeout 90000 -- \
  -c 'projects={"/private/tmp/e16s1/codex"={trust_level="trusted"}}' -c check_for_update_on_startup=false \
  -c 'model_reasoning_effort="low"' "$@" >/dev/null 2>&1
sleep 2
if "$h" pane read $p --source visible | grep -q 'Hooks need review'; then
  "$h" pane send-keys $p 3; sleep 0.5; "$h" pane send-keys $p enter; sleep 3
fi
echo "$(date +%s) $name prompt sent" >> /private/tmp/e16s1/logs/drive.log
"$h" agent prompt $p "$prompt" >/dev/null
i=0; while [ $i -lt 60 ]; do
  sleep 3; i=$((i+1))
  s=$("$h" agent get $p | python3 -c 'import json,sys; print(json.load(sys.stdin)["result"]["agent"]["agent_status"])')
  [ "$s" != working ] && break
done
echo "status=$s"
[ "$s" = blocked ] && exit 0
"$here/case-log.sh" "$name" $p 80
