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
