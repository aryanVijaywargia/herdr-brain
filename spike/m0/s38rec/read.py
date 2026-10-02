#!/usr/bin/env python3
"""Make one pane.read socket call, as herdr.Socket.Read makes it, and print the text.

Usage: read.py <socket> <pane> <visible|recent_unwrapped> <lines, 0 for the herdr default>
"""
import json, socket, sys

sock, pane, source, lines = sys.argv[1], sys.argv[2], sys.argv[3], int(sys.argv[4])
s = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
s.connect(sock)
req = {"id": "s38", "method": "pane.read",
       "params": {"pane_id": pane, "source": source, "format": "text", "strip_ansi": True}}
if lines > 0:
    req["params"]["lines"] = lines  # as herdr.Socket.Read: 0 is the herdr default
s.sendall((json.dumps(req) + "\n").encode())
reply = json.loads(s.makefile("r").readline())
if "error" in reply:
    sys.exit(json.dumps(reply["error"]))
sys.stdout.write(reply["result"]["read"]["text"])
