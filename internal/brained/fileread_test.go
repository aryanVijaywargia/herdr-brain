package brained

import (
	"strings"
	"testing"

	"github.com/aryanVijaywargia/herdr-brain/internal/signal"
	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

// The README lines of proposal 16 of the M2 eval run 5 (2026-10-09, codex 0.160.1): the agent
// printed them in 2 sessions, and their error words made a failure.
const readme = "All note endpoints return 500 with {\"error\":\"internal error\"} when the database cannot be read.\n" +
	"    Database errors are logged at error level with the file path."

// toolFailure captures one turn of harness in a pane, with the screens under the prompt, one
// read each, and returns the stored tool output that holds text and whether a failure signal
// starts at it.
func toolFailure(t *testing.T, harness, text string, screens ...string) (store.Observation, bool) {
	t.Helper()
	var base, prompt, foot string
	session := ""
	switch harness {
	case "codex":
		base, prompt, foot, session = "• Earlier.", "› Check how the API handles database errors.", "\n\n  gpt-6.1-sol high · ~/w/notes-api\n", codexRun2
	case "claude":
		base, prompt, foot = "⏺ Earlier.", "❯ Check how the API handles database errors.", "\n"
	}
	k := newCapture(t, "s1", t.TempDir())
	k.pane("w1:p1", harness, session)
	texts := []string{base + foot}
	for i, sc := range screens {
		if i == len(screens)-1 {
			sc += "\n\n" + strings.Replace(base, "Earlier", "Done", 1)
		}
		texts = append(texts, base+"\n"+prompt+"\n\n"+sc+foot)
	}
	k.raw(texts...)
	k.must(t, Baseline, "w1:p1", "", GapNone)
	for i := range screens {
		status := "working"
		if i == len(screens)-1 {
			status = "idle"
		}
		k.must(t, Change, "w1:p1", status, GapNone)
	}
	var tool store.Observation
	for _, o := range k.rows(t).obs {
		if o.Sender == store.SenderTool && strings.Contains(o.Excerpt, text) {
			tool = o
		}
	}
	if tool.ID == 0 {
		t.Fatalf("no tool output holds %q", text)
	}
	sigs, _ := k.signals(t)
	for _, s := range sigs {
		if s.Kind == signal.Failure && s.Observations[0] == tool.ID {
			return tool, true
		}
	}
	return tool, false
}

// #363 AC 1 (M2 eval run 5, R5-3): text of files that the agent printed, as the README lines of
// proposal 16 under the Codex tool mark, and a passing test, give no failure signal. The output
// is stored and still counts, so its read stays evidence of the agent's state (§10.4, review of
// PR #369). The eval report gives the lines, not the command above them, so the call lines are
// made: a command of readers that Codex does not fold into "Explored" (two files), a Codex
// search, a Claude shell read, and a Claude read whose call line came in the read before.
func TestCaptureFileRead(t *testing.T) {
	for _, tc := range []struct {
		name, harness, text string
		screens             []string
	}{
		{"codex README lines", "codex", "All note endpoints",
			[]string{"• Ran sed -n '1,40p' README.md && sed -n '1,20p' docs/API.md\n  └ " + readme}},
		{"codex long command", "codex", "All note endpoints",
			[]string{"• Ran cd ~/w/notes-api && sed -n '1,40p' README.md 2>&1 |\n  │ head -n 40\n  └ " + readme}},
		{"codex search", "codex", "Search error", []string{"• Explored\n  └ Search error|failed in README.md\n    Read README.md"}},
		{"claude README lines", "claude", "All note endpoints",
			[]string{"⏺ Bash(cat README.md)\n  ⎿  " + strings.Replace(readme, "    ", "     ", 1)}},
		{"claude call line before", "claude", "All note endpoints", []string{"⏺ Bash(cat README.md)\n  ⎿  Running…",
			"⏺ Bash(cat README.md)\n  ⎿  " + strings.Replace(readme, "    ", "     ", 1)}},
		{"codex passing test", "codex", "ok 1 -", []string{"• Ran npm test\n" +
			"  └ ok 1 - all note routes return 500 and log the file path when the database is missing\n" +
			"    # Subtest: GET /notes returns a JSON error when the database cannot be read\n" +
			"    ok 2 - GET /notes returns a JSON error when the database cannot be read"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o, failed := toolFailure(t, tc.harness, tc.text, tc.screens...)
			if failed || !o.Counts {
				t.Errorf("tool output %q: counts %v, failure %v; want a counted output with no failure", o.Excerpt, o.Counts, failed)
			}
		})
	}
}

// #363 AC 1: a failed command, or an error line that a run printed, still gives a failure
// signal: a Codex command that failed, also a read of a file that is not there, a read whose
// error a pipe hides (review of PR #369), a command with a program that is not a reader, and a
// Claude command that failed or whose error a pipe hides.
func TestCaptureRealFailure(t *testing.T) {
	for _, tc := range []struct{ name, harness, screen, text string }{
		{"codex test that fails", "codex", "• Failed (exit 1) npm test\n  └ not ok 2 - GET /notes returns a JSON error\n" +
			"    error: expected 500, got 200", "error: expected 500, got 200"},
		{"codex read that fails", "codex", "• Failed (exit 1) cat docs/errors.md\n  └ cat: docs/errors.md: No such file or directory",
			"No such file or directory"},
		{"codex read error behind a pipe", "codex", "• Ran cat missing-file 2>&1 | head || echo done\n" +
			"  └ cat: missing-file: No such file or directory", "No such file or directory"},
		{"codex read and a run", "codex", "• Ran sed -n '1,5p' README.md && npm start\n  └ # notes-api\n" +
			"    Error: listen EADDRINUSE: address already in use :::3000", "EADDRINUSE"},
		{"claude command that fails", "claude", "⏺ Bash(cat docs/errors.md)\n  ⎿  Error: Exit code 1\n" +
			"     cat: docs/errors.md: No such file or directory", "No such file or directory"},
		{"claude error behind a pipe", "claude", "⏺ Bash(npm test 2>&1 | tail -n 3)\n  ⎿  not ok 3 - POST /notes without a title\n" +
			"     error: expected 400, got 500", "expected 400, got 500"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o, failed := toolFailure(t, tc.harness, tc.text, tc.screen)
			if !failed || !o.Counts {
				t.Errorf("tool output %q: counts %v, failure %v; want a counted failure", o.Excerpt, o.Counts, failed)
			}
		})
	}
}
