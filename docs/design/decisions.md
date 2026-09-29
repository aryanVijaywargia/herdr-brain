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
