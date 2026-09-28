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

Recorded on 2026-09-28 from a headless scratch server (`herdr --session brain-t0012-move
server` with a scratch HOME):

- `pane-moved.json`: `herdr pane get w1:p2` after `herdr pane move w1:p2 --tab w2:t1 --split
  right`. herdr answers the old id with the moved pane and its new id `w2:p2`. A move to a
  new tab in the same workspace kept the id.
