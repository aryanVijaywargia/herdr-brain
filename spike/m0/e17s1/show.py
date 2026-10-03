#!/usr/bin/env python3
"""Print the probe log in short lines: time, kind, pane, event status, agent.get, method result."""
import json, sys
path = sys.argv[1] if len(sys.argv) > 1 else "/private/tmp/e17s1/run/log.jsonl"
for line in open(path):
    r = json.loads(line)
    k = r["kind"]
    g = r.get("get") or {}
    gs = "get=%s/%s/%s%s" % (g.get("status"), g.get("seq"), (g.get("session") or "-")[:8], "" if g.get("found", True) else " ERR " + g.get("error", "")) if g else ""
    if k in ("event", "dup"):
        raw = r.get("raw", {})
        d = raw.get("data", {})
        ev = raw.get("event", raw.get("result", {}).get("type", ""))
        st = d.get("agent_status", "")
        extra = " released" if d.get("released") else ""
        print(r["t"], r["run"], k, r.get("pane", d.get("pane_id", "")), ev, st + extra, ("prev=" + r["prev"]) if r.get("prev") else "", gs)
    elif k in ("claim", "skip"):
        print(r["t"], r["run"], "   ", k.upper(), r["method"], r["key"][:30], r.get("why", ""))
    else:
        print(r["t"], r["run"], k, r.get("pane", ""), r.get("why", ""), gs)
