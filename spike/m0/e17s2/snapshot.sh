#!/bin/sh
# Names, sizes and mtimes of the real Claude and Codex config files (stat only; no file is read).
# Run before and after the spike, then diff.
for f in ~/.claude.json ~/.claude/settings.json ~/.claude/settings.local.json ~/.mcp.json ~/.codex/config.toml; do
  stat -f '%m %z %N' "$f" 2>&1 | sed "s#$HOME#~#g"
done
for d in ~/.claude ~/.codex; do
  echo "## $d"
  ls -lT "$d" 2>&1 | awk 'NR>1 && $NF !~ /^(projects|sessions|history.jsonl|log|logs|shell_snapshots|todos|statsig|file-history|session-env|debug|telemetry|cache|tmp|paste-cache|plans|ide|shell-snapshots|tasks|backups)$/ {print $5, $6, $7, $8, $9, $10}' | sed "s#$HOME#~#g"
done
