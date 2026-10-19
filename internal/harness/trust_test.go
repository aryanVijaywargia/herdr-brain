package harness

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aryanVijaywargia/herdr-brain/internal/adapter"
	"github.com/aryanVijaywargia/herdr-brain/internal/install"
)

func ptr[T any](v T) *T { return &v }

// Codex 0.154 wrote the golden values of the E8-S3 test to config.toml
// when the owner trusted these hooks in the TUI. The home path in the commands is now /Users/me,
// so the want values are those of the same hookHash for the new commands.
func TestHookHashMatchesCodex(t *testing.T) {
	tests := []struct {
		name    string
		event   string
		matcher *string
		h       handler
		want    string
	}{
		{
			name:  "M0 inline session flags hook",
			event: "SessionStart",
			h:     handler{Command: "/Users/me/Projects/herdr-brain/spike/m0/hook/brainspike codex", Timeout: ptr[uint64](5)},
			want:  "sha256:4fed4ee72a2f82fa75f16a67f8c029472a60db04e4f514cad71688593c7fec5f",
		},
		{
			name:  "hooks.json session start, no matcher",
			event: "SessionStart",
			h:     handler{Command: "bash '/Users/me/.codex/herdr-agent-state.sh' session", Timeout: ptr[uint64](10)},
			want:  "sha256:5f6e3cf290a1a1d3bb540c44b4e67ea428b1b011a759d70107ff33d9b29aa31f",
		},
		{
			name:    "hooks.json subagent start, matcher, default timeout",
			event:   "SubagentStart",
			matcher: ptr("tldraw-offline"),
			h:       handler{Command: "sh '/Users/me/skills/tldraw-offline/inject-server-context.sh' SubagentStart"},
			want:    "sha256:9f0508aa58e15ee9d34edd5d468d37aed5d5baf373b75883eb09fc497ba486eb",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.h.Type = "command"
			got, err := hookHash(tt.event, eventLabels[tt.event], tt.matcher, tt.h)
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Errorf("hash = %s, want %s", got, tt.want)
			}
		})
	}
}

// trustEnv is a Codex home with an other group before the Brain group, so the Brain key has
// group index 1. It returns the recorded entry, its key and its hash.
func trustEnv(t *testing.T) (dir string, e install.Entry, key, sum string) {
	t.Helper()
	home := t.TempDir()
	dir = filepath.Join(home, ".codex")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	other := `{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"other","timeout":10}]}]}}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, "hooks.json"), []byte(other), 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := adapter.Load("codex")
	if err != nil {
		t.Fatal(err)
	}
	inst := &install.Installer{Home: home, Record: filepath.Join(home, "install.json"), Binary: "/opt/brain/bin/brain"}
	if _, err := inst.Install(p); err != nil {
		t.Fatal(err)
	}
	rec, err := install.Read(inst.Record)
	if err != nil {
		t.Fatal(err)
	}
	e = rec.Entries["codex"]
	// The entry sets timeout 5 and additionalContextLimit 0, and both are in the hash.
	sum, err = hookHash("SessionStart", "session_start", nil, handler{Type: "command",
		Command: "/opt/brain/bin/brain hook --profile codex --event start", Timeout: ptr[uint64](5),
		AdditionalContextLimit: ptr[uint64](0)})
	if err != nil {
		t.Fatal(err)
	}
	return dir, e, filepath.Join(dir, "hooks.json") + ":session_start:1:0", sum
}

func writeConfig(t *testing.T, dir, text string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}

// AC: the trusted, modified and untrusted cases of the status rule, and enabled = false.
func TestCodexTrust(t *testing.T) {
	dir, e, key, sum := trustEnv(t)
	tests := []struct {
		name, config string
		want         Trust
	}{
		{"no config", "", Untrusted},
		{"trusted", fmt.Sprintf("model = \"x\"\n\n[hooks.state.%q]\ntrusted_hash = %q\n", key, sum), Trusted},
		{"modified", fmt.Sprintf("[hooks.state.%q]\ntrusted_hash = \"sha256:00\"\n", key), Modified},
		{"other key", fmt.Sprintf("[hooks.state.%q]\ntrusted_hash = %q\n", key[:len(key)-3]+"0:0", sum), Untrusted},
		{"disabled", fmt.Sprintf("[hooks.state.%q]\ntrusted_hash = %q\nenabled = false\n", key, sum), Disabled},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.config == "" {
				_ = os.Remove(filepath.Join(dir, "config.toml"))
			} else {
				writeConfig(t, dir, tt.config)
			}
			got, err := CodexTrust(dir, e, "SessionStart")
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Errorf("CodexTrust() = %q, want %q", got, tt.want)
			}
		})
	}
}

// AC #315: an entry that you edited, here its timeout, is found by its command, and its trust
// state is the one of its new hash: the trust of the old entry makes it modified.
func TestCodexTrustEdited(t *testing.T) {
	dir, e, key, old := trustEnv(t)
	file := filepath.Join(dir, "hooks.json")
	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	edited := strings.Replace(e.Text, `"timeout":5`, `"timeout":9`, 1)
	if err := os.WriteFile(file, []byte(strings.Replace(string(b), e.Text, edited, 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	sum, err := hookHash("SessionStart", "session_start", nil, handler{Type: "command", Command: e.Command,
		Timeout: ptr[uint64](9), AdditionalContextLimit: ptr[uint64](0)})
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		hash string
		want Trust
	}{{old, Modified}, {sum, Trusted}} {
		writeConfig(t, dir, fmt.Sprintf("[hooks.state.%q]\ntrusted_hash = %q\n", key, tt.hash))
		if got, err := CodexTrust(dir, e, "SessionStart"); err != nil || got != tt.want {
			t.Errorf("CodexTrust(edited, trusted %s) = %q, %v; want %q", tt.hash, got, err, tt.want)
		}
	}
}

// AC: a Codex home whose hooks.json holds no Brain entry, for example another CODEX_HOME.
func TestCodexTrustNoEntry(t *testing.T) {
	_, e, _, _ := trustEnv(t)
	got, err := CodexTrust(t.TempDir(), e, "SessionStart")
	if err != nil || got != NoEntry {
		t.Errorf("CodexTrust() = %q, %v; want %q", got, err, NoEntry)
	}
}

func TestCodexHome(t *testing.T) {
	if got, err := CodexHome("/home/u", ""); err != nil || got != "/home/u/.codex" {
		t.Errorf("CodexHome no env = %q, %v", got, err)
	}
	dir := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	want, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := CodexHome("/home/u", link); err != nil || got != want {
		t.Errorf("CodexHome(%s) = %q, %v; want %q", link, got, err, want)
	}
}
