package main

import (
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aryanVijaywargia/herdr-brain/internal/adapter"
)

// fakeClaude makes the fake claude CLI answer `--version` with version.
func fakeClaude(t *testing.T, home, version string) {
	t.Helper()
	script := "#!/bin/sh\necho '" + version + " (fake)'\n"
	if err := os.WriteFile(filepath.Join(home, "path", "claude"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}

// Setup offers the hook record entries of a CLI with no launch data after its start entry, with
// one question (E11-S5, #285). A CLI version newer than tested still gets them (#251). Claude
// and Codex have launch data, so they get no global entry (E24-S8).
func TestSetupHookRecords(t *testing.T) {
	agy := func(home string) string {
		return string(mustRead(t, filepath.Join(home, ".gemini", "config", "hooks.json")))
	}

	t.Run("yes", func(t *testing.T) {
		home := setupHome(t, "0.9.1")
		fakeAgy(t, home)
		code, out, errOut := runSetup(t, "\ny\ny\n")
		if code != 0 {
			t.Fatalf("setup = %d: %s\n%s", code, errOut, out)
		}
		for _, want := range []string{"--profile agy --event start", "--profile agy --event tool", `"matcher":"run_command"`} {
			if !strings.Contains(agy(home), want) {
				t.Errorf("agy hooks.json does not contain %q:\n%s", want, agy(home))
			}
		}
		// #249: setup records the install time of each start entry it adds.
		if _, err := os.Stat(filepath.Join(home, ".local", "share", "brain", "installed", "agy.json")); err != nil {
			t.Errorf("install time of agy: %v", err)
		}
		for _, want := range []string{"Add these entries to ", "Added the hook record entries for agy"} {
			if !strings.Contains(out, want) {
				t.Errorf("output does not contain %q:\n%s", want, out)
			}
		}
		before := snapshot(t, home)
		if code, out, _ := runSetup(t, ""); code != 0 || strings.Contains(out, "[y/N]") {
			t.Errorf("second setup = %d and asked a question:\n%s", code, out)
		}
		if after := snapshot(t, home); !maps.Equal(before, after) {
			for k := range after {
				if before[k] != after[k] {
					t.Errorf("the second setup changed %s", k)
				}
			}
		}
	})
	// AC 1 #251: a CLI version newer than tested.
	t.Run("newer than tested", func(t *testing.T) {
		home := setupHome(t, "0.9.1")
		p, err := adapter.Load("agy")
		if err != nil {
			t.Fatal(err)
		}
		script := "#!/bin/sh\necho '" + p.TestedVersion + "9'\n"
		if err := os.WriteFile(filepath.Join(home, "path", "agy"), []byte(script), 0o755); err != nil { //nolint:gosec // a test CLI must be executable
			t.Fatal(err)
		}
		code, out, errOut := runSetup(t, "\ny\ny\n")
		if code != 0 {
			t.Fatalf("setup = %d: %s\n%s", code, errOut, out)
		}
		if s := agy(home); !strings.Contains(s, "--event tool") || !strings.Contains(out, "Added the hook record entries for agy") ||
			strings.Contains(out, "retest") {
			t.Errorf("setup on a newer version did not add the hook records:\n%s\n%s", out, s)
		}
	})
}

// #249 (coordinator check on PR #262): the install time of a start entry changes only when
// Brain adds a new start entry for that CLI. An entry that is present keeps its time, and the
// install of another CLI, which writes install.json too, does not touch it.
func TestAddStartInstallTime(t *testing.T) {
	home := setupHome(t, "0.9.1")
	inst, err := installer("/opt/brain/bin/brain")
	if err != nil {
		t.Fatal(err)
	}
	load := func(kind string) *adapter.Profile {
		t.Helper()
		p, err := adapter.Load(kind)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	claude, codex := load("claude"), load("codex")
	timeFile := filepath.Join(home, ".local", "share", "brain", "installed", "claude.json")
	first := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	if added, err := addStart(inst, claude, filepath.Join(home, ".claude", "settings.json"), first); err != nil || !added {
		t.Fatalf("addStart(claude) = %v, %v, want added", added, err)
	}
	want := string(mustRead(t, timeFile))
	if !strings.Contains(want, "2026-10-06T12:00:00Z") {
		t.Fatalf("install time of claude = %s, want 2026-10-06T12:00:00Z", want)
	}
	later := first.Add(24 * time.Hour)
	if added, err := addStart(inst, claude, filepath.Join(home, ".claude", "settings.json"), later); err != nil || !added {
		t.Fatalf("addStart(claude, present) = %v, %v, want no error", added, err)
	}
	if _, err := addStart(inst, codex, filepath.Join(home, ".codex", "hooks.json"), later); err != nil {
		t.Fatal(err)
	}
	if got := string(mustRead(t, timeFile)); got != want {
		t.Errorf("install time of claude after a present entry and a codex install = %s, want %s", got, want)
	}
}
