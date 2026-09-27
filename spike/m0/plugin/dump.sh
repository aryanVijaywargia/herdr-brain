#!/bin/sh
# Log env, argv and stdin for the M0 spike. A pane stays open to show it.
out=/tmp/brainspike/plugin-$1-$(date +%s).log
mkdir -p /tmp/brainspike
{ echo "argv: $*"; echo "cwd: $(pwd)"; env | grep -E '^(HERDR_|BRAIN)' | sort; echo "--- stdin"; if [ "$1" != pane ]; then cat; fi; } > "$out" 2>&1
if [ "$1" = pane ]; then cat "$out"; echo "Brain spike pane. Waiting."; sleep 600; fi
