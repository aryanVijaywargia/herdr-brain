#!/bin/sh
# Spike hook: save stdin with the receive time; print nothing.
t=$(/usr/bin/python3 -c 'import time;print("%.3f"%time.time())')
f=/tmp/e11s1/hooks/$1-$t.json
cat > "$f"
echo "$t $1 pane=$HERDR_PANE_ID" >> /tmp/e11s1/hooks/index.log
exit 0
