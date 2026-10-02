#!/bin/sh
# herdr for the named test session only, with a clean env (no main-session HERDR_* vars).
exec env -i HOME="$HOME" PATH="$HOME/.local/bin:$(dirname "$(command -v codex)"):/usr/bin:/bin:/usr/sbin:/sbin" \
  TERM=xterm-256color LANG=en_US.UTF-8 herdr --session brain-spike-e16s1 "$@"
