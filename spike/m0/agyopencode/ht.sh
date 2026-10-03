#!/bin/sh
# herdr for the named test session only, with a clean env (no main-session HERDR_* vars).
# PATH: the stand-in brain first, then agy and opencode.
exec env -i HOME="$HOME" PATH="/private/tmp/e16s2/bin:$HOME/.local/bin:$HOME/.opencode/bin:/usr/bin:/bin:/usr/sbin:/sbin" \
  TERM=xterm-256color LANG=en_US.UTF-8 herdr --session brain-spike-e16s2 "$@"
