package main

import (
	"context"
	"database/sql"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

// extractorOf returns the extractor setting of the main store of home.
func extractorOf(t *testing.T, home string) store.ExtractorSetting {
	t.Helper()
	ctx := context.Background()
	s, err := store.Open(ctx, filepath.Join(home, ".local", "share", "brain", "main"), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	}()
	var x store.ExtractorSetting
	if err := s.Write(ctx, func(tx *sql.Tx) (err error) {
		x, err = store.Extractor(ctx, tx)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return x
}

// AC: setup explains where the excerpts go (D9), asks which CLI extracts and its model (D4),
// and stores the choice with extraction on. A second setup keeps it and asks nothing;
// --extractor asks again.
func TestSetupExtractor(t *testing.T) {
	home := setupHome(t, "0.9.1")
	code, out, errOut := runSetup(t, "claude\nsonnet\ny\n")
	if code != 0 {
		t.Fatalf("setup = %d: %s\n%s", code, errOut, out)
	}
	for _, want := range []string{"Step 2: the extractor CLI.", "sends short", "to the provider that this CLI is set up for",
		"brain extract off [--workspace <id>]", "Extractor: claude, sonnet.", "Step 4: trust the hook in Codex."} {
		if !strings.Contains(out, want) {
			t.Errorf("output does not contain %q:\n%s", want, out)
		}
	}
	if x := extractorOf(t, home); x.CLI != store.ExtractorClaude || x.Model != "sonnet" || !x.On {
		t.Errorf("extractor = %+v, want claude, sonnet, on", x)
	}

	first := snapshot(t, home)
	code, out, errOut = runSetup(t, "")
	if code != 0 {
		t.Fatalf("second setup = %d: %s\n%s", code, errOut, out)
	}
	if strings.Contains(out, "Which CLI") || !strings.Contains(out, "Extractor: claude, sonnet. Already done. To change it: brain setup --extractor") {
		t.Errorf("the second setup asked for the extractor, or did not show it:\n%s", out)
	}
	if second := snapshot(t, home); !maps.Equal(first, second) {
		t.Errorf("the second setup changed files")
	}

	if code, out, errOut = runSetup(t, "codex\n\ny\n", "--extractor"); code != 0 {
		t.Fatalf("setup --extractor = %d: %s\n%s", code, errOut, out)
	}
	if x := extractorOf(t, home); x.CLI != store.ExtractorCodex || x.Model != "" || !x.On {
		t.Errorf("extractor after --extractor = %+v, want codex, default model, on", x)
	}

	if code, out, errOut = runSetup(t, "gemini\n", "--extractor"); code != 0 {
		t.Fatalf("setup --extractor = %d: %s\n%s", code, errOut, out)
	}
	if x := extractorOf(t, home); x.CLI != store.ExtractorCodex {
		t.Errorf("extractor after an unknown answer = %+v, want codex kept", x)
	}
	if code, out, errOut = runSetup(t, "none\ny\n", "--extractor"); code != 0 {
		t.Fatalf("setup --extractor = %d: %s\n%s", code, errOut, out)
	}
	if x := extractorOf(t, home); x.CLI != "" || x.Model != "" {
		t.Errorf("extractor after none = %+v, want no CLI: no call", x)
	}
}

// No CLI, an unknown answer, or the answer no: setup chooses no CLI. Brain never picks one by
// itself. The choice none is recorded, so a second setup asks nothing (AC 3); an unknown answer
// or the answer no makes no store file.
func TestSetupExtractorNone(t *testing.T) {
	for name, c := range map[string]struct {
		answers  string
		recorded bool
	}{
		"none":      {"none\ny\n", true},
		"no answer": {"\ny\n", true},
		"unknown":   {"gemini\ny\n", false},
		"no":        {"claude\n\nn\n", false},
	} {
		t.Run(name, func(t *testing.T) {
			home := setupHome(t, "0.9.1")
			code, out, errOut := runSetup(t, c.answers)
			if code != 0 {
				t.Fatalf("setup = %d: %s\n%s", code, errOut, out)
			}
			if strings.Contains(out, "\nExtractor: ") { // the line after a write; the plan line starts with "- "
				t.Errorf("setup chose an extractor:\n%s", out)
			}
			_, err := os.Stat(filepath.Join(home, ".local", "share", "brain", "main", store.FileName))
			if !c.recorded {
				if !os.IsNotExist(err) {
					t.Errorf("store file: %v, want none", err)
				}
				return
			}
			if x := extractorOf(t, home); x.CLI != "" || x.Changed.IsZero() {
				t.Errorf("extractor = %+v, want the choice none recorded", x)
			}
			before := snapshot(t, home)
			code, out, errOut = runSetup(t, "")
			if code != 0 || strings.Contains(out, "?") || !strings.Contains(out, "- Extractor: none. Already done.") {
				t.Errorf("second setup = %d asked a question: %s\n%s", code, errOut, out)
			}
			if after := snapshot(t, home); !maps.Equal(before, after) {
				t.Errorf("the second setup changed files")
			}
		})
	}
}

// A store whose extraction you turned off keeps it off when setup records the CLI.
func TestSetupExtractorKeepsSwitch(t *testing.T) {
	home := setupHome(t, "0.9.1")
	if code, out, errOut := runSetup(t, "\ny\n"); code != 0 {
		t.Fatalf("setup = %d: %s\n%s", code, errOut, out)
	}
	dir := filepath.Join(home, ".local", "share", "brain", "main")
	ctx := context.Background()
	s, err := store.Open(ctx, dir, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Write(ctx, func(tx *sql.Tx) error { return store.SetExtraction(ctx, tx, "", false, time.Now()) }); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	code, out, errOut := runSetup(t, "claude\n\ny\n", "--extractor")
	if code != 0 || !strings.Contains(out, "Extraction stays off for the whole store") {
		t.Fatalf("setup = %d: %s\n%s", code, errOut, out)
	}
	if x := extractorOf(t, home); x.CLI != store.ExtractorClaude || x.On {
		t.Errorf("extractor = %+v, want claude and off", x)
	}
}
