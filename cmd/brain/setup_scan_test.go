package main

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

const scanQuestion = "Read your past Claude and Codex sessions once, to find repeats now? [y/N] "

// scanOf returns the scan row of the store in folder dir, and whether it has one.
func scanOf(t *testing.T, dir string) (store.Scan, bool) {
	t.Helper()
	sc, ok, err := readScan(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	return sc, ok
}

func mainStoreOf(home string) string { return filepath.Join(home, ".local", "share", "brain", "main") }

// AC 2: after the extractor choice, setup says what the scan reads, keeps and sends, and asks
// the history question. The answer is one plan line, and setup asks "Apply all of this?" once.
// The yes of the plan writes one pending row. AC 3: a store with a row asks nothing and shows
// its state, also for another real session on the main store.
func TestSetupScanYes(t *testing.T) {
	home := setupHome(t, "0.9.1")
	code, out, errOut := runSetup(t, "\ny\ny\n")
	if code != 0 {
		t.Fatalf("setup = %d: %s\n%s", code, errOut, out)
	}
	extractor, question := strings.Index(out, "Which CLI runs the extractor"), strings.Index(out, scanQuestion)
	if extractor < 0 || question < extractor {
		t.Errorf("setup did not ask the history question after the extractor choice:\n%s", out)
	}
	for _, want := range []string{"Step 3: past sessions\n", "~/.claude and ~/.codex", "newest 300 files of the last 30 days",
		"It keeps short excerpts", "token counts", "secret filter", "no whole transcript and no file path",
		"Only extractor calls send excerpts off this machine", "brain forget --session <id>",
		"- History scan: record your yes in the store " + mainStoreOf(home),
		"History scan: brained reads your past sessions at its next tick."} {
		if !strings.Contains(out, want) {
			t.Errorf("output does not contain %q:\n%s", want, out)
		}
	}
	if n := strings.Count(out, "Apply all of this?"); n != 1 {
		t.Errorf("setup asked %d times to apply its plan, want 1:\n%s", n, out)
	}
	sc, ok := scanOf(t, mainStoreOf(home))
	if !ok || sc.Status != store.ScanPending || !sc.Claimed.IsZero() {
		t.Fatalf("scan row = %+v, %v; want one pending row with no claim", sc, ok)
	}

	for _, args := range [][]string{nil, {"--session", "brain-test2"}} {
		code, out, errOut = runSetup(t, "y\n", args...)
		if code != 0 {
			t.Fatalf("setup %v = %d: %s\n%s", args, code, errOut, out)
		}
		if strings.Contains(out, scanQuestion) || !strings.Contains(out, "- History scan: you said yes on ") {
			t.Errorf("setup %v asked the history question, or did not show the state:\n%s", args, out)
		}
	}

	finished := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	s, err := store.Open(context.Background(), mainStoreOf(home), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Write(context.Background(), func(tx *sql.Tx) error {
		return store.SetScanStatus(context.Background(), tx, sc.ID, store.ScanDone, finished)
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	code, out, errOut = runSetup(t, "")
	want := "- History scan: done on " + finished.Local().Format(time.DateOnly) + "."
	if code != 0 || strings.Contains(out, "[y/N]") || !strings.Contains(out, want) {
		t.Errorf("setup after a done scan = %d, want no question and %q: %s\n%s", code, want, errOut, out)
	}
}

// AC 2, S85: a "no" to the history question, or to the plan, writes no scan row, and the next
// setup asks again.
func TestSetupScanNo(t *testing.T) {
	for name, c := range map[string]struct {
		answers, line string
	}{
		"no to the question": {"\nn\ny\n", "- History scan: not now. The next brain setup asks again."},
		"no to the plan":     {"\ny\nn\n", "- History scan: record your yes"},
	} {
		t.Run(name, func(t *testing.T) {
			home := setupHome(t, "0.9.1")
			code, out, errOut := runSetup(t, c.answers)
			if code != 0 || !strings.Contains(out, c.line) {
				t.Fatalf("setup = %d, want the plan line %q: %s\n%s", code, c.line, errOut, out)
			}
			if sc, ok := scanOf(t, mainStoreOf(home)); ok {
				t.Errorf("scan row = %+v, want none", sc)
			}
			if code, out, errOut := runSetup(t, ""); code != 0 || !strings.Contains(out, scanQuestion) {
				t.Errorf("next setup = %d did not ask again: %s\n%s", code, errOut, out)
			}
		})
	}
}

// AC 3, S87: a test binding never gets the history question, and its store gets no scan row.
func TestSetupScanTestBinding(t *testing.T) {
	setupHome(t, "0.9.1")
	folder := filepath.Join(t.TempDir(), "test-store")
	code, out, errOut := runSetup(t, "\ny\n", "--session", "brain-test", "--test", folder)
	if code != 0 {
		t.Fatalf("setup --test = %d: %s\n%s", code, errOut, out)
	}
	if strings.Contains(out, scanQuestion) || strings.Contains(out, "History scan") {
		t.Errorf("setup --test asked the history question or showed a scan line:\n%s", out)
	}
	if sc, ok := scanOf(t, folder); ok {
		t.Errorf("test store scan row = %+v, want none", sc)
	}
}
