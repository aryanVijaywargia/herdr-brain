#!/bin/sh
# Long-running startup probe: logs start, a tick each second, and signals.
L=/tmp/e11s1/log/startup.log
now() { python3 -c 'import time;print("%.3f"%time.time())'; }
echo "$(now) start pid=$$ ppid=$PPID sess=$HERDR_SESSION event=$HERDR_PLUGIN_EVENT" >> $L
env | grep -E '^HERDR_' | sort >> /tmp/e11s1/log/startup-env-$$.txt
trap 'echo "$(now) TERM pid=$$" >> $L; exit 0' TERM
trap 'echo "$(now) HUP pid=$$" >> $L; exit 0' HUP
trap 'echo "$(now) INT pid=$$" >> $L; exit 0' INT
i=0
while :; do i=$((i+1)); echo "$(now) tick $i pid=$$ ppid=$(ps -o ppid= -p $$)" >> /tmp/e11s1/log/tick-$$.log; sleep 1; done
