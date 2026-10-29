package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// manifestTables returns the key = value lines of each [[name]] table in herdr-plugin.toml.
func manifestTables(t *testing.T, name string) []map[string]string {
	t.Helper()
	b, err := os.ReadFile("../../herdr-plugin.toml")
	if err != nil {
		t.Fatal(err)
	}
	var tables []map[string]string
	var cur map[string]string
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "[") {
			cur = nil
			if line == "[["+name+"]]" {
				cur = map[string]string{}
				tables = append(tables, cur)
			}
			continue
		}
		if k, v, ok := strings.Cut(line, " = "); ok && cur != nil {
			cur[k] = v
		}
	}
	return tables
}

func findTable(tables []map[string]string, id string) map[string]string {
	for _, tb := range tables {
		if tb["id"] == `"`+id+`"` {
			return tb
		}
	}
	return nil
}

// The setup action opens the setup pane, and the setup pane runs `brain setup` from the plugin root.
func TestManifestSetupAction(t *testing.T) {
	action := findTable(manifestTables(t, "actions"), "setup")
	if action == nil {
		t.Fatal("no [[actions]] with id setup")
	}
	if !strings.Contains(action["command"], `plugin pane open --plugin brain --entrypoint setup`) {
		t.Errorf("setup action command = %s, want it to open the setup pane", action["command"])
	}
	pane := findTable(manifestTables(t, "panes"), "setup")
	if pane == nil {
		t.Fatal("no [[panes]] with id setup")
	}
	if !strings.Contains(pane["command"], `$HERDR_PLUGIN_ROOT/bin/brain\" setup`) {
		t.Errorf("setup pane command = %s, want it to run bin/brain setup", pane["command"])
	}
}

// E2-S6: one [[events]] entry, for workspace.closed, runs `brain event workspace-closed`.
func TestManifestClosedEvent(t *testing.T) {
	events := manifestTables(t, "events")
	if len(events) != 1 {
		t.Fatalf("%d [[events]] entries, want 1", len(events))
	}
	if events[0]["on"] != `"workspace.closed"` {
		t.Errorf("event on = %s, want \"workspace.closed\"", events[0]["on"])
	}
	if !strings.Contains(events[0]["command"], `$HERDR_PLUGIN_ROOT/bin/brain\" event workspace-closed`) {
		t.Errorf("event command = %s, want it to run bin/brain event workspace-closed", events[0]["command"])
	}
}

// E5-S3: the review action opens the queue split, and the queue runs `brain review`.
func TestManifestReviewAction(t *testing.T) {
	action := findTable(manifestTables(t, "actions"), "review")
	if action == nil {
		t.Fatal("no [[actions]] with id review")
	}
	if !strings.Contains(action["command"], `plugin pane open --plugin brain --entrypoint queue`) {
		t.Errorf("review action command = %s, want it to open the queue pane", action["command"])
	}
	queue := findTable(manifestTables(t, "panes"), "queue")
	if queue == nil {
		t.Fatal("no [[panes]] with id queue")
	}
	if queue["placement"] != `"split"` || !strings.Contains(queue["command"], `$HERDR_PLUGIN_ROOT/bin/brain\" review;`) {
		t.Errorf("queue pane = %v, want a split that runs bin/brain review", queue)
	}
}

// E5-S1: the review pane is a split that runs `brain pane review` with the proposal id of its
// --env, never an id from pane text. E5-S2: exec, so herdr shows brain as the pane's process,
// and a close finds it (runsReview).
func TestManifestReviewPane(t *testing.T) {
	pane := findTable(manifestTables(t, "panes"), "review")
	if pane == nil {
		t.Fatal("no [[panes]] with id review")
	}
	if pane["placement"] != `"split"` {
		t.Errorf("review pane placement = %s, want \"split\"", pane["placement"])
	}
	want := `exec \"$HERDR_PLUGIN_ROOT/bin/brain\" pane review --proposal \"$BRAIN_PROPOSAL_ID\"`
	if !strings.Contains(pane["command"], want) {
		t.Errorf("review pane command = %s, want it to contain %s", pane["command"], want)
	}
}

// E11-S2: one [[startup]] entry runs `brain brained`, with exec, so the process that
// herdr starts is brained itself.
func TestManifestStartup(t *testing.T) {
	startup := manifestTables(t, "startup")
	if len(startup) != 1 {
		t.Fatalf("%d [[startup]] entries, want 1", len(startup))
	}
	if !strings.Contains(startup[0]["command"], `exec \"$HERDR_PLUGIN_ROOT/bin/brain\" brained"`) {
		t.Errorf("startup command = %s, want it to exec bin/brain brained", startup[0]["command"])
	}
}

// E19-S3: the start action runs `brain start` from the plugin root.
func TestManifestStartAction(t *testing.T) {
	action := findTable(manifestTables(t, "actions"), "start")
	if action == nil {
		t.Fatal("no [[actions]] with id start")
	}
	if !strings.Contains(action["command"], `$HERDR_PLUGIN_ROOT/bin/brain\" start`) {
		t.Errorf("start action command = %s, want it to run bin/brain start", action["command"])
	}
}

// E20-S3 AC 1: the recall action opens the recall popup for the focused pane's workspace, and
// the popup runs `brain pane recall`, which takes that pane from the action context.
func TestManifestRecallAction(t *testing.T) {
	action := findTable(manifestTables(t, "actions"), "recall")
	if action == nil {
		t.Fatal("no [[actions]] with id recall")
	}
	if action["contexts"] != `["workspace", "pane"]` {
		t.Errorf("recall action contexts = %s, want [\"workspace\", \"pane\"]", action["contexts"])
	}
	if !strings.Contains(action["command"], `plugin pane open --plugin brain --entrypoint recall`) {
		t.Errorf("recall action command = %s, want it to open the recall pane", action["command"])
	}
	pane := findTable(manifestTables(t, "panes"), "recall")
	if pane == nil {
		t.Fatal("no [[panes]] with id recall")
	}
	if pane["placement"] != `"popup"` || !strings.Contains(pane["command"], `exec \"$HERDR_PLUGIN_ROOT/bin/brain\" pane recall"`) {
		t.Errorf("recall pane = %v, want a popup that runs bin/brain pane recall", pane)
	}
}

// E20-S11 AC 1 and #358: the actions "Brain: retired items" and "Brain: links to review" open
// their popup for the focused pane's workspace, and each popup runs `brain pane <id>`, which
// takes that pane from the action context, as the recall popup does.
func TestManifestRetiredAction(t *testing.T) {
	for id, title := range map[string]string{"retired": "Brain: retired items", "links": "Brain: links to review"} {
		action := findTable(manifestTables(t, "actions"), id)
		if action == nil {
			t.Fatalf("no [[actions]] with id %s", id)
		}
		if action["title"] != `"`+title+`"` || action["contexts"] != `["workspace", "pane"]` {
			t.Errorf("%s action = %v, want title %s in the workspace and pane contexts", id, action, title)
		}
		if !strings.Contains(action["command"], `plugin pane open --plugin brain --entrypoint `+id) {
			t.Errorf("%s action command = %s, want it to open the %s pane", id, action["command"], id)
		}
		pane := findTable(manifestTables(t, "panes"), id)
		if pane == nil {
			t.Fatalf("no [[panes]] with id %s", id)
		}
		if pane["placement"] != `"popup"` || !strings.Contains(pane["command"], `exec \"$HERDR_PLUGIN_ROOT/bin/brain\" pane `+id+`"`) {
			t.Errorf("%s pane = %v, want a popup that runs bin/brain pane %s", id, pane, id)
		}
	}
}

// E24-S7 AC 1: the actions "Brain: new Claude pane" and "Brain: new Codex pane" split the
// focused pane in the cwd of that pane, then type `brain claude` or `brain codex` into the new
// pane that herdr names in its JSON reply. The test runs each action's shell with a fake herdr
// that logs its args. The paths have spaces. A cwd with a JSON escape gives no --cwd, so herdr
// uses the cwd of the split pane. A failed split runs nothing.
func TestManifestAgentPaneActions(t *testing.T) {
	const split = "pane|split|--direction|right|--focus|"
	cases := []struct {
		name      string
		cwd       string
		failSplit bool
		wantSplit string
	}{
		{"cwd", "/work/my proj", false, split + "--cwd|/work/my proj|"},
		{"cwd with a quote", `/work/a"b`, false, split},
		{"cwd with a backslash", `/work/a\b`, false, split},
		{"failed split", "/work", true, split + "--cwd|/work|"},
	}
	for id, title := range map[string]string{"claude": "Brain: new Claude pane", "codex": "Brain: new Codex pane"} {
		action := findTable(manifestTables(t, "actions"), id)
		if action == nil {
			t.Fatalf("no [[actions]] with id %s", id)
		}
		if action["title"] != `"`+title+`"` || action["contexts"] != `["workspace", "pane"]` {
			t.Errorf("%s action = %v, want title %s in the workspace and pane contexts", id, action, title)
		}
		var argv []string
		if err := json.Unmarshal([]byte(action["command"]), &argv); err != nil {
			t.Fatalf("%s action command = %s: %v", id, action["command"], err)
		}
		for _, tc := range cases {
			t.Run(id+"/"+tc.name, func(t *testing.T) {
				root := filepath.Join(t.TempDir(), "plugin root")
				calls, err := runPaneAction(t, argv, root, tc.cwd, tc.failSplit)
				want := []string{tc.wantSplit}
				if !tc.failSplit {
					want = append(want, "pane|run|w1:p2|'"+root+"/bin/brain' "+id+"|")
				}
				if (err != nil) != tc.failSplit {
					t.Errorf("action error = %v, want an error only for a failed split", err)
				}
				if strings.Join(calls, "\n") != strings.Join(want, "\n") {
					t.Errorf("herdr calls = %q, want %q", calls, want)
				}
			})
		}
	}
}

// runPaneAction runs argv as herdr runs an action, with the focused pane w1:p1 in cwd and a fake
// herdr. It returns the fake's calls, one line of |-ended args each. The fake answers a split
// with the new pane w1:p2, or fails it with no output when failSplit is set.
func runPaneAction(t *testing.T, argv []string, root, cwd string, failSplit bool) ([]string, error) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "herdr bin")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	herdr, log := filepath.Join(dir, "herdr"), filepath.Join(dir, "calls")
	script := `#!/bin/sh
printf '%s|' "$@" >> "$LOG"; echo >> "$LOG"
[ "$1 $2" = "pane split" ] || exit 0
[ -z "$FAIL_SPLIT" ] || exit 1
echo '{"id":"cli:pane:split","result":{"pane":{"cwd":"/w","pane_id":"w1:p2","scroll":{"viewport_rows":38},"workspace_id":"w1"},"type":"pane_info"}}'
`
	if err := os.WriteFile(herdr, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	ctx, err := json.Marshal(map[string]string{"workspace_id": "w1", "focused_pane_id": "w1:p1", "focused_pane_cwd": cwd})
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "LOG=" + log, "HERDR_BIN_PATH=" + herdr,
		"HERDR_PLUGIN_ROOT=" + root, "HERDR_PANE_ID=w1:p1", "HERDR_PLUGIN_CONTEXT_JSON=" + string(ctx)}
	if failSplit {
		cmd.Env = append(cmd.Env, "FAIL_SPLIT=1")
	}
	runErr := cmd.Run()
	b, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSuffix(string(b), "\n"), "\n"), runErr
}
