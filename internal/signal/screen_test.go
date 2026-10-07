package signal

import (
	"reflect"
	"strings"
	"testing"

	"github.com/aryanVijaywargia/herdr-brain/internal/adapter"
	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

// #291 AC 2: a code diff line that wraps onto a line with no number, with "error:" in its
// text, is tool text under a tool mark. The screen rules of the shipped profiles drop it, so
// it starts no failure signal. A real failure line in the next tool output still starts one.
// The screens are made from the PR #288 review: no wrap is recorded.
func TestFailureDiffWrap(t *testing.T) {
	for _, tc := range []struct {
		harness, mark, screen string
	}{
		{"claude", "  ⎿", strings.Join([]string{
			"⏺ Update(src/server.js)",
			"  ⎿  Updated src/server.js with 1 addition",
			"      58 +      return badRequest(req, res, 'the request body has no title',",
			"         + 'error: missing title');",
			"",
			"⏺ Bash(npm test)",
			"  ⎿  Error: Exit code 1",
			"     not ok 3 - POST /notes without a title",
		}, "\n")},
		{"codex", "  └", strings.Join([]string{
			"• Edited src/server.js (+1 -0)",
			"  └ src/server.js (+1 -0)",
			"    58 +      return badRequest(req, res, 'the request body has no title',",
			"              'error: missing title');",
			"",
			"• Ran npm test",
			"  └ not ok 3 - POST /notes without a title",
			"    error: expected 400, got 500",
		}, "\n")},
	} {
		t.Run(tc.harness, func(t *testing.T) {
			p, err := adapter.Load(tc.harness)
			if err != nil {
				t.Fatal(err)
			}
			raw := toolTexts(strings.Split(tc.screen, "\n"), tc.mark)
			if got := ofKind(Find(raw), Failure); !reflect.DeepEqual(got, [][]int64{{1}, {2}}) {
				t.Fatalf("Find Failure of the unfiltered screen = %v, want [[1] [2]]: the wrapped line must hold an error", got)
			}
			if got := ofKind(Find(toolTexts(p.Screen(tc.screen), tc.mark)), Failure); !reflect.DeepEqual(got, [][]int64{{2}}) {
				t.Errorf("Find Failure = %v, want [[2]]: only the real failure", got)
			}
		})
	}
}

// toolTexts returns the tool output of lines as observations, ids from 1: each runs from a line
// with tool mark to the next line at column 0, as capture groups a read (§6.5).
func toolTexts(lines []string, mark string) []store.SignalInput {
	var ins []store.SignalInput
	var part []string
	flush := func() {
		if len(part) > 0 {
			ins = append(ins, obs(int64(len(ins)+1), 1, tool, strings.Join(part, "\n")))
			part = nil
		}
	}
	for _, l := range lines {
		switch {
		case strings.HasPrefix(l, mark):
			flush()
			part = []string{l}
		case part != nil && strings.HasPrefix(l, " "):
			part = append(part, l)
		default:
			flush()
		}
	}
	flush()
	return ins
}
