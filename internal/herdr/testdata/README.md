# herdr 0.9.1 replies

Recorded on 2026-09-28 from a headless scratch server (`herdr --session brain-t0005 server`
with a scratch HOME), never the main server:

- `snapshot-0.9.1.json`, `pane-no-agent.json`, `workspace-list.json`, `session-list.json`,
  `plugin-pane-opened.json`, `plugin-pane-closed.json`, `version.txt`
- `error-pane-not-found.json`: stderr of `herdr pane get bogus` (exit 1)

Made from the recorded replies and `spike/m0/data/api-schema-0.9.1.json`:

- `pane-agent.json`: a pane with `agent` and `agent_session` (values from `spike/m0/FINDINGS.md`)
- `pane-null-optional.json`: `agent`, `agent_session` and `cwd` are null
- `pane-bad-id.json`: `pane_id` is `p1`
- `snapshot-0.8.2.json`, `snapshot-0.9.4.json`: only `version` and `protocol` changed
- `pane-list.json`: `herdr pane list` (`pane_list` in the schema) with the panes of
  `pane-no-agent.json` and `pane-agent.json` (E8-S4)

Recorded on 2026-09-28 from a headless scratch server (`herdr --session brain-t0012-move
server` with a scratch HOME):

- `pane-moved.json`: `herdr pane get w1:p2` after `herdr pane move w1:p2 --tab w2:t1 --split
  right`. herdr answers the old id with the moved pane and its new id `w2:p2`. A move to a
  new tab in the same workspace kept the id.

Recorded on 2026-09-29 from a headless scratch server (`herdr --session t48-live server` with
a scratch HOME), with the plugin folder shortened to `/tmp/brain-t0048/plug`:

- `pane-process-info.json`: `herdr pane process-info --pane w1:p2` of the Brain review pane,
  opened with `--env BRAIN_PROPOSAL_ID=7` (E5-S2)

Recorded on 2026-09-29 from a headless scratch server (`herdr --session brain-eval-dev server`
with a scratch HOME), for the eval runner (E10-S1):

- `workspace-created.json`: `herdr workspace create --cwd <folder> --label eval-case --no-focus`
- `pane-split.json`: `herdr pane split w1:p1 --direction right --cwd <folder> --no-focus`
- `output-matched.json`: `herdr pane wait-output w1:p1 --regex '<done> [0-9]+' --timeout 5000`
  after `herdr pane run w1:p1 'echo hi; echo <done> $?'`
- `pane-process-info-sleep.json`: `herdr pane process-info --pane w1:p2` with `sleep 300` in
  the foreground
- `pane-closed.json`: `herdr pane close w1:p2`
- `error-agent-not-found.json`: stderr of `herdr agent read w1:p1` on a shell pane (exit 1)
