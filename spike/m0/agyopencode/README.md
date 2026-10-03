# E16-S2 spike: agy and OpenCode facts

Results: `spike/m0/FINDINGS.md`, section "E16-S2: agy and OpenCode facts (§17 items 16, 18)".

- `bin/brain`: the stand-in `brain` (Python). It logs its args, the `HERDR_*` env and stdin, and prints a fixed brief with a fresh marker. It is not Brain: no store, no binding, no herdr call.
- `agy/hooks.json`: the agy hook file. `setup.sh` copies it to `<workspace>/.agents/hooks.json`.
- `opencode/brain.ts`: the OpenCode V2 shim prototype. `setup.sh` copies it to `<project>/.opencode/plugins/brain.ts`.
- `ht.sh`: herdr for the test session `brain-spike-e16s2`, with a clean env and the stand-in first on `PATH`.
- `setup.sh`: builds `/private/tmp/e16s2`. `snapshot.sh`: names, sizes and mtimes of the real CLI config folders, before and after.
- `save-logs.py`: copies the logs to `data/` and the hook inputs to `fixtures/`, with the home path, user name and host name replaced.
- `data/runs.txt`: the drive log (commands, replies, screens). `data/brain-calls.jsonl`: every hook and shim call with its input. `data/opencode-shim.jsonl`: the shim log. `data/opencode-screens.txt`: OpenCode screens for the noise lines.
- `fixtures/`: hook inputs from the live runs, for the golden tests of E16-S12 and E16-S13.
