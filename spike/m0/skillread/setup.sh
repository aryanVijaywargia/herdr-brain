#!/bin/sh
# Build the E16-S1 test folder under /private/tmp/e16s1. Writes nothing in ~/.claude, ~/.codex or a repo.
#   store/skills/<item id>/r1/SKILL.md  the fixture skills (E16-S3 path)
#   claude/                              Claude project folder (no git repo)
#   codex/AGENTS.md                      the brief for Codex (no hook trust, no config write)
#   hooks/                               the Claude --settings hooks and the brief JSON
set -eu
here=$(cd "$(dirname "$0")" && pwd)
t=/private/tmp/e16s1
mkdir -p "$t/store" "$t/claude" "$t/codex" "$t/hooks" "$t/logs"
rm -rf "$t/store/skills"
cp -R "$here/skills" "$t/store/skills"
cp "$here/hooks/session-start.sh" "$here/hooks/log-tool.sh" "$here/claude-settings.json" "$t/hooks/"
chmod +x "$t/hooks/"*.sh
python3 -c 'import json,sys; print(json.dumps({"hookSpecificOutput":{"hookEventName":"SessionStart","additionalContext":open(sys.argv[1]).read()}}))' \
  "$here/brief.md" > "$t/hooks/claude-brief.json"
cp "$here/brief.md" "$t/codex/AGENTS.md"
printf '# shop\n\nA small web shop. The orders service uses Postgres. The shop CLI keeps a local SQLite cache.\n' > "$t/claude/README.md"
cp "$t/claude/README.md" "$t/codex/README.md"
