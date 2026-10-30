# Brain commands

Run these in a pane of a herdr session. `brain <command> --help` prints the usage of one command.

| Command | What it does |
|---|---|
| `brain version` | Print the Brain version |
| `brain status` | Show Brain status, with the start, capture, brief and tools of each live session |
| `brain doctor [--fix]` | Check the Brain setup and print the fix of each failed check; `--fix` applies each offered repair with no question |
| `brain setup [--session <name> [--test <folder>]] [--extractor]` | Bind a herdr session, add the hooks of an agent CLI with no Brain launch, ask to remove the old global hooks of Claude Code and Codex, choose the extractor CLI |
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
| `brain update [--check \| --records]` | Update the plugin to the newest version of its repo, ask to remove the old global hooks of Claude Code and Codex (one question for each), add the missing hook record entries (one question), then run `brain doctor --fix`; `--check` only compares; `--records` only does the hook step, with no network call: the removal questions of the old global hooks of Claude Code and Codex, then the missing hook record entries of the other CLIs. An install with no `--ref` and a linked checkout follow the newest commit of the default branch. An install at a release tag (`--ref v<major>.<minor>.<patch>`) follows the newest release tag. An install at any other ref is pinned: `brain update` changes nothing and prints the command to leave the ref |

## herdr actions

In herdr, the actions "Brain: setup", "Brain: status", "Brain: review", "Brain: recall", "Brain: retired items", "Brain: links to review" and "Brain: start" open the setup popup, show the status, open the queue of proposals, open the recall popup, list the retired items, list the links to review and start `brained`.
"Brain: new Claude pane" and "Brain: new Codex pane" split the focused pane, in the cwd of that pane, and run `brain claude` or `brain codex` in the new pane. The pane keeps its shell when the CLI exits.
They work without `brain` on your `PATH`.
