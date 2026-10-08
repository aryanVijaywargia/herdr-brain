# Herdr Brain

One memory for every coding agent in herdr.

<p align="center"><a href="assets/herdr-brain-launch.mp4"><img src="assets/herdr-brain-launch.webp" width="640" alt="Animation in a herdr window. Lines typed in a Claude Code pane and a Codex pane become fragments, and a password turns into [secret]. The same correction, use pnpm, not npm, comes from both sessions and fuses into one lesson. You approve it in Brain's review pane. New Claude Code and Codex sessions get the lesson in their start brief. Brain suggests a tool for a repeated prompt. The pieces settle into a small memory graph, then the install command: herdr plugin install aryanVijaywargia/herdr-brain" /></a></p>

> [!NOTE]
> Are you an agent that sets up Brain for your user? Read [`SETUP.md`](SETUP.md). It has the exact steps.

You tell Claude Code "from now on, use pnpm, not npm". Then you open Codex in the same project, and it uses npm. Each agent keeps its own memory, and each new session starts blank.

Brain is a herdr plugin that fixes this. It keeps one store of facts, preferences and lessons, and it gives them to every new Claude Code and Codex session in herdr when the session starts. You approve each item before any agent sees it. Brain keeps its data on your machine. If you choose an extractor CLI in setup, Brain sends short, filtered excerpts of your sessions to that CLI's provider. [Your data stays local](#your-data-stays-local) has the details.

The name is temporary.

Status: M1 to M4 are built. The repo is private, so there is no release binary yet. The design is in [`docs/design/HLD.md`](docs/design/HLD.md), and the M0 spike results are in [`spike/m0/FINDINGS.md`](spike/m0/FINDINGS.md).

## What you get

- **You say it once.** Tell Brain a fact or a preference in plain words in your agent session, for example "remember ..." or "from now on ...". Brain finds it when the session ends or goes idle. You can also run `brain remember`.
- **You decide what sticks.** Each item stays a draft until you approve it in a review pane beside your session.
- **Every new session knows it.** Each new Claude Code or Codex session in herdr gets the approved items at start, through one hook. Global items go to every workspace. Workspace items go to one herdr workspace.
- **You stay in control.** You can pin, undo and forget an item. `brain status` and `brain doctor` show what works and what to fix.
- **It learns more over time.** After M1 came learning from your corrections and live suggestions (M2), skills and upkeep (M3), and tools and automations (M4).

## Requirements

- macOS or Linux.
- herdr 0.9.1 or later.
- Claude Code or Codex.
- While the repo is private, you need access to the repo and Go 1.23 or later. There is no release binary yet.

## Quick start

Install the plugin:

```sh
herdr plugin install aryanVijaywargia/herdr-brain
```

herdr shows what the plugin runs and asks "Install this plugin? [y/N]". Answer y.
The setup popup opens by itself. Read the plan and answer y.
If it does not open, run `herdr plugin action invoke brain.setup`.

Then try it in a pane of a herdr workspace:

```sh
# In your agent session, say "from now on, use pnpm, not npm". Or run:
brain remember "use pnpm, not npm"   # a draft for this workspace; a review pane opens beside you
brain doctor                         # checks the setup and prints the fix of each failed check
```

After you approve the item, the next Claude Code or Codex session in that workspace gets it at start.

Setup offers a `~/.local/bin/brain` link. If that folder is not on your `PATH`, setup prints the binary path to use instead.

## What setup does

`brain setup` shows all its changes in one plan and asks once. "No" writes nothing.
You can run it again at any time. It changes nothing that is already right.

1. It checks the herdr version of the session, and stops if herdr is older than 0.9.1.
2. It explains where the excerpts of learning go, and asks which CLI runs the extractor: `claude`, `codex` or `none`.
   For `claude` or `codex` it also asks for a model. Leave it empty for the default model of that CLI.
   It asks this only once. `brain setup --extractor` asks again.
3. It shows the whole plan:
   - The binding of this herdr session, or of the one that `--session <name>` names, to the main store `~/.local/share/brain/main`.
     `--session <name> --test <folder>` binds a test session to a test store instead.
     Setup refuses a test store that is, holds or sits inside the main store or another test store, also through links.
   - herdr does not put the plugin on your `PATH`. Setup shows the binary path and a `~/.local/bin/brain` link.
     It never replaces a file at that path, and it never edits your shell config.
   - For Claude Code, Codex and agy, when they are on `PATH`, the hook entries and how to remove them.
   - The extractor choice.
4. It asks once: "Apply all of this? [y/N]". "y" writes every step. Any other answer writes nothing.
   A step that is already right shows as "already done". With nothing to write, setup asks nothing.
5. Codex runs a new hook only after you trust it. Setup prints the steps: start `codex`, open `/hooks`, and press `t`.
6. It ends with a short first-use guide.

herdr 0.9.1 gives a session no lasting identity. If you rename or recreate a session, run `brain setup` in it again.

### Without Go

When the checkout is clean and `HEAD` is the commit of tag `v<manifest version>`, `scripts/build.sh` downloads the release asset `brain_<os>_<arch>`.
It checks its SHA-256 against `checksums.txt`. If it cannot use the asset, it builds from source with Go.
Any other ref builds from source and needs Go 1.23 or later.
The asset path works only when the repo is public.

## Commands

| Command | What it does |
|---|---|
| `brain version` | Print the Brain version |
| `brain status` | Show Brain status |
| `brain doctor [--fix]` | Check the Brain setup and print the fix of each failed check; `--fix` applies each offered repair with no question |
| `brain setup [--session <name> [--test <folder>]] [--extractor]` | Bind a herdr session, add the agent hooks, choose the extractor CLI |
| `brain remember "<text>" [--global \| --workspace <w>] [--kind <k>]` | Draft an item for your review; `<k>` is `fact`, `preference`, `decision` or `handoff` |
| `brain recall <words>` | Print the approved items of this pane's brief that match the words, best match first |
| `brain review [<id> [--approve \| --reject \| --later \| --change "<text>"]]` | The queue of proposals (`brain review --help`) |
| `brain pin [--off] <item>` | Pin an active item, so it never ages |
| `brain archive [--all \| --keep]` | List the items with no relevant session for 90 days; `--all` retires them, `--keep` keeps them |
| `brain promote <item>` | Propose to move a workspace item to the global part of the brief |
| `brain undo <item>` | Undo the latest revision of an item |
| `brain workspace link <old> <new> [--confirm]` | Move the items of an old workspace key to this workspace |
| `brain forget <item> \| --session <id>` | Remove the text of an item from the store and the output, or the capture evidence of a session |
| `brain repair [--confirm]` | Repair the output and list its conflicts; `--confirm` deletes them |
| `brain learn on\|off [--workspace <id> \| --pane <id>]` | Record the learning switch; delivery stays on |
| `brain extract on\|off [--workspace <id>]` | Record the extraction switch; capture and rules keep running |
| `brain tools` | List the approved tools that this workspace can run, with their inputs |
| `brain run <tool> [--<input> <value>]...` | Run an approved tool in this workspace; exits with its exit code |
| `brain automation add <tool> --folder <dir> --cli <cli> \| list \| pause <item> \| resume <item> \| remove <item>` | Run an approved tool after each stop of the agent (`brain automation --help`) |
| `brain uninstall [--apply] [--data]` | List what uninstall removes; `--apply` removes it, `--data` the stores too |
| `brain update [--check \| --records]` | Update the plugin to the newest commit of its repo, add the missing hook record entries (one question), then run `brain doctor --fix`; `--check` only compares; `--records` only adds the hook record entries |

herdr and the agent CLIs run these. You do not run them by hand:

| Command | What it does |
|---|---|
| `brain hook --profile <name> --event start\|prompt\|tool\|stop` | The agent hooks; `start` prints the brief or nothing |
| `brain pane review --proposal <id> \| recall` | The review pane of one proposal, or the recall popup |
| `brain event workspace-closed` | herdr's `workspace.closed` event (D4); marks the workspace closed |
| `brain brained` | The herdr listener of one herdr session (D7) |
| `brain start` | Start `brained` for this herdr session, and open setup when the session has no binding |
| `brain suggest` | The live suggestion of one prompt; the hook or `brained` starts it |

In herdr, the actions "Brain: setup", "Brain: status", "Brain: review", "Brain: recall", "Brain: retired items" and "Brain: start" open the setup popup, show the status, open the queue of proposals, open the recall popup, list the retired items and start `brained`.
They work without `brain` on your `PATH`.

## How it works

- `brain` is one Go binary. It runs for a command, an agent hook or a herdr event.
  One `brain brained` process runs for each herdr session (D7). herdr starts it, and the install starts it too.
- SQLite is the only source of truth. Each store has one `brain.db`. The main store is `~/.local/share/brain/main`.
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
- Learning sends short excerpts to the extractor CLI that you choose in setup. Each excerpt is at most 500 characters and passes a secret filter first.
  For a shell command that could become a tool, it sends the whole command record, at most 4 KB.
  Extraction is on after setup. `brain extract off [--workspace <id>]` turns it off for one workspace or for all.

## Update

Run these in a pane of a herdr session:

```sh
brain update --check   # compares the installed commit with the newest commit of the repo
brain update           # installs the newest commit, adds the missing hook record entries, then runs brain doctor --fix
```

A new Brain can store more hook records: the prompts, commands and replies of Claude and Codex. `brain update` shows the hook record entries that are missing and asks once, as `brain setup` does. Answer `y` to add them. Codex needs trust again for each new entry. Start codex, type `/hooks` and press `t` on each. `brain status` shows `hook records on` or `off` for each CLI, and `brain doctor` names each missing or untrusted entry.

herdr has no plugin update, so `brain update` runs `herdr plugin install` again. The running `brained` switches to the new binary by itself.
A plugin that you installed with `--ref` does not follow the default branch. For it, `brain update` prints the install command instead.

## Problems

Run these in a pane of a herdr session:

```sh
brain doctor         # checks the setup and prints the fix of each failed check
brain doctor --fix   # applies each repair that brain doctor offers, with no question
```

[`SETUP.md`](SETUP.md) lists the common failures and their fixes.

## Uninstall

Run the steps in this order:

```sh
brain uninstall            # dry run: lists what would change
brain uninstall --apply    # removes it; add --data to remove your stores too
herdr plugin uninstall brain
```

`brain uninstall --apply` removes the Brain hook entries from `~/.claude/settings.json`, `~/.codex/hooks.json` and `~/.gemini/config/hooks.json`, the install record, the `~/.local/bin/brain` link and the session bindings file.
It keeps every other byte of the hook files. If you edited a hook entry, it keeps that entry and its install record, and tells you to remove the entry by hand.
Without `--data`, your stores stay in `~/.local/share/brain`.
With `--data`, it also removes `~/.local/share/brain`, `~/.config/herdr/plugins/config/brain` and `~/.local/state/herdr/plugins/brain`.

If you ran `herdr plugin uninstall brain` first, the hook entries point to a missing binary. To fix this, do one of these:

- Install the plugin again, which gets the same path. Run `brain uninstall --apply`, then uninstall the plugin.
- Or edit the files by hand. In `~/.claude/settings.json` and `~/.codex/hooks.json`, remove each hook group whose command contains `hook --profile claude --event` or `hook --profile codex --event`. In `~/.gemini/config/hooks.json`, remove the `brain` key. Then remove `~/.local/share/brain/install.json`, `~/.local/share/brain/bindings.json` and the `~/.local/bin/brain` link.

## Contributing

Build, test, release steps and the repo layout are in [`CONTRIBUTING.md`](CONTRIBUTING.md).
