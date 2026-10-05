# Decisions

Each decision gives the choice, the reason, the test commands and the results.
Tests ran on macOS with herdr 0.9.1 on 2026-09-27.

## E1-S2: binary path, data home and first command (#3)

### D1. The binary path in CLI hook entries

**Decision:** option (a). A hook entry holds `<plugin_root>/bin/brain` with all symlinks resolved.
`internal/paths.BinaryPath()` gives it: the path of the running binary (`os.Executable`), with symlinks resolved.
`brain setup` runs from the plugin, so this is the managed checkout path. A hook entry never holds `~/.local/bin/brain`.

**Reason:**
- herdr puts a GitHub install at `~/.config/herdr/plugins/github/<id>-<first 12 hex of sha256(id)>`.
  For `brain` this is `brain-bbbf7a6412d6`. The name depends only on the plugin id, not on the repo, the ref or the commit.
- A second `install` and an `install --ref` keep this path (tests below). A failed install keeps the old checkout.
- Option (b), a symlink at a fixed path that setup owns, points to the same managed path.
  It adds one more file to own, and it breaks in the same cases (uninstall). So it gives no gain.

**Cases that break a hook entry** (E9-S3 `brain doctor` checks "hook entry points to a missing binary"):
- `herdr plugin uninstall brain` deletes the checkout, so `bin/brain` is gone.
- A change between `herdr plugin link <path>` and `herdr plugin install` changes the plugin root.
  The old entry still works while the old path exists, but it runs another binary.
- During a reinstall, herdr replaces the folder (new inode). For a very short time `bin/brain` does not exist:
  a busy poll saw it missing in 18 of 29,377 checks. A hook that starts in that window fails, and the CLI starts without the brief.
  This is a missed delivery, not a break. The next session gets the brief.

**Test (scratch `HOME`, no `HERDR_*` vars, local clone in place of GitHub):**

```sh
S=$(mktemp -d /tmp/brain-e1s2-XXXX); mkdir -p $S/home
# h: runs herdr with the scratch HOME only
printf '#!/bin/sh\nexec env -i HOME=%s/home PATH=/usr/bin:/bin:/usr/local/go/bin:%s/.local/bin GIT_TERMINAL_PROMPT=0 herdr "$@"\n' $S $HOME > $S/h; chmod +x $S/h
# The repo is private: git reads a local bare clone, so no token is used.
git clone -q --bare . $S/remote.git
printf '[url "%s/remote.git"]\n\tinsteadOf = https://github.com/aryanVijaywargia/herdr-brain\n' $S > $S/home/.gitconfig
# test refs, only in the scratch clone: v0.0.2-test (version 0.0.2), broken-build (build.sh exits 1), other (= main)
$S/h plugin install aryanVijaywargia/herdr-brain -y
$S/h plugin install aryanVijaywargia/herdr-brain -y
$S/h plugin install aryanVijaywargia/herdr-brain --ref v0.0.2-test -y
$S/h plugin install aryanVijaywargia/herdr-brain --ref broken-build -y
$S/h plugin install aryanVijaywargia/herdr-brain --ref other -y
$S/h plugin list --json --plugin brain      # plugin_root, resolved_commit
$S/h plugin uninstall brain
```

**Results:**

| Step | Plugin root | `bin/brain version` | Folder inode |
|---|---|---|---|
| install (main, 7100c6c) | `.../plugins/github/brain-bbbf7a6412d6` | 0.0.1 | 34753878 |
| install again | same | 0.0.1 | 34755054 (new) |
| install `--ref other` | same | 0.0.1 | new |
| install `--ref v0.0.2-test` | same | 0.0.2 | 34768932 (new) |
| install `--ref broken-build` | same, "Plugin was not installed." | 0.0.2 (old checkout kept) | 34768932 (same) |
| install main again | same | 0.0.1 | new |
| uninstall | folder deleted | — | — |

- The existing plugins on this machine follow the same rule: `herdr-projects-b1278ffb803c` and `official.browser-ff2a44eccae9` are the sha256 of `herdr-projects` and `official.browser`.
- `plugin list` reports `plugin_root` with symlinks resolved (`/private/tmp/...`) and `managed_path` without (`/tmp/...`).
  `BinaryPath()` resolves symlinks too, so the hook path matches `plugin_root`.
- After uninstall, `~/.config/herdr/plugins/config/brain` and `~/.local/state/herdr/plugins/brain` stay on disk.

### D2. The data home

**Decision:** keep the HLD path `~/.local/share/brain/` (HLD §7.5, §8.1), for all data: bindings, main store, test stores, outputs and receipts.
`internal/paths.DataHome()` gives it. It reads only `HOME`. It ignores `HERDR_PLUGIN_STATE_DIR`, `HERDR_PLUGIN_CONFIG_DIR` and `XDG_DATA_HOME`.
No HLD change is needed.

**Reason:**
- A CLI hook runs without `HERDR_PLUGIN_*`. The state and config dirs are fixed paths too, so the hook could build them.
  But then Brain depends on an undocumented herdr path rule. The HLD path is Brain's own.
- `uninstall` and `unlink` keep the herdr state and config dirs. So those dirs give no cleanup gain.
- The store is data that you cannot rebuild (your items). XDG puts such data in `~/.local/share`, not in `~/.local/state`.
- Brain does not read `XDG_DATA_HOME`: a plugin action gets the herdr server env, and a hook gets the agent pane env.
  These can differ, and then the store splits in two. `HOME` is the same for all entry points of one user.

**Test:** `go test ./internal/paths/ ./cmd/brain/`
- `TestDataHomeSameInPluginAndHookEnv`: with and without `HERDR_PLUGIN_*` and `XDG_DATA_HOME`, the data home is `$HOME/.local/share/brain`. PASS.
- `TestStatusDataHomeSameInPluginAndHookEnv`: `brain status` shows the same data line in both envs. PASS.
- Live: the `brain.status` action in a scratch herdr server printed `data: <scratch HOME>/.local/share/brain`.
  `env -i HOME=<scratch HOME> HERDR_ENV=1 bin/brain status` printed the same line.

### D3. The first run of `brain`

**Decision:**
- The manifest has a `setup` action (contexts `global`, `workspace`, `pane`).
  It runs `$HERDR_BIN_PATH plugin pane open --plugin brain --entrypoint setup`.
- The `setup` pane runs `$HERDR_PLUGIN_ROOT/bin/brain setup`. Its placement is `popup` [NEW]: the simpler choice. E5 decides popup or split.
- `brain setup` shows the absolute binary path. It asks before it makes the symlink `~/.local/bin/brain`.
  On "no" it prints the path to use. It never replaces a file at the link path, and it never edits a shell config file.
  If `~/.local/bin` is not on `PATH`, it says so and prints the path. A popup closes when its command exits, so in a plugin pane setup waits for Enter.
- This is a stub until E9-S1: bindings and hook entries come there.
- The user starts from the herdr UI (action "Brain: setup") or with `herdr plugin action invoke brain.setup`.

**Reason:** herdr does not put `<plugin_root>/bin` on the `PATH` of shell panes. So after install, `brain setup` in a shell gives "command not found".
An action needs no `PATH`. A popup closes by itself, and it needs no pane id.

**Test:**
- Unit: `TestManifestSetupAction`, `TestOfferLinkYes`, `TestOfferLinkNo`, `TestOfferLinkKeepsExistingFiles`, `TestSetupCommand`. PASS.
- Live, in a headless herdr server with the scratch `HOME` (not the main server), with `herdr plugin link` of the worktree:
  - `herdr plugin action list` shows `setup` and `status`. herdr accepted the manifest.
  - `herdr plugin action invoke brain.setup` with no workspace: the action fails with `plugin_pane_open_failed: no active workspace`.
  - After `herdr workspace create`: the action succeeds, and `bin/brain setup` runs as a child of the scratch server (seen with `ps`).
    A popup has no pane id, so the test could not read its screen or type into it. The unit tests cover the questions.
  - `printf 'y\n' | env -i HOME=<scratch HOME> bin/brain setup` made the link. A second run said "already points to it".
- Open: E10-S5 (first-use test in a clean home with `remember`) needs E4 and E10. It is not run yet.

**Note (2026-10-05, E19-S2 #238, owner decision "One yes for all"):** `brain setup` no longer asks before each write.
It checks herdr, asks the extractor choice, and then shows every write in one plan: the binding, the link, the hook entries, the hook record entries and the extractor setting, with the uninstall steps.
It asks once: "Apply all of this? [y/N]". "y" writes every step. "n", an empty answer or EOF writes nothing.
A step that is already right shows as "already done". With nothing to write, setup asks nothing.
The extractor choice "none" is recorded too (a store row with no CLI), so a second setup does not ask it again. `brain setup --extractor` asks it again.
The link rule stays: setup never replaces a file at the link path. The plan says so and prints the binary path.
If one write fails, setup stops with the error. A second `brain setup` completes the rest.
Tests: `TestSetupOneYes`, `TestSetupNo`, `TestSetupKeepsEditedEntry`, `TestSetupExtractorNone`, `TestSetupTestBinding`, `TestPlanLinkKeepsExistingFiles`.

## E2-S6: plugin `[[events]]` in M1 (#11)

### D4. `workspace.closed` marks a workspace closed

**Decision:** yes, for one event. The manifest gets one `[[events]]` entry for `workspace.closed`.
Its command runs one short `brain` subcommand. The subcommand marks the (herdr session, workspace id) pair closed in the workspace map and marks its `workspaces` rows `closed`. Then it exits.
- No listener process. herdr starts the command once for each event.
- No handler for `workspace.renamed` or `workspace.created`. The label refresh stays on each `brain` call.
- Other `brain` calls keep `Session.Sync` as it is. The hook keeps `Session.Lookup` as it is.
- A closed pair has no current entry. So a later workspace with the same id is unmapped, and the next `brain` call gives it a new key.
- This story writes the decision only. The coordinator schedules the subcommand, because other threads change `cmd/brain/main.go` now.

**Reason: herdr reuses a closed workspace id after a restart.**
- In a running session, ids only go up. After `session stop` and a new start, the next id is the highest open id + 1 (test step 3).
  So when you close the workspace with the highest id and herdr restarts, the next new workspace gets the old id.
- Without the event, Brain never learns about the close. The map keeps the pair (session, `w4`) on the old key.
  The next workspace `w4` then gets the closed workspace's items:
  - Same project folder: the hook says "mapped" and delivers the old workspace items. HLD §11 says a new herdr id never inherits items by folder, and §9.4 says Brain never links by itself. Both break.
  - Another project folder: the hook withholds ("withheld: identity"), and `brain status` asks for a remap. This fails closed, but you must act for a workspace that is simply new.
- A refresh on `brain` calls cannot fix this. `workspace list` and `session.json` hold no creation time and no unique id (E2-S5).
  A refresh that marks absent ids closed helps only when a `brain` call runs between the close and the reuse. The hook can run before any `brain` call.
- A restart after a close is a normal flow: `herdr update`, a reboot, `session stop`.
- The event gives exactly the missing fact. It fires about 40 ms after the close, with `HERDR_SESSION`, `HERDR_SOCKET_PATH` and `workspace_id` in its env (test step 2). It also fires when the last pane of a workspace closes (test step 4).
- HLD §9.4 ("Close: Brain marks the workspace closed") and the `workspaces` status (live/closed, §11) already ask for this. No HLD change is needed.

**Why not `workspace.renamed`:** a stale label is display only (the `workspaces` row and the map). Keys and block file names do not use it. The next `brain` call fixes it. The gain is small, so the simpler choice wins.

**Limits (known, no extra work in M1):**
- A close while Brain is not installed, or while `bin/brain` is missing (D1: reinstall window, uninstall), is lost. herdr does not replay events (HLD §4.2). The reused id then maps to the old key, as today. No backfill.
- An event reaches only the herdr server of the user's `HOME`. The plugin config is shared by all herdr sessions of that `HOME`, so every bound session gets the event (test step 1).

**Cost (the later build task):**
- `herdr-plugin.toml`: one `[[events]]` entry, `on = "workspace.closed"`, command `"$HERDR_PLUGIN_ROOT/bin/brain" <subcommand>`.
- One `brain` subcommand: read `HERDR_PLUGIN_EVENT_JSON` (`data.workspace_id`) and `HERDR_SESSION` (not set means `default`, see M0 FINDINGS). Find the binding with `bindings.BySocket` and `HERDR_SOCKET_PATH` and check its session name. Do not use `resolver.Resolve`: it needs `HERDR_PANE_ID` and a live pane, and a close event has neither (test step 2). Take the map lock, mark the pair closed, and set its rows to `closed`. An unbound session or an unknown pair does nothing.
- `internal/workspace`: a close method and a closed flag in the map entry. `current` skips closed entries. The flag is optional (`omitempty`), so the map file stays version 1: `readMap` rejects any other version, and an old map file has no flag, so it still parses.
- Tests: a closed pair gives `Unmapped` in `Lookup`. A reused id gets a new key in `Refresh`. The subcommand parses the event env and ignores an unbound session. A manifest test for the `[[events]]` entry.

**E2-S4 open point (old row stays `live` after a remap):** the event does not change it while the workspace stays open, because a remap is not a close. When the workspace closes, the handler marks all rows of the pair `closed`, the old remapped rows too.

**Test (scratch `HOME`, named session `brain-e2s6`, a probe plugin that logs its env; the main server and `default` were not touched):**

```sh
S=/tmp/e2s6
# h: herdr with the scratch HOME and the test session only
printf '#!/bin/sh\nexec env -i HOME=%s/home PATH=/usr/bin:/bin:%s/.local/bin herdr --session brain-e2s6 "$@"\n' $S $HOME > $S/h
$S/h server &                                   # headless test server
$S/h plugin link $S/plugin                      # [[events]] on workspace.created, .renamed, .closed; each logs its env
# step 2: events
$S/h workspace create --cwd $S/rootA --label A  # w1
$S/h workspace create --cwd $S/rootB --label B  # w2
$S/h workspace rename w2 B2
$S/h workspace close w2
$S/h workspace create --cwd $S/rootC --label C  # w3 (w2 not reused)
$S/h workspace close w1
$S/h workspace create --cwd $S/rootA --label D  # w4
# step 3: restart
$S/h workspace close w4; sleep 6               # session.json keeps only w3
$S/h session stop brain-e2s6; $S/h server &
$S/h workspace create --cwd $S/rootA --label E
# step 4: close the last pane
$S/h workspace create --cwd $S/rootB --label F  # w5
$S/h pane close w5:p1
$S/h session stop brain-e2s6; $S/h session delete brain-e2s6
```

**Results (herdr 0.9.1, 2026-09-28):**

| Step | Result |
|---|---|
| 1. `plugin link` | The plugin is linked in the scratch config. `session list` shows `default` (not running) and `brain-e2s6`. |
| 2. events | Every create, rename and close ran its command once, 20-60 ms after the CLI call. Env: `HERDR_SESSION=brain-e2s6`, `HERDR_SOCKET_PATH`, `HERDR_WORKSPACE_ID`, `HERDR_PLUGIN_EVENT=workspace.closed`, no `HERDR_PANE_ID` on a close, `HERDR_PLUGIN_EVENT_JSON={"event":"workspace_closed","data":{"type":"workspace_closed","workspace_id":"w2","workspace":{…,"label":"B2",…}}}`. The close context has no `workspace_cwd`. |
| 2. ids in a running session | After `close w2`, the next workspace got `w3`. After `close w1`, the next got `w4`. No reuse. |
| 3. `session stop` | No event ran. |
| 3. restart | `workspace list`: `w3` only. The new workspace E (in `rootA`, like the closed `w4` D) got **`w4`**. |
| 4. `pane close` of the only pane | The workspace closed, and `workspace.closed` ran for `w5`. |
| cleanup | `session stop` and `session delete` of `brain-e2s6`. No process left. The plugin link lived only in the scratch `HOME`. |

## Owner decisions of 2026-09-28

### D5. Owner decisions of 2026-09-28

**Decisions** (owner, 2026-09-28; no test, docs only):
- **E8-S2 snapshot (#39):** the hook-file snapshot is the full file, original bytes, for `~/.claude/settings.json` and `~/.codex/hooks.json`. It may hold tokens that the file already holds. There is no secret filter for the snapshot. Brain never uses or sends the tokens. HLD §1.4 has this exception line; §8.1 has no change.
- **Conflict repair:** you confirm a conflict with `brain repair --confirm`. It deletes the hand-edited generation folder and builds it again. `brain status` and `brain doctor` show the conflict (E9-S2, E9-S3). HLD §7.5 step 5.
- **Codex extractor (item 13):** Codex is accepted as an extractor. "No tools" means no tool that reaches files, the shell, the network, MCP or agents. Codex keeps its built-in code-mode `exec`/`wait` and `request_user_input_async`, because `codex exec` cannot turn them off (M0 FINDINGS, E13-S1). E13-S2 may set a time limit for the call. HLD §7.3, §17 item 13.
- **`agent read` of a reply (item 28):** PASS for Claude and Codex (E10-S4). HLD §17 item 28.

## E5-S3: popup placement and the plugin actions (#27)

### D6. The `review` action opens the queue in a split

**Decision:** `split`. The `review` action runs `plugin pane open --plugin brain --entrypoint queue`. The `queue` pane has `placement = "split"` and runs `brain review`, then waits for Enter.
- `popup` stays for the `setup` pane only. `brain setup` resolves no caller.
- The `remember` review pane (E5-S1, E5-S2) stays a no-focus split, as before. It is a separate `[[panes]]` entry.
- The queue pane is a normal plugin split pane with focus, because you start it by hand. It is not a `review_panes` row.
- In the herdr UI, you run the action with a key that you bind in your herdr `config.toml` (`[[keys.command]]`, `type = "plugin_action"`, `command = "brain.review"`). Brain does not write that file.

**Reason: a popup gives no clear gain.**
- A popup has no `HERDR_PANE_ID` and no `HERDR_WORKSPACE_ID`. `brain review` in a popup failed: "not in herdr: HERDR_PANE_ID is not set". A popup queue needs new code: resolve the caller from `HERDR_PLUGIN_CONTEXT_JSON` and apply the E4 scope check.
- A popup takes all input while it is open. A split leaves the other panes usable.
- A split pane has its own `HERDR_PANE_ID` in the caller's workspace. So `brain review` resolves the caller and applies the E4 scope check with no code change.
- Both close when the process exits. `brain review` prints and exits, so the pane command waits for Enter.

**Test** (herdr 0.9.1, 2026-09-28, scratch HOME, named session `brain-e5s3`, test binding; the main server and `default` were not touched). Full commands: `spike/m0/data/e5s3-popup-placement.txt`.

| Step | Result |
|---|---|
| Probe popup | No pane id in the reply, not in `pane list`. Env: `HERDR_PLUGIN_CONTEXT_JSON` with `workspace_id` and `focused_pane_id`; no `HERDR_PANE_ID`, `HERDR_WORKSPACE_ID`. Takes all input; Esc does not close it; closes on exit. |
| Popup `brain review` | FAIL: "no Brain store for this pane (not in herdr: HERDR_PANE_ID is not set)". |
| Probe split | `HERDR_PANE_ID` of the new pane and `HERDR_WORKSPACE_ID` of the focused workspace. Closes on exit. |
| `plugin action invoke brain.review` in B | Split `Brain queue` right of the focused pane, with focus. Lists the B and global proposals, not the A proposal. Enter closes it. |
| UI key `prefix+y` in B, then in A | B: B + global. A: A + global. |

## E11-S1: how `brained` runs (#55)

### D7. A `[[startup]]` command hosts `brained`

**Decision:** `[[startup]]`. The Brain manifest starts `brained` with one `[[startup]]` entry. It runs as one long process for each herdr session (each herdr server). Brain uses no `[[events]]` handler for capture, and no launchd or systemd.

`brained` must do these things itself, because herdr does not do them:
- **One copy for each herdr session [NEW].** It takes one lock file [NEW] in `HERDR_PLUGIN_STATE_DIR`, keyed by `HERDR_SESSION` (empty for `default`). A second copy exits. herdr can start a second copy while an old one still runs (test below).
- **Exit when the server stops [NEW].** herdr sends no signal on `session stop`, and the process lives on with ppid 1. The event socket closes (EOF), so `brained` exits on EOF.
- **Exit when Brain is disabled or unlinked.** herdr does not stop it. `brained` checks this itself. E11-S2 picks the check.
- **Subscribe for each pane.** `events.subscribe` needs a `pane_id` for `pane.agent_status_changed`. `brained` subscribes to `pane.created` and `pane.agent_detected` (no pane id), lists the panes at start, and adds a subscription for each pane.
- **No restart after a crash.** herdr does not start it again until the next server start or live handoff. `brain status` and `brain doctor` show "brained is not running". A new link or install also starts nothing until the server starts again.

**Reason:**
- HLD §6.2 needs one read at a time for each pane, in event order, and the last read in memory. A long process gives this.
- `[[events]]` runs one new process for each event. A burst of 10 changes gave 10 processes at the same time, with 3 s hooks. Nothing orders them, and none keeps state. Each call would need a lock and a state file.
- The `[[events]]` payload has no time and no event id. So a late process cannot tell that its event is old.
- The herdr plugin docs say that `[[startup]]` runs after the API socket is ready. The probe did not connect to the socket at startup, so this is not tested. `brained` retries the first connect [NEW].
- A long startup process is allowed: two ran for 12 minutes with no kill.

**Test** (herdr 0.9.1, 2026-09-29, scratch HOME, named sessions `brain-e11s1` and `brain-e11s1b` and a scratch-HOME `default`; the main server and the real `default` were not touched). Probe: `spike/m0/e11probe/`. Raw log: `spike/m0/data/e11s1-hosting.txt`.

| Step | Result |
|---|---|
| Server start | One startup process for each session. Env: `HERDR_SESSION`, `HERDR_SOCKET_PATH`, `HERDR_PLUGIN_STATE_DIR`, `HERDR_PLUGIN_EVENT=startup`, context `invocation_source` = `startup`. |
| Long run | Two processes ran about 12 min. Plugin log status: `running`. |
| `plugin disable`, then `enable` | The process runs on. `enable` starts no second copy. Event hooks do not run while disabled. |
| `plugin unlink`, then `link` | The process runs on. Event hooks stop. `link` starts no copy. |
| `kill -9` | No restart. Plugin log status: `failed`, no exit code. |
| `session stop` | The process runs on with ppid 1 and gets no signal. The next server start runs a second copy beside it. |
| `[[events]]` burst, 10 changes, 3 s hook | 10 hook processes at the same time. All 10 ran. |
| `events.subscribe` with no `pane_id` for `pane.agent_status_changed` | `invalid_request`: "missing field `pane_id`". |
| Socket subscriber on `session stop` | It reads EOF and exits. |

### D8. Claude capture: the screen read plus hooks, and no scroll

**Decision** (owner, 2026-09-29; no new test): Brain keeps the rule "never sends keys or scrolls a pane". For an alternate-screen agent (Claude), Brain reads only `visible`. Hook records give the prompt, the commands, the errors and the final reply (`Stop` `last_assistant_message`). Brain reports lower coverage for such agents.
- The M2 evals measure the loss. Brain builds a transcript reader only if the loss matters (HLD D5).
- HLD §6.1 (`Stop` line) and §6.2 Limits have this text.

**Reason:**
- E11-S1 showed that a herdr read longer than the screen scrolls the Claude pane through mouse-scroll input at idle. This breaks the "never scrolls" rule.
- While Claude is `working` or `blocked`, herdr can read only the screen (`agent_not_idle` above it). At idle, a longer read often returned only the screen with exit 0. So the longer read gives little extra text that Brain can trust.
- Raw log: `spike/m0/data/e11s1-capture.txt`.

## E15-S3: M2 exit run (#80)

### D9. M2 exit record

**Status:** the exit stays open, for one reason. The cross-harness case is a check that a person runs, and it is not run yet, so the sentence "One inferred correction helps a later task in another harness" has no passing record. All listed scenarios and the reject test pass as Go tests (2026-10-02, branch of #80; S38 with the resize reads of #199 and the fix of #203, note 1).

**Scenarios** (HLD §14). Each test names its scenario id in its name or its comment. The tests use temp stores, the fake herdr and the fake extractor: no live herdr, no CLI and no model call.

| # | Expected (§14, short) | Tests | Result |
|---|---|---|---|
| S31 | Fragments kept; no invented user turns | `internal/brained` `TestCaptureS31` (codex, claude) | PASS |
| S32 | Partial baseline and a gap; no invented start message | `internal/brained` `TestCaptureS32` (new), `TestStartReconcile`, `TestCaptureNativeIDLater` | PASS |
| S33 | A gap; no text goes to a new pane | `internal/brained` `TestCaptureS33` | PASS |
| S34 | One occurrence | `internal/brained` `TestCaptureS34`, `TestCaptureReplay`; `internal/store` `TestAddObservationOnce` | PASS |
| S35 | One occurrence with both references | `internal/brained` `TestCaptureS35` | PASS |
| S36 | Read dropped, or a new segment in the new workspace | `internal/brained` `TestCaptureS36`, `TestCaptureMovedEnd` | PASS |
| S37 | No owner mark and no permission item | `internal/brained` `TestCaptureS37`; `internal/signal` `TestAssistantQuote` | PASS |
| S38 | No extra count; lost text is a gap | `internal/brained` `TestCaptureS38`, `TestCaptureScreenLost` (repaint, scroll-away, alternate-screen loss), `TestCaptureS38Resize` (recorded resize and rewrap reads, Claude and Codex; note 1), `TestNewPartRewrap` | PASS |
| S39 | Reconcile live state and record a gap | `internal/brained` `TestBehind` | PASS |
| S40 | Two occurrences | `internal/brained` `TestCaptureS40` | PASS |
| S41 | Sender stays unconfirmed | `internal/brained` `TestCaptureS41` | PASS |
| S42 | Queue fair and bounded; hook delivery fast | `internal/extract` `TestQueueFairAndBounded` (fair turns, the daily call cap, and each hook write during an extractor call stays inside the hook cap of 300 ms); `internal/hook` `TestTiming` (the whole hook run) | PASS |
| S55 | Notification and no-focus pane; no model call; the agent keeps working | `internal/cli` `TestSuggestShows`; `internal/brained` `TestCaptureSuggestion` | PASS |
| S56 | Request stored; new draft later; proposal not published | `internal/cli` `TestReviewChange`, `TestPaneChange`; `internal/extract` `TestChangeRequestDraft` | PASS |
| S57 | One occurrence with both references; one suggestion | `internal/brained` `TestSuggestionOnce`, `TestSuggestionConcurrent`; `internal/store` `TestReserveSuggestionOneOccurrence` | PASS |
| S68 | Prompt text and hash rows gone; proposals listed first | `internal/cli` `TestForgetSessionPromptIndex` | PASS |
| S69 | Retry counts once; nothing twice; forget discards the batch | `internal/extract` `TestCrashAfterResultBeforeCommit` (new: a valid result, then the commit transaction aborts at its last write; everything rolls back, and the retry counts once), `TestCrashBeforeCommit` (a crash during the call), `TestCrashAfterCommit`, `TestForgetDuringExtraction` | PASS |
| S70 | Empty moves the watermark; invalid keeps it, status shows it | `internal/extract` `TestEmptyAndInvalidResults`; `internal/cli` `TestStatusFailedBatch` | PASS |
| S71 | Each labeled prompt gives its result; 0 model calls | `internal/store` `TestPromptSet` (`evals/prompts.toml`) | PASS |
| S75 | Model-call counter of the workspace stays 0 | `internal/extract` `TestExtractionOffWorkspace` | PASS |

Notes:
1. S38 resize and rewrap (#199, #203, 2026-10-02): a headless herdr does not resize the pane terminal, so the reads come from a herdr client in a pseudo-terminal that a script resizes (`spike/m0/s38rec/`, notes in `internal/brained/testdata/README.md`). Claude draws its screen again at the new width; Codex clears its history and draws it again. `TestCaptureS38Resize` checks with these reads: a rewrap to a narrower pane and back gives no observation; a screen too small for the old text gives one `screen lost` gap; a screen that grows again shows the old text with no count; a new turn after a resize keeps only the new prompt and reply. The first reads gave two extra counts (#203): the rewrapped old turn and the new turn in one block of new lines, and the old text of a screen that grows. The fix in `newPart` treats the text of the replaced old lines at the start of such a block as a rewrap; a block after the last old line stays new (S40). A partial read does not count text that the last two different screens show. Repaint, scroll-away and alternate-screen loss keep their tests.
2. New test for the issue: `internal/cli` `TestSuggestRejectNoNag`. You reject a live suggestion. The same prompt again in the same session and in a new session reserves no suggestion, so no notification and no pane. The fake extractor then gives the same claim for the repeats: the evidence attaches, and no new proposal comes for the claim in the workspace. PASS. A check without the reject fails the test.

**§13 M2 exit sentences**

| Sentence | Proof | Result |
|---|---|---|
| One inferred correction helps a later task in another harness | The cross-harness record below | NOT RUN YET (exit open) |
| A repeated prompt shows a ready proposal beside the agent, and you decide without leaving the session | S55 (`TestSuggestShows`); the pane keys approve, reject, later and change (`internal/cli` `TestPaneKeys`); each decision gives the suggestion its result (`TestSuggestResults`) | PASS |
| A wrong proposal can be rejected with no repeated nagging | E12-S3 `internal/store` `TestRejectionNewEvidence`, `internal/cli` `TestReviewRejectClaim`; the new `TestSuggestRejectNoNag` | PASS |
| Close prompt matches turn on only after the labeled prompt set passes | S71 (`TestPromptSet`) | PASS |
| S31-S42, S55-S57, S68-S71 and S75 pass | The scenario table above | PASS |

**Cross-harness case: checklist for a person** (HLD §13, §8.3 test bindings, §12.1 control; as E10-S5). A person runs it, not the eval runner and not an agent. It uses official, unmodified `claude` and `codex` binaries. It never touches the main herdr session or the main store.

Preparation:
1. Pick a fresh opaque marker that no session has seen, for example `vorn-5821`. Below, `<marker>` is that marker.
2. Make a new run folder that did not exist before, so the test store is empty and the control gets an empty brief (§12.1). Use the date in its name, for example `~/brain-m2-20261005`. Below, `<run>` is that folder. Never reuse the store of an earlier run:
   `test ! -e <run> && mkdir -p <run>/store <run>/work && git -C <run>/work init`
3. Start the separate test herdr session: `herdr --session brain-m2`. Every step below runs in a pane of this session.
4. Open a workspace in `brain-m2` with root `<run>/work`, and a shell pane in it. Run every `brain` command below in this shell pane.
5. Bind the session to the test store and choose the extractor CLI (claude or codex; write down which):
   `brain setup --session brain-m2 --test <run>/store --extractor`
6. A test binding starts with learning off (§12.1). Extraction is on by default; turn it on anyway, so the run does not depend on the default:
   `brain learn on` and `brain extract on`
7. Run `brain status` until it shows the workspace as mapped. A workspace that no `brain` call mapped gives its capture no workspace key, and the extractor cannot use that evidence. Write down the workspace id.
8. Check: `brain status` and `brain doctor` name the `brain-m2` test binding, learning on, extraction on, the chosen extractor CLI, and the Claude and Codex profile states (write them down).

Steps (example order: correction in Claude, later task in Codex; the other order is also valid):
1. **Correction.** In the workspace, start `claude`. Ask: "Create a release note file for version 1.2 with one line." When the agent picks a file name, correct it: "No. Release note files here are always named RELNOTES-<marker>.md. Rename it." Let it finish. Exit Claude, so the pane ends the session.
2. **Draft.** Wait until the extractor drafts the proposal. brained runs a pass about once a minute. A session that does not end waits 10 minutes of quiet. In a shell pane, `brain status` shows the backlog. Then run `brain review`. If the proposal is below the gate, run `brain review --all`. Write down the proposal id. Do not approve yet.
3. **Control.** Before the approve, start a fresh `codex` session in the same workspace, after a reset of the scratch folder (`git -C <run>/work clean -fdx`). Ask the same task: "Create a release note file for version 1.2 with one line." Write down the file name. Exit Codex.
4. **Approve.** `brain review <proposal id> --approve`. Write down the item id that it prints.
5. **Later task.** Reset the scratch folder again. Start a fresh `codex` session in the same workspace. Ask the same task. Write down the file name. Exit Codex.
6. **Receipts and item revision.** In the shell pane (read-only, test store only). The receipt list shows every receipt of the run, so the control and the later session each have their own row:
   - `sqlite3 -readonly <run>/store/brain.db "SELECT id, profile, pane_id, agent_session_id, workspace_key, result, withheld_reason, brief_hash, output_generation, cli_version, item_revisions, at FROM deliveries ORDER BY id;"`
   - `sqlite3 -readonly <run>/store/brain.db "SELECT i.id, i.current_revision, i.state, r.revision, r.text FROM items i JOIN item_revisions r ON r.item_id = i.id ORDER BY i.id, r.revision;"`
   - `brain status` (delivery gaps per profile, and the M2 rows).

Pass rule:
- The control (step 3) does not use the rule: the file is not `RELNOTES-<marker>.md`, and its receipt holds no revision of the item.
- The later task (step 5) uses the rule: the file is `RELNOTES-<marker>.md`.
- The receipt of the later Codex session is `emitted`, and its `item_revisions` holds `{"item": <item id>, "revision": <n>}`, where `<n>` is the current revision of the item from step 4.
- Anything else is a FAIL, with the reason written here. A skipped or blocked run is not a pass.

After the run: copy the query output and the replies into the result below. Then close the `brain-m2` session (`herdr session stop brain-m2`, never the main server). Keep `<run>` until the result is written.

Result (fill in): **not run yet.**
- Run: date, `<run>` folder, herdr version, store binding (`brain-m2` → `<run>/store`), extractor CLI, marker, Claude and Codex versions and profile states.
- Correction: the Claude session id, the proposal id, and whether it needed `brain review --all`.
- Control: the Codex reply text and file name; its receipt row (id, result, brief hash, output generation, item revisions); run status (delivered, not delivered, reply incomplete or skipped, §12.1).
- Approve: the item id and its current revision.
- Later task: the Codex reply text and file name; its receipt row (id, result, brief hash, output generation, item revisions); run status.
- Verdict: PASS or FAIL, with the reason.

**Review prompt** (§13 "stop or rethink", a note, not a gate): "Comebacks do not fall in the examples, or suggestions feel like noise." Look at it with the M2 rows of `brain status` (comebacks per revision, live suggestion counts) after some weeks of real use. Brain makes no automatic decision from it.

## E16-S14: M3 exit run (#165)

### D10. M3 exit record

**Status:** the exit stays open, for three reasons. The kept-skill check (A), the skill choice stage on real agents (B) and the agy delivery stage (C) are checks that a person runs, and they are not run yet. So the sentences "One skill that you keep, and agents use it" and "Skill choice passes with real agents" have no passing record, and the agy delivery AC has none. All listed scenarios pass as Go tests (2026-10-03, branch of #165, main 716c14f).

**Not part of this run** (owner, 2026-10-03): pi and OpenCode. pi comes later with E16-S15 #217 and E16-S16 #218. OpenCode stays a gap, and E16-S13 #164 is deferred. They do not block the M3 exit.

**Scenarios** (HLD §14). Each test names its scenario id in its name or its comment. The tests use temp stores, test HOME folders, the fake herdr, fake CLIs and the fake extractor: no live herdr, no CLI and no model call. Five tests are new for this run (marked "new").

| # | Expected (§14, short) | Tests | Result |
|---|---|---|---|
| S8 | Workspace skill draft; after approval the "shop" hook lists it; other workspaces do not see it | `internal/cli` `TestSkillDraftS8` (the "shop" brief lists it, the "blog" brief on the same checkout does not); `internal/extract` `TestSkillDraft` | PASS |
| S9 | Item becomes disputed; the queue shows the anchor diff | `internal/cli` `TestDisputeKeep`; `internal/brained` `TestStale` | PASS |
| S11 (skill part) | Hash mismatch: Brain reports a conflict and asks before it overwrites or removes the file | `internal/output` `TestSkillHandEdit`; `internal/cli` `TestSkillConflictRepair` (status and `brain repair` show it; only `--confirm` writes it again), `TestForgetSkillPendingOnConflict` (forget stays pending and keeps the file) | PASS |
| S19 | Items stay linked; the block file does not move | `internal/cli` `TestRenameS19` (new: after a rename of "shop", its item, its skill and a new item keep key `ws_a14`; the brief holds all three; the block files keep their names); `internal/workspace` `TestRootPaneCloseAndRenameKeepKey` | PASS |
| S20 | No automatic link and no question; `brain status` notes the old workspace; relink on request | `internal/cli` `TestClosedSameRootS20` (new: after a brain call the closed key keeps its items, the new brief does not hold them, and status lists the old key with the link command), `TestWorkspaceLink` (the link on request); `internal/workspace` `TestRelinks` | PASS |
| S27 | Each workspace on one checkout gets only its own items and skills | `internal/cli` `TestSameCheckoutS27` (new: one item and one skill in each of two workspaces on one checkout); `internal/output` `TestSkillList` | PASS |
| S29 | A pinned procedure stays active; no archive candidate | `internal/cli` `TestPinnedSkillS29` (new: a pinned skill after 183 days is no candidate and stays active after `brain archive --all`; the unpinned skill is retired), `TestArchiveList`; `internal/store` `TestArchiveCandidatesPinned` | PASS |
| S46 | Undo of a new item retires it; undo of a promotion moves it back | `internal/cli` `TestUndoCreationAndRetirement`, `TestPromoteSkillAndUndo` | PASS |
| S60 | agy sessions get the brief with no new Go code | `internal/adapter` `TestProfileCases/agy` (golden hook cases: the brief in agy's output form); `internal/install` `TestProfileRoundTrip/agy`. PR #222 adds `adapters/agy.toml` and test data only, no `.go` file. PR #221 added the eval field `eval.dir` for the runner (owner, 2026-10-03), not for delivery. The live agy check is checklist C. | PASS |
| S61 (skill part) | No brief and no skill in `~/.claude/skills`; Brain has no effect | `internal/cli` `TestSkillFilesStayInStore` (test HOME: no file in `~/.claude/skills`, `~/.codex` or the repo after build, approve, retire and forget); `internal/hook` `TestOutsideHerdr` (outside herdr: no output, no file read, no herdr call) | PASS |
| S72 | The brief shows the item with a disputed mark until you decide | `internal/output` `TestDisputedSkill`; `internal/brief` `TestGolden/disputed`; `internal/cli` `TestDisputeKeep` (the mark goes after keep) | PASS |
| S83 (relink part) | `w1` gets a new key and no old items; the old items are listed for relink | `internal/cli` `TestReplacedNoOldItemsS83` (new: the items of the replaced key stay there, the brief of the new key does not hold them, and status lists the old key for relink); `internal/workspace` `TestOtherProjectWithheldAndRemap` (the remap gives a new key), `TestRelinks`; `internal/cli` `TestStatusRelink`, `TestWorkspaceLinkReplaced` | PASS |

**§13 M3 exit sentences**

| Sentence | Proof | Result |
|---|---|---|
| One skill that you keep, and agents use it | Checklist A below | NOT RUN YET (exit open) |
| A new profile works with no new Go code (S60) | E16-S12 #163: PR #221 and PR #222; S60 above | PASS (Go tests). The live agy delivery is checklist C: NOT RUN YET. |
| Disputed items show their mark (S72) | E16-S6 #157; S72 above | PASS |
| Skill choice passes with real agents (§12.1) | The E16-S5 #156 stage: `cmd/brain-eval` `TestSkillChoiceStage`, `TestSkillChoiceStealFails`, `TestSkillProbes` pass with fake CLIs; the live run is checklist B | NOT RUN YET (exit open) |

**Common preparation for A, B and C.** A person runs these checks, not the eval runner and not an agent. They use the official, unmodified `claude`, `codex` and `agy` binaries. Brain is installed as the herdr plugin, so `brain` is on the PATH of herdr panes. The data home is `~/.local/share/brain`; the main store is `~/.local/share/brain/main`. Every `sqlite3` call below is read-only. B and C never touch the main herdr session or the main store; A uses the main store on purpose.
1. Check the setup: `brain doctor` and `brain status`. Write down the herdr version and the state of each CLI profile.
2. For B and C, build the eval runner from the Brain repo at the main commit: `go build -o /tmp/brain-eval ./cmd/brain-eval`. It needs the install record with the Brain hook of each CLI it runs (`brain setup`), and a test binding of the `brain-eval` session. Make a new test store folder that did not exist before, for example `~/brain-m3-eval-20261005` (below, `<evalstore>`):
   - `test ! -e <evalstore> && mkdir -p <evalstore>`
   - `herdr --session brain-eval server` (in its own terminal; it stops when the run is done)
   - `brain setup --session brain-eval --test <evalstore>`
3. Run `brain-eval` from the Brain repo root, so the probe paths below resolve. It writes `<evalstore>/eval-results.json` and `<evalstore>/eval-report.md`.

**Checklist A: one skill that you keep, and agents use it** (§13; as E10-S5)
1. **Draft.** In a real herdr workspace (below, `<ws>`), with learning and extraction on (`brain learn on`, `brain extract on`), explain one real procedure of your work in three sessions, as S8 does. Then wait for a pass of the extractor (`brain status` shows the backlog).
2. **Approve.** `brain review` lists `skill <name>: <description>`. Read the whole file with `brain review <proposal id>`. Approve it, with an edit if needed: `brain review <proposal id> --approve` or `brain review <proposal id> --approve --edit "<skill text>"`. Write down the item id that it prints.
3. **Keep.** `brain pin <item id>`, so the skill never ages. The skill stays active through the run.
4. **Check the brief.** In a shell pane of `<ws>`: `sqlite3 -readonly ~/.local/share/brain/main/brain.db "SELECT i.id, i.kind, i.scope, i.workspace_key, i.state, i.pinned, i.current_revision FROM items i WHERE i.id = <item id>;"` Write down the current revision `<rev>`. The skill file is `~/.local/share/brain/main/skills/<item id>/r<rev>/SKILL.md`.
5. **Claude.** In `<ws>`, start a fresh `claude` session. Give it a task that matches the skill's example, without the skill name. Watch the pane: Claude reads the SKILL.md (a read of the path from step 4) and follows its steps. Write down the line that shows the read, and which steps it followed. Exit Claude.
6. **Codex.** In `<ws>`, start a fresh `codex` session. Give it the same kind of task. Watch the pane for the read of the SKILL.md (for example a `cat` or `sed` of the path) and the steps. Write down the line. Exit Codex.
7. **Receipts.** `sqlite3 -readonly ~/.local/share/brain/main/brain.db "SELECT id, profile, pane_id, agent_session_id, workspace_key, result, brief_hash, output_generation, cli_version, item_revisions, at FROM deliveries ORDER BY id DESC LIMIT 5;"` The receipts of the two sessions are `emitted`, and their `item_revisions` hold `{"item": <item id>, "revision": <rev>}`.
8. **Shell records (optional).** When the tool hook records are on, a shell read of the file is also a command record: `sqlite3 -readonly ~/.local/share/brain/main/brain.db "SELECT observation_id, argv, cwd FROM command_records WHERE argv LIKE '%SKILL.md%';"` A Claude `Read` is no shell command, so the screen is the proof there. An automatic count of skill reads stays an open point of #165.

Pass rule: both sessions read the SKILL.md of the item and follow its steps, and both receipts name the item at revision `<rev>`. Anything else is a FAIL, with the reason written here. A skipped or blocked run is not a pass.

Result (fill in): **not run yet.**
- Run: date, herdr version, workspace, Claude and Codex versions and profile states.
- Skill: name, item id, revision `<rev>`, pinned.
- Claude: session id, task text, how the read was seen (the screen line), steps followed, receipt row.
- Codex: session id, task text, how the read was seen, steps followed, receipt row.
- Verdict: PASS or FAIL, with the reason.

**Checklist B: skill choice stage on real agents** (§12.1 "Skill choice", E16-S5)
1. Do the common preparation.
2. Run: `/tmp/brain-eval --probes evals/probes/skills.toml --stage skill-choice --harness claude --harness codex`. Each example prompt and each negative prompt runs in a fresh CLI session with the three fixture skills (release-notes and the near pair pg-migration and sqlite-migration) in the brief, and again as a control with no skill.
3. Read the result: the run prints `skill choice claude: <verdict>` and `skill choice codex: <verdict>`. `<evalstore>/eval-report.md` has the "Skill choice" section; `<evalstore>/eval-results.json` holds each result with its reply text, rule, run status, CLI version and profile hash.
4. Resolve each UNSURE result by hand to PASS or to a failure, with the reply text (§12.1).

Pass rule (E16-S5 [NEW], from the E16-S1 data): the stage passes for a CLI when every result of the stage passes, the controls too. The example of each skill uses that skill's marker; other prompts and every control use no marker. A fail or a skip fails the stage. An unresolved UNSURE is not a pass.

Result (fill in): **not run yet.**
- Run: date, `<evalstore>`, herdr version, Claude and Codex versions.
- Verdict per CLI, with each non-PASS result and its reply text.

**Checklist C: agy delivery stage** (E10 delivery stage; owner, 2026-10-03; moved from #163)
1. Do the common preparation. Run `brain setup` and accept the agy hook: Brain writes its entry in the real `~/.gemini/config/hooks.json`. Check `brain status`: agy is `configured` (or `retest`, step 2).
2. Record the version: `agy --version`. The profile's tested version is 1.2.16; the target is 1.2.16 or later (owner, 2026-10-03). On a later version, `brain status` shows agy `retest` before the run; a pass on that version makes it `verified` (§8.5).
3. Trust the fixed eval folder once, by hand. The profile's `eval.dir` is `/tmp/brain-eval-agy`. On macOS `/tmp` is `/private/tmp`, and agy trusts the exact real path. `mkdir -p /private/tmp/brain-eval-agy && (cd /private/tmp/brain-eval-agy && agy)`. Accept the trust question, then exit agy. The subshell keeps your shell in the Brain repo root for step 4. This is the one trust write in agy's own settings. Do no other config change.
4. Run: `/tmp/brain-eval --probes evals/probes/delivery.toml --stage delivery --harness agy`. The case `delivery-agy` approves a workspace item with a fresh marker in the test store and asks agy for it, in the fixed folder.
5. Read the result: the printed line `delivery-agy agy: <status>, <label>`, `<evalstore>/eval-report.md`, and the conformance record: `jq '[.results[] | select(.harness == "agy")] | last' ~/.local/share/brain/conformance.json`. `brain status` shows agy `verified` after a pass.

Pass rule: the reply holds the exact marker, the run status is `delivered`, the control has no marker, and the conformance record of agy is `pass` on the recorded version, so `brain status` shows agy `verified`. Anything else is a FAIL, with the reason written here.

After the runs of B and C: stop the test session (`herdr session stop brain-eval`, never the main server). Keep `<evalstore>` until the results are written.

Result (fill in): **not run yet.**
- Run: date, `<evalstore>`, herdr version, `agy --version`.
- Reply text, run status, label, and the conformance record (result, cli_version, run_id).
- Verdict: PASS or FAIL, with the reason.

**Review prompt** (§13 "stop or rethink", a note, not a gate): "No procedure repeats in 6 weeks." Look at it after some weeks of real use, with the skill drafts in `brain review` and the repeat counts of `brain status`. Brain makes no automatic decision from it.

## E17-S12: M4 exit run (#177)

### D11. M4 exit record

**Status:** the exit stays open, for two reasons. The two-harness check (checklist A) is a check that a person runs, and it is not run yet, so the sentence "Two harnesses reuse one checked tool" has no passing record. The gateway part of S63 is deferred with the MCP gateway E17-S7 #172 (owner, 2026-10-03), so the sentence "The gateway part of S63 passes" has no record; deferred is not a pass. All listed scenarios and the learned automation pipeline test pass as Go tests (2026-10-05, branch of #177, main fc98de0).

**Not part of this run:** the MCP gateway E17-S7 #172 (owner, 2026-10-03: "the mcp server does not look much useful rn, we can do that later") and the hook and allow-rule suggestions E17-S9 #174 (owner, 2026-10-05, D12 answer B). Both stay open, with revisit points in #151. #177 does not depend on them. The S63 gateway sentence stays open until #172 runs.

**Scenarios** (HLD §14). Each test names its scenario id in its name or its comment. The tests use temp stores, the fake herdr, fake CLIs and the fake extractor: no live herdr, no CLI and no model call. Two tests are new for this run (marked "new").

| # | Expected (§14, short) | Tests | Result |
|---|---|---|---|
| S21 | A change to the command, schema, cwd, timeout or effects stops the tool until you approve it again | `internal/tool` `TestHashCoversTheContract` (each change, and the scope, gives a new contract hash); `internal/output` `TestToolApproval` (a draft revision, another approved hash or another row contract does not run; an approved change runs with its new hash); `internal/cli` `TestToolChangeNeedsApproval` (for each of command, schema, cwd, timeout, effects and the read-only restriction, the changed contract does not run while the change waits; after the approval only the new hash runs), `TestRunChangedFile` (a `tool.toml` changed outside Brain: `brain run` refuses with a refused row, and status shows the tool as stopped), `TestAutomationContractChange` (the automation of a changed tool stops until a new approval) | PASS |
| S58 | `; rm -rf ~` or `$(…)`: the pattern fails, or the value stays one argv element; nothing runs in a shell | `internal/cli` `TestRunInjection` (`; rm -rf ~`, `$(touch pwned)` and a backtick form fail the pattern with a refused row; with an open pattern the value reaches the program as one argv element, and no `pwned` file appears); `internal/tool` `TestFill` (the pattern check), `TestExecSteps` (`run.sh` gets `$(c)` as `"$2"`); `internal/cli` `TestToolEditRefused` (an input in a command position or in `bash -c` is refused at the review) | PASS |
| S59 | A stop while `brained` is down does not run later; a stop while an agent works in the pinned folder is skipped and logged | `internal/brained` `TestAutomationBaseline` (at a start the current state is a baseline: the missed stop never runs), `TestAutomationReconnect` (the same after a reconnect, and a late copy of the missed stop claims nothing); `internal/cli` `TestLaunchSkip` cases "AC S59: an agent works in the folder" and "an agent starts to work while the run waits" (nothing runs, and the run row says `skipped`), `TestLaunchBusyElsewhere` (an agent in another folder or workspace does not skip) | PASS |
| S51 duplicate | One run | `internal/brained` `TestAutomationStop` (a duplicate of the stop claims nothing), `TestS51Automation` (new); `internal/store` `TestClaimRun`, `TestClaimRunRace` (two claims of one event at the same time give one row); `internal/cli` `TestLaunchRun` (a second launch of a run refuses); `internal/brained` `TestM4LearnedAutomation` (new) | PASS |
| S51 pause | No run after pause | `internal/brained` `TestS51Automation` (new: a stop after the pause claims nothing; a stop after the resume claims one run), `TestAutomationSkip` "an already-paused binding"; `internal/cli` `TestLaunchSkip` "pause while the run waits" (a claimed run with no launch mark does not start), `TestAutomationPause` | PASS |
| S51 removal | No run after removal | `internal/brained` `TestS51Automation` (new: a stop after the removal claims nothing); `internal/cli` `TestLaunchSkip` "removal while the run waits" and "undo while the run waits", `TestAutomationRemove`, `TestAutomationUndo` | PASS |
| S51 failure | The failure shows in status | `internal/cli` `TestLaunchFailed` (the run row says `failed`, `brain status` has the `last run failed` line, and the pane label says `touch-ran failed`) | PASS |
| S51 Brain-origin | No chain, and nothing is learned from the Brain-origin run | `internal/cli` `TestLaunchOrigin` (the tool gets `BRAIN_CALL_ORIGIN=automation`, and a `brain run` inside it logs origin `automation`), `internal/brained` `TestM4LearnedAutomation` (new: after the Brain-origin run, brained claims nothing and starts no launch process, and the next extraction pass makes no call and no proposal); `internal/store` `TestStopCalls` (an automation call is never a learned case). The trigger is a herdr agent status event of a bound pane; the launch process is no agent and has no pane of its own, so its run cannot be a stop. | PASS |
| S62 | A crash during a run shows `unknown`; no retry; you review it | `internal/cli` `TestLaunchCrash` (a launch mark with no end stays `unknown`, the launch of the run refuses, the same event claims nothing, and `brain status` shows the run with the cancel hint); `internal/store` `TestRunEnd`. The crash is a written launch mark with no end; a real process kill was not tested (E17-S11). | PASS |
| S61 (gateway part) | No Brain MCP tools outside herdr | Deferred with E17-S7 #172 (owner, 2026-10-03). The hook and skill parts passed in M1 and M3 (D10). | DEFERRED (not a pass) |
| S63 (gateway part) | The gateway shows no tools and refuses a direct `run_tool`, `recall` or `remember` call | Deferred with E17-S7 #172 (owner, 2026-10-03). The hook part passed in M1. | DEFERRED (not a pass) |

**Learned automation pipeline** (§10.4 "M4 must prove that one learned automation runs"): `internal/brained` `TestM4LearnedAutomation` (new, in the external test package `brained_test`), offline with the fake herdr of the brained tests and the fake extractor (0 model calls). PASS. It drives the real code of each step: `brain run` (`cli.Run`), the extraction pass (`extract.Pass`), `brain review` (`cli.Review`), brained with its status events, and the launch process (`cli.Automation` with the arguments and the environment that brained gave the launch).
- `approve`: the recorded learned case of E17-S10 (in 3 sessions the claude agent stops, then `brain run` runs the approved tool `lint` in its folder) gives one workspace proposal in `ws_1`. Before the approval, a stop of the agent claims nothing, and the tool does not run. After the approval, one stop event through brained claims one run and starts one launch process. The launch runs the tool once, and the run is `ok`. A duplicate of the event claims nothing, and the tool does not run again. After the Brain-origin run, brained claims nothing and starts no launch process, and the next extraction pass makes no call and no proposal.
- `reject`: the same case, rejected. A stop through brained claims nothing, and after a fourth session the next pass gives no new proposal in that scope.
- `internal/brained/export_test.go` gives the test the brained harness (package brained cannot import `internal/cli`). The harness gets an option with no automation and a tool that writes a file; no production code changed.

**§13 M4 exit sentences**

| Sentence | Proof | Result |
|---|---|---|
| Two harnesses reuse one checked tool | Checklist A below | NOT RUN YET (exit open) |
| A changed contract needs a new review | S21 above | PASS |
| Input injection fails (S58) | S58 above | PASS |
| The automation tests pass (S51, S59, S62) | S51 (all five parts), S59 and S62 above; the pipeline test | PASS |
| The gateway part of S63 passes | Deferred with E17-S7 #172 (owner, 2026-10-03) | DEFERRED (exit open for this sentence; not a pass) |

**Checklist A: two harnesses reuse one checked tool** (§13; as E10-S5). A person runs it, not the eval runner and not an agent. It uses the official, unmodified `claude` and `codex` binaries. Brain is installed as the herdr plugin, so `brain` is on the PATH of herdr panes, and the Claude and Codex start hooks are in place (`brain setup` was run once). The check never touches the main herdr session or the main store. Every `sqlite3` call is read-only.

Preparation:
1. Make a new run folder that did not exist before, so the test store is empty. Use the date in its name, for example `~/brain-m4-20261006`. Below, `<run>` is the full path of that folder (no `~`):
   `test ! -e <run> && mkdir -p <run>/store <run>/work && git -C <run>/work init`
2. Start the separate test herdr session: `herdr --session brain-m4`. Every step below runs in a pane of this session.
3. Open a workspace in `brain-m4` with root `<run>/work`, and a shell pane in it. Run every `brain` command below in this shell pane.
4. Bind the session to the test store: `brain setup --session brain-m4 --test <run>/store`. When setup asks to add the hook record entries for Claude (`PostToolUse`), answer `y`: the tool draft comes from a command record.
5. A test binding starts with learning off (§12.1). Turn it on: `brain learn on`. Run `brain status` until it shows the workspace as mapped. Write down the workspace id.
6. Check: `brain status` and `brain doctor` name the `brain-m4` test binding and learning on. Write down the Claude and Codex profile states, `claude --version` and `codex --version`.

Steps:
1. **A command record.** In the workspace, start `claude`. Ask: "Run the shell command `/bin/ls -1` in this folder and show me the output." Allow the command. Exit Claude, so the pane ends the session.
2. **Tool draft.** Wait for a pass of the extractor (about once a minute; `brain status` shows the backlog). Run `brain review --all`. Find the command candidate of `/bin/ls -1` and its line `make this a tool: brain review --tool <candidate>`. Run that command. Write down the proposal id that it prints. If no candidate comes after 10 minutes, write BLOCKED with the `brain status` output, and stop.
3. **Approve one tool.** Read the draft: `brain review <proposal id>`. Approve it with this exact contract (put the real `<run>` path in `cwd`):
   ```
   brain review <proposal id> --approve --edit 'name = "list-work"
   description = "List the files of the work folder"
   cwd = "<run>/work"
   timeout_s = 30
   effects = "reads the folder; writes nothing"
   read_only = true
   argv = ["/bin/ls", "-1"]
   '
   ```
   Write down the item id that it prints. Then `brain tools` lists `list-work` with `brain run list-work`.
4. **Approved hash.** `sqlite3 -readonly <run>/store/brain.db "SELECT id, name, contract_hash, approved_hash, approved FROM tools WHERE name = 'list-work';"` Write down `approved_hash`; below it is `<hash>`. It must equal `contract_hash`.
5. **Claude.** Start a fresh `claude` session in the workspace. Ask: "Run the shell command `brain run list-work` and show me its output." Allow the command. Write down the reply. Exit Claude.
6. **Codex.** Start a fresh `codex` session in the workspace. Ask the same: "Run the shell command `brain run list-work` and show me its output." Allow the command. Write down the reply. Exit Codex.
7. **The two rows.**
   ```
   sqlite3 -readonly <run>/store/brain.db "SELECT c.id, c.tool_id, c.origin, c.result, c.contract_hash, t.approved_hash, c.exit_code, c.at, s.harness, s.pane_id FROM tool_calls c JOIN tools t ON t.id = c.tool_id LEFT JOIN sessions s ON s.id = c.session_id WHERE t.name = 'list-work' ORDER BY c.id;"
   ```
   `s.harness` names the CLI when capture had an open session for the pane; it is empty when capture had none. Then the time order of steps 5 and 6 says which row is which.

Pass rule: both replies show the file list of `<run>/work` and no refusal, and the query shows two rows (one from the Claude session, one from the Codex session) with `origin` `agent`, `result` `ran`, exit code 0 and `contract_hash` equal to `<hash>`. A `refused` row, a third row from these sessions or another hash is a FAIL, with the reason written here. A skipped or blocked run is not a pass.

Result (fill in): **not run yet.**
- Run: date, `<run>` folder, herdr version, store binding (`brain-m4` → `<run>/store`), Claude and Codex versions and profile states.
- Tool: candidate id, proposal id, item id, `<hash>`.
- Claude: reply, its `tool_calls` row.
- Codex: reply, its `tool_calls` row.
- Verdict: PASS or FAIL, with the reason.

**Notes B: live herdr replies of E17-S11** (not a gate, no AC). E17-S11 parsed these replies from recorded spike data and tested them with a fake herdr; nothing ran against a live herdr. A person can check them in the same `brain-m4` session, after checklist A. Below, `<pane>` is the pane id of an agent pane of the workspace (`herdr --session brain-m4 pane list`).
1. **`agent get` and `agent list`.** Start `claude` in a pane, and leave it at rest. In the shell pane: `herdr --session brain-m4 agent get <pane>` and `herdr --session brain-m4 agent list`. Check that each agent object holds `pane_id`, `workspace_id`, `agent` (`claude`), `agent_status`, `agent_session.value`, `cwd`, `terminal_id` and `state_change_seq` (`internal/herdr/agent.go`). Ask Claude one short question; while it works, run `agent get` again: `agent_status` is `working` and `state_change_seq` is higher. Result: **not run yet.**
2. **`pane report-metadata` reply.** `herdr --session brain-m4 pane report-metadata <pane> --source brain --token brain-automation="list-work check"`, then `herdr --session brain-m4 pane report-metadata <pane> --source brain --clear-token brain-automation`. Each prints the `ok` reply that Brain reads; write the exact output. Result: **not run yet.**
3. **How herdr shows the run label.** Look at the pane in the herdr screen after the first command of note 2, before the clear: write down where and how herdr shows `list-work check`. For a live run label (optional): `brain automation add list-work --folder <run>/work --cli claude`, approve the proposal with `brain review <proposal id> --approve`, ask the Claude agent of that folder one short question, and let it stop. `brain automation list` shows the last result, and the pane shows the label while the run is `unknown` or after it `failed` (`ok` clears it). Then `brain automation remove <item>`. Result: **not run yet.**

After the run: copy the query output and the replies into the results above. Then stop the test session (`herdr session stop brain-m4`, never the main server). Keep `<run>` until the results are written.

**Review prompt** (§13 "stop or rethink", a note, not a gate): "Tools get no calls in 4 weeks." Look at it after 4 weeks of real use, with the call counts of `brain tools` (only `ran` rows count, §10.2, E17-S5). Brain makes no automatic decision from it.
