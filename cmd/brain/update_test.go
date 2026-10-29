package main

import (
	"bytes"
	"io"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aryanVijaywargia/herdr-brain/internal/adapter"
	"github.com/aryanVijaywargia/herdr-brain/internal/install"
	"github.com/aryanVijaywargia/herdr-brain/internal/paths"
)

// updateHome is setupHome with a Brain plugin that is up to date: a fake herdr lists it at
// commit c1, and a fake git gives c1 as the newest commit. agy is on the PATH. The home holds the
// Brain start entry of each of kinds and no hook record entry, as after an update before #315.
func updateHome(t *testing.T, kinds ...string) string {
	t.Helper()
	home := setupHome(t, "0.9.1")
	fakeAgy(t, home)
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
	inst := testInstaller(t)
	for _, kind := range kinds {
		p, err := adapter.Load(kind)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(filepath.Join(home, strings.TrimPrefix(p.InstallTarget, "~/"))), 0o700); err != nil {
			t.Fatal(err)
		}
		if _, err := inst.Install(p); err != nil {
			t.Fatal(err)
		}
	}
	return home
}

// testInstaller is the installer of the test home, for this test binary.
func testInstaller(t *testing.T) *install.Installer {
	t.Helper()
	bin, err := paths.BinaryPath()
	if err != nil {
		t.Fatal(err)
	}
	inst, err := installer(bin)
	if err != nil {
		t.Fatal(err)
	}
	return inst
}

// runUpdate runs `brain update args...` with answers on stdin.
func runUpdate(t *testing.T, answers string, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code = run(append([]string{"update"}, args...), strings.NewReader(answers), &out, &errOut)
	return code, out.String(), errOut.String()
}

// AC #315, the M2 case: a home with only the Brain start entry of agy. brain update, up to
// date, shows its missing hook record entries and asks once, as setup does. No changes
// nothing; yes adds every entry. Then there is nothing to add, and brain update asks nothing.
// A CLI with launch data gets no hook record entry (E24-S8).
func TestUpdateHookRecords(t *testing.T) {
	home := updateHome(t, "agy")
	agy := filepath.Join(home, ".gemini", "config", "hooks.json")
	before := snapshot(t, home)

	code, out, errOut := runUpdate(t, "n\n")
	if code != 0 {
		t.Fatalf("update (no) = %d: %s\n%s", code, errOut, out)
	}
	// The text of brain update stays as it was before the short setup text (#430): one head line
	// for each CLI, then its entries, and none of the setup lines.
	for _, want := range []string{"up to date\n",
		"Hook records: the profiles of this Brain turn on hook records, and these entries are missing.\n",
		"\n  Brain also stores hook records of agy: prompts, commands and replies. Add these entries to " + agy + ":\n    ",
		"    PostToolUse: {", "Apply all of this? [y/N] ",
		"brain update wrote no hook record entry. To add them later, run brain update again.\n"} {
		if !strings.Contains(out, want) {
			t.Errorf("update (no):\n%s\nwant %q", out, want)
		}
	}
	if strings.Contains(out, "Hook records of ") || strings.Contains(out, "- Hooks:") || strings.Contains(out, "of claude") {
		t.Errorf("update (no) shows the setup text or a Claude entry:\n%s", out)
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
	if !strings.Contains(out, "Added the hook record entries for agy.\n") {
		t.Errorf("update (yes):\n%s\nwant the agy entries", out)
	}
	if got := string(mustRead(t, agy)); !strings.Contains(got, " --event tool") {
		t.Errorf("%s after yes has no tool entry:\n%s", agy, got)
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

// startGone is an answer of brain update that removes the Brain start entry of agy before
// update reads it, as you can while update waits for the answer.
type startGone struct {
	t      *testing.T
	answer io.Reader
	gone   bool
}

func (r *startGone) Read(b []byte) (int, error) {
	if !r.gone {
		r.gone = true
		p, err := adapter.Load("agy")
		if err != nil {
			r.t.Fatal(err)
		}
		if st, err := testInstaller(r.t).UninstallHook(p, p.Hooks()[0]); err != nil || st != install.Removed {
			r.t.Fatalf("UninstallHook(agy start) = %q, %v", st, err)
		}
	}
	return r.answer.Read(b)
}

// Review of PR #324 (no over-defensive code): a start entry that goes away while update waits for the
// answer gets no hook record entry.
func TestUpdateRecordsStartGone(t *testing.T) {
	home := updateHome(t, "agy")
	var out, errOut bytes.Buffer
	if code := run([]string{"update", "--records"}, &startGone{t: t, answer: strings.NewReader("y\n")}, &out, &errOut); code != 0 {
		t.Fatalf("update --records = %d: %s\n%s", code, errOut.String(), out.String())
	}
	agy := filepath.Join(home, ".gemini", "config", "hooks.json")
	if !strings.Contains(out.String(), agy+" no longer holds the Brain hook entry for agy, so brain update adds no hook record entry") {
		t.Errorf("update --records:\n%s\nwant no agy entries", out.String())
	}
	if s := string(mustRead(t, agy)); strings.Contains(s, "--event tool") {
		t.Errorf("%s has a hook record entry with no start entry:\n%s", agy, s)
	}
}

// AC #315: a CLI with no Brain start entry needs brain setup, which adds both, so brain update
// adds no hook record entry for it.
func TestUpdateRecordsNeedsStart(t *testing.T) {
	home := updateHome(t)
	before := snapshot(t, home)
	code, out, errOut := runUpdate(t, "y\n")
	if code != 0 || !strings.HasSuffix(out, "up to date\nHook records: no hook record entry to add.\n") {
		t.Errorf("update = %d, %q, %s; want nothing to add", code, out, errOut)
	}
	if after := snapshot(t, home); !maps.Equal(before, after) {
		t.Error("update with no start entry changed the home")
	}
}
