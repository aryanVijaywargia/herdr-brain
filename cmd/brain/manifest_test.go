package main

import (
	"os"
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

// D4: one [[events]] entry, for workspace.closed, runs `brain event workspace-closed`.
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

// D6 (E5-S3): the review action opens the queue split, and the queue runs `brain review`.
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
