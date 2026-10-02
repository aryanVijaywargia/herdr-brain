package adapter

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
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
		harness    string
		sessionID  string
		transcript string
	}{
		{"claude", "6a345ec1-3050-4e61-b736-d0e9d2edab82",
			"/Users/aryanvijaywargia/.claude/projects/-Users-aryanvijaywargia-Projects-herdr-projects/6a345ec1-3050-4e61-b736-d0e9d2edab82.jsonl"},
		{"codex", "01a0e36c-7b20-7172-b5f9-59a6946c8aba",
			"/Users/aryanvijaywargia/.codex/sessions/2026/09/27/rollout-2026-09-27T20-42-12-01a0e36c-7b20-7172-b5f9-59a6946c8aba.jsonl"},
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
			want := Input{SessionID: tt.sessionID, CWD: "/Users/aryanvijaywargia/Projects/herdr-projects", TranscriptPath: tt.transcript}
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

// AC E15-S4: a start input with no transcript path, or with null, an empty or a non-string
// path, still parses, with no path. Codex sends string|null (spike/m0/data/e11s1-codex-hooks.txt).
func TestTranscriptPath(t *testing.T) {
	for _, harness := range []string{"claude", "codex"} {
		p, err := Load(harness)
		if err != nil {
			t.Fatal(err)
		}
		for _, tc := range []struct{ name, field, want string }{
			{"path", `,"transcript_path":"/t/s.jsonl"`, "/t/s.jsonl"},
			{"none", "", ""},
			{"null", `,"transcript_path":null`, ""},
			{"empty", `,"transcript_path":""`, ""},
			{"number", `,"transcript_path":7`, ""},
		} {
			t.Run(harness+"/"+tc.name, func(t *testing.T) {
				raw := `{"session_id":"s","cwd":"/w"` + tc.field + `}`
				in, err := p.Parse([]byte(raw))
				if err != nil || in.TranscriptPath != tc.want {
					t.Errorf("Parse(%s) = %+v, %v, want transcript path %q", raw, in, err, tc.want)
				}
			})
		}
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

// TestProfileHash checks that the hash covers the whole profile file, so any change to the
// file changes it.
func TestProfileHash(t *testing.T) {
	good := readFile(t, "fake/fake.toml")
	load := func(data []byte) string {
		t.Helper()
		p, err := LoadFS(fstest.MapFS{"fake.toml": {Data: data}}, "fake")
		if err != nil {
			t.Fatalf("LoadFS() = %v", err)
		}
		return p.SHA256
	}
	sum := sha256.Sum256(good)
	if got, want := load(good), hex.EncodeToString(sum[:]); got != want {
		t.Errorf("SHA256 = %s, want %s", got, want)
	}
	if load(append(good, "# a comment\n"...)) == load(good) {
		t.Error("SHA256 is the same after a change to the profile file")
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
		{"unknown field", good + "events.notify = \"PostToolUse\"\n", `unknown field "events.notify"`},
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

// noise_lines is optional; a pattern matches a whole line, and a bad pattern is an error.
func TestNoiseLines(t *testing.T) {
	good := string(readFile(t, "fake/fake.toml"))
	p, err := LoadFS(fstest.MapFS{"fake.toml": {Data: []byte(good + "\nnoise_lines = ['─+', '› .*']\n")}}, "fake")
	if err != nil {
		t.Fatalf("LoadFS() = %v", err)
	}
	for line, want := range map[string]bool{"──────": true, "› type here": true, "a ── b": false, "done": false} {
		if got := p.Noise(line); got != want {
			t.Errorf("Noise(%q) = %v, want %v", line, got, want)
		}
	}
	if _, err := LoadFS(fstest.MapFS{"fake.toml": {Data: []byte(good + "\nnoise_lines = ['(']\n")}}, "fake"); err == nil {
		t.Error("LoadFS with a bad noise pattern = nil, want an error")
	}
	if p, err := Load("codex"); err != nil || p.Noise("› type here") {
		t.Errorf("Load(codex) = %v; a profile without noise_lines drops no line", err)
	}
}
