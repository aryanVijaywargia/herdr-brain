package signal

import (
	"reflect"
	"testing"

	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

// #363: noFailure takes a whole call of readers that worked, in the pane text above the tool
// output, with no error of a reader in the output. The review of PR #369 found a read error
// behind a pipe, a quoted find -exec and a GNU sed e command. #375: it also takes a call of
// readers that failed with status 1 because a search found no match. #376: it takes a call of
// brain, also one that Brain refused, but not brain run, which runs a tool of the project.
func TestFileText(t *testing.T) {
	for _, tc := range []struct {
		name, call, out string
		cut, want       bool
	}{
		{"codex readers", "• Ran LC_ALL=C grep -rn 'a|b' src | sort | uniq -c; wc -l README.md", "  └ 3 src/a.js", false, true},
		{"codex redirect", "• Ran rg -n error docs 2>/dev/null || echo none &>/dev/null", "  └ none", false, true},
		{"codex sed lines", "• Ran sed -n '1,40p;$p' README.md && sed -n -e 10p docs/API.md", "  └ error", false, true},
		{"reply above the call", "• I will read it.\n• Ran cat README.md", "  └ error", false, true},
		{"codex long command", "• Ran cat README.md &&\n  │ head -n 5 docs/API.md", "  └ error", false, true},
		{"codex exploring", "• Exploring", "  └ Search error in src", false, true},
		{"claude reader", "⏺ Bash(cd docs && cat API.md)", "  ⎿  error", false, true},
		{"claude long command", "⏺ Bash(sed -n '1,40p' README.md &&\n      head -n 5 docs/API.md)", "  ⎿  error", false, true},
		{"codex run", "• Ran npm test", "  └ error", false, false},
		{"codex failed", "• Failed (exit 1) cat README.md", "  └ cat: README.md: No such file", false, false},
		{"read error behind a pipe", "• Ran cat missing-file 2>&1 | head || echo done",
			"  └ cat: missing-file: No such file or directory", false, false},
		{"shell error", "• Ran cd missing; cat README.md", "  └ zsh:1: no such file or directory: missing\n    text", false, false},
		{"find that runs", "• Ran find . -name '*.test.js' -exec node {} ';'", "  └ error", false, false},
		{"quoted find -exec", "• Ran find . -name '*.test.js' '-exec' node {} ';'", "  └ error", false, false},
		{"escaped find -exec", `• Ran find . -name x \-execdir node {} +`, "  └ error", false, false},
		{"sed e command", "• Ran sed -n '1e npm test' README.md", "  └ error", false, false},
		{"sed script file", "• Ran sed -n -f run.sed README.md", "  └ error", false, false},
		{"rg preprocessor", "• Ran rg --pre ./run.sh error docs", "  └ error", false, false},
		{"substitution", `• Ran echo "$(npm test)"`, "  └ error", false, false},
		{"backquote", "• Ran cat `npm bin`/x", "  └ error", false, false},
		{"variable", "• Ran find . $FLAGS", "  └ error", false, false},
		{"process substitution", "• Ran cat <(npm test)", "  └ error", false, false},
		{"awk", "• Ran awk 'NR<5' README.md", "  └ error", false, false},
		{"cut command", "• Ran cat README.md && cat docs/API.md && np…", "  └ error", false, false},
		{"new line", "• Ran cat README.md\n  │ npm test", "  └ error", false, false},
		{"cut call", "• Ran cat README.md", "  └ error", true, false},
		{"codex edit", "• Edited README.md (+1 -0)", "  └ README.md (+1 -0)", false, false},
		{"prompt above", "› cat README.md", "  └ error", false, false},
		{"claude failed", "⏺ Bash(cat README.md)", "  ⎿  Error: Exit code 1", false, false},
		{"claude command not closed", "⏺ Bash(cat README.md", "  ⎿  error", false, false},
		{"claude run", "⏺ Bash(npm test)", "  ⎿  error", false, false},
		{"run 6 read chain with no match", "• Failed (exit 1) cat package.json CHANGELOG.md README.md && rg --files -g 'AGENTS.md' -g '*lock*'",
			"  └ " + run6Readme, false, true},
		{"grep with no match", "• Failed (exit 1) grep -rn 'error' src | head -n 20", "  └ (no output)", false, true},
		{"claude read chain with no match", "⏺ Bash(cat README.md && rg --files -g 'AGENTS.md')",
			"  ⎿  Error: Exit code 1\n     " + run6Readme, false, true},
		{"search error", "• Failed (exit 2) rg -n error docs", "  └ rg: docs: IO error for operation on docs: No such file or directory",
			false, false},
		{"search exit 2", "• Failed (exit 2) rg -n error docs", "  └ error", false, false},
		{"reader exit 1 with no search", "• Failed (exit 1) cat README.md && head -n 5 docs/API.md", "  └ error", false, false},
		{"search and a run", "• Failed (exit 1) npm test && rg --files -g 'AGENTS.md'", "  └ error", false, false},
		{"claude error with no status", "⏺ Bash(rg --files -g 'AGENTS.md')", "  ⎿  Error: error", false, false},
		{"claude exit 2", "⏺ Bash(rg -n error docs)", "  ⎿  Error: Exit code 2\n     error", false, false},
		{"run 6 brain recall and status", "• Ran brain recall endpoint && brain status", "  └ " + run6Brain, false, true},
		{"brain refusal", "• Failed (exit 1) brain review 9 --approve", "  └ " + run6Refusal, false, true},
		{"claude brain refusal", "⏺ Bash(brain review 9 --approve)", "  ⎿  Error: Exit code 1\n     " + run6Refusal, false, true},
		{"brain and a reader", "• Ran ./bin/brain status | grep -E 'failed batches'", "  └ failed batches: 3", false, true},
		{"brain run", "• Failed (exit 1) brain run test", "  └ error", false, false},
		{"brain automation launch", "• Ran brain automation launch --run 3", "  └ error", false, false},
		{"brain and a run", "• Failed (exit 1) brain status && npm test", "  └ error", false, false},
		{"brain and a read error", "• Failed (exit 1) brain status && cat notes.md", "  └ cat: notes.md: No such file or directory",
			false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := noFailure(tc.call, tc.cut, tc.out); got != tc.want {
				t.Errorf("noFailure(%q, %v, %q) = %v, want %v", tc.call, tc.cut, tc.out, got, tc.want)
			}
		})
	}
}

// #363: the failure rule reads the call line in the last pane text of the segment that is no
// tool output: a tool part of the same call in between, or a hook record, does not hide it,
// and the call of another segment does not count.
func TestFailureFileText(t *testing.T) {
	seg := func(in store.SignalInput, s int64) store.SignalInput { in.SegmentID = s; return in }
	hook := with(obs(3, 1, assistant, "Done."), func(in *store.SignalInput) { in.Source = store.SourceHook })
	sigs := Find([]store.SignalInput{seg(obs(1, 1, assistant, "⏺ Bash(cat README.md)"), 1),
		seg(obs(2, 1, tool, "  ⎿  Running…"), 1), seg(hook, 1), seg(obs(4, 1, tool, "  ⎿  Database errors are logged at error level."), 1),
		seg(obs(5, 2, tool, "  ⎿  Database errors are logged at error level."), 2)})
	if got := ofKind(sigs, Failure); !reflect.DeepEqual(got, [][]int64{{5}}) {
		t.Errorf("Find failure = %v, want [[5]]: only the output with no call above it in its segment", got)
	}
}

// The pane text of the M2 eval run 6 (2026-10-10, codex 0.160.1, #375 and #376). The eval report
// gives the claims and the commands, so the lines are made from them.
const (
	// run6Readme holds README lines of proposal 5: codex printed them in sessions 4 and 7 with
	// cat ... && rg --files -g 'AGENTS.md' ..., and rg found no AGENTS.md.
	run6Readme = "# notes-api\n    DELETE /notes/:id returns 204 and an empty body.\n" +
		"    Errors contain a JSON error field, as {\"error\":\"not found\"}."
	// run6Brain holds the output of brain recall endpoint && brain status of candidate 488.
	run6Brain = "Brain items for \"endpoint\", best match first:\n" +
		"    - [2] Every endpoint returns a JSON error case with a 4xx status\n" +
		"    last 24h: 29 of 40 extractor calls, 6 of 15 new proposals\n" +
		"    waiting in workspace notes-api-w: 1 batch; the last codex call failed: codex: exit status 1\n" +
		"    failed batches: 3; their evidence waits for a retry"
	// run6Refusal is the #371 refusal of the same turn.
	run6Refusal = "brain review: pane w1:p1 runs an agent (codex), so brain review does not approve or reject " +
		"here: decide in the review pane or in a shell pane; nothing was written"
)

// #375 AC 1 to 3 (M2 eval run 6, R6-1): a read chain whose only failure is a search with no
// match, a git commit that worked with "error" in its message, and rg lines of a docs file give
// no failure signal. A real failure still does: a test run or a read that fails, a search error,
// and the error line of a commit hook.
func TestFailureNoMatch(t *testing.T) {
	seg := func(in store.SignalInput, s int64) store.SignalInput { in.SegmentID = s; return in }
	call := func(id, s int64, line string) store.SignalInput { return seg(obs(id, s, assistant, line), s) }
	out := func(id, s int64, text string) store.SignalInput { return seg(obs(id, s, tool, text), s) }
	runRule(t, Failure, []ruleCase{
		{"run 6 read chain", []store.SignalInput{
			call(1, 1, "• Failed (exit 1) cat package.json CHANGELOG.md README.md && rg --files -g 'AGENTS.md' -g '*lock*'"),
			out(2, 1, "  └ "+run6Readme)}, nil},
		{"run 6 git commit", []store.SignalInput{
			call(1, 1, "• Ran git add -A && git commit -m 'Return a JSON error for a missing note'"),
			out(2, 1, "  └ [main 3f2a1bc] Return a JSON error for a missing note\n"+
				"     2 files changed, 14 insertions(+), 2 deletions(-)\n     create mode 100644 test/error.test.js")}, nil},
		{"first commit", []store.SignalInput{out(1, 1, "  ⎿  [main (root-commit) 3f2a1bc] Add the error handler")}, nil},
		{"rename in a commit", []store.SignalInput{out(1, 1, "     rename src/{errors.js => error.js} (100%)")}, nil},
		{"run 6 rg docs lines", []store.SignalInput{call(1, 1, "• Ran rg -n 'error|404' docs/API.md"),
			out(2, 1, "  └ 12:Errors contain a JSON error field.\n    18:DELETE /notes/:id returns 404 when the note is not found.")},
			nil},
		{"rg docs lines with no match after", []store.SignalInput{
			call(1, 1, "• Failed (exit 1) rg -n 'error' docs/API.md && rg -n 'TODO' src"),
			out(2, 1, "  └ 12:Errors contain a JSON error field.")}, nil},
		{"test run that fails", []store.SignalInput{call(1, 1, "• Failed (exit 1) npm test"),
			out(2, 1, "  └ not ok 2 - DELETE /notes/:id\n    error: expected 204, got 500")}, [][]int64{{2}}},
		{"read that fails in the chain", []store.SignalInput{
			call(1, 1, "• Failed (exit 1) cat package.json README.md && rg --files -g 'AGENTS.md'"),
			out(2, 1, "  └ cat: README.md: No such file or directory")}, [][]int64{{2}}},
		{"search error", []store.SignalInput{call(1, 1, "• Failed (exit 2) rg -n error docs"),
			out(2, 1, "  └ rg: docs: IO error for operation on docs: No such file or directory")}, [][]int64{{2}}},
		{"commit hook error", []store.SignalInput{call(1, 1, "• Failed (exit 1) git commit -m 'Fix the error case'"),
			out(2, 1, "  └ error: 2 lint errors in src/app.js")}, [][]int64{{2}}},
		{"git log line", []store.SignalInput{out(1, 1, "  └ 3f2a1bc fatal: the build broke")}, [][]int64{{1}}},
	})
}

// #376 AC 1 and 2 (M2 eval run 6, R6-2): the output of brain commands in an agent pane, the
// recall items and status lines of candidate 488, and the #371 refusal give no failure signal:
// in the pane under their call, as a Brain line with no call above it, and as a hook command
// record that failed. brain run runs a tool of the project, so its failure still gives one.
func TestFailureBrainOutput(t *testing.T) {
	seg := func(in store.SignalInput, s int64) store.SignalInput { in.SegmentID = s; return in }
	call := func(id, s int64, line string) store.SignalInput { return seg(obs(id, s, assistant, line), s) }
	out := func(id, s int64, text string) store.SignalInput { return seg(obs(id, s, tool, text), s) }
	runRule(t, Failure, []ruleCase{
		{"run 6 recall and status", []store.SignalInput{call(1, 1, "• Ran brain recall endpoint && brain status"),
			out(2, 1, "  └ "+run6Brain)}, nil},
		{"run 6 refusal", []store.SignalInput{call(1, 1, "• Failed (exit 1) brain review 9 --approve"),
			out(2, 1, "  └ "+run6Refusal)}, nil},
		{"refusal with no call", []store.SignalInput{out(1, 1, "  └ "+run6Refusal)}, nil},
		{"brain warning", []store.SignalInput{out(1, 1, "  ⎿  brain: warning: no Brain receipt for the agent session of this "+
			"pane; this recall is not recorded")}, nil},
		{"refusal command record", []store.SignalInput{cmd(1, 1, "brain review 9 --approve", 1)}, nil},
		{"brain run that fails", []store.SignalInput{call(1, 1, "• Failed (exit 1) brain run test"),
			out(2, 1, "  └ not ok 1 - GET /notes\n    brain run: step 1: npm: exit status 1")}, [][]int64{{2}}},
		{"brain run error line", []store.SignalInput{out(1, 1, "  └ brain run: step 1: npm: exit status 1")}, [][]int64{{1}}},
		{"brain run command record", []store.SignalInput{cmd(1, 1, "brain run test", 1)}, [][]int64{{1}}},
		{"brain and a test run", []store.SignalInput{cmd(1, 1, "brain status && npm test", 1)}, [][]int64{{1}}},
	})
}
