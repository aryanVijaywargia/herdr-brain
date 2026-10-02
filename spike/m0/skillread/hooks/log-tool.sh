#!/bin/sh
# Claude PreToolUse/PostToolUse hook for the spike: append the hook input (tool name and input) to a log.
cat >> /private/tmp/e16s1/logs/claude-hooks.jsonl
echo >> /private/tmp/e16s1/logs/claude-hooks.jsonl
