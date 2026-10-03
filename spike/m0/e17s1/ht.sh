#!/bin/sh
# herdr for the named test session only, with a clean env (no main-session HERDR_* vars).
exec env -i HOME="$HOME" PATH="$HOME/.local/bin:$HOME/.nvm/versions/node/v24.15.0/bin:/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin" \
  TERM=xterm-256color LANG=en_US.UTF-8 herdr --session brain-spike-e17s1 "$@"
