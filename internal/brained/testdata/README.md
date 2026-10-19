# Pane-read fixtures (E11-S3)

The files are the text of real `pane.read` replies, as herdr gives it with `format` text and
`strip_ansi` true. The fake herdr of the tests gives them back in order.

## Recording

- Date: 2026-09-29.
- herdr 0.9.1, Claude Code 2.1.284 (`claude-haiku-4-5-20251001`), codex-cli 0.154.0
  (gpt-6-astra high).
- A test herdr session `brain-rec-e11s3`, not `default`, with a clean environment:

  ```sh
  env -i HOME="$HOME" PATH="$HOME/.nvm/versions/node/v24.15.0/bin:$HOME/.local/bin:/usr/bin:/bin:/usr/sbin:/sbin" \
    TERM=xterm-256color LANG=en_US.UTF-8 herdr --session brain-rec-e11s3 "$@"
  ```

  The session was stopped and deleted after the recording.
- Claude in pane `w1:p1`, cwd `/private/tmp/e11s1/agent`:
  `herdr pane run w1:p1 claude --model claude-haiku-4-5-20251001`. Read: `visible`.
- Codex in pane `w2:p1`, cwd `/private/tmp/e11s3rec/agent` (a git repo with `notes.txt`,
  `sub/data.csv` and `blob.bin`). The folder trust is on the command line only, and nothing
  was written to `~/.codex`:

  ```sh
  codex -c 'projects={"/private/tmp/e11s3rec/agent"={trust_level="trusted"}}' \
    -c check_for_update_on_startup=false -a on-request -c approvals_reviewer='"user"'
  ```

  At "Hooks need review" the answer was 3, "Continue without trusting". Read:
  `recent_unwrapped`, 120 lines.
- Each read was one socket call, as `herdr.Socket.Read` makes it:
  `{"method": "pane.read", "params": {"pane_id": ..., "source": ..., "lines": ..., "format": "text", "strip_ansi": true}}`.
  Prompts and approvals went in with `herdr pane send-text` and `herdr pane send-keys`. Brain
  sends no keys; the recording did.
- Private data is replaced: the user name is `user` in the `ls -la` lines, and the host name
  is `host`. No tokens or e-mail addresses were on the screens.

The test Claude loads the global Claude hooks of the machine. One hook asked it to run a
progress-report command. The answer to that approval was 3 (No), and the next prompt said
"Skip any progress report." That turn is in the history above the `claude-s31-*.txt` reads.

## Files

Codex, `recent_unwrapped`:

- `codex-s31-*.txt`: prompt "Run touch made.txt and request escalated permissions for
  it.", after the turn "Delete blob.bin with rm, then list the files with ls -la.". The reads
  in status order: `0-base` (the end of the first turn), `1-working`, `2-blocked` (the
  approval dialog), `3-working` (after `y`), `4-done`. Codex gives the status `done`, not
  `idle`. The dialog goes away after the approval, and "• Running" becomes "• Ran".
- `codex-s40-*.txt`: the prompt "Reply with only the word ready." two times. `0-base`
  is the first turn at work; `1-done` and `2-done` are the ends of the two turns.

Claude, `visible` (40 rows):

- `claude-s31-*.txt`: prompt "Skip any progress report. Only create the file hi.txt
  with the text hi.". `0-base`, `1-working` (only the spinner changes), `2-blocked` (the
  "Do you want to create hi.txt?" dialog hides the top of the screen), `3-working` (after
  `1`; the hidden text shows again), `4-idle`.
- `claude-s38-*.txt`: prompt "Write the numbers 1 to 50, one per line, and nothing
  else.". `0-base` (the turn at work), `1-idle` (the answer pushed the prompt and the numbers
  1 to 18 off the screen), `2-repaint` (a later read of the same screen, byte for byte).
- `claude-s40-*.txt`: the prompt "Reply with only the word ready." two times, as for
  Codex. The time lines differ ("Sautéed for 1s", "Crunched for 1s").
- `claude-e14s3-3-idle.txt` (E14-S3): made for the test from `claude-s40-2-idle.txt`, not
  recorded. The screen scrolled 6 lines, and the prompt "Reply with only the word ready."
  comes a third time, after a proposal exists.
- `claude-visible-empty.txt`: made for the test, not recorded. It is an empty screen,
  as after an alternate screen closes.

A headless herdr (no client) does not resize the pane terminal: after `herdr pane split w1:p1
--direction right --ratio 0.6` and `herdr pane resize --pane w1:p1 --direction left --amount
0.2`, the layout gave 48 columns, but the reads kept 119 columns. The resize reads below come
from a herdr client.

## Resize reads (bug #199)

- Date: 2026-10-02. herdr 0.9.1, Claude Code 2.1.287 (`claude-haiku-4-5-20251001`), codex-cli
  0.154.0 (gpt-6-astra high).
- A test herdr session `brain-rec-199`, stopped and deleted after the recording. The herdr
  client of the session ran in a pseudo-terminal of a small Python script of the recording, with the
  clean environment above. The script sets the terminal size with `TIOCSWINSZ`, and the client
  resizes the panes of the focused workspace. Each read was one socket call of
  a second script of the recording, as `herdr.Socket.Read` makes it. The rows and columns below are
  the size of the client terminal; the pane is 26 columns narrower (the sidebar).
- Claude in pane `w2:p1`, cwd `/private/tmp/e11s1/agent` (trusted in E11-S3), read `visible`
  (40 rows). Prompt: "Skip any progress report. Write one plain paragraph of about 120 words
  about rivers, and nothing else.". No hook approval came up.
- Codex in pane `w3:p1`, cwd `/private/tmp/s38rec/codex` (a git repo with `notes.txt`), with
  the folder trust on the command line, as above. At "Hooks need review" the answer was 3.
  Read `recent_unwrapped`, 120 lines. Prompt: "Do not run any command. Write one plain
  paragraph of about 120 words about rivers, and nothing else.". Codex gave the status
  `idle` at the end of each turn here, not `done`.
- Private data is replaced: the user name is `user` and the host name is `host`, in the shell
  line of `codex-s38-0-base.txt`.

Files, all at idle:

- `claude-s38-resize-*.txt`: `0-base` (41x160), `1-narrow` (41x100: Claude draws the screen
  again, and the prompt and the answer wrap anew), `2-wide` (41x160, byte for byte the same as
  `0-base`), `3-small` (16x80: only the end of the answer fits, and the top of the screen is
  lost), `4-wide` (41x160 again, the same as `0-base`), `5-narrow-ready` (41x100, then the
  prompt "Reply with only the word ready.").
- `codex-s38-*.txt`: `0-base` (41x160), `1-narrow` (41x100), `2-wide` (41x160),
  `3-narrow-ready` (41x100, then the prompt "Reply with only the word ready."). Codex clears
  the screen and the history at a resize and draws its whole history again at the new width.
  The shell line above Codex is lost after the first resize.

## Noise profiles

The shipped `adapters/codex.toml` and `adapters/claude.toml` carry the `noise_lines` of the
reads above (E11-S4): the screen lines that change on each read (the spinner, the done line,
the rules, the tip, the composer or prompt box, and the footer). The capture tests use the shipped profiles.

## M2 eval screens (#278, #281)

- Made, not recorded: the M2 eval run (2026-10-07, herdr 0.9.3, codex-cli 0.160.1, Claude Code
  2.1.292) kept only the excerpts of its observations, not the pane reads. The files put those
  excerpts back in the order of the reads of session S1 (codex) and S2 (claude). The project
  path is `~/work/notes-api`, and the shell line is `user@host notes-api % codex`.
- `codex-i278-*.txt` (`recent_unwrapped`): `0-base` (the start banner of 0.160.1, with no box),
  `1-working` (the prompt), `2-blocked` (the prompt cut to one line with "…", diff lines and the
  approval dialog), `3-working` (after the approval: diff lines and the approval echo line),
  `4-idle` (Codex draws the banner and the whole prompt again), `5-working` (the next prompt
  under the cut first prompt).
- `codex-i281-trust.txt`, `claude-i281-trust.txt`: the folder trust dialogs.
- `claude-i281-diff.txt` (`visible`): diff lines at the top of the screen, and the folded
  "Ran 2 shell commands" line under a prompt.
