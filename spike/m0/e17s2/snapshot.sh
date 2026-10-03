#!/bin/sh
# Sizes and mtimes of the real Claude and Codex config files (stat only; no file is read).
# Run before and after the spike, then diff.
for f in ~/.claude.json ~/.claude/settings.json ~/.claude/settings.local.json ~/.mcp.json ~/.codex/config.toml; do
  stat -f '%m %z %N' "$f" 2>&1 | sed "s#$HOME#~#g"
done
