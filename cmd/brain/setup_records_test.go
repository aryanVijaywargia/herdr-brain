package main

import (
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"

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

// Setup offers the hook record entries of Claude after its start entry, with one question
// (E11-S5). Codex keeps them off. A CLI version newer than tested still gets them (D13, #251).
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
		code, out, errOut := runSetup(t, "\ny\n")
		if code != 0 {
			t.Fatalf("setup = %d: %s\n%s", code, errOut, out)
		}
		for _, want := range []string{"--event prompt", "--event tool", "--event stop", `"matcher": "Bash"`,
			"PostToolUseFailure"} {
			if !strings.Contains(settings(home), want) {
				t.Errorf("settings.json does not contain %q:\n%s", want, settings(home))
			}
		}
		if strings.Contains(codex(home), "--event tool") {
			t.Errorf("hooks.json holds a Codex hook record entry:\n%s", codex(home))
		}
		// #249: setup records the install time of each start entry it adds.
		for _, kind := range []string{"claude", "codex"} {
			if _, err := os.Stat(filepath.Join(home, ".local", "share", "brain", "installed", kind+".json")); err != nil {
				t.Errorf("install time of %s: %v", kind, err)
			}
		}
		for _, want := range []string{"Add these entries to ", "Added the hook record entries for claude",
			"needs trust again"} {
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
		code, out, errOut := runSetup(t, "\ny\n")
		if code != 0 {
			t.Fatalf("setup = %d: %s\n%s", code, errOut, out)
		}
		if s := settings(home); !strings.Contains(s, "--event tool") || !strings.Contains(out, "Added the hook record entries for claude") ||
			strings.Contains(out, "retest") {
			t.Errorf("setup on a newer version did not add the hook records:\n%s\n%s", out, s)
		}
	})
}
