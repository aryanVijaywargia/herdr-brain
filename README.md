# Herdr Brain

```text
                _.-~~-._  _.-~~-._
        .-.  .-'  .-.   `'  .-.   `-.
   .--.( @ ).'  .-'   )  .-.(    `-.  `.    brain · a herdr plugin
  / >_ `-.'/   (   .-'  (   `-.    )   \    ──────────────────────
 (        (  .-.`-'  .-. `-.   )  .-'   ;   agents  claude code · codex
  `.       ) (  `-. (   `-. )-'  (   .-.|   memory  global · workspace
    `-.__.'   \  `-.  )`-.   ')  .-. `-(;   store   sqlite, local
               `-. `-'    `-.' .'   `-.'/   needs   herdr 0.9.1+
                  `-.____.-~-._  .-(((('
                     ||  ||    `'  ||  ||
                     ''  ''        ''  ''
```

A herdr plugin that gives every coding agent in herdr the same global and workspace memory.
The name is temporary.

Status: M1 to M4 are built. The repo is private, so there is no release binary yet. Design: [`docs/design/HLD.md`](docs/design/HLD.md). M0 spike results: [`spike/m0/FINDINGS.md`](spike/m0/FINDINGS.md).

## What Brain does

- You tell Brain a fact or a preference with `brain remember`. It stays a draft until you approve it in a review pane beside your session.
- Each new Claude Code or Codex session in herdr gets the approved items at start, through one hook. Global items go to every workspace. Workspace items go to one herdr workspace.
- You can pin, undo and forget an item. `brain status` and `brain doctor` show what works and what to fix.
- Built after M1: learning from your corrections and live suggestions (M2), skills and upkeep (M3), tools and automations (M4).

## Requirements

- macOS or Linux.
- herdr 0.9.1 or later.
- Claude Code or Codex.
- While the repo is private: access to the repo, and Go 1.23 or later (there is no release binary yet).

## Quick start

```sh
herdr plugin install aryanVijaywargia/herdr-brain
```

herdr shows what the plugin runs and asks "Install this plugin? [y/N]". Answer y.
The setup popup opens by itself. Read the plan and answer y.
If it does not open, run `herdr plugin action invoke brain.setup`.

Then, in a pane of a herdr workspace (setup offers a `brain` link on your `PATH`):

```sh
brain remember "use pnpm, not npm"   # a draft for this workspace; a review pane opens beside you
brain doctor                         # checks the setup and prints the fix of each failed check
```

After you approve the item, the next Claude Code or Codex session in that workspace gets it at start.

### Setup

`brain setup` shows all its changes in one plan and asks once. "No" writes nothing.
Run it again at any time: it changes nothing that is already right.

1. It checks the herdr version of the session, and stops if herdr is older than 0.9.1.
2. It explains where the excerpts of learning go, and asks which CLI runs the extractor: `claude`, `codex` or `none`.
   It asks this only once. `brain setup --extractor` asks again.
3. It shows the whole plan:
   - The binding of this herdr session (or the one that `--session <name>` names) to the main store `~/.local/share/brain/main`.
     `--session <name> --test <folder>` binds a test session to a test store instead.
     Setup refuses a test store that is, holds or sits inside the main store or another test store, also through links.
   - herdr does not put the plugin on your `PATH`. Setup shows the binary path and a `~/.local/bin/brain` link.
     It never replaces a file at that path, and it never edits your shell config.
   - For Claude Code, Codex and agy, when on `PATH`: the hook entries, and how to remove them.
   - The extractor choice.
4. It asks once: "Apply all of this? [y/N]". "y" writes every step. Any other answer writes nothing.
   A step that is already right shows as "already done". With nothing to write, setup asks nothing.
5. Codex runs a new hook only after you trust it. Setup prints the steps: start `codex`, open `/hooks`, and press `t`.
6. It ends with a short first-use guide.

herdr 0.9.1 gives a session no lasting identity. If you rename or recreate a session, run `brain setup` in it again.

### Without Go

When the checkout is clean and `HEAD` is the commit of tag `v<manifest version>`, `scripts/build.sh` downloads the release asset `brain_<os>_<arch>`.
It checks its SHA-256 against `checksums.txt`. If the asset cannot be used, it builds from source with Go.
Any other ref builds from source and needs Go 1.23 or later.
The asset path works only when the repo is public.

## Commands

| Command | What it does |
|---|---|
| `brain version` | Print the Brain version |
| `brain status` | Show Brain status |
| `brain doctor [--fix]` | Check the Brain setup and print the fix of each failed check; `--fix` applies each offered repair with no question |
| `brain setup [--session <name> [--test <folder>]] [--extractor]` | Bind a herdr session, add the agent hooks, choose the extractor CLI |
| `brain remember "<text>" [--global \| --workspace <w>] [--kind <k>]` | Draft an item for your review |
| `brain review [<id> [--approve \| --reject \| --later]]` | The queue of proposals (`brain review --help`) |
| `brain pin [--off] <item>` | Pin an active item, so it never ages |
| `brain undo <item>` | Undo the latest revision of an item |
| `brain forget <item>` | Remove the text of an item from the store and the output |
| `brain repair [--confirm]` | Repair the output and list its conflicts; `--confirm` deletes them |
| `brain uninstall [--apply] [--data]` | List what uninstall removes; `--apply` removes it, `--data` the stores too |
| `brain update [--check]` | Update the plugin to the newest commit of its repo, then run `brain doctor --fix`; `--check` only compares |

herdr and the agent CLIs run these. You do not run them by hand:

| Command | What it does |
|---|---|
| `brain hook --profile <name> --event start` | The agent start hook (prints the brief or nothing) |
| `brain pane review --proposal <id>` | The review pane of one proposal |
| `brain event workspace-closed` | herdr's `workspace.closed` event (D4); marks the workspace closed |

In herdr, the actions "Brain: setup", "Brain: status" and "Brain: review" run `brain setup`, `brain status` and `brain review`.
They work without `brain` on your `PATH`.

## How it works

- `brain` is one Go binary. It runs for a command, an agent hook or a herdr event.
  One `brain brained` process runs for each herdr session (D7). herdr starts it, and the install starts it too.
- SQLite is the only source of truth: one `brain.db` in each store. The main store is `~/.local/share/brain/main`.
- When an item changes, Brain builds the brief again from SQLite: one global part and one part for each workspace.
- At session start, Claude Code and Codex run `brain hook`. The hook asks herdr to confirm the live pane and its workspace.
  Then it prints the global part and the part of that workspace.
- Outside herdr, or in a herdr session that `brain setup` did not bind, the hook prints nothing.
  If a step fails, the hook prints nothing and the agent starts as normal.
- Workspace items follow the herdr workspace, not the repo. Two workspaces that use the same checkout keep their items apart.

## Your data stays local

- All Brain data stays on your machine, in `~/.local/share/brain`.
- An agent that gets items at start can send them to its model provider, like any other text in its context.
- Brain changes a CLI config file only to add or remove its hook entries, and only after you say yes.
- Brain writes no CLI rules file and no skill folder. It does not read or write Claude or Codex memory.
- `brain forget <item>` removes the text of an item from the store and the output.
  It cannot remove the text from backups or from sessions that already got it.
- Learning sends short excerpts (at most 500 characters, after a secret filter) to the extractor CLI that you choose in setup.
  Extraction is on after setup. `brain extract off [--workspace <id>]` turns it off for one workspace or for all.

## Update

Run these in a pane of a herdr session:

```sh
brain update --check   # compares the installed commit with the newest commit of the repo
brain update           # installs the newest commit, then runs brain doctor --fix
```

herdr has no plugin update, so `brain update` runs `herdr plugin install` again. The running `brained` switches to the new binary by itself.
A plugin that you installed with `--ref` does not follow the default branch: `brain update` prints the install command instead.

## Problems

Run these in a pane of a herdr session:

```sh
brain doctor         # checks the setup and prints the fix of each failed check
brain doctor --fix   # applies each repair that brain doctor offers, with no question
```

## Uninstall

Run the two steps in this order:

```sh
brain uninstall            # dry run: lists what would change
brain uninstall --apply    # removes it; add --data to remove your stores too
herdr plugin uninstall brain
```

`brain uninstall --apply` removes the Brain hook entries from `~/.claude/settings.json`, `~/.codex/hooks.json` and `~/.gemini/config/hooks.json`, the install record, the `~/.local/bin/brain` link and the session bindings file.
It keeps every other byte of the hook files. It keeps a hook entry that you edited, and its install record, and tells you to remove the entry by hand.
Without `--data`, your stores stay in `~/.local/share/brain`.
With `--data`, it also removes `~/.local/share/brain`, `~/.config/herdr/plugins/config/brain` and `~/.local/state/herdr/plugins/brain`.

If you ran `herdr plugin uninstall brain` first, the hook entries point to a missing binary. To fix this, do one of these:

- Install the plugin again (it gets the same path), run `brain uninstall --apply`, then uninstall the plugin.
- Or edit the files by hand. In `~/.claude/settings.json` and `~/.codex/hooks.json`, remove each hook group whose command contains `hook --profile claude --event` or `hook --profile codex --event`. In `~/.gemini/config/hooks.json`, remove the `brain` key. Then remove `~/.local/share/brain/install.json`, `~/.local/share/brain/bindings.json` and the `~/.local/bin/brain` link.

## Develop

```sh
go test ./...
sh scripts/build.sh          # builds bin/brain
herdr plugin link .          # link never runs the build step; build first
herdr plugin unlink brain
```


## Release

1. Set `version` in `herdr-plugin.toml` and merge.
2. Push tag `v<version>` on that commit. `release.yml` fails when the tag does not match the manifest.

## Layout

| Path | What |
|---|---|
| `herdr-plugin.toml` | Plugin manifest: build step, actions, panes and events |
| `cmd/brain/` | The one `brain` binary |
| `internal/paths/` | Binary path and data home for every entry point |
| `internal/` | The other packages, for example `store` (SQLite), `output` (the compiler), `hook`, `cli` and `herdr` |
| `adapters/` | The adapter profiles of Claude Code and Codex |
| `scripts/build.sh` | Build step that `herdr plugin install` runs |
| `scripts/check-tag.sh` | Release gate: the tag must be `v<manifest version>` |
| `.github/workflows/` | `ci.yml` (tests), `release.yml` (assets on tag `v*`) |
| `docs/design/` | HLD, decisions and owner questions |
| `spike/m0/` | M0 spike code and findings |

