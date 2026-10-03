#!/usr/bin/env python3
"""Replay the probe log for each key method and print one row per status event.

Methods K, KT and KJ are the probe's own (the replay must give the probe's claims; it
checks this). Method P is the proposed one:
  - agent.get at once for each status event (the log has it);
  - the event joins only when agent.get has the event status; an event that does not
    join is skipped and does not change the reader's memory;
  - the memory of a pane is its last joined status; a baseline (reader start or
    reconnect) sets it from agent.get;
  - a stop is a joined idle or done event after a joined working, and agent.get has an
    agent session id (Codex has none before the first prompt of a run);
  - key: (pane, terminal_id, agent session id, state_change_seq).
Claims of every method live across reader restarts, as automation_runs rows would.
"""
import json, sys

log = sys.argv[1] if len(sys.argv) > 1 else "/private/tmp/e17s1/run/log.jsonl"
term = {}
for line in open(sys.argv[2] if len(sys.argv) > 2 else "data/terminals.txt"):
    if line.startswith("#") or not line.strip():
        continue
    _, runs, pane, tid = line.split()
    for r in runs.split(","):
        term[(r, pane)] = tid

STOP = ("idle", "done")
held = {m: set() for m in ("K", "KT", "KJ", "P")}
prev, prevP = {}, {}
online = []   # (method, key) claims that the probe wrote
replay = []
note = ""
print("%-12s %-3s %-5s %-6s %-8s %-7s %-22s %-4s %-4s %-4s %-4s  %s" %
      ("time", "run", "kind", "pane", "status", "prev", "agent.get", "K", "KT", "KJ", "P", "case"))
for line in open(log):
    r = json.loads(line)
    k, run = r["kind"], r["run"]
    if k == "note":
        note = r["why"]
        continue
    if k == "claim":
        online.append((r["method"], r["key"]))
        continue
    if k == "baseline":
        g = r["get"]
        prev[r["pane"]] = g.get("status") or ""
        prevP[r["pane"]] = g.get("status") or ""
        continue
    if k not in ("event", "dup") or r["raw"].get("event") != "pane.agent_status_changed":
        continue
    pane, st, g = r["pane"], r["raw"]["data"]["agent_status"], r["get"]
    seq = "none" if g.get("seq") is None else str(g["seq"])
    sess = g.get("session") or ""
    key = "%s|%s|%s" % (pane, sess, seq)
    res = {}
    p = prev.get(pane, "")
    prev[pane] = st
    def claim(m, kk):
        if kk in held[m]:
            return "dup"
        held[m].add(kk)
        replay.append((m, kk)) if m != "P" else None
        return "CLM"
    if st in STOP:
        res["K"] = claim("K", key)
        if p != "working":
            res["KT"] = res["KJ"] = "-"
        else:
            res["KT"] = claim("KT", key)
            ok = g.get("found") and g.get("status") == st and sess
            res["KJ"] = claim("KJ", key) if ok else "-"
    joined = g.get("found") and g.get("status") == st
    if joined:
        pp = prevP.get(pane, "")
        prevP[pane] = st
        if st in STOP and pp == "working" and not sess:
            res["P"] = "skip"
        elif st in STOP and pp == "working":
            res["P"] = claim("P", "%s|%s|%s|%s" % (pane, term[(run, pane)], sess, seq))
        elif st in STOP:
            res["P"] = "-"
    elif st in STOP:
        res["P"] = "skip"
    gs = "%s/%s/%s" % (g.get("status"), seq, sess[:8] or "-") if g.get("found") else "not found"
    print("%-12s %-3s %-5s %-6s %-8s %-7s %-22s %-4s %-4s %-4s %-4s  %s" % (
        r["t"], run, k, pane, st, p, gs, res.get("K", ""), res.get("KT", ""), res.get("KJ", ""), res.get("P", ""), note))
print()
for m in held:
    print("claims %-2s %d" % (m, len(held[m])))
same = sorted(online) == sorted(replay)
print("replay of K, KT, KJ gives the probe's claims:", "yes" if same else "NO")
ms = [json.loads(l).get("get_ms") for l in open(log)]
ms = sorted(x for x in ms if x)
print("agent.get time after the event read: min %.2f ms, median %.2f ms, max %.2f ms (%d calls)" % (ms[0], ms[len(ms)//2], ms[-1], len(ms)))
