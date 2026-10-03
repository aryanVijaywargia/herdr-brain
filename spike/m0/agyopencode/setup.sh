#!/bin/sh
# Build the E16-S2 test folder under /private/tmp/e16s2. Writes nothing in ~/.gemini, ~/.config/opencode or a repo.
#   bin/brain                              the stand-in brain (logs stdin, prints a fixed brief)
#   agy/.agents/hooks.json                 agy workspace hooks (customization root of the workspace)
#   opencode/.opencode/plugins/brain.ts    OpenCode V2 project plugin (the shim prototype)
#   marker-*, code-*                       fresh markers in the brief (the run sets them per case)
#   logs/                                  hook and shim logs
set -eu
here=$(cd "$(dirname "$0")" && pwd)
t=/private/tmp/e16s2
mkdir -p "$t/bin" "$t/agy/.agents" "$t/opencode/.opencode/plugins" "$t/logs"
cp "$here/bin/brain" "$t/bin/brain"; chmod +x "$t/bin/brain"
cp "$here/agy/hooks.json" "$t/agy/.agents/hooks.json"
cp "$here/opencode/brain.ts" "$t/opencode/.opencode/plugins/brain.ts"
printf '# demo\n\nA small demo folder for a CLI test.\n' > "$t/agy/README.md"
cp "$t/agy/README.md" "$t/opencode/README.md"
