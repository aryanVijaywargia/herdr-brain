# E17-S1 spike: "stopped working" and the event key

Results: `spike/m0/FINDINGS.md`, section "E17-S1: stopped working and the event key (§17 item 23)".

- `probe/`: the reader (Go, its own module, no `brain` or `brained`). It subscribes to the agent events of the test panes, calls `agent.get` at once for each status event, and tests the key methods K, KT and KJ live. Claims go to a file, so they live across a reader restart. Control lines (`ctl.sh`): `note <text>`, `dup <pane>` (the last stop event of the pane again), `inject <pane> <workspace> <agent> <status>` (a made event with the bytes of a herdr event).
- `eval.py`: replays the probe log. It gives K, KT and KJ again (and checks them against the probe's claims) and adds the proposed method P.
- `ht.sh`: herdr for the test session `brain-spike-e17s1` only, with a clean env. `probe.sh`: starts the probe. `ctl.sh`: writes one control line. `show.py`: prints the probe log in short lines.
- `data/probe-log.jsonl`: every baseline, event, duplicate, `agent.get` answer, claim, skip and drive note. `data/probe-claims.jsonl`: the probe's claims. `data/terminals.txt`: the `terminal_id` of each pane in each server run. `data/eval.txt`: the output of `eval.py`.

Build: `cd probe && go build -o /private/tmp/e17s1/probe .`
