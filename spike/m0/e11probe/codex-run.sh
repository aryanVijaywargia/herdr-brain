#!/bin/sh
# Run 2: folder trust, notify and one test hook on the command line only. Nothing is written to ~/.codex.
exec codex -c 'projects={"/private/tmp/e11s1b/agent"={trust_level="trusted"}}' -c check_for_update_on_startup=false \
  -c model_reasoning_effort='"low"' -a on-request -c approvals_reviewer='"user"' \
  -c 'notify=["/tmp/e11s1b/hooks/notify.sh"]' \
  -c 'hooks={UserPromptSubmit=[{hooks=[{type="command",command="/tmp/e11s1b/hooks/log.sh ups",timeout=5}]}],Stop=[{hooks=[{type="command",command="/tmp/e11s1b/hooks/log.sh stop",timeout=5}]}]}' "$@"
