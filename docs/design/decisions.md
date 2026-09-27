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
