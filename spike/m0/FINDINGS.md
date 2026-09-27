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
| 10 | Hook timing | PARTIAL | Stdin read 0.4 ms. `herdr pane get` from the hook: 4.5-10.2 ms. Whole hook: 5.4-10.7 ms (Claude and Codex). Open: busy load, manifest check. |
| 11 | Pane split with no focus, then close | PASS | `pane split --no-focus` kept focus. A plugin `split` pane with `--no-focus --env BRAIN_REVIEW=1` kept focus and got the env. `plugin pane close` closed it. `plugin pane close` on a normal pane returned `plugin_pane_not_found` and left it open. Open: `popup` placement. |
| 21 | Conformance test | OPEN | |
| 25 | No `brain` on PATH | PASS | `command -v brain` finds nothing. |
| 27 | Named test session | PASS (session parts) | `herdr --session brain-eval server` (background) ran beside `default` with its own socket. Its first workspace was `w1`. Its panes had `HERDR_SESSION=brain-eval` and their own `HERDR_SOCKET_PATH`. `herdr session stop brain-eval` and `herdr session delete brain-eval` touched only that session. |
| 28 | `agent read` of a reply | PASS for Claude | A 150-line reply: `--lines 80` gave lines 80-150, `--lines 200` gave all 150. Codex: open. |
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
- **Format change: withhold, never guess.** The reader withholds ("withheld: identity") when: the file is missing or does not parse, `version` is not 3, a workspace has no `id` or `identity_cwd`, a type is wrong, the root is not absolute, or the workspace is not in the file. The reader ignores unknown extra fields. The probe shows each case.
- **Owner decision needed (HLD §11 and §17 item 4):** the HLD treats `identity_cwd` as the fixed root folder. It is not. With the E2-S4 rule "same id + new root = new key", a `cd` in the root pane followed by a save gives a new key. The old items then wait for a relink. This fails closed (never a wrong delivery), but it also loses delivery after a normal `cd`. The owner chooses the rule. This spike does not change the HLD.
- **Side result for E2-S6:** a plugin `[[events]]` command for `workspace.renamed` ran. It got `HERDR_PLUGIN_EVENT_JSON` with `workspace_id` and `label`, and a `HERDR_PLUGIN_CONTEXT_JSON`.

## Cleanup done

- E2-S5: the scratch session `brain-spike-e2s5` is stopped and deleted. Its plugin link lived only in the scratch HOME. The main session was not touched.
- Test panes `w14:p2P`, `w14:p2Q`, `w14:p2R` and `w14:p2S` are closed. The spike plugin is unlinked. The `brain-eval` session is stopped and deleted.

## Open items

- Item 4: answered by E2-S5 (see above). The root matching rule waits for an owner decision.
- Item 10: busy load and the manifest check.
- Item 11: the plugin `popup` placement.
- Item 21: the conformance test (built in M1).
- Item 28: Codex long reply.
- Plugin `[[events]]` delivery: `workspace.renamed` ran in a scratch session (E2-S5). Other events and timing are not tested.
- The eval delivery and exclusion stages (scripts plus the owner's check).
