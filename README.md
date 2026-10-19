# Herdr Brain

One memory for every coding agent in herdr.

[![ci](https://img.shields.io/github/actions/workflow/status/aryanVijaywargia/herdr-brain/ci.yml?branch=main&label=ci)](https://github.com/aryanVijaywargia/herdr-brain/actions/workflows/ci.yml) [![release](https://img.shields.io/github/v/release/aryanVijaywargia/herdr-brain)](https://github.com/aryanVijaywargia/herdr-brain/releases) [![herdr](https://img.shields.io/badge/herdr-0.9.1%2B-blue)](https://herdr.dev) [![platforms](https://img.shields.io/badge/platforms-macOS%20%7C%20Linux-lightgrey)](#install) [![license](https://img.shields.io/github/license/aryanVijaywargia/herdr-brain)](LICENSE)

[Why Brain](#why-brain) • [How it works](#how-it-works) • [Install](#install) • [Try it](#try-it) • [Commands](#everyday-commands) • [Privacy](#privacy)

<p align="center"><a href="https://github.com/aryanVijaywargia/herdr-brain/blob/e1719421d5039752a459082d706f6d2f8ec179b8/assets/herdr-brain-launch.mp4"><img src="https://github.com/aryanVijaywargia/herdr-brain/raw/e1719421d5039752a459082d706f6d2f8ec179b8/assets/herdr-brain-launch.webp" width="480" alt="Animation in a herdr window. Lines typed in a Claude Code pane and a Codex pane become fragments, and a password turns into [secret]. The same correction, no, never edit an applied migration, add a new one, comes from both sessions and fuses into one lesson. You approve it in Brain's review pane. New Claude Code and Codex sessions get the lesson in their start brief. Brain suggests a tool, migrate-check, for a repeated prompt. The pieces settle into a small memory graph, then the install command: herdr plugin install aryanVijaywargia/herdr-brain" /></a></p>

> [!NOTE]
> Are you an agent that sets up Brain for your user? Read [`SETUP.md`](SETUP.md). It has the exact steps.

Herdr Brain is a [herdr](https://herdr.dev) plugin that gives Claude Code and Codex one shared memory. It notices what you keep telling your agents, drafts it as a short item, and asks you to approve it. After that, every new session in herdr starts out knowing it. Brain stores its data on your machine, and the [privacy](#privacy) section explains what can leave it.

## Why Brain

If you use more than one coding agent, or even one agent across many sessions, you've probably hit these:

- **Each agent keeps its own memory.** You tell Claude Code never to edit an applied migration, then you open Codex in the same project and it edits one again. Brain keeps one store for both, so a rule you give one agent reaches the other.
- **Every new session starts blank.** So you repeat the same rules and the same corrections every morning. Brain hands the approved items to each new session when it starts, and you say a thing once.
- **You retype the same long prompt.** Brain notices a prompt that you keep sending, or a procedure that you keep explaining, and drafts a skill or a tool from it. Say you keep asking your agents to "reset the test database, run all migrations, then run the integration tests". Brain drafts a tool such as `migrate-check`, and the next time you send that prompt, the draft opens beside you, ready to approve. After that, agents run it with `brain run migrate-check`.
- **Memory files fill up with near-duplicates.** Over a few weeks you end up with three ways of saying the same rule. Brain proposes to merge items that mean the same thing, and it flags an item when a file it mentions is moved or deleted.
- **You can't see what your agents were told to remember.** With Brain, nothing becomes memory until you approve it. `brain status` shows what Brain holds and what waits for you, `brain recall` searches it, and every item stays in a local SQLite file that you own.

## How it works

1. **You work as usual.** You talk to Claude Code or Codex in a herdr pane, the way you always do.
2. **Brain notices what matters.** When a session finishes or goes idle, Brain looks at what happened. A plain rule such as "from now on, never edit an applied migration" or "always run the linter before you commit" becomes a draft. So does a correction that you make more than once.
3. **You decide.** Each draft waits for your review. You can approve it, edit it first, ask for a better draft, or reject it. A rejected idea stays rejected until you reopen it.
4. **Every new session knows it.** When a Claude Code or Codex session starts in herdr, a small hook gives it the approved items. Codex gets them with your first prompt.

Items live at one of two levels, because not every rule fits every project. A **workspace** item belongs to one herdr workspace. "Never edit an applied migration; add a new one" is a good example, since this project has a database but the next one may not. Drafts start at this level. A **global** item goes to every workspace, which suits a rule like "keep commit messages under 72 characters". You choose that level yourself, when you approve a draft or later with `brain promote`.

You don't have to wait for Brain to notice, either. Run `brain remember "never edit an applied migration; add a new one"`, or ask your agent to remember something, and a review pane opens beside your session so you can approve it on the spot.

Two more features round it out:

- **Automations.** An automation runs one approved tool in one folder each time the agent stops working. For example, `migrate-check` can run after every turn, so a broken migration shows up right after the turn that made it. When the same approved tool keeps running in the same folder right after the agent stops, in 3 sessions, Brain proposes the automation. You can also add one yourself with `brain automation add migrate-check --folder <dir> --cli claude`. Either way, you approve the exact tool and folder first.
- **Upkeep.** Items that no session has needed for 90 days show up as candidates to archive. You can pin an item so it never ages, undo the last change to an item, or forget it completely.

## Install

You need:

- macOS or Linux
- herdr 0.9.1 or later
- Claude Code, Codex, or both
- Go 1.23 or later, unless you install the release tag

Install the plugin from herdr:

```sh
herdr plugin install aryanVijaywargia/herdr-brain
```

herdr shows you what the plugin will run and asks for a yes. Then Brain's setup popup opens by itself. Setup collects every change into one plan and asks you once, so a "no" leaves your machine as it was. Along the way it asks two questions.

The first is which CLI Brain may use to learn from your sessions, `claude`, `codex` or `none`. If you pick one, Brain calls it in the background with short excerpts of your finished sessions, and those excerpts go to that CLI's model provider. Your agents already send your sessions there the same way. With `none`, Brain makes no such call, so it learns much less by itself, but `brain remember` still works.

The second is whether Brain may read your past Claude Code and Codex sessions once. That lets it find your repeated corrections on day one instead of after weeks. Say no and setup asks again next time.

> [!IMPORTANT]
> Codex runs a new hook only after you trust it, and only you can do that. Start `codex`, type `/hooks`, and press `t` on the Brain hook.

Setup also offers a `~/.local/bin/brain` link, so you can type `brain` in any shell. If `~/.local/bin` isn't on your `PATH`, setup prints the full path to use.

> [!TIP]
> No Go? Install the release tag with `herdr plugin install aryanVijaywargia/herdr-brain --ref v0.1.1`. Brain then uses the prebuilt binary of the release and checks its checksum.

## Try it

Open a pane in a herdr workspace and give Brain its first item:

```sh
brain remember "never edit an applied migration; add a new one"
```

A review pane opens beside you with the draft. Approve it there. Now start a new Claude Code or Codex session in the same workspace and ask it to add a nickname column to the users table. It writes a new migration and leaves the old ones alone.

If something doesn't work, run `brain doctor`. It checks the setup and prints a fix for each check that fails.

## Everyday commands

| Command | What it does |
|---|---|
| `brain status` | Shows what Brain holds, what waits for review, and whether extraction is on |
| `brain doctor` | Checks the setup and tells you how to fix each problem. `--fix` repairs it for you |
| `brain remember "<text>"` | Saves a draft for you to approve. Add `--global` for every workspace |
| `brain review` | Lists the drafts that wait for you |
| `brain recall <words>` | Searches your approved items |
| `brain pin <item>` | Keeps an item from ever aging out |
| `brain undo <item>` | Rolls back the last change to an item |
| `brain forget <item>` | Removes an item's text from Brain |
| `brain extract off` | Stops the calls to the learning CLI |
| `brain update` | Updates Brain to the newest version |

[`docs/commands.md`](docs/commands.md) lists every command and flag. Some of them are also herdr actions, such as "Brain: review" and "Brain: recall", so you can use them without a shell.

## Privacy

**Where is my data?** On your machine, in `~/.local/share/brain`. Brain has no cloud service and no account.

**What leaves my machine?** Two things. If you chose a learning CLI in setup, Brain sends it short excerpts of your sessions, each at most 500 characters and run through a filter that replaces secrets such as passwords and tokens. A command that could become a tool goes whole, up to 4 KB. That CLI sends the excerpts to its model provider, or to the proxy or gateway that you set for it, such as `ANTHROPIC_BASE_URL`. It signs in as it does in a herdr pane, with its own login or with an API key, token or proxy in the env of the herdr server. Brain passes that env on and does not read it. When a call fails, Brain keeps the error text of the CLI after a filter that hides long tokens, URLs, keys and their encoded forms; a short or all-letter value may not be hidden. Also, the approved items that a session gets at start are part of that agent's context, so the agent can send them to its own provider like any other text it reads.

**What does Brain change on my machine?** Outside its own folders, it touches the hook files of your agent CLIs, such as `~/.claude/settings.json` and `~/.codex/hooks.json`, and the optional `~/.local/bin/brain` link. It adds its hook entries only after you say yes, and it removes only its own entries.

**Can Brain forget something?** Yes. `brain forget <item>` removes the item's text from Brain. It can't reach copies that already left Brain, though, such as your backups and sessions that already got the item.

**How do I turn learning off?** `brain extract off` stops the excerpts for all workspaces, or for one with `--workspace <id>`. `brain learn off` stops learning altogether, and your approved items keep reaching new sessions.

## Update

```sh
brain update --check   # is there a newer version?
brain update           # install it, then check the setup
```

What `brain update` follows depends on how you installed Brain:

- **No `--ref`:** the newest commit of `main`.
- **A release tag, such as `--ref v0.1.1`:** the newest release tag. You keep the prebuilt binary, so you still don't need Go.
- **Any other `--ref`, such as a branch or a commit:** nothing. Brain stays on that ref, and `brain update` prints the command to leave it.
- **A `herdr plugin link` checkout:** the newest commit of `main`. Brain pulls and builds it, but only on `main` with no local changes.

## Troubleshooting

```sh
brain doctor          # find what's wrong
brain doctor --fix    # repair what it can
```

If that doesn't sort it out, [`SETUP.md`](SETUP.md#common-failures) lists the common failures and their fixes.

## Uninstall

Run these in order, because the first two need the plugin's binary:

```sh
brain uninstall               # a dry run that shows what would be removed
brain uninstall --apply       # removes the hooks and the link; add --data to delete your memory too
herdr plugin uninstall brain
```

Already removed the plugin first? The [uninstall section of `SETUP.md`](SETUP.md#uninstall) shows how to clean up by hand.

## Learn more

- [`SETUP.md`](SETUP.md) has the step-by-step setup, written for an agent, and the common failures.
- [`docs/commands.md`](docs/commands.md) lists every command and flag.
- [`CONTRIBUTING.md`](CONTRIBUTING.md) covers building, testing and releasing Brain.

## License

Brain is free and open source under the [MIT license](LICENSE). Each release also carries `THIRD_PARTY_LICENSES.txt`, with the licenses of the Go modules inside the `brain` binary.
