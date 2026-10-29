package launch

import (
	"slices"
	"strings"
	"testing"
)

func mustLoad(t *testing.T, kind string) *cliData {
	t.Helper()
	d, err := load(kind)
	if err != nil {
		t.Fatalf("load(%q) = %v", kind, err)
	}
	return d
}

// Each kind of the launch data loads, and the kinds are the CLIs that have a builder.
func TestKinds(t *testing.T) {
	if got, want := Kinds(), []string{"claude", "codex"}; !slices.Equal(got, want) {
		t.Fatalf("Kinds() = %v, want %v", got, want)
	}
	for _, k := range Kinds() {
		if d := mustLoad(t, k); d.build == nil {
			t.Errorf("load(%q) has no builder", k)
		}
	}
	if _, err := load("nope"); err == nil {
		t.Error("load(nope) = nil error")
	}
}

// The forms that get no Brain flags (E24-S1 Q7, Q8 and its fact-check): a subcommand after
// the values of value, optional and variadic flags, a plain-launch flag in each form, and a
// no-Brain conflict. "" is a form that gets Brain flags.
func TestPlain(t *testing.T) {
	tests := []struct {
		kind string
		args []string
		want string
	}{
		{"claude", nil, ""},
		{"claude", []string{"fix the bug"}, ""},
		{"claude", []string{"--x", "y"}, ""},
		{"claude", []string{"mcp", "list"}, "the subcommand mcp"},
		{"claude", []string{"--model", "opus", "mcp"}, "the subcommand mcp"},
		{"claude", []string{"fix", "mcp"}, ""}, // a second positional arg is not a subcommand
		{"claude", []string{"--model", "mcp"}, ""},
		{"claude", []string{"--resume", "mcp", "--x"}, ""}, // optional value: mcp is the session
		{"claude", []string{"--debug", "mcp"}, ""},
		{"claude", []string{"-r", "--x", "mcp"}, "the subcommand mcp"}, // an optional flag takes no flag
		{"claude", []string{"--add-dir", "a", "b", "mcp"}, ""},         // variadic: mcp is a folder
		{"claude", []string{"--add-dir=a", "mcp"}, "the subcommand mcp"},
		{"claude", []string{"--channels", "a", "mcp"}, ""},
		{"claude", []string{"--", "mcp"}, ""},
		{"claude", []string{"--", "-p"}, ""},
		{"claude", []string{"-p", "hi"}, "the flag -p"},
		{"claude", []string{"hi", "--print"}, "the flag --print"},
		{"claude", []string{"-cp"}, "the flag -p"}, // a cluster of short flags
		{"claude", []string{"--remote"}, "the flag --remote"},
		{"claude", []string{"--rc"}, ""},
		{"claude", []string{"--init-only"}, "the flag --init-only"},
		{"claude", []string{"--bare"}, "the flag --bare"},
		{"claude", []string{"--help"}, "the flag --help"},
		{"claude", []string{"-nmcp"}, ""}, // attached value
		{"codex", nil, ""},
		{"codex", []string{"resume", "--last"}, ""},
		{"codex", []string{"fork"}, ""},
		{"codex", []string{"exec", "hi"}, "the subcommand exec"},
		{"codex", []string{"-m", "exec"}, ""},
		{"codex", []string{"-mo3", "exec"}, "the subcommand exec"},
		{"codex", []string{"-cmodel=o3", "review"}, "the subcommand review"},
		{"codex", []string{"--config", "a=1", "cloud-tasks"}, "the subcommand cloud-tasks"},
		{"codex", []string{"stdio-to-uds"}, "the subcommand stdio-to-uds"},
		{"codex", []string{"-i", "a.png", "exec"}, ""}, // variadic: exec is an image
		{"codex", []string{"resume", "-h"}, "the flag -h"},
		{"codex", []string{"--remote", "ws://h"}, "the flag --remote"},
		{"codex", []string{"--disable", "hooks"}, "--disable hooks: it turns off the hooks"},
		{"codex", []string{"--disable=hooks"}, "--disable hooks: it turns off the hooks"},
		{"codex", []string{"--disable", "apps"}, ""},
		{"codex", []string{"-c", "features.hooks=false"}, "-c features.hooks=false: it turns off the hooks"},
		{"codex", []string{"-c", "features = { hooks = false }"}, "it turns off the hooks"},
		{"codex", []string{"-c", "features={apps=true,hooks=false}"}, "it turns off the hooks"},
		{"codex", []string{"-c", "features.hooks=true"}, ""},
		{"codex", []string{"-c", "features={myhooks=false}"}, ""},
		// The -c value is TOML, as Codex reads it (review of #442): quoted keys, a comment, and
		// the attached and --config forms.
		{"codex", []string{"-c", `features={"hooks"=false}`}, "it turns off the hooks"},
		{"codex", []string{"-c", `features={'hooks'=false}`}, "it turns off the hooks"},
		{"codex", []string{"-c", "features.hooks=false # user override"}, "it turns off the hooks"},
		{"codex", []string{"-cfeatures.hooks=false"}, "it turns off the hooks"},
		{"codex", []string{"--config=features={hooks=false}"}, "it turns off the hooks"},
		{"codex", []string{"resume", "-c", "features.hooks=false"}, "it turns off the hooks"},
		{"codex", []string{"-c", `features.hooks="false"`}, ""}, // a string, not false
		{"codex", []string{"-c", "features.hooks.x=false"}, ""},
		{"codex", []string{"-c", "features"}, ""},
	}
	for _, tt := range tests {
		got := parse(mustLoad(t, tt.kind), tt.args).plain()
		if tt.want == "" && got != "" || !strings.Contains(got, tt.want) {
			t.Errorf("%s %q: plain() = %q, want %q", tt.kind, tt.args, got, tt.want)
		}
	}
}

// A -c of a Brain key or of a parent table of one refuses (E24-S1 fact-check): it would
// replace the Brain server, hooks or trust. Other -c keys and hooks.state entries stay.
func TestRefused(t *testing.T) {
	tests := []struct {
		kind    string
		args    []string
		refused bool
	}{
		{"codex", []string{"-c", "hooks.Stop=[]"}, true},
		{"codex", []string{"-c", `hooks."SessionStart"=[]`}, true},
		{"codex", []string{"-c", "hooks={}"}, true},
		{"codex", []string{"--config=hooks.state={}"}, true},
		{"codex", []string{"-chooks.UserPromptSubmit=[]"}, true},
		{"codex", []string{"resume", "-c", "hooks.PostToolUse=[]"}, true},
		{"codex", []string{"-c", "mcp_servers={}"}, true},
		{"codex", []string{"-c", `mcp_servers.brain.command="x"`}, true},
		{"codex", []string{"-c", `mcp_servers."brain".args=[]`}, true},
		{"codex", []string{"-c", `hooks.state."k".trusted_hash="x"`}, false},
		{"codex", []string{"-c", "hooks.PreToolUse=[]"}, false},
		{"codex", []string{"-c", `mcp_servers.brainstorm.command="x"`}, false},
		{"codex", []string{"-c", `model="o3"`}, false},
		{"codex", []string{"--", "-c", "hooks={}"}, false},
		{"claude", []string{"--settings", "s.json", "-c"}, false},
	}
	for _, tt := range tests {
		err := parse(mustLoad(t, tt.kind), tt.args).refused()
		if (err != nil) != tt.refused {
			t.Errorf("%s %q: refused() = %v, want refused %t", tt.kind, tt.args, err, tt.refused)
		}
		if err != nil && !strings.Contains(err.Error(), "Brain starts nothing") {
			t.Errorf("%s %q: refused() = %q, want why and what to do", tt.kind, tt.args, err)
		}
	}
}

// With an empty plan only the static flags come first, then the user's args with no change.
// A top-level --no-daemon of the user stays and Brain adds none; one after the subcommand is
// at another level, so Brain adds its own. A user --settings stays when Brain has none.
func TestArgvEmptyPlan(t *testing.T) {
	tests := []struct {
		kind string
		args []string
		want []string
	}{
		{"claude", nil, nil},
		{"claude", []string{"--x", "y"}, []string{"--x", "y"}},
		{"claude", []string{"--settings", "s.json", "hi"}, []string{"--settings", "s.json", "hi"}},
		{"codex", nil, []string{"--no-daemon"}},
		{"codex", []string{"--x", "y"}, []string{"--no-daemon", "--x", "y"}},
		{"codex", []string{"--no-daemon", "resume"}, []string{"--no-daemon", "resume"}},
		{"codex", []string{"-m", "o3", "--no-daemon"}, []string{"-m", "o3", "--no-daemon"}},
		{"codex", []string{"resume", "--no-daemon"}, []string{"--no-daemon", "resume", "--no-daemon"}},
		{"codex", []string{"fork", "--no-daemon"}, []string{"--no-daemon", "fork", "--no-daemon"}},
		// A prompt is no subcommand: the --no-daemon after it is a top-level flag (review of #442).
		{"codex", []string{"hello", "--no-daemon"}, []string{"hello", "--no-daemon"}},
	}
	for _, tt := range tests {
		got, notes, err := parse(mustLoad(t, tt.kind), tt.args).argv(t.TempDir(), Plan{})
		if err != nil || !slices.Equal(got, tt.want) || len(notes) != 0 {
			t.Errorf("%s %q: argv() = %q, %q, %v, want %q", tt.kind, tt.args, got, notes, err, tt.want)
		}
	}
}
