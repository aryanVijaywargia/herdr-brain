package adapter

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
)

// hook does what the hook does with one input: parse it, and write the brief only when the
// parse succeeds. It returns the printed bytes and the parse error.
func hook(t *testing.T, p *Profile, raw []byte, brief string) ([]byte, error) {
	t.Helper()
	var out bytes.Buffer
	if _, err := p.Parse(raw); err != nil {
		return out.Bytes(), err
	}
	if err := p.Write(&out, brief); err != nil {
		t.Fatalf("Write() = %v", err)
	}
	return out.Bytes(), nil
}

func readFile(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestGolden feeds the M0 hook inputs (spike/m0, /tmp/brainspike logs) to the embedded
// profiles and compares the output with bytes that an independent JSON encoder built.
func TestGolden(t *testing.T) {
	brief := string(readFile(t, "brief.txt"))
	tests := []struct {
		harness   string
		sessionID string
	}{
		{"claude", "6a345ec1-3050-4e61-b736-d0e9d2edab82"},
		{"codex", "01a0e36c-7b20-7172-b5f9-59a6946c8aba"},
	}
	for _, tt := range tests {
		t.Run(tt.harness, func(t *testing.T) {
			p, err := Load(tt.harness)
			if err != nil {
				t.Fatalf("Load(%q) = %v", tt.harness, err)
			}
			raw := readFile(t, tt.harness+"-input.json")
			in, err := p.Parse(raw)
			if err != nil {
				t.Fatalf("Parse() = %v", err)
			}
			want := Input{SessionID: tt.sessionID, CWD: "/Users/aryanvijaywargia/Projects/herdr-projects"}
			if in != want {
				t.Errorf("Parse() = %+v, want %+v", in, want)
			}
			got, err := hook(t, p, raw, brief)
			if err != nil {
				t.Fatalf("hook() = %v", err)
			}
			if golden := readFile(t, tt.harness+".golden"); !bytes.Equal(got, golden) {
				t.Errorf("output =\n%s\nwant\n%s", got, golden)
			}
		})
	}
}

func TestProfiles(t *testing.T) {
	tests := []struct {
		harness, target, version string
	}{
		{"claude", "~/.claude/settings.json", "2.1.283"},
		{"codex", "~/.codex/hooks.json", "0.154.0"},
	}
	for _, tt := range tests {
		p, err := Load(tt.harness)
		if err != nil {
			t.Fatalf("Load(%q) = %v", tt.harness, err)
		}
		if p.Kind != tt.harness || p.InstallTarget != tt.target || p.TestedVersion != tt.version ||
			p.Events.Start != "SessionStart" || p.StartCondition != "" {
			t.Errorf("Load(%q) = %+v, want kind %s, target %s, version %s, start SessionStart",
				tt.harness, p, tt.harness, tt.target, tt.version)
		}
	}
}

func TestRetest(t *testing.T) {
	p, err := Load("claude")
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		raw  string
	}{
		{"changed payload", string(readFile(t, "claude-changed.json"))},
		{"not json", "SessionStart"},
		{"array", `[{"session_id":"s","cwd":"/w"}]`},
		{"session id not a string", `{"session_id":7,"cwd":"/w"}`},
		{"empty session id", `{"session_id":"","cwd":"/w"}`},
		{"no cwd", `{"session_id":"s"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, err := hook(t, p, []byte(tt.raw), "brief")
			if !errors.Is(err, ErrRetest) {
				t.Errorf("hook(%s) error = %v, want ErrRetest", tt.raw, err)
			}
			if len(out) != 0 {
				t.Errorf("hook(%s) printed %q, want nothing", tt.raw, out)
			}
		})
	}
}

// TestFakeHarness runs a profile for a harness that has no Go code: other event, nested input
// paths and a start condition.
func TestFakeHarness(t *testing.T) {
	p, err := LoadFS(os.DirFS("testdata/fake"), "fake")
	if err != nil {
		t.Fatalf("LoadFS(fake) = %v", err)
	}
	start := `{"turn":{"index":0},"session":{"id":"f-1"},"workspace":{"dir":"/w"}}`
	in, err := p.Parse([]byte(start))
	if want := (Input{SessionID: "f-1", CWD: "/w"}); err != nil || in != want {
		t.Errorf("Parse(start) = %+v, %v, want %+v", in, err, want)
	}
	got, err := hook(t, p, []byte(start), "hi")
	want := `{"hookSpecificOutput":{"hookEventName":"BeforeTurn","additionalContext":"hi"}}` + "\n"
	if err != nil || string(got) != want {
		t.Errorf("hook(start) = %q, %v, want %q", got, err, want)
	}

	tests := []struct {
		name string
		raw  string
		want error
	}{
		{"later turn", `{"turn":{"index":3},"session":{"id":"f-1"},"workspace":{"dir":"/w"}}`, ErrNotStart},
		{"no turn", `{"session":{"id":"f-1"},"workspace":{"dir":"/w"}}`, ErrRetest},
		{"flat id", `{"turn":{"index":0},"session_id":"f-1","workspace":{"dir":"/w"}}`, ErrRetest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, err := hook(t, p, []byte(tt.raw), "hi")
			if !errors.Is(err, tt.want) || len(out) != 0 {
				t.Errorf("hook(%s) = %q, %v, want nothing and %v", tt.raw, out, err, tt.want)
			}
		})
	}
}

func TestLoadErrors(t *testing.T) {
	good := string(readFile(t, "fake/fake.toml"))
	tests := []struct {
		name, profile, want string
	}{
		{"unknown field", good + "events.tool = \"PostToolUse\"\n", `unknown field "events.tool"`},
		{"missing field", strings.Replace(good, `input.cwd = "workspace.dir"`, "", 1), "input.cwd is empty"},
		{"unknown output form", strings.Replace(good, "hookSpecificOutput.additionalContext", "injectSteps.ephemeralMessage", 1), "unknown output_form"},
		{"bad condition", strings.Replace(good, "turn.index == 0", "turn.index < 1", 1), "start_condition"},
		{"bad condition value", strings.Replace(good, "turn.index == 0", "turn.index == zero", 1), "start_condition"},
		{"bad toml", "kind = ", "profile fake.toml"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fsys := fstest.MapFS{"fake.toml": {Data: []byte(tt.profile)}}
			_, err := LoadFS(fsys, "fake")
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("LoadFS() = %v, want an error with %q", err, tt.want)
			}
		})
	}
	if _, err := Load("agy"); err == nil {
		t.Error(`Load("agy") = nil error, want an error: there is no agy profile in M1`)
	}
}
