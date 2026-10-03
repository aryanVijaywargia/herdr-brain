#!/bin/sh
# Write one control line to the running probe: "note <text>" or "dup <pane>".
echo "$*" > /private/tmp/e17s1/run/ctl
