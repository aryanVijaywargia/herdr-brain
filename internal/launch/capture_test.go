package launch

import (
	"path/filepath"
	"slices"
	"testing"
)

// E24-S5: a launch gets Brain's prompt, tool and stop hooks on the CLI events of its profile,
// each with the launch folder, and no start hook. A path with a space is one shell word.
func TestCaptureHooks(t *testing.T) {
	dir := "/s/brain store/launches/7"
	for _, c := range []struct {
		kind string
		want []Hook
	}{
		{"claude", []Hook{
			{Event: "UserPromptSubmit", Command: "/b/brain hook --profile claude --event prompt --launch '/s/brain store/launches/7'"},
			{Event: "PostToolUse", Matcher: "Bash", Command: "/b/brain hook --profile claude --event tool --launch '/s/brain store/launches/7'"},
			{Event: "PostToolUseFailure", Matcher: "Bash", Command: "/b/brain hook --profile claude --event tool --launch '/s/brain store/launches/7'"},
			{Event: "Stop", Command: "/b/brain hook --profile claude --event stop --launch '/s/brain store/launches/7'"},
		}},
		{"codex", []Hook{
			{Event: "UserPromptSubmit", Command: "/b/brain hook --profile codex --event prompt --launch '/s/brain store/launches/7'"},
			{Event: "PostToolUse", Matcher: "Bash", Command: "/b/brain hook --profile codex --event tool --launch '/s/brain store/launches/7'"},
			{Event: "Stop", Command: "/b/brain hook --profile codex --event stop --launch '/s/brain store/launches/7'"},
		}},
	} {
		got, err := captureHooks(c.kind, "/b/brain", dir)
		if err != nil || !slices.Equal(got, c.want) {
			t.Errorf("captureHooks(%s) = %+v, %v\nwant %+v", c.kind, got, err, c.want)
		}
	}
	if _, err := captureHooks("nope", "/b/brain", dir); err == nil {
		t.Error("captureHooks(nope) = nil error, want one")
	}
}

// E24-S5: a Codex launch keeps the absolute Codex home of its env; another CLI keeps none.
func TestCodexHome(t *testing.T) {
	wd, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		kind    string
		environ []string
		want    string
	}{
		{"codex", []string{"HOME=/home/me", "CODEX_HOME=/x/codex"}, "/x/codex"},
		{"codex", []string{"HOME=/home/me"}, "/home/me/.codex"},
		{"codex", []string{"CODEX_HOME=rel"}, filepath.Join(wd, "rel")},
		{"codex", nil, ""},
		{"claude", []string{"HOME=/home/me", "CODEX_HOME=/x/codex"}, ""},
	} {
		if got, err := codexHome(c.kind, c.environ); got != c.want || err != nil {
			t.Errorf("codexHome(%s, %q) = %q, %v, want %q", c.kind, c.environ, got, err, c.want)
		}
	}
}

func TestShellQuote(t *testing.T) {
	for in, want := range map[string]string{"/a/b-c_d.e+f": "/a/b-c_d.e+f", "/a b": "'/a b'", "it's": `'it'\''s'`, "": "''"} {
		if got := shellQuote(in); got != want {
			t.Errorf("shellQuote(%q) = %q, want %q", in, got, want)
		}
	}
}
