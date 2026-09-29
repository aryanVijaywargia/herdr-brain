#!/bin/sh
# Codex legacy notify: the payload is the last argv item. Log it with the receive time.
t=$(/usr/bin/python3 -c 'import time;print("%.3f"%time.time())')
for a; do last=$a; done
printf '%s\n' "$last" > /tmp/e11s1b/hooks/notify-$t.json
echo "$t notify pane=$HERDR_PANE_ID" >> /tmp/e11s1b/hooks/index.log
