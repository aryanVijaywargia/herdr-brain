# Herdr Brain

One memory for every coding agent in herdr.

<p align="center"><a href="assets/herdr-brain-launch.mp4"><img src="assets/herdr-brain-launch.webp" width="640" alt="Animation in a herdr window. Lines typed in a Claude Code pane and a Codex pane become fragments, and a password turns into [secret]. The same correction, use pnpm, not npm, comes from both sessions and fuses into one lesson. You approve it in Brain's review pane. New Claude Code and Codex sessions get the lesson in their start brief. Brain suggests a tool for a repeated prompt. The pieces settle into a small memory graph, then the install command: herdr plugin install aryanVijaywargia/herdr-brain" /></a></p>

> [!NOTE]
> Are you an agent that sets up Brain for your user? Read [`SETUP.md`](SETUP.md). It has the exact steps.

You tell Claude Code "use pnpm, not npm", and then Codex in the same project still uses npm. Brain is a herdr plugin that keeps one store of facts, preferences and lessons for Claude Code and Codex. You approve each item, and each new session in herdr gets the approved items at start. Your data stays on your machine.

## What you get

- **You say it once.** Say "remember ..." or "from now on ..." in your agent session, or run `brain remember`.
- **You decide what sticks.** Each item stays a draft until you approve it in a review pane. You can pin, undo or forget it later.
- **Every new session knows it.** Global items go to every workspace; workspace items go to one herdr workspace.
- **It learns over time.** Brain learns from your corrections and proposes skills and tools. A tool can run as an automation.

## Requirements

- macOS or Linux.
- herdr 0.9.1 or later.
- Claude Code or Codex.
- Go 1.23 or later. You do not need Go when you install the release tag with `--ref v0.1.0`.

## Quick start

```sh
herdr plugin install aryanVijaywargia/herdr-brain
```
Answer y. The setup popup opens, shows one plan and asks once. Codex needs you to trust the Brain hook once in `/hooks`.
Then say "from now on, use pnpm, not npm" to your agent, or run this in a pane of a herdr workspace:
```sh
brain remember "use pnpm, not npm"   # a draft for this workspace; a review pane opens beside you
```
Setup offers a `~/.local/bin/brain` link. If that folder is not on your `PATH`, setup prints the binary path to use.

## Everyday commands

| Command | What it does |
|---|---|
| `brain status` | Show Brain status |
| `brain doctor [--fix]` | Check the setup and fix it |
| `brain remember "<text>"` | Draft an item for your review |
| `brain review` | The queue of proposals |
| `brain recall <words>` | Find approved items |
| `brain pin <item>` | Keep an item from aging |
| `brain undo <item>` | Undo the latest change of an item |
| `brain forget <item>` | Remove the text of an item |
| `brain update` | Update Brain |

[`docs/commands.md`](docs/commands.md) has every command and flag.

## Your data stays local

- All Brain data stays on your machine, in `~/.local/share/brain`.
- Learning sends short excerpts (at most 500 characters, after a secret filter) to the extractor CLI that you choose in setup. `brain extract off` turns this off.
- `brain forget` cannot remove text from backups or from sessions that already got it.

## Update

```sh
brain update --check   # compare the installed commit with the newest one
brain update           # install the newest commit, then run brain doctor --fix
```
A plugin that you installed with `--ref` does not follow the default branch. For it, `brain update` prints the install command.

## Problems

```sh
brain doctor --fix   # checks the setup and applies each offered repair
```
[`SETUP.md`](SETUP.md#common-failures) lists the common failures and their fixes.

## Uninstall

```sh
brain uninstall            # dry run: lists what would change
brain uninstall --apply    # removes it; add --data to remove your stores too
herdr plugin uninstall brain
```
Run the steps in this order. If you uninstalled the plugin first, see the [Uninstall part of `SETUP.md`](SETUP.md#uninstall).

## Contributing

See [`CONTRIBUTING.md`](CONTRIBUTING.md) for build, test and release steps, and [`docs/design/HLD.md`](docs/design/HLD.md) for the design.

## License

MIT. See [`LICENSE`](LICENSE).
