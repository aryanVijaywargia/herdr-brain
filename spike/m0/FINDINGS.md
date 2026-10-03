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
| 12 | Agent reads a skill file from an absolute path in the brief, when relevant | TRUE for Claude (E16-S1). Codex: UNKNOWN for the HLD hook route (not tested, owner hand test); TRUE with the brief in `AGENTS.md` (E16-S1) | Claude Code 2.1.287 and codex-cli 0.154.0, one fresh session per case: each matching prompt read the right skill file and used its markers and its steps (one format slip in C3); each negative prompt read no skill file; a near-description pair picked the right file. No permission prompt for the read in the modes tested. See "E16-S1: skill files from the brief (§17 item 12)". |
| 13 | Headless extractor call, no tools, JSON schema | PASS for no file, shell, network, MCP or hook access; PARTIAL for literal "no tools" (E13-S1) | Claude 2.1.283 and codex-cli 0.154.0 both return schema-valid JSON. Claude keeps only `StructuredOutput`. Codex 0.154 with `gpt-6-sol` keeps code-mode `exec`/`wait` (V8, no nested tools) and `request_user_input_async`; no flag removes them. When the output cannot fit, Claude can exit 0 with no structured output; Codex bends a bad prompt to the schema and fails only for an unsatisfiable schema. See "Headless extractor call (E13-S1)". |
| 14 | Hook payloads: `UserPromptSubmit`, `PostToolUse`, `Stop` | PASS for Claude (live, E11-S1). Codex: PASS from source, live hook OPEN (E11-S1b) | Claude: the `prompt` field is the exact text. An empty hook leaves the prompt unchanged. `PostToolUse` has the command and the shell cwd after the command, but no exit code. A failed command goes to `PostToolUseFailure` with `error` = "Exit code N...". Codex (source): `tool_input.command`, the session cwd, no exit code. See "brained hosting and capture (E11-S1)". Codex live (E11-S1b): a hook set with `-c` still needs the hook-trust step, so no Codex hook ran and the source facts stay. The legacy `notify` command (no trust gate) gave the typed prompt with the @-mention as typed and the final reply. See "Codex live capture (E11-S1b)". |
| 16 | agy: `PreInvocation` with `invocationNum == 0` and `injectSteps[].ephemeralMessage` reach the model; hook file; input paths; tool event | TRUE for delivery on agy 1.2.16, print mode (E16-S2). `invocationNum == 0` is not a session start: FALSE. Interactive mode and 1.2.11: UNKNOWN | `PreInvocation` ran at each turn with `invocationNum` 0; the ephemeral brief reached the model (3 of 3 markers) and stayed in the conversation (B2: a fact that no reply had shown, with no new brief). Input: `conversationId`, `workspacePaths[0]`. Tool event `PostToolUse` (`run_command`, `toolCall.args.CommandLine`). Workspace `.agents/hooks.json`; the global `~/.gemini/config/hooks.json` from docs only. agy updated itself from 1.2.11 during the test. See "E16-S2: agy and OpenCode facts (§17 items 16, 18)". |
| 18 | OpenCode: `session.created` and `experimental.chat.system.transform` add the brief through the shim; the API's stability | TRUE on OpenCode v2.0.18 with the V2 API (E16-S2). The V1 names: the V2 docs say V1 plugins do not run in 2.x (not tested one by one) | A V2 project plugin (`.opencode/plugins/brain.ts`) with `ctx.event.subscribe()` `session.created` and `ctx.session.hook("context")` added the brief; the marker came back (O1, O5), and a reopened session got it through the first `context` call (O7). `ctx.tool.hook("execute.after")` gives `input.command` and `result.output.exit`. The pane env reaches the shim only with `--standalone`. See "E16-S2: agy and OpenCode facts (§17 items 16, 18)". |
| 20 | `recent-unwrapped` while `working` or `blocked` | FAIL for Claude (E11-S1), PASS for Codex (E11-S1b) | Claude: `--lines 300` returns `agent_not_idle` in `working`, `blocked` and just after submit. `--lines 20` (at or below the screen) and `visible` work in every state. At idle, a `--lines` value larger than the history often returns only the screen, with exit 0. Codex (live, E11-S1b): `--lines 300` gave exit 0 in all 112 `working` reads and all 42 `blocked` reads, with the history growing as the reply grew. At idle, `--lines` 300, 1000 and 5000 gave the whole pane history (142 lines) in 9 of 9 reads. `visible` gives the 40-row screen. |
| 21 | Conformance test | OPEN | |
| 23 | "Agent stopped working" condition per CLI; the §10.4 event key | TRUE for both CLIs: `working → idle` (focused pane) or `working → done` (unfocused pane). §10.4 key: FALSE. Proposed method P: TRUE in all tested cases (E17-S1) | Claude Code 2.1.288 and codex-cli 0.154.0 on herdr 0.9.1. A user stop (Esc) gives the same change as a normal end; an approval turn ends `blocked → working → idle/done`; a denied approval (Claude) ends `blocked → idle`, not a stop. `state_change_seq` is one counter for the whole herdr server and starts at 0 again after a server restart, and herdr then resumes the same agent session in the same pane, so the §10.4 key repeats (a real stop was skipped). The key alone also claims a duplicate that comes while the agent works again. P (join the event to `agent get`, keep only joined states, add `terminal_id` to the key) claimed 31 of 31 live stops once and nothing else. See "E17-S1: stopped working and the event key (§17 item 23)". |
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
- **Fresh sessions.** Each case started a new CLI process from the pane shell with `herdr agent start`, with no `--resume` or `--continue`. Claude C1-C4 printed 4 different session ids at exit, and the C1 `SessionStart` input has `"source":"startup"`. The logs of C2-C4 kept only the event name, not the source. Each Codex case shows its own start banner. Evidence: `runs.txt`, "Claude launch evidence" and the X1-X4 screens.
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
| C3 near pair, sqlite | Claude, auto mode | 43 only (`Read`; atime 01:13:02) | `ypyk-7849`, `vfyh-0643` | partly: `cache_001_add_hits.sql` and the end line, but `PRAGMA user_version = 1;`, not the 3-digit `001` of steps 2 and 3 | none |
| C4 near pair, pg | Claude, `--permission-mode default` | 42 only (`Read`) | `aoeg-8532` | yes: `pg_20261003_add_orders_status.up.sql`, `BEGIN;` ... `COMMIT;` | none (see below) |
| C5 positive | Claude, `--setting-sources project`, default mode | not run: "Not logged in" | — | — | unknown |
| X1 positive | Codex, default ("Approve for me") | 41 only (`cat`; atime 01:19:45) | `rlqs-4756`, `phmg-1741` | yes | none |
| X2 negative | Codex, default | none (atimes 2020) | none | — | — |
| X3 near pair, sqlite | Codex, default | 43 only (atime 01:21:11) | `ypyk-7849`, `vfyh-0643` | yes: `cache_001_add_hits.sql`, `PRAGMA user_version = 001;`, end line | none |
| X4 near pair, pg | Codex, `-s read-only -a on-request -c approvals_reviewer="user"` ("Ask for approval") | 42 only (`cat`; atime 01:21:58) | `aoeg-8532` | yes: `pg_20261003_add_orders_status.up.sql`, `BEGIN;` ... `COMMIT;` | none |

- **Item 12: TRUE for Claude 2.1.287.** In 3 of 3 matching prompts Claude read the right file from the absolute path, before the reply, and used its markers. It followed every step in C1 and C4. In C3 it wrote `user_version = 1`, not the 3-digit `001`; the fixture step can be read both ways. In the negative prompt it read no skill file.
- **Item 12 for Codex 0.154.0: UNKNOWN for the HLD route** (§8.2: the brief comes from the `SessionStart` hook). That route needs hook trust, which tests may not use, so it was not tested. It stays for the owner hand test.
- **Codex with the brief in `AGENTS.md`: TRUE.** Same counts: 3 of 3 right files, steps followed, no fixture skill file in the negative prompt. Item 1 shows that the hook text reaches the Codex model, so the hook route will likely act the same. This is not tested.
- **Near descriptions:** each CLI read only the right file of the pair, in both directions (sqlite in C3 and X3, pg in C4 and X4).
- **File form:** both CLIs read the header-then-steps form and followed the steps in every matching case, except the one format slip in C3. Neither treated the header as a step.

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

- No file went into `~/.claude/skills`, `~/.codex/skills` or a repo, and no Brain hook or skill went into a CLI config. `ls` of both skill folders was the same before and after (only the parent folder mtime changed).
- `~/.codex/config.toml` (stat only, not read): same size (10715), but its mtime changed once, at 01:20:43, near the X3 Codex start. The X1, X2 and X4 starts did not change it. The ChatGPT desktop app's Codex app server also runs on this machine and can write this file. The cause is unknown.
- **Not met in full: one write to a real CLI file.** Claude's folder-trust dialog for `/private/tmp/e16s1/claude` was answered "Yes", so Claude wrote a trust entry for that folder into `~/.claude.json`, as in E11-S1. Claude also keeps its own project state there. The spike may not read or edit that file (credential file rule), so the entry is not removed. Owner decision: remove it by hand, or keep it. A later spike can avoid a new entry with a folder that is already trusted.
- Codex keeps its own session logs for the 4 runs in `~/.codex`; they were not read or edited.
- Cleanup done: the test session `brain-spike-e16s1` is stopped and deleted. `/private/tmp/e16s1` and `~/.claude/projects/-private-tmp-e16s1-claude/` (the 5 test transcripts and an empty `memory/` folder) are removed. The real `default` session and the main server were not touched.

## E16-S2: agy and OpenCode facts (§17 items 16, 18)

- Date: 2026-10-03. herdr 0.9.1. agy: `1.2.11` at the start, `1.2.16` for every hook run (see "agy self-update"). OpenCode: `opencode v2.0.18`. pi is out of scope (#217).
- Files: `agyopencode/` (stand-in `brain`, agy hook file, OpenCode shim, run scripts). Raw logs: `agyopencode/data/runs.txt` (drive log), `data/brain-calls.jsonl` (every hook and shim call with its input), `data/opencode-shim.jsonl`, `data/opencode-screens.txt`; the review rerun in `data/rerun-brain-calls.jsonl` and `data/rerun-opencode-shim.jsonl`. Source excerpts: `data/agy-1.2.11-source.txt` (the agy docs and changelog inside the 1.2.11 binary), `data/opencode-herdr-source.txt` (OpenCode 2.0.18 and herdr 0.9.1 binaries), `data/opencode-v2-docs.txt` (the V2 plugin docs). Hook inputs for golden tests: `agyopencode/fixtures/`.
- **Setup.**
  - Named test session `brain-spike-e16s2` beside `default`, with a clean env (`agyopencode/ht.sh`). agy ran in pane `w1:p1` in `/private/tmp/e16s2/agy`. OpenCode ran in pane `w2:p1` in `/private/tmp/e16s2/opencode`. Neither folder is a git repo.
  - **A stand-in `brain`**, not a `brain` built from main (`agyopencode/bin/brain`, first on the pane `PATH`). It logs its args, the `HERDR_*` env and stdin, and prints a fixed brief with a fresh marker: for agy `{"injectSteps":[{"ephemeralMessage":"<brief>"}]}` when `invocationNum` is 0, else `{}`; for OpenCode the brief as plain text. The brief uses the frame of today's hook output. No `install.json`, no Brain store.
  - The real HOME was used, so the CLIs used their own sign-in. The user's own settings and plugins stayed active.
- **How a delivery was seen.** A fresh marker in the brief (agy `itjp-6810`, then `pohq-6776`; OpenCode `ukjp-2944`). The question asks for "the spike marker of this workspace" and does not contain it. The hook input JSON paths come from the logged stdin, not from the docs.
- **Review rerun (after the GPT-6.1 Sol review).** Same setup, agy 1.2.16. The brief got a second fact, a release code, that no turn-1 question asks for (agy marker `kfzt-3917`, code `wbnx-5208`; OpenCode marker `qdlm-4462`, code `hrsv-7735`). B1-B2 check that the agy brief stays in the history. O5-O7 check the fixed shim: one input shape `{session_id, cwd, event}` for every call, the raw event with no cut, and the start from the `context` hook in a reopened session. See `runs.txt`, "Review rerun".

### Config routes with no real file change

| CLI | Route used | Relation to the profile's global path |
|---|---|---|
| agy | Workspace customization root: `<workspace>/.agents/hooks.json`. Print mode (`agy -p`) loads it with no folder-trust dialog; `agy -p "/hooks"` listed the 3 spike hooks. | The global root is `~/.gemini/config/` (agy docs in the binary: "Global Discovery: `~/.gemini/config/`"), so the profile path `~/.gemini/config/hooks.json` has the same file format (`data/agy-1.2.11-source.txt`, "customization discovery"). **Not tested live:** a scratch HOME has no sign-in ("You are not logged into Antigravity" in the scratch `cli.log`; print mode hung with no output). |
| OpenCode | Project plugin folder: `<project>/.opencode/plugins/brain.ts`, with `opencode --standalone` (a private server, child of the TUI in the pane). | The global plugin folder is `~/.config/opencode/plugins/`: the loader scans `plugin/` and `plugins/` in each config folder, and the global config folder is `$OPENCODE_CONFIG_DIR`, else `$XDG_CONFIG_HOME/opencode` (`~/.config/opencode`) (`data/opencode-herdr-source.txt`; the V1 docs name the same folder, `data/opencode-v2-docs.txt`). herdr installs its own OpenCode plugin there (`herdr integration status`). **Not tested live** (a write to the real folder). |

- **agy interactive mode: not run.** The interactive start in an untrusted folder shows "Do you trust the contents of this project?". "Yes" saves the folder in the user's agy settings (`trustedWorkspaces`), a real config write (the settings field `trustedWorkspaces` in the binary, `data/agy-1.2.11-source.txt`); no flag skips the dialog (`agy --help`). The answer was "No, exit". The agy changelog (1.1.1, same file) says workspace hooks load after the folder is trusted. So every agy result below is for print mode.

### agy self-update

- `agy` updated itself from 1.2.11 to 1.2.16 during the first print run (A0, 15:28): the `~/.local/bin/agy` mtime is 15:28, and `agy changelog` starts at 1.2.16 afterwards. No flag or env var to stop it was found in `agy --help` or the binary strings.
- So the hook runs A1-A4 ran on **1.2.16**. Item 16 for 1.2.11 stays **unknown**, except that its interactive start shows the trust dialog. A later agy test must expect a new version at each run.

### Results: agy 1.2.16 (item 16)

| Fact | Result | Evidence |
|---|---|---|
| Agent kind that herdr shows | `agy` | `herdr agent start --kind agy` returned `"agent":"agy"`. |
| `agy --version` | `1.2.16` (a bare version; `1.2.11` before the update) | `runs.txt`, "Versions". |
| `PreInvocation` runs, with `invocationNum` 0 | TRUE | A1-A4: one `PreInvocation` call with `invocationNum` 0 at the start of each turn, then `invocationNum` 1 after a tool step. |
| `invocationNum == 0` means a session start | **FALSE** | `invocationNum` counts model calls inside one turn. It is 0 again at the start of each turn: A2 (a resumed conversation) and A4 got `invocationNum` 0 with `initialNumSteps` 4. `initialNumSteps` was 1 at the first turn of each new conversation (A1, A3). |
| `injectSteps[].ephemeralMessage` reaches the model | TRUE | A1 replied `itjp-6810`, A3 replied `pohq-6776`. |
| The ephemeral brief stays in the conversation | TRUE | B1-B2 (review rerun): B1 asked only for the spike marker (reply `kfzt-3917`). B2, a new `agy` process with `--conversation`, got no brief (`BRAIN_SPIKE_ONCE=1`, the hook printed `{}`) and asked for the release code, which no earlier reply showed. The reply was `wbnx-5208`. (A4 alone did not prove this: A3 had already shown its marker.) So with the HLD condition alone, the brief goes in again at each turn (A2 got it twice). |
| Hook file layout | TRUE for the parts used live (workspace file); the rest from the 1.2.11 docs | Live (A0-A4, `agyopencode/agy/hooks.json`): a top-level JSON object keyed by a hook name (`"brain-spike": {...}`); `PreInvocation` and `Stop` hold a flat list of handlers `{type, command, timeout}`; `PostToolUse` holds groups `{matcher, hooks: [...]}`; the logged hook cwd was the folder of `hooks.json` (`<workspace>/.agents`). From the docs only (`data/agy-1.2.11-source.txt`, "embedded hooks doc"): `"enabled": false` turns a name off; the command runs with `sh -c`; `timeout` is in seconds, default 30; same-event hooks of all names run in order. |
| Input: session id | `conversationId` | All calls. herdr's own agy hook (`antigravity_cli` v3) reports the same field as the agent session (`data/opencode-herdr-source.txt`, "herdr: agy hook"). |
| Input: cwd | `workspacePaths[0]` (an array; there is no `cwd` field) | All calls: `["/private/tmp/e16s2/agy"]`. The process cwd is the `hooks.json` folder, not the workspace. |
| Input: transcript | `transcriptPath` (`~/.gemini/antigravity-cli/brain/<id>/.system_generated/logs/transcript_full.jsonl`) | A1 call. Brain stores the path only (E15-S4). |
| `HERDR_*` env reaches the hook | TRUE | `HERDR_ENV=1`, `HERDR_PANE_ID=w1:p1`, `HERDR_SOCKET_PATH`, `HERDR_WORKSPACE_ID`, `HERDR_TAB_ID`, `HERDR_SESSION`, `HERDR_BIN_PATH` in every call. |
| Tool event | `PostToolUse`; the shell tool is `run_command` | A4: `toolCall.name` `run_command`, `toolCall.args.CommandLine` `ls`, `toolCall.args.Cwd` the folder, `error` `""`, `stepIdx`. A2: `view_file` with `toolCall.args.AbsolutePath`. No exit code and no output in the input. A failed command: `error` holds the error text (docs: `"exit status 1"`, `data/agy-1.2.11-source.txt`); not run live. |
| `Stop` input | `terminationReason`, `executionNum`, `fullyIdle`, `error`; no reply text | A1-A4. |
| Output for "nothing" | `{}` worked; empty stdout **unknown** | The stand-in printed `{}` for each non-start call and for an empty brief, and agy went on. The docs say each hook prints a JSON object (`data/agy-1.2.11-source.txt`); herdr's own agy hook prints `{}` on every path (`data/opencode-herdr-source.txt`). Empty stdout was not tested. |
| Session id source for herdr | `conversationId` through herdr's own hook | `herdr agent get` had no `agent_session` here: herdr's agy integration is not installed on this machine, and print runs are not interactive agents. |
| Noise lines for capture (E11) | **unknown** | Interactive agy was not run (trust write). The owner can trust a test folder by hand, or the E16-S12 author can record the screens then. |

Proposed start condition [NEW]: `invocationNum == 0` **and** `initialNumSteps == 1`. In A1 and A3 only the first call of a new conversation meets it. A resumed conversation then gets no new brief; the old one is still in its history (A4). Not tested: a first message with attachments (which can add steps), and `/clear` or a new conversation inside one interactive agy.

### Results: OpenCode v2.0.18 (item 18)

**The HLD names are the V1 plugin API. The V2 docs say V1 plugins do not run in 2.x** ("V1 plugin implementations do not run in V2", `data/opencode-v2-docs.txt`). The V2 docs ("Migrate plugins from V1") map `event` to `ctx.event.subscribe()`, `experimental.chat.system.transform` to `ctx.session.hook("context", ...)` with an edit of `event.system`, and `tool.execute.after` to `ctx.tool.hook("execute.after", ...)`. A search result for that page says 2.x "rejects the v1 async-function shape with `PluginModule.LoadError`". Live: OpenCode showed "2 plugins failed" for the user's two plugins in `~/.config/opencode/plugins/` (herdr's v10 file and one other); the load error text was not recorded, and V1 plugins were not tested one by one. The shim uses the V2 names (`agyopencode/opencode/brain.ts`).

| Fact | Result | Evidence |
|---|---|---|
| Agent kind that herdr shows | `opencode` | `herdr agent start --kind opencode` returned `"agent":"opencode"`. |
| `opencode --version` | `opencode v2.0.18` (with the `opencode v` prefix) | `runs.txt`. The TUI offers `/update to install v2.0.22`; OpenCode did not update itself. |
| Plugin folder | Project: `<project>/.opencode/plugins/` (TRUE, live). Global: `~/.config/opencode/plugins/` (from source, not live) | The shim loaded from the project folder (setup log: `version` 2.0.18, `location.directory`). Global: `data/opencode-herdr-source.txt`, "plugin folder discovery" and "global folders". |
| A shim with no npm package | TRUE | The shim is a plain default object `{id, setup}` in one `.ts` file. It imports only `node:fs` and `node:child_process`. OpenCode ran the `.ts` file with no install. |
| `session.created` runs | TRUE, at the **first prompt**, not at TUI start | O0 start 15:35:32, `session.created` 15:35:45 with the O1 prompt. Payload (`fixtures/opencode-session-created.json`): `data.sessionID`, `data.location.directory`, `data.projectID`, `data.agent`, `data.model`; no `data.parentID` for a top session. |
| System prompt hook (`ctx.session.hook("context")`) adds the brief | TRUE | O1 replied `ukjp-2944`. The hook got 4 system parts and the shim added a fifth with the 407-character brief. |
| The added text is not kept in history | From the V2 docs only (not tested live); live: the hook runs before **each** model call | 6 `context` calls in O1-O4 (one per model call, two in each tool turn), each with the brief added. The docs: the change applies "only the outgoing model call, not persisted history" (`data/opencode-v2-docs.txt`). So the shim adds the brief on every call. It calls `brain` once per session and keeps the text. |
| A reopened session (no `session.created`) | TRUE with the shim fallback | O7 (review rerun): `opencode --standalone -s <id>` sent no `session.created`; the first `context` call started `brain` (`had_created` false; `fixtures/opencode-shim-start-reopened.json`), and the reply was the release code `hrsv-7735`, which no earlier reply showed. |
| `HERDR_*` env reaches the shim | TRUE with `--standalone` | The private server `opencode serve --stdio --port 0` is a child of the TUI in the pane; the shim logged `HERDR_PANE_ID=w2:p1`, `HERDR_ENV=1`. |
| `HERDR_*` env with the default background service | **FALSE (from source), not tested live** | A plain `opencode` connects to one shared `opencode serve --service`, started detached with the env of the process that first started it (`data/opencode-herdr-source.txt`, "background service spawn": `{detached:!0, ..., env:{...process.env,...o}}`). herdr's own V2 plugin says (same file, "herdr: OpenCode V2 server part"): "Its shared server cannot attribute sessions using its process environment: the pane-local TUI owns both selection and lifecycle reporting there". The test did not load the spike plugin into the user's running service. |
| Tool event (`ctx.tool.hook("execute.after")`) | TRUE | `fixtures/opencode-tool-after-exit0.json` and `-exit1.json`: `tool` `shell`, `sessionID`, `agent`, `messageID`, `id`, `input.command`, `status` `completed`, `result.output.exit` (0 and 1), `result.output.output`, `result.output.truncated`. A failed command still has `status` `completed`; the exit code is in `result.output.exit`. No cwd. The shape matches the source (`data/opencode-herdr-source.txt`, "tool execute.after payload"). |
| Session id source | `data.sessionID` of `session.created`; `sessionID` in the other hooks (`ses_...`) | O1-O3. `herdr agent get` had no `agent_session`: herdr's own OpenCode plugin here is v10 (V1) and did not load in 2.x. |
| How stable the API is | From the docs: the V1 experimental hook has no place in 2.x | The V2 docs say V1 plugins do not run in V2, so the plugin API changed at the major version. The V2 `context` hook is not marked experimental in the V2 docs; `experimental.ws.*` hooks are (`data/opencode-v2-docs.txt`). HLD §18 (an API that can change) stays true. |
| Noise lines for capture (E11) | Found (from `data/opencode-screens.txt`) | Idle footer `  <cwd>  <n>K (<p>%)  ctrl+p commands`; working footer `<spinner> esc interrupt  <n>K (<p>%)  ctrl+p commands` (spinner `⬝` and `■`); the composer mode line `┃  Build · <model> <provider> · <variant>`; its bottom rule `╹▀▀▀…`; empty `┃` rows; the turn footer `Build · <model> · 4.8s · 6.6 tok/s`. Patterns are for E16-S13 [NEW]. |

### Base adapter changes (for E16-S11 #162)

agy:
1. Output form: a JSON path with an array step, `injectSteps[0].ephemeralMessage` (as #162 plans).
2. "Nothing" output: print `{}`, not empty stdout, for the start event with no brief and for the tool and stop events. Empty stdout is untested; `{}` is what herdr's hook does.
3. Start condition with two checks: `invocationNum == 0` and `initialNumSteps == 1` [NEW] (an HLD change, see below).
4. An input path with an array index: `input.cwd = "workspacePaths[0]"`.
5. Install layout: one top-level named entry in `hooks.json` (for example `"brain"`), with a flat handler list for `PreInvocation` and a matcher group for `PostToolUse`. This is not the Claude and Codex layout (`hooks.<Event>[]`). Install and uninstall change only that key.
6. Tool event map: `events.tool = "PostToolUse"`, matcher `run_command`; `input.command = "toolCall.args.CommandLine"`, `input.error = "error"`; no exit status.
7. No hook-trust step (agy has none for hooks). Interactive agy loads workspace hooks only in a trusted folder; for the global file this is unknown.

OpenCode:
1. A shim file install: the Brain-owned V2 plugin `brain.ts` in `~/.config/opencode/plugins/` (as #162 plans), with its hash in `install.json`.
2. A plain text output form for the shim (as #162 plans).
3. No hook file entry: the shim is the whole install.
4. One input shape from the shim for every call: `{session_id, cwd, event}`, with the raw OpenCode event in `event` (the fixed shim, O5-O7; `fixtures/opencode-shim-*.json`). The `session.created` event has `data.sessionID` and `data.location.directory`, but the `execute.after` event has a top `sessionID` and no cwd, so the shim fills `session_id` and `cwd` (from the event, else from `ctx.location.directory`). Profile paths: `input.session_id = "session_id"`, `input.cwd = "cwd"`, `input.command = "event.input.command"`, and an exit path `event.result.output.exit` (a number). The Claude profile has only `exit_pattern` on text; a numeric exit path is new.
5. The shim adds the brief at every `context` call and calls `brain` once per session; with no `session.created` (a reopened session) the first `context` call starts `brain` (O7).
6. Version check: `opencode --version` prints `opencode v2.0.18`; the version parse must drop the prefix.

### Decisions for the owner (not changed here)

- **HLD §8.3 step 2 and §8.5 (agy start condition):** `invocationNum == 0` alone fires at every turn. Proposed: `invocationNum == 0 and initialNumSteps == 1` [NEW].
- **HLD §8.2 (OpenCode row):** the hook names are the V1 API, which OpenCode 2.x does not load. The V2 names are `session.created` through `ctx.event.subscribe()`, `ctx.session.hook("context")` and `ctx.tool.hook("execute.after")`. The agy tool event is `PostToolUse` (`run_command`), not "M0".
- **OpenCode pane identity (affects §8.3 steps 1 and 3 and E16-S13):** the default OpenCode runs plugins in one shared background service. That process does not have the pane's `HERDR_*` env, so `brain hook` would withhold or give the wrong pane. Only `--standalone` (or `--server` to a private server) gives the pane env. Options: a TUI-side part of the shim that runs in the pane process (as herdr's own plugin does) and passes the pane id to the server part; or support only `opencode --standalone`; or keep OpenCode as a gap. This needs a decision before E16-S13.

### Limits

- One run per case (plus the review rerun), one model per CLI (agy `gemini-3.8-flash-high`, OpenCode `space-bunny-free`), one machine. agy in print mode only; OpenCode with `--standalone` only.
- The global install paths of both CLIs were not tested live.
- The stand-in `brain` is not Brain: the live checks of E7 (pane, membership, receipts) did not run.
- agy 1.2.11 hook facts are unknown (the update came first).

### Side effects and cleanup

- No file in `~/.gemini/config/`, `~/.gemini/settings.json`, `~/.gemini/trustedFolders.json`, `~/.gemini/antigravity-cli/settings.json` or `~/.config/opencode/` changed (`agyopencode/snapshot.sh`: names, sizes and mtimes, before and after; no file was read). No trust was given, and no hook or plugin went into a real config.
- **agy updated its own binary** `~/.local/bin/agy` from 1.2.11 to 1.2.16 (see "agy self-update"). Owner decision: keep 1.2.16 (the profile records it as `tested_version`) or go back.
- The CLIs' own state changed, as in normal use: agy wrote its conversations, artifacts, logs and caches in `~/.gemini/antigravity-cli/` (3 conversations with 2 turns each; the scratch-HOME try wrote only to the scratch HOME); OpenCode wrote its 2 sessions to `~/.local/share/opencode/opencode.db` and its prompts to `~/.local/state/opencode/prompt-history.jsonl`. They were not read or edited.
- Cleanup done (after the first run and again after the rerun): the test session `brain-spike-e16s2` is stopped and deleted. `/private/tmp/e16s2` (with the scratch HOME) is removed. The real `default` session, the main server and the user's OpenCode background service were not touched.

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

## E17-S1: stopped working and the event key (§17 item 23)

- Date: 2026-10-03. herdr 0.9.1, Claude Code 2.1.288 (`claude-haiku-4-5-20251001`), codex-cli 0.154.0 (`gpt-6-astra`, effort low). Go 1.23.0.
- Named test session `brain-spike-e17s1` beside `default`, with a clean env (`e17s1/ht.sh`). Claude ran in pane `w1:p1` in `/private/tmp/e11s1/agent/e17s1`; Codex ran in pane `w2:p1` in `/private/tmp/e17s1/codex`, with folder trust, the update check and the approval mode on the command line only (as in "How Codex ran with no config write"). At "Hooks need review" the answer was "Continue without trusting".
- **No `brain` and no `brained`.** The reader is a small probe (`e17s1/probe`, its own Go module). It subscribes to `pane.agent_detected`, `pane.exited`, `pane.closed` and `pane.agent_status_changed` of the two panes, and calls `agent.get` at once for each status event. Its claims go to a file, so they live across a reader restart, as `automation_runs` rows would. No install, no store, no hook.
- **Duplicates are made by the probe.** herdr sent no duplicate by itself in 86 status events. The probe hands a stop event to the methods again (`dup`), or a made event with the same bytes (`inject`), at a chosen time: 17 duplicates.
- Files: `e17s1/data/probe-log.jsonl` (every event, `agent.get` answer, claim, skip and drive note), `e17s1/data/eval.txt` (one row per stop event for each method), `e17s1/data/terminals.txt`, `e17s1/README.md`.

### Stop condition per CLI

| Turn | Claude 2.1.288 | Codex 0.154.0 | Evidence (`probe-log.jsonl`) |
|---|---|---|---|
| Normal end, focused pane | `working → idle` | `working → idle` | Claude seq 5 → 6 (T1); Codex seq 6 → 7 (run r7) |
| Normal end, pane not focused | `working → done` | `working → done` | Claude seq 16 → 17 (T5); Codex seq 22 → 23 (X1) |
| Approval, approved | `working → blocked → working → idle` | `working → blocked → working → done` (not focused) | Claude seq 10-13 (T3); Codex seq 24-27 (X2) |
| Approval, denied | `blocked → idle` (seq 8 → 9): no `working` before it, so **not a stop** | not tested: **unknown** | Claude T2: the test Claude asked to run a command from the user's own SessionStart hook; the answer was "No" |
| User stop (Esc while working) | `working → idle`, the same as a normal end | `working → done` (not focused), the same as a normal end | Claude seq 14 → 15 (T4); Codex seq 28 → 29 (X3) |
| The user sees a `done` pane | `done → idle`; `state_change_seq` stays the same | `done → idle`; `state_change_seq` stays the same | Claude seq 17 (focus `w1`); Codex seq 4 and 5 |
| False stop | none seen | `working → done` for about 1 s just after "Continue without trusting", with **no agent session id** | Codex seq 32 → 33 (x2) and 4 → 5 (x3) |

- **Result:** the stop condition is the same for both CLIs: `working → idle` or `working → done`. Which one comes depends on the pane focus, not on the CLI. TRUE for both CLIs.
- herdr cannot tell a user stop from a normal end. A trigger on this condition also runs after a user stop. TRUE (both CLIs).

### Event and key facts

- The status event holds only `pane_id`, `workspace_id`, `agent` and `agent_status`. Every stop of one pane gives the same bytes, so a duplicate of an old stop looks the same as a new stop.
- **`state_change_seq` is one counter for the whole herdr server**, not one for each pane or agent. Its values interleave across the two panes (Claude start 1, Codex 2-4, Claude turn 5-6). It moves at each status change of any pane, also `unknown` (restore: seq 1). It does not move at `done → idle` (seen).
- **The M0 fact explained:** `state_change_seq` changed between runs in one pane (E11-S1: 10 and 12) because other changes moved the shared counter. **After a server restart it starts again at 0**: the restored Claude had `idle`/0, then `unknown`/1, then `idle`/2.
- **herdr 0.9.1 restores agents at a server restart with the same session id**: it ran `claude --resume 72fe6516-…` in `w1:p1` and `codex resume 01a10251-…` in `w2:p1`, with the same pane ids. `agent_session.value` stayed the same. `terminal_id` changed at each restart (`data/terminals.txt`). The restored Codex had no `-c` flags from the first start, so it showed the update dialog and the folder-trust dialog; "No, quit" ended it.
- So the §10.4 key (pane, agent session id, `state_change_seq`) **repeats after a server restart**. Live: after the restart, the real stop at seq 20 of session `72fe6516` had the key of a duplicate claim from before the restart, so K and KT skipped it (`eval.txt`, 20:43:27).
- At a reconnect herdr sends only `subscription_started`. It does not send the current state.
- `agent.get` took 0.14 ms at least, 0.28 ms as the median and 102 ms at most (the first call after a reader start), counted from the event read. 116 calls.
- Codex has no `agent_session` at the `working` event of the first turn of a run. It has one at that turn's stop (seq 22 → 23, 34 → 35, 6 → 7). E11-S1b found the same.
- `pane list` and `pane get` do not give `state_change_seq`. `agent get` (and `agent start`) do.

### Methods tested

All methods start at a reader start or reconnect with the current `agent.get` state as a baseline, and claim nothing for it (§10.4 step 1).

- **K:** the §10.4 key alone. Each event with status `idle` or `done` builds (pane, agent session id, `state_change_seq`) from `agent.get` and claims it.
- **KT:** K, but only when the last status that the reader saw for the pane (from the stream) is `working`.
- **KJ:** KT, and `agent.get` must have the event status and an agent session id.
- **P (proposed) [NEW]:**
  1. At each status event, call `agent.get` for the pane at once.
  2. The event **joins** only when `agent.get` gives the event status. An event that does not join is skipped and does not change the reader's memory.
  3. The memory of a pane is its last joined status. A reader start or reconnect sets it from `agent.get` (baseline).
  4. A stop is a joined `idle` or `done` event after a joined `working`, and `agent.get` gives an agent session id.
  5. Key: (pane, `terminal_id`, agent session id, `state_change_seq`), all from that `agent.get`.

K, KT and KJ ran live in the probe. P ran on the same log (`eval.py`). The replay of K, KT and KJ gives the same claims as the probe.

### Key results

"TRUE" means the method passes the case: no second claim for one stop, and one claim for each stop.

| Case | Evidence (`eval.txt`) | K | KT | KJ | P |
|---|---|---|---|---|---|
| One stop, then a duplicate while the agent works again | Claude 20:34:48, 20:39:14; Codex 20:39:33, 20:40:56 | FALSE (claims again) | FALSE (claims again) | TRUE | TRUE |
| A duplicate while the agent is `blocked` | Claude 20:36:49, 20:37:32; Codex 20:40:07 | FALSE | TRUE | TRUE | TRUE |
| A duplicate after an agent replacement | Claude 20:38:33 (no agent), 20:38:39 (new agent at rest), 20:39:14 (new agent works); Codex 20:40:48, 20:40:56 | FALSE | FALSE (20:39:14, 20:40:56) | TRUE | TRUE |
| A duplicate at rest after its own stop | Claude 20:37:40, 20:39:18; Codex 20:39:44, 20:40:11 | TRUE (key held) | TRUE | TRUE | TRUE |
| Two separate stops: one claim each (31 live stops) | all turns | TRUE for the stops, but see the rows above | FALSE: missed seq 21, 23 and 35 (a duplicate had changed its memory) and seq 20 after the server restart | FALSE: missed seq 21, 23 and 35 | TRUE: 31 of 31 |
| Reader restart with the agents at rest, then duplicates | 20:41:32 (r2) | TRUE (key held) | TRUE | TRUE | TRUE |
| Reader restart while the agent works, then its stop | 20:41:45 (r3) | TRUE (a new stop, claimed) | TRUE | TRUE | TRUE |
| A stop while the reader is down (no backfill), then a duplicate of it | 20:41:58 (r4) | FALSE (claims late) | TRUE | TRUE | TRUE |
| Server restart: the restored state (`idle` → `unknown` → `idle`) | 20:44:29 (r6); 20:42:51 (r5, Codex `blocked` → `idle`) | FALSE | TRUE | TRUE | TRUE |
| Server restart: a real stop after the restart with a used key | 20:43:27 (seq 20) | FALSE (skipped) | FALSE (skipped) | FALSE by design (the same key; it passed here only because KJ had no claim at seq 20) | TRUE (`terminal_id` changed) |
| Codex false stop after the hooks dialog | 20:40:43, 20:44:56 | FALSE (claims) | FALSE (claims) | TRUE | TRUE |

- **Result:** the §10.4 key alone (K) and its tested forms (KT, KJ) do **not** pass. P passes every tested case: 31 claims for 31 live stops, no claim for a duplicate, a restored state or a false stop.
- Claims: K 48, KT 33, KJ 28, P 31.
- P is not the §10.4 key. It goes to the owner as an HLD update before E17-S11 #176.

### Source agent (E11 binding and capture identity)

- The event names only the pane. P finds the source agent from the same `agent.get` answer that joins the event: `agent` (the harness) and `agent_session.value`. With the bound herdr session (the socket that sent the event), this is the §6.2 session key (binding, harness, agent session id). TRUE in the live runs.
- A stop with no herdr agent session id (a §6.2 local capture id only) is skipped. In the live runs every real stop had the id; only the Codex hooks-dialog false stops had none.
- Not tested: the join with the state of a running `brained` (`internal/brained` keeps the agent session id of each followed pane in `capturePane.native`). The probe is not `brained`. E17-S11 can compare the `agent.get` answer with that state.
- `brained` reads panes with `pane.get`, which has no `state_change_seq`. The automation runner needs `agent.get`.

### Proposed §10.4 text (for the owner; the HLD is not changed)

Step 1 of "Claim before run", in place of the present step 1:

> 1. The event arrives. Brain calls `agent get` for the pane at once. The event joins only when `agent get` gives the event status; Brain skips an event that does not join, and it does not change Brain's state for the pane. Brain keeps the last joined status of each followed pane in memory. A stop is a joined `idle` or `done` event after a joined `working`. Brain builds the event key from (pane, `terminal_id`, agent session id, `state_change_seq`), all from that `agent get` answer [NEW]. `state_change_seq` is one counter for the whole herdr server and starts again after a server restart, and herdr then resumes the same agent session in the same pane, so the key needs `terminal_id`, which changes at a restart (M0 E17-S1). herdr events have no id and no time. Brain finds the source agent with the binding and the agent session id from `agent get` (§6.2). It skips a stop with no herdr agent session id. After a restart or a reconnect of `brained`, the current agent state from `agent get` is a baseline, not a stop.

Trigger row of the automation table: "`working → done` or `working → idle`" stays. Add: "The two are one condition for Claude and Codex; focus decides which one comes (M0 E17-S1). A user stop also matches."

### Limits

- One run per case, one model per CLI, one machine. The duplicates are made by the probe; herdr sent none by itself.
- Reader lag was not tested. If a whole turn passes between the event and `agent.get`, P skips the stop (a missed run, not a second run). The measured gap was below 1 ms, and the shortest turn was about 0.9 s.
- A denied approval in Codex was not tested.
- `terminal_id` changed at each of the 2 restarts. herdr does not document its format, so it is not proven unique across all restarts. The values grow with time (`term_65cf0eb9…`, `term_65cf112d…`, `term_65cf11aa…`).
- The test Claude loaded the user's own Claude settings and hooks (the real HOME, as in E11-S1). One SessionStart hook asked the model to run a command; the approval answer was "No".

### Side effects and cleanup

- The test session `brain-spike-e17s1` is stopped and deleted. Its restored `claude --resume` and `codex resume` processes ended with it. The real `default` session and the main server were not touched.
- `/private/tmp/e17s1`, `/private/tmp/e11s1/agent/e17s1` and `~/.claude/projects/-private-tmp-e11s1-agent-e17s1/` are removed. `/private/tmp/e11s1/agent` was there before (E11-S1) and stays.
- `~/.codex/config.toml` has the same mtime and size before and after (01:20, 10,715 bytes); it was not read. The restored Codex update dialog got ctrl+c and its folder-trust dialog got "No, quit". Codex and Claude keep their own session logs and history lines for these runs; they were not read or edited.
- No hook or Brain install went into `~/.claude` or `~/.codex`. No credential file was read.

### Open points

- Owner: accept P and the proposed §10.4 text before E17-S11 #176, or choose another method.
- Owner: should a user stop (Esc) run the automation? herdr gives the same change as a normal end, so P cannot tell them apart.
