#!/bin/sh
# Event probe: one line per call with the start time and the event JSON.
t=$(python3 -c 'import time;print("%.3f"%time.time())')
printf '%s pid=%s ev=%s json=%s\n' "$t" "$$" "$HERDR_PLUGIN_EVENT" "$HERDR_PLUGIN_EVENT_JSON" >> /tmp/e11s1/log/event.log
if [ -n "$E11_SLEEP" ] || [ -f /tmp/e11s1/slow ]; then sleep 3; fi
