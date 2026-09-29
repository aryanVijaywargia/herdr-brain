#!/bin/sh
# Poll one pane: every 0.5 s log time, agent status and what each read source returns.
# Usage: poll.sh <pane> <marker> <seconds> <out>
P=$1; M=$2; N=$3; O=$4; H=/tmp/e11s1/ht
end=$(( $(date +%s) + N ))
while [ $(date +%s) -lt $end ]; do
  t=$(/usr/bin/python3 -c 'import time;print("%.3f"%time.time())')
  st=$($H agent get $P 2>/dev/null | /usr/bin/python3 -c 'import json,sys;print(json.load(sys.stdin)["result"]["agent"]["agent_status"])' 2>/dev/null)
  ru=$($H agent read $P --source recent-unwrapped --lines ${RL:-300} 2>&1); rc1=$?
  vi=$($H agent read $P --source visible 2>&1); rc2=$?
  n1=$(printf '%s\n' "$ru" | grep -c "$M"); n2=$(printf '%s\n' "$vi" | grep -c "$M")
  e1=$(printf '%s\n' "$ru" | grep -o 'error[^}]*' | head -1)
  echo "$t status=$st ru_rc=$rc1 ru_lines=$(printf '%s\n' "$ru" | wc -l | tr -d ' ') ru_marker=$n1 vis_rc=$rc2 vis_marker=$n2 $e1" >> $O
  sleep 0.5
done
