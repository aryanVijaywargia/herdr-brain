#!/bin/sh
# Start Codex for the spike: folder trust and options on the command line only. Nothing is written to ~/.codex.
# The brief reaches Codex through /private/tmp/e16s1/codex/AGENTS.md (the folder is not a git repo). No hook, no hook trust.
exec codex -c 'projects={"/private/tmp/e16s1/codex"={trust_level="trusted"}}' -c check_for_update_on_startup=false \
  -c model_reasoning_effort='"low"' "$@"
