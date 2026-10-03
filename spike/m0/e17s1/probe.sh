#!/bin/sh
# Start the probe for the test session in the background. $1 is the run label.
SOCK="$HOME/.config/herdr/sessions/brain-spike-e17s1/herdr.sock"
D=/private/tmp/e17s1/run
[ -p $D/ctl ] || mkfifo $D/ctl
nohup /private/tmp/e17s1/probe -sock "$SOCK" -log $D/log.jsonl -claims $D/claims.jsonl -ctl $D/ctl \
  -panes "${PANES:-w1:p1,w2:p1}" -run "$1" >> $D/probe.err 2>&1 &
echo $! > $D/probe.pid
