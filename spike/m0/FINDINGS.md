# M0 spike findings

- Date: 2026-09-27
- Machine: macOS (Darwin 27.0.0), herdr 0.9.1 (protocol 22), Claude Code 2.1.283, codex-cli 0.154.0, Go 1.23.0.
- Spike hook: `hook/main.go` (built as `hook/brainspike`). It logs to `/tmp/brainspike/<nonce>.json`.
- Status legend: PASS, FAIL, OPEN (not tested yet), BLOCKED (needs an owner decision).

## Results against HLD §17

| Item | Fact | Result | Evidence |
|---|---|---|---|
| 1 | Claude SessionStart `additionalContext` reaches the model | PASS | `claude -p --settings claude-settings.json` and an interactive Claude both returned the nonce. |
| 1 | Codex SessionStart `additionalContext` reaches the model | PASS | After trust in the Codex TUI, an interactive Codex returned the nonce. The hook fired at the first prompt, not at start. |
| 2 | Claude `additionalContext` size limit | PASS, limit found | 10,000 characters reach the model inline. At 10,001 characters Claude writes the text to `~/.claude/projects/<proj>/<sid>/tool-results/hook-...-additionalContext.txt` and gives the model only the path. |
| 2 | Codex size limit | PASS, no limit seen | `codex exec` returned both markers at 10,500, 30,000 and 100,000 characters. |
| 4 | Workspace root | ANSWERED (E2-S5), owner decision open | The API has no root. `workspace_cwd` is the focused pane's live folder. `identity_cwd` is the root pane's folder at the last save. Both follow `cd`. See "Workspace root folder source". |
| 8 | Event delay after a state change | PASS (E11-S1) | Claude 2.1.284, 7 real changes: the socket `pane.agent_status_changed` came 0.14-0.42 s after the matching Claude hook or key press. A fake `pane report-agent` change: 0.03-0.10 s to the socket and to an `[[events]]` hook. The event has no time and no id. See "brained hosting and capture (E11-S1)". Codex 0.154 (E11-S1b), 8 real changes: 0.04-0.31 s after Enter or the approval key, 0.39-0.58 s after the Codex turn-end `notify`. See "Codex live capture (E11-S1b)". |
| 9 | Alternate-screen loss, user message at turn start | PASS for Claude (E11-S1), PASS for Codex (E11-S1b) | Claude draws in the alternate screen. While `working` or `blocked`, only the screen (37 rows here) can be read; rows that scroll off are gone until idle. At idle herdr can scroll-capture the full history (138 of 138 lines), but most idle reads with `--lines` above the history size returned only the screen, with exit 0 (6 of 6, then 15 of 19). The user prompt line is on the screen at turn start. Codex (live, E11-S1b): draws inline; the scrollback grew with the reply (`max_offset_from_bottom` 3 → 102) and no row was lost. Its approval prompt is inline too. The prompt line is on the screen and in the read at turn start. See "Codex live capture (E11-S1b)". |
| 10 | Hook timing | PASS (E7-S4) | Normal load (M0): whole hook 5.4-10.7 ms. E7-S4, with the manifest check and a SQLite receipt, 100 runs per level: whole hook p95 7.6 ms idle, 8.1 ms with output bursts, 16.5 ms with a flood (max 80.9 ms, from one slow `pane get`). Manifest check ≤ 0.6 ms. Proposed lookup deadline: 150 ms from hook start [NEW]. See "Hook under load and manifest check (E7-S4)". |
| 11 | Pane split with no focus, then close | PASS | `pane split --no-focus` kept focus. A plugin `split` pane with `--no-focus --env BRAIN_REVIEW=1` kept focus and got the env. `plugin pane close` closed it. `plugin pane close` on a normal pane returned `plugin_pane_not_found` and left it open. `popup` placement: tested in E5-S3; M1 keeps `split` (see "Popup placement and plugin actions (E5-S3)"). |
| 12 | Agent reads a skill file from an absolute path in the brief, when relevant | TRUE for Claude (E16-S1), TRUE for Codex with the brief in `AGENTS.md` (E16-S1); the Codex hook route stays for the owner hand test | Claude Code 2.1.287 and codex-cli 0.154.0, one fresh session per case: each matching prompt read the right skill file and used its markers and steps; each negative prompt read no skill file; a near-description pair picked the right file. No permission prompt for the read in the modes tested. See "E16-S1: skill files from the brief (§17 item 12)". |
| 13 | Headless extractor call, no tools, JSON schema | PASS for no file, shell, network, MCP or hook access; PARTIAL for literal "no tools" (E13-S1) | Claude 2.1.283 and codex-cli 0.154.0 both return schema-valid JSON. Claude keeps only `StructuredOutput`. Codex 0.154 with `gpt-6-sol` keeps code-mode `exec`/`wait` (V8, no nested tools) and `request_user_input_async`; no flag removes them. When the output cannot fit, Claude can exit 0 with no structured output; Codex bends a bad prompt to the schema and fails only for an unsatisfiable schema. See "Headless extractor call (E13-S1)". |
| 14 | Hook payloads: `UserPromptSubmit`, `PostToolUse`, `Stop` | PASS for Claude (live, E11-S1). Codex: PASS from source, live hook OPEN (E11-S1b) | Claude: the `prompt` field is the exact text. An empty hook leaves the prompt unchanged. `PostToolUse` has the command and the shell cwd after the command, but no exit code. A failed command goes to `PostToolUseFailure` with `error` = "Exit code N...". Codex (source): `tool_input.command`, the session cwd, no exit code. See "brained hosting and capture (E11-S1)". Codex live (E11-S1b): a hook set with `-c` still needs the hook-trust step, so no Codex hook ran and the source facts stay. The legacy `notify` command (no trust gate) gave the typed prompt with the @-mention as typed and the final reply. See "Codex live capture (E11-S1b)". |
| 20 | `recent-unwrapped` while `working` or `blocked` | FAIL for Claude (E11-S1), PASS for Codex (E11-S1b) | Claude: `--lines 300` returns `agent_not_idle` in `working`, `blocked` and just after submit. `--lines 20` (at or below the screen) and `visible` work in every state. At idle, a `--lines` value larger than the history often returns only the screen, with exit 0. Codex (live, E11-S1b): `--lines 300` gave exit 0 in all 112 `working` reads and all 42 `blocked` reads, with the history growing as the reply grew. At idle, `--lines` 300, 1000 and 5000 gave the whole pane history (142 lines) in 9 of 9 reads. `visible` gives the 40-row screen. |
| 21 | Conformance test | OPEN | |
| 25 | No `brain` on PATH | PASS | `command -v brain` finds nothing. |
| 27 | Named test session | PASS (session parts) | `herdr --session brain-eval server` (background) ran beside `default` with its own socket. Its first workspace was `w1`. Its panes had `HERDR_SESSION=brain-eval` and their own `HERDR_SOCKET_PATH`. `herdr session stop brain-eval` and `herdr session delete brain-eval` touched only that session. |
| 28 | `agent read` of a reply | PASS for Claude and Codex | Claude, a 150-line reply: `--lines 80` gave lines 80-150, `--lines 200` gave all 150. Codex (E10-S4), a 150-line reply: `--lines 80` gave lines 76-150, `--lines 200` gave all 150. See "Codex long reply (E10-S4)". |
| 29 | Agent session id match | PASS, with a timing limit | See "Session id timing". |
| 30 | Socket reach from a hook subprocess | PASS | `HERDR_PANE_ID`, `HERDR_SOCKET_PATH` and `HERDR_BIN_PATH` reach the hook. `pane get` works in 5-10 ms. |
| 31 | Restart versus state deletion, session identity | PASS | Restart: `w1` came back with the same `identity_cwd`. Deletion: a new workspace in `/private/tmp` got `w1` again. `session.json` (version 3) has no session id. |

## Session id timing (item 29)

- The hook input has `session_id`. For the interactive test it was `6a345ec1-3050-4e61-b736-d0e9d2edab82`.
- After start, `agent get` reports `agent_session.value` = `6a345ec1-...` with `source` = `herdr:claude`. The ids match.
- **At hook time, `pane get` shows no `agent` and no `agent_session`.** herdr learns the agent after the hook exits.
- **Result:** the hook cannot check the match itself. The hook records (pane id, session id). A later step (`brained` or the next command) checks the match.
- **Nested runs:** a `claude -p` started inside an agent pane sees the parent's `agent_session`. Its own `session_id` differs. The later check must treat this as "not this pane's agent".

## Claude size limit (item 2): effect on the HLD

- The HLD brief cap is "≤ 10 KB" (global ≤ 4 KB, workspace ≤ 6 KB). 10 KB is 10,240 bytes.
- Claude's inline limit is 10,000 characters. A 10 KB brief would go to a file, and the model would see only a path.
- **Required HLD change:** the full hook output text must stay ≤ 10,000 characters, with the wrapper and receipts included. Keep a margin. The number of the margin is a guess [NEW].

## Codex hook trust (first-use fact)

- Codex 0.154 reads inline hooks from `-c hooks.<Event>=[...]` (the SessionFlags layer). No file in `~/.codex` changes.
- Codex runs a hook only when `[hooks.state."<key>"]` has a matching `trusted_hash`, or with `--dangerously-bypass-hook-trust`.
- An untrusted inline hook is **skipped with no warning** in `codex exec`. The model answered NONE. Only the existing trusted hook ran.
- **Product effect:** `brain setup` must guide the user through Codex hook trust. `brain doctor` must detect an untrusted Brain hook and print the fix.
- The owner chose trust through the Codex UI. The TUI showed "Hooks need review" with 3 choices: review, trust all, continue without trust. The review screen shows the event, the source (`Session flags - /<session-flags>/config.toml`), the command, the timeout and the trust state.
- Trust added this entry to `~/.codex/config.toml` (a copy of the file before is at `/tmp/codex-config-before-spike.toml`):
  `[hooks.state."/<session-flags>/config.toml:session_start:0:0"]` with `trusted_hash = "sha256:e13f..."`.
- The key uses the position of the hook, and the hash covers the command. A changed command needs trust again.
- **Cleanup:** remove this entry at the end of M0, or keep it for more spike runs.

### E8-S3: trust check

- Date: 2026-09-28. codex-cli 0.154.0. Source: tag `rust-v0.154.0`, read only. Raw notes: `data/e8s3-trust-hash.txt`. Probe: `trusthash/`. No Codex session ran for this spike.
- **Key format:** `<key source>:<event label>:<group index>:<handler index>` (`hook_key` in `hooks/src/lib.rs`).
  - For `hooks.json`, the key source is the file path as Codex builds it: `<codex home>/hooks.json`. The codex home is `$HOME/.codex` when `CODEX_HOME` is not set (not resolved for symlinks). When `CODEX_HOME` is set, it is the canonical path of `CODEX_HOME`.
  - Example: `[hooks.state."/Users/<user>/.codex/hooks.json:session_start:0:0"]`.
  - The event label is snake case: `session_start`, `user_prompt_submit`, `post_tool_use` and so on.
  - The group index is the position of the matcher group in the event's array. The handler index is the position in that group's `hooks` array. Both start at 0.
  - For `-c hooks.*` flags, the key source is `/<session-flags>/config.toml` (the M0 key).
- **Hash input and algorithm** (`hook_hash` in `hooks/src/engine/discovery.rs`, `version_for_toml` in `config/src/fingerprint.rs`):
  - The input is the normalized identity of one handler: `{"event_name": <label>, "matcher": <matcher>, "hooks": [<handler>]}`.
  - The handler has `type`, `command`, `timeout`, `async`, and `statusMessage` only when set. `additionalContextLimit` is in it only when set and not 2,500. `commandWindows` is never in it.
  - `timeout` is always in it: the set value (at least 1), else 600. SessionEnd and Interrupt use 1 to 3, default 1.
  - `matcher` is in it only when set, and never for UserPromptSubmit, Stop or Interrupt.
  - Codex writes this as JSON with sorted keys and no spaces, and takes SHA-256. The value is `sha256:<64 lowercase hex>`.
  - So the hash covers the command, the timeout, the matcher and the event. It does not cover the file path or the position; the key covers those.
- **Status rule** (`hook_trust_status`): trusted when the stored `trusted_hash` equals the current hash, "modified" when it differs, "untrusted" when there is none. Codex runs the hook only when it is trusted and `enabled` is not `false`. Codex reads `hooks.state` only from the user `config.toml` and `-c` flags.
- **Proof:** the probe computed the hash of the hooks and compared it with the stored values. All 3 stored entries match:
  - M0 inline hook (`.../spike/m0/hook/brainspike codex`, timeout 5): `sha256:e13f3a03...` = stored. With timeout 10 or 600 the hash differs.
  - `~/.codex/hooks.json:session_start:0:0` (no matcher, timeout 10): `sha256:9b6563c1...` = stored.
  - `~/.codex/hooks.json:subagent_start:0:0` (matcher, no timeout): `sha256:a7e36c07...` = stored.
  - `trusthash/main_test.go` keeps these 3 values as a golden test.
- **Side result:** 3 other entries in the user's `~/.codex/hooks.json` have no state, so Codex skips them: `session_start:1:0`, `post_tool_use:0:0` and `user_prompt_submit:0:0` (the herdr-projects hooks). This spike did not change them.
- **Chosen `brain doctor` check (stable, no Codex run):**
  1. Read `<codex home>/hooks.json` and find the Brain entry: its event, group index and handler index.
  2. Build the key and compute the hash as above.
  3. Read only the `hooks.state` table of `<codex home>/config.toml`, and get `trusted_hash` and `enabled` for the key.
  4. Trusted: the hashes are equal and `enabled` is not `false`. Else print "Codex hook not trusted" with the trust steps.
  - The key uses the position. If another tool adds a group before the Brain group, the key changes and Codex asks for trust again. The check then says "not trusted", which is correct.
  - A Codex update can change the hash rule. The doctor check then says "not trusted" for a trusted hook. The receipt fallback below catches this case.
- **Fallback (kept as the second signal):** a Codex pane with no Brain receipt after its first prompt shows "Codex hook not trusted or not firing" with the fix.
- **Trust steps for the user** (for the `brain setup` guide, E9; the guide does not exist yet):
  1. Start `codex` in a terminal after `brain setup` adds the hook. Codex shows **Hooks need review** and "1 hook is new or changed."
  2. Choose **Review hooks**. Find the SessionStart hook with the Brain command, from source `~/.codex/hooks.json`.
  3. Press `t` to trust it. Press `t` on the event list to trust all hooks that need review.
  4. Or choose **Trust all and continue** on the first screen. This trusts every new or changed hook, not only Brain's.
  5. Do not choose **Continue without trusting (hooks won't run)**. Brain then gets no context.
  6. Later, type `/hooks` in Codex to see the trust state or to trust the hook.
  7. Trust again after a Brain update that changes the hook command or timeout. Run `brain doctor` to check.
  - `codex exec` does not show the review screen. It skips an untrusted hook with no warning [M0].

## herdr 0.9.1 plugin system (new since 0.8.2)

- The plugin manifest `herdr-plugin.toml` has: `id`, `name`, `version`, `min_herdr_version`, `description`, `platforms`, `[[actions]]`, `[[panes]]`, `[[events]]`, `startup`, `build` and `[[link_handlers]]`.
- `[[panes]]` placement: `overlay`, `popup`, `split`, `tab` or `zoomed`. A plugin opens its pane with `herdr plugin pane open --plugin <id> --entrypoint <pane-id> --placement split --direction ... --env K=V --no-focus`.
- `[[events]]` run a command on herdr events. Events include `workspace.renamed` and `workspace.closed`.
- Plugin env vars: `HERDR_PLUGIN_ID`, `HERDR_PLUGIN_ROOT`, `HERDR_PLUGIN_STATE_DIR`, `HERDR_PLUGIN_CONFIG_DIR`, `HERDR_PLUGIN_ENTRYPOINT_ID`, `HERDR_SESSION`, `HERDR_BIN_PATH`, `HERDR_SOCKET_PATH`, `HERDR_PANE_ID`, `HERDR_ENV`.
- `HERDR_SESSION` is not set in a normal pane of the default session. Brain must treat "not set" as `default`. The official browser plugin does the same.
- **Possible simplifications (not decided):**
  - Review panes can be plugin panes. herdr then tracks their ownership.
  - Event hooks may replace part of `brained`.
  - `workspace_cwd` may replace the `session.json` read.
- The API schema is in `data/api-schema-0.9.1.json`.

## Named session and continuity (items 27, 31)

- Workspace ids are per herdr session. Brain must key a workspace by (herdr session, workspace id), which the HLD does.
- herdr reuses `w1` after a deletion of session state. The `identity_cwd` match catches a new folder. It cannot catch a new workspace in the same folder. That case is treated as the same workspace, as in the HLD.
- herdr has no persistent session identity, so the HLD branch "if herdr gives a persistent session identity" does not apply on 0.9.1.

## Workspace root folder source (E2-S5, items 4 and 27)

- Date: 2026-09-28. herdr 0.9.1. Headless scratch server, session `brain-spike-e2s5`, scratch HOME `/tmp/e2s5/home`. The raw replies are in `data/e2s5-root-source.txt`. The read probe is `rootread/main.go`.
- **Question 1: does `workspace_cwd` equal `identity_cwd` after the root pane changes folder? No.**
  - `workspace_cwd` is the live folder of the **focused** pane of the workspace at call time. With a second pane focused, it gave that pane's folder, not the root pane's folder.
  - `identity_cwd` is the folder of the tab's **root pane** at the **last save** of `session.json`. It is not the folder at creation.
  - herdr saves on a structure change (create, split, rename, close) about 5 s later, and on `session stop`. A `cd` alone does not save. So `identity_cwd` can lag the live folder.
  - When the root pane closes, the next pane becomes the root pane, and `identity_cwd` follows it.
  - A restart keeps the ids and the last saved `identity_cwd`, including a changed one.
  - **So neither value is a fixed root folder. Both follow `cd`.**
- **Question 2: can a non-action `brain` call get the root folder from the API? No.**
  - `workspace list`, `workspace get` and `api snapshot` have no folder for a workspace.
  - The reply of `plugin action invoke` has `context.workspace_cwd`. But it covers only the focused workspace (no workspace argument), it runs the action's command, and it is the focused pane's folder.
  - `pane get` gives a pane's `cwd`. The API does not say which pane is the root pane.
- **Root source for E2-S4:** `<session_dir>/session.json`, read only, field `workspaces[id == <workspace id>].identity_cwd`.
  - `session_dir` comes from `herdr session list --json`. For `default` it is `~/.config/herdr`. For a named session it is `~/.config/herdr/sessions/<name>`. In both, it is the folder of `socket_path`.
  - No `workspace list` call. One file read and one JSON parse.
  - Read time: 0.06-0.47 ms in the process (median 0.18 ms for a 33,445-byte file). The whole probe process: 2.3-3.2 ms.
  - herdr replaces the file on each save (the inode changes). No partial read was seen.
  - A new workspace is not in the file for about 5 s after creation. It is unmapped in that time anyway.
- **Format change: withhold, never guess.** The reader withholds ("withheld: identity") when: the file is missing or does not parse, `version` is not 3, a workspace has no `id` or `identity_cwd`, a type is wrong, the root is not absolute, the workspace is not in the file, or two entries have the same id. The reader checks every entry, so the result does not depend on entry order. The reader ignores unknown extra fields. `rootread/main_test.go` tests each case.
- **Owner decision needed (HLD §11 and §17 item 4):** the HLD treats `identity_cwd` as the fixed root folder. It is not. With the E2-S4 rule "same id + new root = new key", a `cd` in the root pane followed by a save gives a new key. The old items then wait for a relink. This fails closed (never a wrong delivery), but it also loses delivery after a normal `cd`. The owner chooses the rule. This spike does not change the HLD.
- **Side result for E2-S6:** a plugin `[[events]]` command for `workspace.renamed` ran. It got `HERDR_PLUGIN_EVENT_JSON` with `workspace_id` and `label`, and a `HERDR_PLUGIN_CONTEXT_JSON`.

## Hook under load and manifest check (E7-S4, item 10)

- Date: 2026-09-28. herdr 0.9.1, Claude Code 2.1.283, Go 1.23. Probe: `hookload/` (its own module). Raw records: `data/e7s4-raw.txt`. Stats: `data/e7s4-summary.txt`.
- **Setup.** Headless scratch server, session `brain-spike-e7s4`, scratch HOME `/tmp/e7s4/home`. The main session was not touched. The probe pane was `w1:p1`. The load panes were 32 shells in 4 other workspaces (8 panes each).
- **Hook probe.** `hookload hook` does the HLD §8.3 steps in order and times each one: read stdin, `herdr --session <s> pane get <id>` (as `internal/herdr` does) with a workspace check, read the pointer, read the manifest, read both parts and check their size and SHA-256, print the JSON, then open the SQLite store (WAL, `internal/store` DSN) and insert one `deliveries` row. It does not read the workspace map (E2-S4 is not built). Any check failure withholds. `main_test.go` tests the pointer, manifest and part cases and the outside-herdr case; the `pane get` checks and the receipt failure have no test in the probe.
- **Assumed E6 shape** (E6 is not built; names and JSON form are [NEW]; the folder, pointer and manifest come from HLD §7.5):
  - `out/current` holds `gen-<n>\n`, replaced by a rename.
  - `out/gen-<n>/manifest.json`: `{"generation":n,"files":[{"path","bytes","sha256"}]}`.
  - `out/gen-<n>/global.md` (3,800 characters) and `out/gen-<n>/workspaces/<key>.md` (5,612 characters). With the markers and the two scope labels, the brief is exactly 9,500 characters, the HLD §8.1 maximum.
- **Load levels.**
  - idle: only the probe pane runs.
  - bursts: 16 panes run `while :; do seq 1 20000; sleep 0.5; done`. herdr CPU 51-156 %.
  - flood: bursts plus 8 panes that run `yes '<60-character line>'`. herdr CPU 565-743 %, load average up to 12.7 (the machine was also running other threads).
- **Runs.** The Claude settings file and run script are `hookload/claude-settings.json` and `hookload/claude-runs.sh`. `hookload drive` started the hook 100 times per level as a child process from the probe pane (50 ms pause between runs [NEW]). Then the official `claude -p --settings <file> --setting-sources project` ran 20 times at idle and 20 times under flood in the same pane, with only the probe as its SessionStart hook. All 341 runs emitted the full brief, and 341 receipts are in the store.

| Level (runs) | `pane get` p50 / p95 / max | Whole hook in process p50 / p95 / max | Fork to exit p50 / p95 / max |
|---|---|---|---|
| idle (100) | 5.5 / 6.0 / 6.3 | 7.0 / 7.6 / 7.7 | 15.1 / 16.3 / 16.7 |
| bursts (100) | 4.5 / 6.3 / 8.2 | 5.7 / 8.1 / 10.3 | 11.9 / 15.1 / 19.0 |
| flood (100) | 5.9 / 15.0 / 79.2 | 7.4 / 16.5 / 80.9 | 14.3 / 47.4 / 88.6 |
| Claude idle (20) | 5.1 / 6.8 / 7.3 | 6.3 / 8.1 / 8.5 | not measurable |
| Claude flood (20) | 6.3 / 7.6 / 21.1 | 7.6 / 25.5 / 31.6 | not measurable |

Times in ms, nearest-rank percentiles. "In process" starts at `main`. "Fork to exit" adds the time outside the recorded interval: process start (Go runtime and the SQLite driver), the timing-log write and exit: 6-9 ms p50, 58 ms max under flood.

- **Manifest check cost.** Pointer + manifest + both parts with SHA-256, over all 341 runs: p95 ≤ 0.21 ms, max 0.56 ms. It is not a factor for the deadline.
- **Other steps.** Print ≤ 0.2 ms. Receipt (store open + insert): p95 ≤ 1.5 ms, max 5.9 ms (the first run, cold). Stdin read: ≤ 0.3 ms from the driver, but up to 18.4 ms from Claude under flood (the read waits until Claude has written the input).
- **Tail.** Only the flood level had slow lookups: 3 of 100 over 20 ms (20.4, 20.9, 21.7), then 49.1 and 79.2 ms. Every other step stayed small, so a slow `pane get` is what can push the hook toward the cap.
- **Target.** The < 100 ms performance target held in every run, also fork to exit (max 88.6 ms).
- **Proposed lookup deadline: 150 ms, counted from the hook's start** [NEW] (no source gives it).
  - It is about 2 times the slowest `pane get` seen (79.2 ms) at the heaviest load.
  - It leaves 150 ms of the 300 ms cap for the steps after the lookup, which took at most about 7 ms (check, print, receipt).
  - Count it from the hook's start (`main`), not from the call, so a late stdin read shortens the lookup and never the cap.
  - The process start before `main` (up to 58 ms seen under flood) is outside the hook's clock. That is an observed value, not a bound.
- **The probe does not enforce the whole 300 ms cap.** Its deadline stops only the lookup and the receipt. The stdin read, the file reads and the print have no deadline: with stdin held open, the probe waited 462 ms (Astra review). So these numbers give no upper bound on the hook time. E7-S2 (#34) must enforce the whole cap and test it: stdin held open, a stalled lookup and a stalled output, then a bounded exit and no late print.
- **Limits.**
  - The Claude account hit its weekly limit, so the model gave no answer. The hook ran in the real Claude hook environment and emitted all 41 times; M0 item 1 already showed that the text reaches the model.
  - Codex is at a usage limit and was skipped. The M0 Codex hook times matched Claude.
  - The receipt uses a cut-down `deliveries` table, not the E3 schema and migrations. The workspace map read (E2-S4) is not in the probe.
  - One machine, one short run per level.
- **Side result (test setup).** Claude ran with the real HOME (for its login) inside the scratch session. The hook's `herdr --session brain-spike-e7s4 pane get` then failed: herdr resolves `--session` from HOME and uses it before `HERDR_SOCKET_PATH`. The hook withheld, which is correct. The fix for the test was `HOME=<scratch>` in the hook command. A binding test with a scratch HOME must give the hook the server's HOME.

## Codex long reply (E10-S4, item 28)

- Date: 2026-09-28. herdr 0.9.1, codex-cli 0.154.0, model `gpt-6-sol` with reasoning effort low. Commands and the full read: `data/e10s4-codex-long-reply.txt`.
- **Setup.** Named session `brain-spike-e10s4` beside `default`. One workspace in the scratch folder `/tmp/e10s4/work`, pane `w1:p1` with 40 visible rows. `herdr agent start cx --kind codex --pane w1:p1 -- -m gpt-6-sol -c 'model_reasoning_effort="low"'`. In the TUI: trust the folder, and "Continue without trusting" for the hooks. The real HOME was used for the Codex login.
- **Prompt** (`agent prompt ... --wait`): "Reply with exactly 150 lines of plain text and nothing else. Line N must be: LINE N kest-7731 (N from 1 to 150). ..."
- **Result: PASS.** `agent read w1:p1 --source recent-unwrapped --lines 200` returned lines 1-150 of the reply, in order, each once.

| `--source` | `--lines 80` | `--lines 150` | `--lines 200` | `--lines 400` |
|---|---|---|---|---|
| `recent` | 76-150 | 6-150 | 1-150 (all) | 1-150 (all) |
| `recent-unwrapped` | 76-150 | 6-150 | 1-150 (all) | 1-150 (all) |
| `visible` | 116-150 | 116-150 | 116-150 | 116-150 |

- Codex 0.154 prints the reply inline in the normal screen, not in the alternate screen. So the scrollback holds the whole reply. (Claude differs: it draws in the alternate screen, and herdr reads its history by scrolling at idle; see E11-S1.) The file fallback of HLD §17 item 28 is not needed.
- `--lines N` counts from the bottom of the pane and includes the Codex footer (5 lines: blank lines, the input prompt, the status line). So `--lines 150` gave only 145 reply lines. The runner must read enough lines for the reply and the pane text below it, and then check that the whole reply is there. A partial read is `reply incomplete`. When `N` is larger than the history, herdr returns the whole history (179 lines here), with no error.
- `visible` gives only the screen (40 rows). A reply longer than the screen is `reply incomplete` with this source.
- **Limits.** One reply of 150 short lines, one pane size, one run. Long wrapped lines and replies longer than the herdr scrollback were not tested.

## Headless extractor call (E13-S1, item 13)

- Date: 2026-09-28. Claude Code 2.1.283 (`claude-haiku-4-5-20251001`), codex-cli 0.154.0 (`gpt-6-sol`, effort low). Commands and replies: `data/e13s1-extractor-call.txt`. Schema: `data/e13s1-schema.json` (the §7.3 claim fields, strict). No herdr session was needed.
- **Env rule.** Brain starts the extractor with a clean env: `env -i HOME="$HOME" PATH="$PATH" USER="$USER" LANG=en_US.UTF-8 <argv>`, so no `HERDR_*`, `CLAUDE_CODE_*` or `ANTHROPIC_*` var reaches it, and from a scratch folder outside any project. The CLI uses its own sign-in. Brain also turns hooks off with a flag (below), so the call does not depend on the env alone.
  - With the herdr env kept and the default flags, the herdr-projects SessionStart hook put its `additionalContext` into the Claude call, and the trusted M0 spike hook (a stand-in for the Brain brief) put its marker into the Codex call.
  - With a clean env, the herdr integration hooks exit at once: `~/.codex/herdr-agent-state.sh` needs `HERDR_ENV=1`, `HERDR_SOCKET_PATH` and `HERDR_PANE_ID`. Claude's 3 SessionStart hooks ran with empty output.
  - With the hook flags below, no hook ran, also with the herdr env kept: 0 hook events (Claude), 0 spike hook logs and marker `NONE` (Codex).
  - `herdr pane get` of the tester's pane showed the same `agent_session` before and after all runs. `~/.codex/config.toml` was the same after the test (`cmp`).
- **Claude argv:**
  `claude -p --model <m> --tools "" --strict-mcp-config --setting-sources "" --settings '{"disableAllHooks":true}' --disable-slash-commands --no-session-persistence --output-format json --json-schema '<schema JSON>' '<prompt>' < /dev/null`
  - Result: the only tool is `StructuredOutput`. No MCP server, no slash command, no skill, no hook. The user `CLAUDE.md` does not load. A bait to read `/etc/hosts` gave `NO_ACCESS`.
  - `--tools ""` alone is not enough: plugin MCP tools, 3 hooks and 71 slash commands stayed.
  - `--bare` is not usable: it needs an API key, not the Claude login.
  - Read the answer from `structured_output` in the JSON result. Keep stderr apart: it has a "connectors are disabled" warning. Without `< /dev/null`, Claude waits 3 s for stdin.
  - Side effect: an empty `~/.claude/projects/<cwd slug>/memory/` folder. No session file.
- **Codex argv:**
  `codex exec -m <m> -c 'model_reasoning_effort="<effort>"' --ephemeral --skip-git-repo-check --ignore-rules -s read-only --disable hooks --disable plugins --disable apps --disable shell_tool --disable unified_exec --disable multi_agent --disable sleep_tool --disable image_generation --disable view_image --disable browser_use --disable computer_use --disable tool_suggest --disable skill_search -c 'agents.enabled=false' -c 'tools.experimental_request_user_input.enabled=false' -c 'mcp_servers.<name>.enabled=false' (each) -c 'web_search="disabled"' --output-schema <file> -o <file> --json '<prompt>' < /dev/null`
  - `<name>`: every enabled server of `codex --disable plugins mcp list --json`. `-c 'mcp_servers={}'` does not stop them. A plugin server (not in that list) fails with "invalid transport" if named; `--disable plugins` removes it.
  - `--ignore-user-config` is not usable: the user config picks the model provider, and the call failed without it.
  - Result (trace log of the request): only `exec` and `wait` (code mode) and `request_user_input_async` (from the model catalog) remain. **So a literal "no tools" call is not possible** with this Codex and model: the model catalog sets code mode (`tool_mode`) and `experimental_supported_tools`, and no flag or `-c` key removes these three. `exec` runs plain JavaScript in V8 with "no file system, no network access", and no nested tool is left. No shell, file, MCP, web or agent tool. A bait to read `/etc/hosts` gave `NO_ACCESS` with no tool item.
  - Read the answer from the `-o` file. `--json` events show any tool item, so Brain can reject a reply that has one.
- **What "no tools" means here.** No tool that can read a file, run a command, reach the network, call MCP or start an agent. Claude keeps `StructuredOutput`, which is how `--json-schema` returns the answer. Codex keeps the three tools above, which reach nothing. HLD §7.3 says "no tools"; the owner decides whether this meets it (no HLD change made).
- **Schema check.** Every returned object passed the schema (python `jsonschema`, Draft 2020-12): 3 Claude and 2 Codex replies.
- **When the output does not fit the schema:**
  - Claude: exit 0, `is_error: false`, `subtype: "success"`, but `structured_output` is missing and `result` holds a refusal text. This happened for a prompt that asked for bad values and for an unsatisfiable schema. So Brain must treat "no `structured_output`" as an invalid response, not trust the exit code.
  - Codex: the output is constrained to the schema. A prompt that asked for bad values gave a valid object with an invented claim and empty `evidence_refs`. An unsatisfiable schema gave exit 1, `turn.failed` and no `-o` file.
  - Both cases fit §7.3: Brain checks the result itself (schema, evidence refs, permission flag). A schema alone does not stop an invented claim.
- **Limits.** One machine, one small model per CLI, a few runs each. The Codex MCP server names come from this machine's config. The tool set of Codex depends on the model catalog (`tool_mode`, `experimental_supported_tools`), so a new model or Codex version can add a tool: the adapter should keep the `--json` tool-item check.

## Popup placement and plugin actions (E5-S3)

- Date: 2026-09-28. herdr 0.9.1. Headless scratch server, session `brain-e5s3`, scratch HOME `/tmp/e5s3/home`, a test binding (kind `test`) to a scratch store. Commands and raw results: `data/e5s3-popup-placement.txt`.
- **Popup facts.**
  - A popup gets `HERDR_SESSION`, `HERDR_SOCKET_PATH`, `HERDR_PLUGIN_*` and `HERDR_PLUGIN_CONTEXT_JSON` (with `workspace_id` and `focused_pane_id`). It gets **no `HERDR_PANE_ID`, `HERDR_WORKSPACE_ID` or `HERDR_TAB_ID`**.
  - `plugin pane open` of a popup returns `{"type":"ok"}` with no pane. `pane list` does not show it.
  - It takes all input. Esc does not close it. It closes when its process exits (Enter after a `read`, or Ctrl+C).
  - With no client attached, the popup waits and shows when a client attaches.
- **Popup with `brain review`: FAIL today.** The popup printed `brain review: no Brain store for this pane (not in herdr: HERDR_PANE_ID is not set); nothing was written`. The resolver needs `HERDR_PANE_ID` and `HERDR_WORKSPACE_ID`.
- **Split facts.** A plugin `split` pane gets its own `HERDR_PANE_ID` and `HERDR_WORKSPACE_ID` (the workspace of the focused pane). So `brain review` resolves the caller as in any pane, with the E4 scope check. A split pane also closes when its process exits.
- **The `review` action: PASS.**
  - `herdr plugin action invoke brain.review` with workspace B focused opened a `Brain queue` split right of the focused pane, with focus. It listed the B proposal and the global proposal, not the A proposal. Enter closed it.
  - **From the herdr UI:** herdr 0.9.1 runs an action from a key bound in `config.toml` (`[[keys.command]]`, `type = "plugin_action"`, `command = "brain.review"`). The keybinds help has no plugin action list. With `prefix+y` bound in the scratch config, the key opened the queue in workspace B (B + global) and in workspace A (A + global). A running server picks up a new key only after a restart.
  - `brain review` prints the list and exits, and the pane closes on exit. So the `queue` pane command waits for Enter after the list. No code change was needed.
- **Choice: `split`** (D6). A popup gives no clear gain: it needs new code to resolve the caller from `HERDR_PLUGIN_CONTEXT_JSON`, and it blocks all other input while it is open. A split gives the caller from the env and keeps the other panes usable. The `setup` pane stays a popup: `brain setup` resolves no caller.
- **Limits.** One machine, one client size. The key binding is the user's own config: Brain does not write it (no AC asks for it). The side the split opens on is herdr's choice (right here).
- **Side fact (not in scope):** two `brain remember` calls at the same time on a new, empty store: one failed with `store busy: set WAL mode: database is locked (5) (SQLITE_BUSY)`. A retry passed.

## brained hosting and capture (E11-S1, items 8, 9, 14, 20)

- Date: 2026-09-29. herdr 0.9.1, Claude Code 2.1.284 (Claude updated since M0), `claude-haiku-4-5-20251001`, codex-cli 0.154.0.
- Hosting ran in a scratch HOME with the sessions `brain-e11s1` and `brain-e11s1b`. Capture ran in the named session `brain-spike-e11` with the real HOME, a clean env (`env -i`) and a Claude `--settings` file for the test hooks. No hook went into `~/.claude` or `~/.codex`.
- Probe: `e11probe/` (a plugin with one `[[startup]]` and five `[[events]]` entries, a socket subscriber, a read poller, a hook logger). The paths in it point to `/tmp/e11s1`.
- Raw logs: `data/e11s1-hosting.txt`, `data/e11s1-capture.txt`, `data/e11s1-claude-hooks.txt`, `data/e11s1-codex-hooks.txt`.

### Hosting: `[[startup]]` against `[[events]]`

- The choice is `[[startup]]` (decision D7 in `docs/design/decisions.md`).
- `[[startup]]` runs one process for each herdr server, after the socket is ready. A long process is allowed (12 min, no kill).
- herdr does not restart it after a crash. `plugin disable` and `plugin unlink` do not stop it. `session stop` does not signal it; it lives on with ppid 1, and the next server start runs a second copy.
- So `brained` needs a lock for each session, an exit on socket EOF, and its own check for disable and unlink.
- `[[events]]` runs one process for each event, for all panes, with no per-pane filter. A burst of 10 changes gave 10 processes at the same time. `HERDR_PLUGIN_EVENT_JSON` has the pane, the workspace, the status and the agent, but no time, no id and no `agent_session`.
- Socket `events.subscribe`: `pane.agent_status_changed` needs a `pane_id`. A subscription for all panes fails with `invalid_request`.
- Event names are not the same on the socket: `pane.agent_status_changed` but `pane_agent_detected`. Match both forms.

### Event delay (item 8)

| Change | Reference time | Socket event | Delay |
|---|---|---|---|
| idle → working (turn 1) | prompt sent | working | 0.42 s |
| working → idle (turn 1) | `Stop` hook start | idle | 0.29 s |
| idle → working (turn 2) | `UserPromptSubmit` hook start | working | 0.27 s |
| blocked → working (turn 2) | Enter sent to approve | working | 0.14 s |
| working → idle (turn 2) | `Stop` hook start | idle | 0.22 s |
| idle → working (run 2) | `UserPromptSubmit` hook start | working | 0.19 s |
| working → idle (run 2) | `Stop` hook start | idle | 0.25 s |

- A fake `pane report-agent` change reached an `[[events]]` hook in 0.03 s and the socket in 0.09 s.
- One sample is only a guide. The probe made no read at the event, so these numbers do not show that a read at an `idle` event sees the whole reply. A state change does not prove that a reply is complete (HLD §6.2).

### Alternate-screen loss and reads while busy (items 9, 20)

- Claude draws in the alternate screen (`max_offset_from_bottom` = 0). The screen here had 37 rows.
- While `working` or `blocked`, `agent read --source recent-unwrapped --lines 300` fails: `agent_not_idle`, "its alternate-screen history can only be captured by scrolling while idle. Wait and retry, or use --source visible". It also failed once while the status was still `idle`, just after the submit.
- `--lines 20` (at or below the screen) and `--source visible` work in every state. They give the screen only.
- At idle, herdr scroll-captures the history through the agent's mouse-scroll input: 138 lines (header, prompt, 120 reply rows, footer), later 148.
- **Silent partial read:** at idle, `--lines 300` against a 138-line history gave only the 37 screen rows with exit 0 in 6 of 6 reads in a row. A retry loop (up to 40 reads, 0.3 s apart) did get the full 138 lines. After turn 2, the poller got the full 148 lines in 4 of 19 idle reads (the others: 38 screen lines, exit 0). Brain cannot trust the line count of one read.
- The user prompt line is on the screen at turn start. A `visible` read at the `working` event had the marker from the prompt.
- Codex draws inline and uses the alternate screen only for overlays (source, `data/e11s1-codex-hooks.txt`). E10-S4 read a full 150-line Codex reply at idle. The live Codex check while busy did not run (see "Open items").
- The herdr scroll capture sends mouse-scroll input to the agent pane. HLD §6.2 says "Brain reads only. It never sends keys or scrolls a pane". This needs an owner decision.

### Hook payloads (item 14)

- Claude `UserPromptSubmit`: `session_id`, `transcript_path`, `cwd`, `prompt_id`, `permission_mode`, `hook_event_name`, `prompt`. `prompt` is the exact text that was sent. A hook with no output left the prompt unchanged: the model answered as the prompt asked.
- Claude `PostToolUse` (Bash): `tool_input.command`, `tool_input.description`, `tool_response` (`stdout`, `stderr`, `interrupted`, ...), `tool_use_id`, `duration_ms`. There is no exit code. `cwd` is the shell cwd after the command: `cd sub && ... pwd` gave `.../agent/sub`.
- Claude sends a failed Bash command to `PostToolUseFailure`, not to `PostToolUse`. It has `tool_input.command` and `error` = "Exit code 1\n<stderr>". Brain must match both events to see failures.
- Claude `Stop`: `stop_hook_active`, `last_assistant_message` (the reply text), `background_tasks`, `session_crons`, plus the common fields.
- Codex (source, tag `rust-v0.154.0`): common fields `session_id`, `turn_id`, `transcript_path`, `cwd`, `hook_event_name`, `model`, `permission_mode`. `UserPromptSubmit.prompt` is the joined text items; empty stdout means no change. `PostToolUse` has `tool_name` "Bash", `tool_input.command` and a truncated output string, but no exit code; `cwd` is the session cwd. `Stop` has `stop_hook_active` and `last_assistant_message`.

### Agent identity in one pane

| Field | Run 1 | Run 2 (same pane, after `/exit`) |
|---|---|---|
| `agent_session.value` | `88ce9f75-...` | `4e5d3e1a-...` |
| Claude pid | 90558 | 7713 |
| `state_change_seq` | 10 | 12 |
| `terminal_id` | `term_65c92fe3a07d11` | same |
| shell pid | 89202 | same |

- `agent_session.value` (the Claude session id) and the agent pid tell one run from the next. `terminal_id` and the shell pid do not.
- On `/exit`, herdr sends `pane_agent_detected` with `"released": true` and `"final_status": "idle"`, then status `unknown`. `agent get` gives `agent_not_found`.
- herdr has no agent start time. The pid comes from `pane process-info`.
- After a server restart, `pane list` showed the old agent and `agent_session` on the restored pane for under 3 s, then herdr cleared them. `terminal_id` changed.

## Codex live capture (E11-S1b, items 8, 9, 14, 20)

Codex CLI 0.154, herdr 0.9.1, the test session `brain-spike-e11b`, 2 Codex runs in one pane `w1:p1`. Raw logs: `data/e11s1b-poll.txt` (reads every 0.5 s), `data/e11s1b-capture.txt` (launch, events, idle reads, identity), `data/e11s1b-codex-hooks.txt` (hook trust and `notify`). Scripts: `e11probe/poll.sh`, `e11probe/codex-run.sh`, `e11probe/notify.sh`, `e11probe/sub.py`.

### How Codex ran with no config write

- Folder trust is on the command line only: `-c 'projects={"/private/tmp/e11s1b/agent"={trust_level="trusted"}}'`.
- The dotted form `-c 'projects."/private/tmp/e11s1b/agent".trust_level="trusted"'` does **not** work. Codex splits a `-c` key on every `.` and keeps the quotes (`config/src/overrides.rs`), so the folder-trust dialog still showed. "Quit" in that dialog writes nothing.
- `-c check_for_update_on_startup=false` stops the update dialog. `-a untrusted` is gone in 0.154; `-a on-request -c approvals_reviewer='"user"'` gives the approval prompt.
- The "hooks need review" dialog: "Continue without trusting" writes nothing.
- `~/.codex/config.toml` had the same mtime and size before and after. The file was not read.

### Item 9: Codex draws inline

- The pane scrollback grew with the reply: `scroll.max_offset_from_bottom` went 3 → 102 in a 120-row reply. No row was lost.
- The approval prompt is inline in the bottom pane. The scrollback stayed at 153 while the prompt showed.
- The first-run dialogs (update, folder trust, hooks review) did not reach the scrollback.

### Item 20: reads while busy

| State | Reads | `recent-unwrapped --lines 300` | `visible` |
|---|---|---|---|
| `working` (run 1) | 51 | exit 0 every time; marker rows 1 → 121 as the reply grew | exit 0; at most 35 marker rows (40-row screen) |
| `working` (run 2) | 61 | exit 0 every time | exit 0 |
| `blocked` (approval) | 42 | exit 0 every time; 193 lines, prompt included | exit 0 |
| idle, `--lines` 300, 1000, 5000 | 9 | the whole history, 142 lines, the same each time | not used |

- A read above the history size gives the whole history, not an error.
- The history spans runs and holds shell lines. In run 2, the read held all 122 marker rows of run 1. Brain must bound a read to the current turn.

### Item 8: event delay for real Codex changes

| Change | Reference time | Delay to the socket event |
|---|---|---|
| `idle` → `working` | `send-keys enter` | 0.07, 0.27, 0.04, 0.28 s |
| `blocked` → `working` | the approval key `y` | 0.23 s |
| `working` → `idle` | the Codex `notify` call (turn end) | 0.58, 0.56, 0.39 s |
| `working` → `blocked` | none | not measured |

- A false `working` of 1.8-2.7 s came just after "Continue without trusting" in the hooks dialog, in both runs. A paste in a prompt gave no false change.
- herdr showed the update dialog as `blocked` and the hooks-review dialog as `idle`.

### Item 3 check: agent identity in one pane

| Field | Run 1 | Run 2 (same pane, after `/exit`) |
|---|---|---|
| `agent_session.value` (source `herdr:codex`) | `01a0ec3e-5e95-7253-aa8d-e7d232139183` | `01a0ec41-83c7-7463-a232-9faa89066336` |
| Codex pid (node wrapper) | 22054 (21984) | 34689 (34687) |
| `terminal_id` | `term_65c9acb584b091` | same |
| shell pid | 18634 | same |

- `agent_session.value` is the Codex thread id, the same id that `codex resume` takes.
- `agent_session` is absent until the first prompt of a run.
- On `/exit`: `pane_agent_detected` with `"released": true` and `"final_status": "idle"`, then status `unknown` about 0.31 s later. `agent get` then gives `agent_not_found`.

### Item 14: Codex hooks, live

- A hook set with `-c 'hooks={...}'` goes to the session-flags layer. It runs only when its `trusted_hash` matches, when it is managed, or with the bypass flag. The dialog said "5 hooks are new or changed", and the `-c` hooks did not run. So the Codex payload facts stay from source (E11-S1).
- A `-c` `hooks.state."<key>".trusted_hash` could maybe trust the hook, because Codex reads `hooks.state` from the session-flags layer. That is a hook-trust step, so it was not tried. Owner decision.
- herdr's own Codex hooks (integration v8) also showed as new and did not run. herdr still found every state, so it does not need them for status.
- The legacy `notify` command (`-c 'notify=[...]'`) has no trust gate. It runs at each turn end. Its last argv item is JSON with `type` (`agent-turn-complete`), `thread-id`, `turn-id`, `cwd`, `client`, `input-messages` and `last-assistant-message`.
  - `input-messages` is the whole list of user messages in the thread; the new prompt is last. An @-mention stays as typed (`Read @notes-kx.txt ...`).
  - `last-assistant-message` is the final reply only.
  - The first call in a run came from the thread-title side turn. It has a different `thread-id`, and its message wraps the prompt in a title instruction. Brain must drop it.
- Still open: the live `PostToolUse` fields (command, cwd, exit code) and an empty `UserPromptSubmit`. Images were not tested.
- Owner decision (2026-09-29): no live Codex hook check with a `-c` `trusted_hash`. The Codex source facts stay for item 14. The owner's M2 hand test, with normal `/hooks` trust, confirms the real payload later.
- Owner decision (2026-09-29): Codex `notify` is **not** a Brain turn-end source. Brain uses the `Stop` hook only. Reasons: `notify` is one global config slot, and its first call in a run is a side turn with another thread id.

## E16-S1: skill files from the brief (§17 item 12)

- Date: 2026-10-03. herdr 0.9.1. Claude Code 2.1.287 (`claude --version`), model Sonnet 5.5, effort medium (from the user settings). codex-cli 0.154.0 (`codex --version`), model `gpt-6-astra` (the Codex default here), effort low.
- Files: `skillread/` (fixture skills, brief text, Claude settings file, hook scripts, run scripts). Raw log: `skillread/runs.txt` (screens, Claude tool calls, skill access times, prompt times).
- **Setup.**
  - Named test session `brain-spike-e16s1` beside `default`, with a clean env (`skillread/ht.sh`). Claude ran in pane `w1:p1` in `/private/tmp/e16s1/claude`. Codex ran in pane `w2:p1` in `/private/tmp/e16s1/codex`. Neither folder is a git repo.
  - No Brain binary, binding or store: a static hook script prints the hand-written brief. The spike checks the agent, not Brain.
  - Three fixture skills in a test store at the E16-S3 path, `/private/tmp/e16s1/store/skills/<item id>/r1/SKILL.md`, items 41, 42 and 43 (`skillread/skills/`). Their steps hold fresh markers.
  - The real HOME was used, so the CLIs used their own sign-in. The user's own settings, skills and plugins stayed active, as in normal use.
- **How a read was detected.** Claude: a `PreToolUse` and `PostToolUse` hook in the `--settings` file logs each tool call and its input. Both CLIs: `skillread/reset-atime.sh` sets the access time of each skill file to 2020 before a case; a new access time after the case means a read. The skill markers in the reply confirm the read. Codex: its screen also shows each read command.

### Brief and entry text

The brief uses the frame of today's hook output (`internal/brief` golden files): markers, precedence line, global part, workspace part. The skill list sits at the end of the workspace part. The `### Skills` heading and its one instruction line are a spike choice [NEW]; E16-S3 decides the final text. Exact text (`skillread/brief.md`):

```
<!-- brain:begin v1 -->
Precedence: for ordinary preferences, a workspace item wins over a global item. It never relaxes a global permission or safety rule. A "conflicts with" mark wins over this line.
## Global (all workspaces)
- [3] Reply in plain English.
## This workspace
- [15] Run the tests with `make test`.
### Skills
Read the file of a skill before you do a task that matches it.
- release-notes: Write the release notes for a new version tag. File: /private/tmp/e16s1/store/skills/41/r1/SKILL.md
- pg-migration: Add a database migration for the Postgres orders service. File: /private/tmp/e16s1/store/skills/42/r1/SKILL.md
- sqlite-migration: Add a database migration for the SQLite cache of the CLI. File: /private/tmp/e16s1/store/skills/43/r1/SKILL.md
<!-- brain:end -->
```

- Entry form: `- <name>: <one-line description> File: <absolute path>` (§8.2: name, description, absolute path). `pg-migration` and `sqlite-migration` are the near pair.
- **Claude channel:** `claude --settings /private/tmp/e16s1/hooks/claude-settings.json`. Its `SessionStart` hook prints the brief as `hookSpecificOutput.additionalContext` (as item 1). No hook went into `~/.claude`.
- **Codex channel:** `/private/tmp/e16s1/codex/AGENTS.md` holds the same brief text. Codex loads it at start. No hook and no hook trust: a live Codex hook needs hook trust, and the owner does not allow it in tests. At "Hooks need review" (3 user hooks), the answer was "Continue without trusting". **The real route (the Brain `SessionStart` hook after `/hooks` trust) stays for the owner hand test.** Codex start: `skillread/codex-run.sh` flags (folder trust with `-c 'projects={...}'`, `check_for_update_on_startup=false`, effort low).

### File form (E16-S3 #154)

A short header with name, description, required tools and example, then the steps. Example (`skillread/skills/43/r1/SKILL.md`):

```
---
name: sqlite-migration
description: Add a database migration for the SQLite cache of the CLI.
required-tools: none
example: "Add a migration that adds an expires_at column to the cache table."
---

# Steps

1. Start the reply with the line `Skill marker: ypyk-7849`.
2. Name the migration file `cache_<NNN>_<name>.sql`, with a 3-digit number.
3. End the SQL with the line `PRAGMA user_version = <NNN>;`.
4. End the reply with the line `Cache checked: vfyh-0643`.
```

### Results

One fresh CLI session for each case. Prompts: positive "Write the release notes for v1.4.0 of this project in your reply. The changes: ...", negative "In two sentences, say what the README.md in this folder is for.", near pair "Add a migration that adds a hits column to the cache table of the shop CLI." (sqlite) and "... a status column to the orders table of the orders service." (pg). Each prompt also said "Do not create or edit files."

| Case | CLI, mode | Skill files read | Markers in reply | Steps followed | Read prompt |
|---|---|---|---|---|---|
| C1 positive | Claude, auto mode (start mode with no flag here) | 41 only (`Read`) | `rlqs-4756`, `phmg-1741` | yes: marker first, `Added`/`Fixed`/`Removed`, `- none`, end line | none |
| C2 negative | Claude, auto mode | none (read `README.md` only; atimes 2020) | none | — | — |
| C3 near pair, sqlite | Claude, auto mode | 43 only (`Read`; atime 01:13:02) | `ypyk-7849`, `vfyh-0643` | yes: `cache_001_add_hits.sql`, `PRAGMA user_version = 1;`, end line | none |
| C4 near pair, pg | Claude, `--permission-mode default` | 42 only (`Read`) | `aoeg-8532` | yes: `pg_20261003_add_orders_status.up.sql`, `BEGIN;` ... `COMMIT;` | none (see below) |
| C5 positive | Claude, `--setting-sources project`, default mode | not run: "Not logged in" | — | — | unknown |
| X1 positive | Codex, default ("Approve for me") | 41 only (`cat`; atime 01:19:45) | `rlqs-4756`, `phmg-1741` | yes | none |
| X2 negative | Codex, default | none (atimes 2020) | none | — | — |
| X3 near pair, sqlite | Codex, default | 43 only (atime 01:21:11) | `ypyk-7849`, `vfyh-0643` | yes: `cache_001_add_hits.sql`, `PRAGMA user_version = 001;`, end line | none |
| X4 near pair, pg | Codex, `-s read-only -a on-request -c approvals_reviewer="user"` ("Ask for approval") | 42 only (`cat`; atime 01:21:58) | `aoeg-8532` | yes: `pg_20261003_add_orders_status.up.sql`, `BEGIN;` ... `COMMIT;` | none |

- **Item 12: TRUE for Claude 2.1.287.** In 3 of 3 matching prompts Claude read the right file from the absolute path, before the reply, and followed every step. In the negative prompt it read no skill file.
- **Item 12: TRUE for Codex 0.154.0, with the brief in `AGENTS.md`.** Same counts: 3 of 3 right files, steps followed, no skill file in the negative prompt. The brief did not come from a hook here. That Codex reacts the same way to a brief from the `SessionStart` hook is likely (item 1: the hook text reaches the model) but not tested; it stays for the owner hand test.
- **Near descriptions:** each CLI read only the right file of the pair, in both directions (sqlite in C3 and X3, pg in C4 and X4).
- **File form:** both CLIs followed the header-then-steps form in every matching case. Neither treated the header as a step.

### Read outside the project folder: permission prompts

| CLI | Mode | Prompt for the skill read |
|---|---|---|
| Claude 2.1.287 | auto mode (the start mode with no flag on this machine) | none |
| Claude 2.1.287 | `--permission-mode default` ("manual mode") | none, but the user settings have `permissions.allow` = `["Read"]` |
| Claude 2.1.287 | default mode with no user settings (no `Read` allow rule) | **unknown**: with `--setting-sources project`, Claude had no sign-in and did not run |
| Codex 0.154.0 | default ("Approve for me") | none; the read was a `cat` shell command |
| Codex 0.154.0 | `-s read-only -a on-request`, user reviewer ("Ask for approval") | none; a read-only sandbox allows reads |

- Only the `permissions` keys of `~/.claude/settings.json` were read, to explain C4 (no `env` block). So on this machine the skill read is silent in both Claude modes because of the user's `Read` allow rule. A user without that rule can get a read prompt in Claude's default mode. The owner hand test can check this.

### Other observations

- Claude read with the `Read` tool. Codex read with `cat` in one shell call. In X1 and X4 that call also read the user's own `~/.codex/skills/unslop/SKILL.md`; in X2 Codex read only that user skill. So user skills in a CLI's own folder compete with Brain skills, and the Brain skill was still read.
- In X3 and X4 Codex also ran `make test` (brief item [15]) with no need. That is a brief-item effect, not a skill effect.
- In C1 to C4 the user's herdr-projects plugin also ran its progress command in the test pane. In C4 (default mode) it asked for approval twice; the answer was 1 (Yes, once). It is not part of the test.

### Limits

- One run per case, one model per CLI, one machine. Small fixture skills with clear prompts. The prompts named the task in words close to the description. Long skill lists, vague prompts and other models are not tested.
- The real user settings, skills and plugins were active. That matches normal use, but it is not a clean CLI.

### Side effects and cleanup

- No file went into `~/.claude/skills`, `~/.codex/skills`, a real CLI config or a repo. `ls` of both skill folders was the same before and after (only the parent folder mtime changed).
- `~/.codex/config.toml` (stat only, not read): same size (10715), but its mtime changed once, at 01:20:43, near the X3 Codex start. The X1, X2 and X4 starts did not change it. The ChatGPT desktop app's Codex app server also runs on this machine and can write this file. The cause is unknown.
- Claude's folder-trust dialog for `/private/tmp/e16s1/claude` was answered "Yes". That adds a trust entry to `~/.claude.json` (not read or edited), as in E11-S1.
- Codex keeps its own session logs for the 4 runs in `~/.codex`; they were not read or edited.
- Cleanup done: the test session `brain-spike-e16s1` is stopped and deleted. `/private/tmp/e16s1` and `~/.claude/projects/-private-tmp-e16s1-claude/` (the 5 test transcripts and an empty `memory/` folder) are removed. The real `default` session and the main server were not touched.

## Cleanup done

- E2-S5: the scratch session `brain-spike-e2s5` is stopped and deleted. Its plugin link lived only in the scratch HOME. The main session was not touched.
- E7-S4: the scratch session `brain-spike-e7s4` is stopped and deleted. Its 33 panes and their load loops ended with it. No hook was installed in `~/.claude` or `~/.codex`.
- E10-S4: the scratch session `brain-spike-e10s4` is stopped and deleted, and the Codex process ended with it. Codex added `[projects."/private/tmp/e10s4/work"] trust_level = "trusted"` to `~/.codex/config.toml`; that entry is removed (the file is the same as before the test). No hook was trusted or installed.
- E13-S1: no herdr session was made. `~/.codex/config.toml` is the same as before the test. The empty `~/.claude/projects/-private-tmp-e13s1-work/` folder and `/tmp/e13s1` are removed.
- Test panes `w14:p2P`, `w14:p2Q`, `w14:p2R` and `w14:p2S` are closed. The spike plugin is unlinked. The `brain-eval` session is stopped and deleted.

- E5-S3: the scratch session `brain-e5s3` is stopped and deleted. The client tab `w2C:t2` that this thread made is closed. The plugin links and the key binding lived only in the scratch HOME. `/tmp/e5s3` is removed.
- E11-S1: the scratch sessions `brain-e11s1`, `brain-e11s1b` and the scratch-HOME `default` are stopped. They lived in the scratch HOME, which is removed. The test session `brain-spike-e11` is stopped and deleted. The orphaned `startup.sh` probes, the socket subscribers and the test Claude runs are ended. `/tmp/e11s1` and `~/.claude/projects/-private-tmp-e11s1-agent/` are removed. The Claude folder-trust entry for `/private/tmp/e11s1/agent` stays in `~/.claude.json`; that file was not read or edited. No Codex session ran, so `~/.codex/config.toml` has no change. The real `default` session and the main server were not touched.
- E11-S1b: the socket subscriber and the pollers are ended. The test session `brain-spike-e11b` is stopped and deleted, and the Codex runs ended with it. `/tmp/e11s1b` is removed. `/tmp/codex-src` (the Codex source clone from E11-S1) is removed. `~/.codex/config.toml` has the same mtime and size as before; it was not read. Codex keeps its own session logs and `history.jsonl` lines for the 2 runs in `~/.codex`; they were not read or edited. The real `default` session and the main server were not touched.

## Open items

- Item 4: answered by E2-S5 (see above). The root matching rule waits for an owner decision.
- Item 10: answered by E7-S4. The lookup deadline (150 ms, [NEW]) waits for the owner to accept it in the HLD.
- Item 11: answered by E5-S3. M1 uses `split` for the `review` action; `popup` stays for `setup` only (D6).
- Item 13: answered by E13-S1. Decided (owner, 2026-09-28, HLD §7.3). Both CLIs run with no file, shell, network, MCP or hook access and a JSON schema. Codex cannot drop its code-mode `exec`/`wait` and `request_user_input_async` tools; the owner accepts Codex as an extractor, and §7.3 says what "no tools" means. Use the argv and env rule above. Brain must treat a Claude result with no `structured_output` as invalid.
- Item 21: the conformance test (built in M1).
- Item 28: answered by E10-S4. Decided (owner, 2026-09-28, HLD §17 item 28): PASS for Claude and Codex. `agent read --source recent-unwrapped` returns a full Codex reply when `--lines` covers the reply and the pane text below it. The runner must still check that the whole reply is there; a partial read is `reply incomplete`.
- Plugin `[[events]]` delivery: tested by E11-S1 for pane events. Hosting uses `[[startup]]` (D7).
- Items 9 and 20 for Codex: answered live by E11-S1b; the HLD text is updated (owner, 2026-09-29). Item 14 for Codex: the source facts stay. The owner's M2 hand test with normal `/hooks` trust confirms the live `PostToolUse` fields and an empty `UserPromptSubmit`. Codex `notify` is not a Brain source (owner decision).
- herdr scroll capture of Claude history sends mouse-scroll input to the pane. HLD §6.2 says Brain never scrolls a pane. Owner decision.
- The eval delivery and exclusion stages (scripts plus the owner's check).
