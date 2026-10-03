#!/usr/bin/env python3
# Copy the E16-S2 logs from /private/tmp/e16s2/logs into data/ and fixtures/, with the home path,
# the user name and the host name replaced. OpenCode stream events (deltas, MCP, catalog) are dropped.
import json, os, re, sys

here = os.path.dirname(os.path.abspath(__file__))
src = "/private/tmp/e16s2/logs"
home = os.path.expanduser("~")
user = os.path.basename(home)

def clean(s):
    s = s.replace(home, "~").replace(user, "<user>")
    return re.sub(r"\b192:(agy|opencode) ", r"<host>:\1 ", s)

os.makedirs(f"{here}/data", exist_ok=True)

if sys.argv[1:] == ["--rerun"]:
    # The review rerun (B1-B2, O5-O7): its logs go to data/rerun-*, and the shim inputs of the
    # fixed shim go to fixtures/opencode-shim-*.json. The first-run files stay as they are.
    keep = re.compile(r'"kind":"(setup|context|context-done|tool\.after|brain-[a-z]+|subscribe-error)"|"type":"session\.created"')
    with open(f"{src}/brain.jsonl") as f, open(f"{here}/data/rerun-brain-calls.jsonl", "w") as o:
        calls = [json.loads(clean(l)) for l in f]
        for c in calls:
            o.write(json.dumps(c) + "\n")
    with open(f"{src}/opencode-shim.jsonl") as f, open(f"{here}/data/rerun-opencode-shim.jsonl", "w") as o:
        for line in f:
            if keep.search(line):
                o.write(clean(line))
    oc = [c for c in calls if c["args"][2] == "opencode"]
    for name, c in (("opencode-shim-start.json", oc[0]), ("opencode-shim-tool.json", oc[1]), ("opencode-shim-start-reopened.json", oc[2])):
        with open(f"{here}/fixtures/{name}", "w") as o:
            json.dump(c["stdin"], o, indent=2, sort_keys=True)
            o.write("\n")
    print("saved rerun:", len(calls), "brain calls")
    sys.exit(0)
os.makedirs(f"{here}/fixtures", exist_ok=True)

# The stand-in brain log: every hook and shim call.
with open(f"{src}/brain.jsonl") as f, open(f"{here}/data/brain-calls.jsonl", "w") as o:
    for line in f:
        o.write(clean(line))

# The OpenCode shim log, without the stream and catalog events.
keep = re.compile(r'"kind":"(setup|context|context-done|tool\.after|brain-[a-z]+|subscribe-error)"|"type":"session\.(created|execution\.[a-z]+|tool\.[a-z.]+|instructions\.updated|step\.(started|ended)|renamed)"')
with open(f"{src}/opencode-shim.jsonl") as f, open(f"{here}/data/opencode-shim.jsonl", "w") as o:
    for line in f:
        if keep.search(line):
            o.write(clean(line))

with open(f"{src}/screens.txt") as f, open(f"{here}/data/opencode-screens.txt", "w") as o:
    o.write(clean(f.read()))

# Fixtures: the exact hook input (stdin) of chosen calls, one JSON file each.
calls = [json.loads(clean(l)) for l in open(f"{src}/brain.jsonl")]
def first(pred):
    return next(c for c in calls if pred(c))
def a(c, i):
    return c["args"][i] if len(c["args"]) > i else ""
fx = {
    "agy-preinvocation-new.json": first(lambda c: a(c, 2) == "agy" and a(c, 4) == "start" and c["stdin"]["invocationNum"] == 0 and c["stdin"]["initialNumSteps"] == 1),
    "agy-preinvocation-next-turn.json": first(lambda c: a(c, 2) == "agy" and a(c, 4) == "start" and c["stdin"]["invocationNum"] == 0 and c["stdin"]["initialNumSteps"] > 1),
    "agy-preinvocation-after-tool.json": first(lambda c: a(c, 2) == "agy" and a(c, 4) == "start" and c["stdin"]["invocationNum"] == 1),
    "agy-posttooluse-view_file.json": first(lambda c: a(c, 4) == "tool" and c["stdin"].get("toolCall", {}).get("name") == "view_file"),
    "agy-posttooluse-run_command.json": first(lambda c: a(c, 4) == "tool" and c["stdin"].get("toolCall", {}).get("name") == "run_command"),
    "agy-stop.json": first(lambda c: a(c, 2) == "agy" and a(c, 4) == "stop"),
    "opencode-session-created.json": first(lambda c: a(c, 2) == "opencode" and a(c, 4) == "start"),
    "opencode-tool-after-exit0.json": first(lambda c: a(c, 2) == "opencode" and a(c, 4) == "tool" and c["stdin"]["result"]["output"]["exit"] == 0),
    "opencode-tool-after-exit1.json": first(lambda c: a(c, 2) == "opencode" and a(c, 4) == "tool" and c["stdin"]["result"]["output"]["exit"] == 1),
}
for name, c in fx.items():
    with open(f"{here}/fixtures/{name}", "w") as o:
        json.dump(c["stdin"], o, indent=2, sort_keys=True)
        o.write("\n")
# The exact agy output of the start hook (the brief in injectSteps[0].ephemeralMessage).
with open(f"{here}/fixtures/agy-preinvocation-output.json", "w") as o:
    o.write(fx["agy-preinvocation-new.json"] and first(lambda c: a(c, 2) == "agy" and c["printed"].startswith('{"injectSteps"'))["printed"] + "\n")
print("saved", len(calls), "brain calls,", len(fx), "fixtures")
