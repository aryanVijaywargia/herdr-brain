#!/bin/sh
# Names, sizes and mtimes of the real CLI config folders (no file is read). Run before and after the spike.
for d in ~/.gemini ~/.gemini/config ~/.gemini/config/plugins ~/.gemini/antigravity-cli ~/.config/opencode ~/.config/opencode/plugins; do
  echo "## $d"
  ls -lT "$d" 2>&1 | awk 'NR>1 {print $5, $6, $7, $8, $9, $10}' | sed "s#$HOME#~#g"
done
