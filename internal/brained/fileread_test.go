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

// toolFailure captures one turn of harness in a pane, with screen under the prompt, and returns
// the stored tool output that holds text and whether a failure signal starts at it.
func toolFailure(t *testing.T, harness, screen, text string) (store.Observation, bool) {
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
	k.raw(base+foot, base+"\n"+prompt+"\n\n"+screen+"\n\n"+strings.Replace(base, "Earlier", "Done", 1)+foot)
	k.must(t, Baseline, "w1:p1", "", GapNone)
	k.must(t, Change, "w1:p1", "idle", GapNone)
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
// is stored, and it does not count. The eval report gives the lines, not the command above them,
// so the call lines are made: a command of readers that Codex does not fold into "Explored"
// (two files), a Codex search, and a Claude shell read.
func TestCaptureFileRead(t *testing.T) {
	for _, tc := range []struct{ name, harness, screen string }{
		{"codex README lines", "codex", "• Ran sed -n '1,40p' README.md && sed -n '1,20p' docs/API.md\n  └ " + readme},
		{"codex long command", "codex", "• Ran cd ~/w/notes-api && sed -n '1,40p' README.md 2>&1 |\n  │ head -n 40\n  └ " + readme},
		{"codex search", "codex", "• Explored\n  └ Search error|failed in README.md\n    Read README.md"},
		{"claude README lines", "claude", "⏺ Bash(cat README.md)\n  ⎿  " + strings.Replace(readme, "    ", "     ", 1)},
		{"codex passing test", "codex", "• Ran npm test\n  └ ok 1 - all note routes return 500 and log the file path when the database is missing\n" +
			"    # Subtest: GET /notes returns a JSON error when the database cannot be read\n" +
			"    ok 2 - GET /notes returns a JSON error when the database cannot be read"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			text := tc.screen[strings.LastIndex(tc.screen, "\n")+1:]
			if strings.Contains(tc.screen, "└ ") {
				text = strings.TrimSpace(tc.screen[strings.Index(tc.screen, "└ ")+len("└ "):])
				text, _, _ = strings.Cut(text, "\n")
			}
			o, failed := toolFailure(t, tc.harness, tc.screen, strings.TrimSpace(text))
			if failed {
				t.Errorf("failure signal at %q, want none", o.Excerpt)
			}
		})
	}
}

// #363 AC 1: a failed command, or an error line that a run printed, still gives a failure
// signal: a Codex command that failed, also a read of a file that is not there, a command with a
// program that is not a reader, and a Claude command that failed or whose error a pipe hides.
func TestCaptureRealFailure(t *testing.T) {
	for _, tc := range []struct{ name, harness, screen, text string }{
		{"codex test that fails", "codex", "• Failed (exit 1) npm test\n  └ not ok 2 - GET /notes returns a JSON error\n" +
			"    error: expected 500, got 200", "error: expected 500, got 200"},
		{"codex read that fails", "codex", "• Failed (exit 1) cat docs/errors.md\n  └ cat: docs/errors.md: No such file or directory",
			"No such file or directory"},
		{"codex read and a run", "codex", "• Ran sed -n '1,5p' README.md && npm start\n  └ # notes-api\n" +
			"    Error: listen EADDRINUSE: address already in use :::3000", "EADDRINUSE"},
		{"claude command that fails", "claude", "⏺ Bash(cat docs/errors.md)\n  ⎿  Error: Exit code 1\n" +
			"     cat: docs/errors.md: No such file or directory", "No such file or directory"},
		{"claude error behind a pipe", "claude", "⏺ Bash(npm test 2>&1 | tail -n 3)\n  ⎿  not ok 3 - POST /notes without a title\n" +
			"     error: expected 400, got 500", "expected 400, got 500"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o, failed := toolFailure(t, tc.harness, tc.screen, tc.text)
			if !failed || !o.Counts {
				t.Errorf("tool output %q: counts %v, failure %v; want a counted failure", o.Excerpt, o.Counts, failed)
			}
		})
	}
}

// fileRead takes only a call line in the read whose command is whole, made of readers, and
// that worked.
func TestFileRead(t *testing.T) {
	for _, tc := range []struct {
		name, harness, screen string
		want                  bool
	}{
		{"codex readers", "codex", "• Ran LC_ALL=C grep -rn 'a|b' src | sort | uniq -c; wc -l README.md\n  └ 3 src/a.js", true},
		{"codex redirect", "codex", "• Ran rg -n error docs 2>/dev/null || echo none &>/dev/null\n  └ none", true},
		{"codex later output line", "codex", "• Ran cat README.md\n  └ # notes-api\n    error words", true},
		{"codex run", "codex", "• Ran npm test\n  └ ok", false},
		{"codex failed", "codex", "• Failed (exit 1) cat README.md\n  └ cat: README.md: No such file", false},
		{"codex find that runs", "codex", "• Ran find . -name '*.test.js' -exec node {} ;\n  └ error", false},
		{"codex cut command", "codex", "• Ran cat README.md && cat docs/API.md && np…\n  └ error", false},
		{"codex new line", "codex", "• Ran cat README.md\n  │ npm test\n  └ error", false},
		{"codex edit", "codex", "• Edited README.md (+1 -0)\n  └ README.md (+1 -0)", false},
		{"codex no call line", "codex", "  └ error", false},
		{"codex prompt above", "codex", "› cat README.md\n  └ error", false},
		{"claude reader", "claude", "⏺ Bash(cd docs && cat API.md)\n  ⎿  error", true},
		{"claude long command", "claude", "⏺ Bash(sed -n '1,40p' README.md &&\n      head -n 5 docs/API.md)\n  ⎿  error", true},
		{"claude failed", "claude", "⏺ Bash(cat README.md)\n  ⎿  Error: Exit code 1", false},
		{"claude command not closed", "claude", "⏺ Bash(cat README.md\n  ⎿  error", false},
		{"claude run", "claude", "⏺ Bash(npm test)\n  ⎿  error", false},
		{"claude read tool", "claude", "⏺ Read(README.md)\n  ⎿  Read 40 lines", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lines := strings.Split(tc.screen, "\n")
			r := read{agent: tc.harness, lines: lines, senders: senders(tc.harness, lines)}
			if got := fileRead(r, len(lines)-1); got != tc.want {
				t.Errorf("fileRead(%q) = %v, want %v", tc.screen, got, tc.want)
			}
		})
	}
}
