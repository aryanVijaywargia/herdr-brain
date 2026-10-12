#!/bin/zsh
# usage: claude-runs.sh <label> <n>
export USER=me LOGNAME=me
export HOOKLOAD_LOG=/tmp/e7s4/data/claude-$1-hook.jsonl
for i in $(seq $2); do
  t0=$(/usr/bin/perl -MTime::HiRes=time -e 'printf "%.3f", time')
  HOME=/Users/me /Users/me/.local/bin/claude -p --model haiku --settings /tmp/e7s4/claude-settings.json --setting-sources project "Your context has a Brain brief. Reply with only its first bracketed tag, for example [x 000]." > /tmp/e7s4/data/claude-$1-reply-$i.txt 2>&1
  t1=$(/usr/bin/perl -MTime::HiRes=time -e 'printf "%.3f", time')
  echo "{\"run\":$i,\"s_claude\":$(echo "$t1 - $t0" | bc)}" >> /tmp/e7s4/data/claude-$1-wall.jsonl
done
touch /tmp/e7s4/data/claude-$1.done
