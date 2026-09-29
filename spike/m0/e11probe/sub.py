#!/usr/bin/env python3
"""Subscribe to herdr events on a socket; print receive time and the event, one per line."""
import json, socket, sys, time
sock_path, out = sys.argv[1], sys.argv[2]
types = sys.argv[3:] or ["pane.agent_detected", "pane.exited", "pane.closed", "pane.created"]
s = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
s.connect(sock_path)
def sub(t):
    # "type@pane" subscribes one pane (pane.agent_status_changed needs pane_id).
    if "@" in t:
        k, p = t.split("@", 1)
        return {"type": k, "pane_id": p}
    return {"type": t}
req = {"id": "e11sub", "method": "events.subscribe", "params": {"subscriptions": [sub(t) for t in types]}}
s.sendall((json.dumps(req) + "\n").encode())
f = s.makefile("r")
with open(out, "a", buffering=1) as o:
    for line in f:
        o.write("%.3f %s" % (time.time(), line if line.endswith("\n") else line + "\n"))
