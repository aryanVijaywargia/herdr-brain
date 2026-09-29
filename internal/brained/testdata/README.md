# Pane-read fixtures (E11-S3)

The text of `pane.read` replies, as herdr 0.9.1 gives it with `format` text and `strip_ansi`.

Codex, `recent_unwrapped` (the fake herdr gives the last `lines` lines):

- `codex-run1-idle.txt`: the full idle read of run 1 in `spike/m0/data/e11s1b-capture.txt`
  (142 lines). The head and the tail are recorded; the 120 rows between them are the rows that
  the prompt asked for and the counts of the log confirm.
- `codex-run2-*.txt`: run 2 in the same pane, whose history still holds run 1 (M0 E11-S1b),
  through the recorded status order idle, working, blocked, working, idle. The screen parts
  (the header, the composer, the footer, the spinner, the approval box) have the recorded
  shape; the prompt and the reply text are made for the test.
- `codex-repeat-*.txt`: the same prompt in two real turns (S40), in the same shape.

Claude, `visible` (M0 E11-S1: the alternate screen):

- `claude-visible-1.txt`: one turn on the screen, 80 columns.
- `claude-visible-1-repaint.txt`: the same screen, drawn again.
- `claude-visible-1-narrow.txt`: the same turn after a resize to 60 columns: the text wraps
  again.
- `claude-visible-2-scrolled.txt`: a long answer that pushed the turn off the screen.

`noise/codex.toml` is `adapters/codex.toml` with test `noise_lines`. The shipped profiles have
none, so their hash and their conformance results stay the same.
- `claude-visible-empty.txt`: an empty screen, as after an alternate screen closes (S38).
