package signal

import (
	"reflect"
	"testing"

	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

// #363: fileText takes only a whole call of readers that worked, in the pane text above the
// tool output, with no error of a reader in the output. The review of PR #369 found a read
// error behind a pipe, a quoted find -exec and a GNU sed e command.
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
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := fileText(tc.call, tc.cut, tc.out); got != tc.want {
				t.Errorf("fileText(%q, %v, %q) = %v, want %v", tc.call, tc.cut, tc.out, got, tc.want)
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
