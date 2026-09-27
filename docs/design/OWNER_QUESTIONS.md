# Brain: open questions for the owner (after HLD v1.3)

Date: 2026-09-27. Context: `HLD.md` v1.3 in this folder. Implementation is not authorized.

Some items below were discussed in chat after v1.3 and are **not yet in the HLD** (marked NOT IN HLD).

## A. Owner decisions from HLD §16

| # | Decision | Current recommendation in HLD |
|---|---|---|
| D1 | Final name | Open |
| D2 | Runtime | One packaged executable. TypeScript on Bun first, like the official herdr plugins. The hook is a small script. |
| D3 | Harnesses without a SessionStart hook | Global items only by default. Opt-in fallback B (herdr types a pointer). M4: `recall`. Repo export only on request. |
| D4 | Extractor CLI | The official CLI that the owner picks in `brain setup`. No quota guess. |
| D5 | Transcript readers | Off by default. Opt-in per harness, extra source only. |
| D6 | herdr version floor | 0.8.2. Confirm 0.9.1 in M0. |
| D7 | Items of a closed workspace | Keep them. Ask once when the same root returns. Ask "archive?" after 90 days. |
| D8 | Global items: native files or the hook | Native files. |
| D9 | Locality | Standard by default (extractor through official CLI may use a cloud model), with a per-workspace "extraction off" switch. Agents always send delivered items to their own cloud model. |
| D10 | Build weight review (proposed cuts, not applied) | (a) fallback B only if needed, not an M2 default; (b) transcript readers only if M0 shows real capture loss; (c) `brain status` in place of the weekly digest; (d) comebacks per item in M2, per revision only if revisions often overlap; (e) M2-M4 stay direction-level until M1 data exists. |

## B. NOT IN HLD: D11 hybrid capture

Proposal:
- For Claude Code and Codex, capture through their own hooks: `UserPromptSubmit` (exact submitted text), `PostToolUse` (exact command, result), `Stop`, `PreCompact`. Codex main-branch source `codex-rs/hooks/src/engine/dispatcher.rs` lists these events; installed Codex 0.154.0 and Claude 2.1.283 must be tested in M0.
- herdr screen capture stays the fallback for CLIs with no hooks.
- herdr stays the source of workspace identity, live membership and automation triggers.
- Note: `UserPromptSubmit` text can also come from a script or `herdr agent prompt`, so the sender is still not fully confirmed.
- Owner requirement to weigh: "harness agnostic, herdr as the main mechanism".

Question: adopt D11 (hooks first for Claude/Codex, herdr capture as fallback), keep herdr-only capture, or something else?

## C. NOT IN HLD: tool detection

Proposed pipeline:
1. Record each agent command (text, cwd, exit code, order, session, workspace), secret filter first. Best source: `PostToolUse` hooks; screen reads are poor evidence for commands.
2. Normalize changing parts to placeholders (`pnpm test --filter <package>`).
3. Find the same normalized chain in 3+ separate sessions.
4. Filter: drop failed chains, one-session chains, chains with secrets, and judgment-heavy procedures (those become skills).
5. Extractor drafts a recipe: name, description, inputs, commands, timeout, risk marks, expected benefit.
6. Owner reviews exact commands and source sessions. Brain stores a hash.
7. Deliver (see D).

Other signal mappings: same error then same fix → memory or tool; repeated approval of a safe command → allow-rule suggestion; same command after each edit → CLI hook suggestion; same request after each finished task → automation candidate.
For CLIs with no hooks: owner says "make this a tool" and Brain drafts from the owner's words.

Questions: is 3 sessions a reasonable default? Is the placeholder step (model work) acceptable given review? Anything missing?

## D. NOT IN HLD: tool form and use

- A tool = `tool.toml` (name, description, workspace, cwd, timeout, risk, typed inputs with patterns, `code_sha256`) + `run.sh` (POSIX sh, `set -eu`, inputs passed as `"$1"`, never interpolated).
- Brain never writes new program logic. A job that needs loops or parsing becomes a skill, or normal repo code.
- Stored outside repos: `~/.local/share/brain/workspaces/<key>/tools/<name>/`.
- The SessionStart workspace block lists each tool in about two lines.
- The agent runs `brain run <tool> [args]` through its normal shell tool, under the CLI's permission rules.
- `brain run` checks live workspace membership, approval and hash, input patterns; sets cwd and timeout; returns output and exit code; logs the call.
- Trade-off: the CLI prompt shows `brain run <tool>`, not the inner commands. The owner's review of the tool covers that gap.
- Later: MCP gateway `find_tool`/`run_tool` with the same checks; skills can reference tools; automations call the same `brain run`.

Questions: shell-only tools acceptable? `brain run` wrapper vs direct script path? Any safer or simpler form?

## E. NOT IN HLD: automation detection and behavior

- Two forms: suggestions (CLI hook, allow rule, script) that the owner installs; Brain automations (approved tool + herdr trigger).
- Detection: "after event X, the same action Y follows" in many sessions (e.g. owner asks "run the tests" after each working→done in "shop").
- An automation always uses an already approved tool.
- M4 trigger: only herdr `working → done` in the bound workspace.
- Brain runs the tool as its own process in the workspace folder. It does not type into the agent pane and does not start agents. Result goes to `brain status` and the pane label.
- Safety: approval covers exact tool + trigger + workspace; claim-before-run (Prime); hash check before run; no chains; idempotent actions only; pause/remove at once; no auto-retry; undo stops future runs only.

Questions: should a failed automation result ever be sent back to the agent (today Brain never types into agents)? Is `working → done` the right single trigger? Anything unsafe?

## F. Sequencing

- Recommended next step: pre-M0 look-back (1 day, no code) before D10/D11 decisions, then the M0 spike (2-3 days, 21 facts in §17).
- Question: agree with this order, or change it?

## G. Older open items (for awareness)

- Drover `PENDING_REVIEW.md` is not approved.

## Owner answers (2026-09-27)

Astra's recommendations: `../astra-brain-20260927/OWNER_QUESTIONS_ANSWERS.md`.

| # | Owner decision |
|---|---|
| D8 | Deliver global and workspace items through the herdr hook. No native global files by default. |
| D10 | Accept cuts a, b, c and a qualified e (keep capabilities, safety contracts and exit tests). Reject d: keep the item revision in comeback records. |
| D11 | herdr capture stays the main path. Optional Claude/Codex hooks add structured evidence to the same stream, enabled only after an M0 test. |
| Sequencing | 30-60 minute owner-selected look-back, then the M0 spike. Facts that M1 does not need stay "unknown". |
| D1 | Name "Brain" (plugin id `brain`); UI shows "Herdr Brain". Check for conflicting `brain` executables first. |
| D2 | **Go**, one binary. The hook is a `brain hook` subcommand. (Differs from the TypeScript/Bun recommendation.) |
| D3 | Show the gap in `brain status` for a CLI with no tested start-time delivery. Export to its own rules file on request. Fallback B only if needed. |
| D3a | **Adapter design:** one configurable base adapter (shared Go code) plus a small data profile per harness; code overrides only where a profile cannot express the behavior. Checked docs 2026-09-27: agy 1.2.11 has no SessionStart but `PreInvocation` (`invocationNum == 0`) can return `injectSteps[].ephemeralMessage`; OpenCode v2.0.18 has `session.created` and an experimental `experimental.chat.system.transform`; pi has `session_start` and `before_agent_start`. Gemini CLI is replaced by agy. All untested locally. |
| D4 | The owner picks one extractor CLI in setup. No automatic provider switch; failures wait and show in status. |
| D5 | Transcript readers off; build one only if M0 or later use shows consequential evidence loss. |
| D6 | **Support the latest herdr release only.** (Differs from the test-set floor recommendation.) Installed: 0.8.2. |
| D7 | Keep items of a closed workspace without delivery. Relink only on owner request. Archive candidates appear in status; no forced 90-day prompt. |
| D9 | **Extraction on by default** after setup, with a per-workspace off switch. (Differs from the explicit setup choice recommendation.) Setup still explains both data flows; wording becomes "agents may send to their configured provider". |
| Publish + live suggestions | **New owner requirement.** When the owner sends a prompt that repeats earlier prompts, Brain surfaces a ready proposal (skill, lesson, tool or automation, new or modified) at that moment. The owner approves, rejects or requests a change without leaving the work. **Surface: herdr side pane** (herdr notification + a small `brain review <id>` pane opened `--no-focus` beside the agent; Brain closes only its own pane). Proposals are drafted in the background beforehand; the prompt-time match is local and makes no model call. `UserPromptSubmit` gives the fastest match; herdr capture is the fallback. Tools and automations never publish without review. herdr 0.8.2 `notification show` has no reply, and any same-user process can drive panes, so no path proves human origin; the HLD must state this limit. `herdr integration install` already supports claude, codex, pi, opencode, antigravity-cli and others: a reference for the base adapter. |
| Tool form | `tool.toml` holds the program and its argv list with typed, validated inputs, a pinned folder and a timeout; short ordered steps are allowed; a reviewed `run.sh` only when a pipeline needs one. Approval covers the whole execution contract, not only the code. Run through `brain run`. |
| Automation trigger | One tested "agent stopped working" condition per CLI (working→done or working→idle, set by M0 records). The binding pins a folder. No backfill of missed events; no run while an agent edits that folder. |

Not asked separately; adopted as defaults from Astra's §C-E unless the owner objects: 3 sessions sets priority, not a gate; keep failures and one-session chains as evidence; exact repeats before placeholders; no tools from secret-bearing commands; claim-before-run is duplicate prevention, crash leaves "unknown"; mark Brain-origin runs and exclude them from learning; automation failures show in status and never auto-resume the agent.
