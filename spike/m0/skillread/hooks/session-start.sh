#!/bin/sh
# Claude SessionStart hook for the spike: print the hand-written brief as additionalContext.
# No Brain binary and no Brain store: the spike checks the agent, not Brain.
cat >> /private/tmp/e16s1/logs/claude-hooks.jsonl
exec cat /private/tmp/e16s1/hooks/claude-brief.json
