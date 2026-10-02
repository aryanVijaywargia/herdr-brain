package adapter

import (
	"io/fs"
	"slices"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/aryanVijaywargia/herdr-brain/adapters"
)

// The profile list is the embedded set by default, and any profile set in name order: the
// entry points take it, so a new CLI needs only its profile file (E16-S10, S60).
func TestKinds(t *testing.T) {
	if got := Kinds(nil); !slices.Equal(got, []string{"claude", "codex"}) {
		t.Errorf("Kinds(nil) = %q, want claude and codex", got)
	}
	fake := readFile(t, "fake/fake.toml")
	fsys := fstest.MapFS{"zz.txt": {Data: []byte("not a profile")}, "fake.toml": {Data: fake}}
	for _, k := range Kinds(nil) {
		data, err := fs.ReadFile(adapters.Profiles, k+".toml")
		if err != nil {
			t.Fatal(err)
		}
		fsys[k+".toml"] = &fstest.MapFile{Data: data}
	}
	if got := Kinds(fsys); !slices.Equal(got, []string{"claude", "codex", "fake"}) {
		t.Errorf("Kinds(with fake) = %q, want claude, codex and fake", got)
	}
	ps, err := LoadAll(fsys)
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, p := range ps {
		kinds = append(kinds, p.Kind)
	}
	if !slices.Equal(kinds, []string{"claude", "codex", "fake"}) {
		t.Errorf("LoadAll() kinds = %q, want claude, codex and fake", kinds)
	}
	fsys["bad.toml"] = &fstest.MapFile{Data: []byte("kind = ")}
	if _, err := LoadAll(fsys); err == nil || !strings.Contains(err.Error(), "profile bad.toml") {
		t.Errorf("LoadAll(with a bad profile) = %v, want its error", err)
	}
}

// The values that differ per CLI come from the profile (E16-S10).
func TestProfileCLIFields(t *testing.T) {
	claude, err := Load("claude")
	if err != nil {
		t.Fatal(err)
	}
	codex, err := Load("codex")
	if err != nil {
		t.Fatal(err)
	}
	if !claude.AltScreen || claude.HookTrust != "" || len(claude.Eval.Args) != 0 ||
		!slices.Equal(claude.Eval.SetupArgs, []string{"--allowedTools", "Bash(brain remember:*)"}) {
		t.Errorf("claude profile: alt_screen %t, hook_trust %q, eval %+v", claude.AltScreen, claude.HookTrust, claude.Eval)
	}
	if codex.AltScreen || codex.HookTrust != TrustCodex || len(codex.Eval.SetupArgs) != 0 ||
		!slices.Equal(codex.Eval.Args, []string{"-c", `projects={{{dir}}={trust_level="trusted"}}`, "-c", "check_for_update_on_startup=false"}) {
		t.Errorf("codex profile: alt_screen %t, hook_trust %q, eval %+v", codex.AltScreen, codex.HookTrust, codex.Eval)
	}
	fake, err := LoadFS(fstest.MapFS{"fake.toml": {Data: readFile(t, "fake/fake.toml")}}, "fake")
	if err != nil {
		t.Fatal(err)
	}
	if fake.AltScreen || fake.HookTrust != "" || len(fake.Eval.Args)+len(fake.Eval.SetupArgs) != 0 {
		t.Errorf("fake profile: alt_screen %t, hook_trust %q, eval %+v; want none", fake.AltScreen, fake.HookTrust, fake.Eval)
	}
}

func TestLoadProfileListErrors(t *testing.T) {
	good := string(readFile(t, "fake/fake.toml"))
	tests := []struct {
		name, file, profile, want string
	}{
		{"unknown hook trust", "fake.toml", good + "hook_trust = \"gemini\"\n", `unknown hook_trust "gemini"`},
		{"kind is not the file name", "other.toml", good, `kind "fake" is not the file name`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fsys := fstest.MapFS{tt.file: {Data: []byte(tt.profile)}}
			_, err := LoadFS(fsys, strings.TrimSuffix(tt.file, ".toml"))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("LoadFS() = %v, want an error with %q", err, tt.want)
			}
		})
	}
}

// StartInput gives input of the profile's own shape that Parse reads back, with the start
// condition met: the eval runner's exclusion hook probes use it (§12.1).
func TestStartInput(t *testing.T) {
	fake, err := LoadFS(fstest.MapFS{"fake.toml": {Data: readFile(t, "fake/fake.toml")}}, "fake")
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range Kinds(nil) {
		p, err := Load(k)
		if err != nil {
			t.Fatal(err)
		}
		testStartInput(t, p)
	}
	testStartInput(t, fake)
	raw, err := fake.StartInput("s-1", "/w")
	if err != nil || string(raw) != `{"session":{"id":"s-1"},"turn":{"index":0},"workspace":{"dir":"/w"}}` {
		t.Errorf("fake StartInput() = %s, %v", raw, err)
	}
}

func testStartInput(t *testing.T, p *Profile) {
	t.Helper()
	raw, err := p.StartInput("s-1", "/w")
	if err != nil {
		t.Fatalf("%s StartInput() = %v", p.Kind, err)
	}
	in, err := p.Parse(raw)
	if err != nil || in.SessionID != "s-1" || in.CWD != "/w" {
		t.Errorf("%s Parse(StartInput()) = %+v, %v; want s-1 and /w", p.Kind, in, err)
	}
}
