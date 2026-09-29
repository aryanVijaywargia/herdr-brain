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
"Skip any progress report." That turn is in the history above the Claude S31 reads.

## Files

Codex, `recent_unwrapped`:

- `codex-s31-*.txt` (S31): prompt "Run touch made.txt and request escalated permissions for
  it.", after the turn "Delete blob.bin with rm, then list the files with ls -la.". The reads
  in status order: `0-base` (the end of the first turn), `1-working`, `2-blocked` (the
  approval dialog), `3-working` (after `y`), `4-done`. Codex gives the status `done`, not
  `idle`. The dialog goes away after the approval, and "• Running" becomes "• Ran".
- `codex-s40-*.txt` (S40): the prompt "Reply with only the word ready." two times. `0-base`
  is the first turn at work; `1-done` and `2-done` are the ends of the two turns.

Claude, `visible` (40 rows):

- `claude-s31-*.txt` (S31): prompt "Skip any progress report. Only create the file hi.txt
  with the text hi.". `0-base`, `1-working` (only the spinner changes), `2-blocked` (the
  "Do you want to create hi.txt?" dialog hides the top of the screen), `3-working` (after
  `1`; the hidden text shows again), `4-idle`.
- `claude-s38-*.txt` (S38): prompt "Write the numbers 1 to 50, one per line, and nothing
  else.". `0-base` (the turn at work), `1-idle` (the answer pushed the prompt and the numbers
  1 to 18 off the screen), `2-repaint` (a later read of the same screen, byte for byte).
- `claude-s40-*.txt` (S40): the prompt "Reply with only the word ready." two times, as for
  Codex. The time lines differ ("Sautéed for 1s", "Crunched for 1s").
- `claude-visible-empty.txt` (S38): made for the test, not recorded. It is an empty screen,
  as after an alternate screen closes.

No resize read is recorded. A headless herdr (no client) does not resize the pane terminal:
after `herdr pane split w1:p1 --direction right --ratio 0.6` and `herdr pane resize --pane
w1:p1 --direction left --amount 0.2`, the layout gave 48 columns, but the reads kept 119
columns.

## Noise profiles

The shipped `adapters/codex.toml` and `adapters/claude.toml` carry the `noise_lines` of the
reads above (E11-S4): the screen lines that change on each read (the spinner, the done line,
the rules, the tip, the composer or prompt box, and the footer). The capture tests use the shipped profiles.
