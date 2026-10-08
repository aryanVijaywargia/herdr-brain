package main

import (
	"bytes"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aryanVijaywargia/herdr-brain/internal/adapter"
	"github.com/aryanVijaywargia/herdr-brain/internal/paths"
)

// updateHome is setupHome with a Brain plugin that is up to date: a fake herdr lists it at
// commit c1, and a fake git gives c1 as the newest commit. With start, the home holds the
// Brain start entries of Claude and Codex and no hook record entry, as after an update before
// #315.
func updateHome(t *testing.T, start bool) string {
	t.Helper()
	home := setupHome(t, "0.9.1")
	const commit = "c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1"
	list := `{"id":"cli:plugin","result":{"type":"plugin_list","plugins":[{"plugin_id":"brain","version":"0.0.1",` +
		`"plugin_root":"` + filepath.Join(home, "plugin") + `","source":{"kind":"github","owner":"acme","repo":"brain",` +
		`"resolved_commit":"` + commit + `"}}]}}`
	for name, script := range map[string]string{
		"fake-herdr/update-herdr": "#!/bin/sh\n[ \"$*\" = \"plugin list --json --plugin brain\" ] || exit 2\necho '" + list + "'\n",
		"path/git":                "#!/bin/sh\n[ \"$1\" = ls-remote ] || exit 2\nprintf '" + commit + "\\tHEAD\\n'\n",
	} {
		if err := os.WriteFile(filepath.Join(home, name), []byte(script), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HERDR_BIN_PATH", filepath.Join(home, "fake-herdr", "update-herdr"))
	if !start {
		return home
	}
	bin, err := paths.BinaryPath()
	if err != nil {
		t.Fatal(err)
	}
	inst, err := installer(bin)
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"claude", "codex"} {
		p, err := adapter.Load(kind)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := inst.Install(p); err != nil {
			t.Fatal(err)
		}
	}
	return home
}

// runUpdate runs `brain update args...` with answers on stdin.
func runUpdate(t *testing.T, answers string, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code = run(append([]string{"update"}, args...), strings.NewReader(answers), &out, &errOut)
	return code, out.String(), errOut.String()
}

// AC #315, the M2 case: a home with only the Brain start entries. brain update, up to date,
// shows the missing hook record entries of Claude and Codex in its plan and asks once, as
// setup does. No changes nothing; yes adds every entry and says that Codex needs trust. Then
// there is nothing to add, and brain update asks nothing.
func TestUpdateHookRecords(t *testing.T) {
	home := updateHome(t, true)
	settings := filepath.Join(home, ".claude", "settings.json")
	codex := filepath.Join(home, ".codex", "hooks.json")
	before := snapshot(t, home)

	code, out, errOut := runUpdate(t, "n\n")
	if code != 0 {
		t.Fatalf("update (no) = %d: %s\n%s", code, errOut, out)
	}
	for _, want := range []string{"up to date\n",
		"Hook records: the profiles of this Brain turn on hook records, and these entries are missing.\n",
		"Brain also stores hook records of claude: prompts, commands and replies. Add these entries to " + settings + ":\n",
		"    PostToolUseFailure: {", "Add these entries to " + codex + ":\n", "Apply all of this? [y/N] ",
		"brain update wrote no hook record entry. To add them later, run brain update again.\n"} {
		if !strings.Contains(out, want) {
			t.Errorf("update (no):\n%s\nwant %q", out, want)
		}
	}
	if n := strings.Count(out, "[y/N]"); n != 1 {
		t.Errorf("update (no) asked %d questions, want 1:\n%s", n, out)
	}
	if after := snapshot(t, home); !maps.Equal(before, after) {
		t.Errorf("update (no) changed the home:\n%s", out)
	}

	code, out, errOut = runUpdate(t, "y\n")
	if code != 0 {
		t.Fatalf("update (yes) = %d: %s\n%s", code, errOut, out)
	}
	for _, want := range []string{"Added the hook record entries for claude.\n", "Added the hook record entries for codex.\n",
		"Codex needs trust again for each new hook entry"} {
		if !strings.Contains(out, want) {
			t.Errorf("update (yes):\n%s\nwant %q", out, want)
		}
	}
	for file, events := range map[string][]string{settings: {"prompt", "tool", "stop"}, codex: {"prompt", "tool", "stop"}} {
		got := string(mustRead(t, file))
		for _, e := range events {
			if !strings.Contains(got, " --event "+e) {
				t.Errorf("%s after yes has no %s entry:\n%s", file, e, got)
			}
		}
	}

	before = snapshot(t, home)
	code, out, _ = runUpdate(t, "", "--records")
	if code != 0 || out != "Hook records: no hook record entry to add.\n" {
		t.Errorf("update --records after yes = %d, %q; want nothing to add", code, out)
	}
	if after := snapshot(t, home); !maps.Equal(before, after) {
		t.Error("update --records with nothing to add changed the home")
	}
}

// AC #315: a CLI with no Brain start entry needs brain setup, which adds both, so brain update
// adds no hook record entry for it.
func TestUpdateRecordsNeedsStart(t *testing.T) {
	home := updateHome(t, false)
	before := snapshot(t, home)
	code, out, errOut := runUpdate(t, "y\n")
	if code != 0 || !strings.HasSuffix(out, "up to date\nHook records: no hook record entry to add.\n") {
		t.Errorf("update = %d, %q, %s; want nothing to add", code, out, errOut)
	}
	if after := snapshot(t, home); !maps.Equal(before, after) {
		t.Error("update with no start entry changed the home")
	}
}
