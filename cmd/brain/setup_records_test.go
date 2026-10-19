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

// Setup offers the hook record entries of Claude and Codex after their start entries, with
// one question (E11-S5, #285). A CLI version newer than tested still gets them (#251).
func TestSetupHookRecords(t *testing.T) {
	p, err := adapter.Load("claude")
	if err != nil {
		t.Fatal(err)
	}
	settings := func(home string) string { return string(mustRead(t, filepath.Join(home, ".claude", "settings.json"))) }
	codex := func(home string) string { return string(mustRead(t, filepath.Join(home, ".codex", "hooks.json"))) }

	t.Run("yes", func(t *testing.T) {
		home := setupHome(t, "0.9.1")
		fakeClaude(t, home, p.TestedVersion)
		code, out, errOut := runSetup(t, "\ny\ny\n")
		if code != 0 {
			t.Fatalf("setup = %d: %s\n%s", code, errOut, out)
		}
		for _, want := range []string{"--event prompt", "--event tool", "--event stop", `"matcher": "Bash"`,
			"PostToolUseFailure"} {
			if !strings.Contains(settings(home), want) {
				t.Errorf("settings.json does not contain %q:\n%s", want, settings(home))
			}
		}
		for _, want := range []string{"--profile codex --event prompt", "--profile codex --event tool",
			"--profile codex --event stop", `{"matcher":"Bash","hooks":[`} {
			if !strings.Contains(codex(home), want) {
				t.Errorf("hooks.json does not contain %q:\n%s", want, codex(home))
			}
		}
		if strings.Contains(codex(home), "PostToolUseFailure") {
			t.Errorf("hooks.json holds a PostToolUseFailure entry, an event that Codex does not have:\n%s", codex(home))
		}
		// #249: setup records the install time of each start entry it adds.
		for _, kind := range []string{"claude", "codex"} {
			if _, err := os.Stat(filepath.Join(home, ".local", "share", "brain", "installed", kind+".json")); err != nil {
				t.Errorf("install time of %s: %v", kind, err)
			}
		}
		for _, want := range []string{"Add these entries to ", "Added the hook record entries for claude",
			"Added the hook record entries for codex", "needs trust again"} {
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
	// AC 1 #251: the M2 case, claude 2.1.292 against tested 2.1.283.
	t.Run("newer than tested", func(t *testing.T) {
		home := setupHome(t, "0.9.1")
		fakeClaude(t, home, "2.1.292")
		code, out, errOut := runSetup(t, "\ny\ny\n")
		if code != 0 {
			t.Fatalf("setup = %d: %s\n%s", code, errOut, out)
		}
		if s := settings(home); !strings.Contains(s, "--event tool") || !strings.Contains(out, "Added the hook record entries for claude") ||
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
