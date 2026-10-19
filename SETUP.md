# Set up Herdr Brain: steps for an agent

This file is for a coding agent that installs and sets up Brain in herdr for its user.
People can read [`README.md`](README.md) instead.

Each step gives the command, the result to expect, and what to do when it fails.
Run every command in a shell inside a pane of the herdr session that the user wants Brain in.
`brain status`, `brain doctor` and `brain remember` need that pane. Outside herdr they print "not in a herdr pane".

Two steps ask the user. Do not answer for them:

- The plugin install asks "Install this plugin? [y/N]".
- `brain setup` asks which CLI runs the extractor, and whether Brain may read the past sessions once. Then it asks once "Apply all of this? [y/N]" for the whole plan.

Ask your user the same questions, show them what setup will change, and answer only with their answer.

## 1. Check the requirements

| Check | Command | Expected |
|---|---|---|
| OS | `uname -s` | `Darwin` or `Linux`. Brain has no Windows support. |
| herdr | `herdr --version` | `herdr 0.9.1` or later |
| An agent CLI | `command -v claude codex` | At least one path. Brain works with Claude Code and Codex. |
| Go, except for the release tag | `go version` | `go1.23` or later |

If a check fails:

- Wrong OS: stop and tell the user that Brain runs on macOS and Linux only.
- herdr is missing or older than 0.9.1: tell the user to install or update herdr. Brain setup stops on an older herdr.
- No `claude` and no `codex`: Brain installs, but no session gets items. Tell the user.
- No Go 1.23 or later: install the release tag in step 2 with `--ref v0.1.0`. It uses the release binary. Any other ref needs Go.

## 2. Install the plugin

Tell the user what the install does: herdr clones the repo, runs the build step `sh scripts/build.sh`, which builds `bin/brain`, and then Brain starts its `brained` process for each running herdr session.
Ask: "Install this plugin? [y/N]". Go on only with a yes.

In a terminal that the user can see, run:

```sh
herdr plugin install aryanVijaywargia/herdr-brain
```

herdr shows what the plugin runs and asks "Install this plugin? [y/N]". The user answers y.
If your shell cannot take input, run it with the user's yes already given:

```sh
herdr plugin install --yes aryanVijaywargia/herdr-brain
```

Expected:

- The build prints `brain build: bin/brain <version> (built from source)`, or `(release asset <asset>, checksum OK)` for the release tag.
- `herdr plugin list --plugin brain` lists the plugin.
- Within about a minute, the setup popup "Brain setup" opens in each running herdr session that has no Brain binding.

If it fails:

- `brain build: this ref needs Go 1.23 or later`: install Go 1.23 or later, then install again.
- `brain build: go build failed`: show the user the build output.
- The clone fails: check the network, then install again.

## 3. Run setup

Setup binds this herdr session to the main store `~/.local/share/brain/main`, adds the hook entries for Claude Code, Codex and agy when they are on `PATH`, offers the `~/.local/bin/brain` link, and records the extractor choice.
It shows all of this in one plan and asks once. "No" writes nothing.

There are two ways to run it. Use one.

**A. The user answers in the popup.** The popup opens by itself after the install. If it did not open, run:

```sh
herdr plugin action invoke brain.setup
```

The user reads the plan and answers in the popup.

**B. You run setup and relay the questions.** `brain` is not on `PATH` before setup.
The binary of a GitHub install is at `~/.config/herdr/plugins/github/brain-bbbf7a6412d6/bin/brain`.

```sh
BRAIN=~/.config/herdr/plugins/github/brain-bbbf7a6412d6/bin/brain
"$BRAIN" setup < /dev/null
```

With no input, setup takes no extractor and answers no, so it writes nothing.
It prints the steps "herdr version", "extractor CLI" and "past sessions", then the plan under the step "plan".
Each step is a title line, such as "Step 2: extractor CLI", and short bullets.

Now ask your user:

1. Which CLI runs the extractor: `claude`, `codex` or `none`? Show them the bullets of the "extractor CLI" step. They say where the excerpts go.
   Skip this question when the plan says "Extractor: ... Already done".
2. For `claude` or `codex`: which model? Empty means the default model of that CLI.
3. May Brain read the past Claude Code and Codex sessions once? Show them the bullets of the "past sessions" step.
   Skip this question when the plan shows a "History scan:" state line other than "History scan: not now".
4. Show them the plan, with the extractor line that matches their choice. Ask: "Apply all of this? [y/N]".

Then run setup again and give each answer on its own line, in the order setup asks:

```sh
printf '%s\n' "<cli>" "<model>" "<scan yes or no>" "<apply yes or no>" | "$BRAIN" setup
```

Leave out the lines of the questions that setup does not ask: with `none` it asks no model, with a recorded choice it asks neither, and with a recorded scan answer it does not ask about the past sessions.
A line that answers the wrong question does no harm: any answer except `y` to the last question writes nothing.

Expected after a yes:

- Under "Step 1: herdr version": `- Brain is tested with this version. OK.` The lines `- <version> is not one of the tested versions: ...` and `- Brain works. brain status shows a warning.` are fine too.
- Each plan line was written, or it says "Already done".
- For Codex, a last step "trust the Codex hook" with the Codex trust steps. Do step 4 below.
- A "First use" guide at the end.
- With nothing to write, setup prints "Every step is already done. Setup changes nothing." and asks nothing.

If it fails:

- `herdr has no session "<name>"`: start it with `herdr --session <name>`, then run setup again.
- `herdr session "<name>" is not running`: start it, then run setup again.
- herdr older than 0.9.1: setup stops and prints the version. Update herdr.
- `Setup wrote nothing.`: the answer was not y. Run setup again when the user wants to apply.
- `<cli> is not on PATH`: each extractor call fails until that CLI is on `PATH`. `brain doctor` says why.
- The extractor CLI fails to sign in: `brain status` and `brain doctor` show its error and say how to sign in again. The call sees the stored sign-in of the CLI, and only these variables of the herdr server env: `HOME`, `PATH`, `USER`, `TMPDIR`, the proxy variables (`HTTPS_PROXY`, `HTTP_PROXY`, `ALL_PROXY`, `NO_PROXY`, in either case), the CA bundle variables (`SSL_CERT_FILE`, `SSL_CERT_DIR`, `NODE_EXTRA_CA_CERTS`), and the Amazon Bedrock variables (`AWS_REGION`, `AWS_PROFILE`, `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`, `AWS_SESSION_TOKEN`, `AWS_BEARER_TOKEN_BEDROCK`). claude also gets `ANTHROPIC_API_KEY`, `ANTHROPIC_AUTH_TOKEN`, `CLAUDE_CODE_OAUTH_TOKEN`, `ANTHROPIC_BASE_URL`, `ANTHROPIC_CUSTOM_HEADERS`, `CLAUDE_CONFIG_DIR`, `CLAUDE_CODE_USE_BEDROCK`, `CLAUDE_CODE_USE_VERTEX`, `ANTHROPIC_VERTEX_PROJECT_ID`, `CLOUD_ML_REGION` and `GOOGLE_APPLICATION_CREDENTIALS`. codex also gets `OPENAI_API_KEY`, `CODEX_API_KEY`, `CODEX_ACCESS_TOKEN`, `OPENAI_BASE_URL`, `CODEX_HOME` and `CODEX_CA_CERTIFICATE`. For a codex proxy such as CLIProxyAPI, codex also gets the variable that `env_key` names for its `model_provider` (or the `model_provider` of its default `profile`) in `$CODEX_HOME/config.toml`. Brain reads only that name from the file. Brain only passes them on: it never shows, logs or stores their values. The call loads no settings file of the CLI, so a variable in the `env` block of `~/.claude/settings.json` does not reach it: set it in the herdr server env.
- The plan says `~/.local/bin/brain` exists and points elsewhere, or is broken: Brain does not change it. Use the binary path that setup prints.

Brain needs `~/.local/bin` on `PATH` to run as `brain`. Setup never edits the shell config. If the folder is not on `PATH`, tell the user and use the full binary path.

To bind a different herdr session, add `--session <name>`. To choose the extractor again later, run `brain setup --extractor`.
herdr 0.9.1 gives a session no lasting identity. After the user renames or recreates a session, run `brain setup` in it again.

## 4. Trust the Codex hook

Skip this step when the user does not use Codex.
Codex runs a new hook only after the user trusts it. You cannot do this for them. Tell the user:

1. Start `codex` in a terminal. Codex shows "Hooks need review" and "1 hook is new or changed."
2. Choose "Review hooks". Find the SessionStart hook with the Brain command, from source `~/.codex/hooks.json`.
3. Press `t` to trust it. Press `t` on the event list to trust all hooks that need review.
4. Do not choose "Continue without trusting (hooks won't run)". Brain then gets no context.

Later, `/hooks` in Codex shows the trust state. `codex exec` does not show the review screen and skips an untrusted hook with no warning.
Each new Brain hook entry needs trust again, also after an update.

Codex can ask for the user's approval each time an agent runs a `brain` command in its sandbox. Brain does not change Codex rules or permissions.
To allow `brain`, the user adds it to the allow list when Codex asks, or adds `prefix_rule(pattern = ["brain"], decision = "allow")` to `~/.codex/rules/default.rules` and restarts Codex.
An allow rule runs the command outside the sandbox with no prompt. See [Codex rules](https://developers.openai.com/codex/rules). Do not add the rule for the user; let them decide.

## 5. Check the setup

```sh
brain doctor
brain status
```

Expected:

- `brain doctor` ends with `all checks passed`.
- `brain status` shows `binding: herdr session "<name>", ...`, the herdr version, and the agent CLIs.

If `brain doctor` fails, it prints one line for each failed check, with its fix after `fix:`. Do the fix, then run `brain doctor` again.
`brain doctor` asks before a repair that writes a hook entry. `brain doctor --fix` applies each repair that `brain doctor` offers, with no question.

## 6. Try it

```sh
brain remember "never edit an applied migration; add a new one"
```

Expected: `proposal <id>: ... draft, waiting for your review in pane <pane> (or brain review)`. A review pane opens beside the pane, and the user approves it there.

If it fails:

- `workspace <id> is not mapped yet (herdr saves a new workspace after about 5 s)`: wait about 5 seconds and run it again, or use `--global`.
- `the review pane did not open: <reason>`: the draft still waits. The user approves it with `brain review`.
The next Claude Code or Codex session in that workspace gets the item at start. Codex gets it at its first prompt.

## Common failures

| Output | Cause | Fix |
|---|---|---|
| `not in a herdr pane: run brain <cmd> in a pane of a herdr session` | The shell is not in a herdr pane | Run the command in a pane of a herdr session |
| `herdr session "<name>" has no Brain binding, so Brain gives its agents nothing.` | Setup did not run in this session | `brain setup --session <name>` |
| The shell cannot find `brain` | `~/.local/bin` is not on `PATH`, or setup made no link | Use the full binary path that setup prints |
| `herdr pane list: ...` | The herdr server of the session does not answer | Check that the herdr server of that session runs, then run `brain doctor` again |
| `<cli>: no Brain hook in <file>, so <cli> sessions get no Brain items` | The hook entry is missing | `brain setup` |
| `<cli>: the Brain hook in <file> runs <command>` | The hook points to an old binary | Run `brain doctor` again and answer y, or `brain setup` |
| `holds the Brain <event> entry that you edited` | The user edited a Brain hook entry | Brain keeps it. The user edits it by hand. |
| `codex: Codex hook not trusted` | Codex skips the hook | Step 4: start codex, type `/hooks`, and press t on the Brain hook |
| `codex: Codex hook not trusted: the Brain hook is disabled in Codex` | The user turned the hook off | Enable it in `/hooks` in Codex |
| `codex: Codex reads <dir>/hooks.json, which holds no Brain hook entry` | `CODEX_HOME` points Codex to another folder. `brain setup` writes only `~/.codex/hooks.json` and does not read `CODEX_HOME`, so a new setup run does not fix this. | Run Codex with `CODEX_HOME` unset, or set to `~/.codex`, so that it reads `~/.codex/hooks.json` |
| `hook records off for <cli>` | A newer Brain has hook record entries that are missing | `brain update` |
| `hook not trusted or not firing: <n> live agent(s) with no Brain receipt` | A session started before setup, or the hook does not run | Start a new session. If it gets no receipt either, the CLI sandbox may block the herdr socket. |
| `extraction: signals wait, and no extractor CLI is chosen` | No extractor | `brain setup --extractor` |
| `extraction: the last extractor call for workspace <key> failed` | The extractor CLI fails. The reason in parentheses is the error of the CLI. | Do what the fix says. For a sign-in failure: run `claude`, then `/login` (or `codex login`), or set `ANTHROPIC_API_KEY` (or `OPENAI_API_KEY`) in the herdr server env. Else run that CLI in a terminal and check that it works. Brain tries again later. |
| `extraction: ..., so Brain makes no extractor call` | A bad `[gates]` or `[caps]` key in the Brain config | Correct or remove the `[gates]` and `[caps]` keys |
| `fix: brain repair --confirm deletes them` | Output conflicts | `brain repair` lists them. `brain repair --confirm` deletes them. |

## Update

```sh
brain update --check
brain update
```

`brain update --check` compares the installed version with the newest version of the repo and changes nothing.
`brain update` installs the newest version, shows the missing hook record entries and asks once, then runs `brain doctor --fix`.
Ask the user before you answer that question. For Codex, each new entry needs trust again (step 4).

The newest version depends on the install:

- No `--ref`, or a linked checkout: the newest commit of the default branch.
- A release tag, such as `--ref v0.1.0`: the newest release tag. It needs no Go.
- Any other `--ref`: none. `brain update` changes nothing and prints the command to leave the ref. Run it after the user agrees. The default branch needs Go.

## Uninstall

Run the steps in this order:

```sh
brain uninstall
brain uninstall --apply
herdr plugin uninstall brain
```

1. `brain uninstall` is a dry run. It lists what would change. Show the list to the user.
2. `brain uninstall --apply` removes the Brain hook entries, the install record, the `~/.local/bin/brain` link and the session bindings file. It keeps the stores in `~/.local/share/brain`.
   Add `--data` only when the user wants their stores removed too. It also removes `~/.local/share/brain`, `~/.config/herdr/plugins/config/brain` and `~/.local/state/herdr/plugins/brain`.
3. `herdr plugin uninstall brain` removes the plugin.

If it fails:

- `brain uninstall --apply` keeps a hook entry that the user edited, and tells them to remove it by hand.
- If `herdr plugin uninstall brain` ran first, the hook entries point to a missing binary. Do one of these:
  - Install the plugin again, which gets the same path. Run `brain uninstall --apply`, then uninstall the plugin.
  - Or edit the files by hand. In `~/.claude/settings.json` and `~/.codex/hooks.json`, remove each hook group whose command contains `hook --profile claude --event` or `hook --profile codex --event`. In `~/.gemini/config/hooks.json`, remove the `brain` key. Then remove `~/.local/share/brain/install.json`, `~/.local/share/brain/bindings.json` and the `~/.local/bin/brain` link.
