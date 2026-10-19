package adapter

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/aryanVijaywargia/herdr-brain/adapters"
)

// The profile list is the embedded set by default, and any profile set in name order: the
// entry points take it, so a new CLI needs only its profile file (E16-S10, S60).
func TestKinds(t *testing.T) {
	names, err := fs.Glob(adapters.Profiles, "*.toml")
	if err != nil {
		t.Fatal(err)
	}
	var embedded []string
	for _, n := range names {
		embedded = append(embedded, strings.TrimSuffix(n, ".toml"))
	}
	if got := Kinds(nil); !slices.Equal(got, embedded) || !slices.Contains(got, "claude") || !slices.Contains(got, "codex") {
		t.Errorf("Kinds(nil) = %q, want the embedded profiles %q, claude and codex among them", got, embedded)
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
	want := append(slices.Clone(embedded), "fake")
	slices.Sort(want)
	if got := Kinds(fsys); !slices.Equal(got, want) {
		t.Errorf("Kinds(with fake) = %q, want %q", got, want)
	}
	ps, err := LoadAll(fsys)
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, p := range ps {
		kinds = append(kinds, p.Kind)
	}
	if !slices.Equal(kinds, want) {
		t.Errorf("LoadAll() kinds = %q, want %q", kinds, want)
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
// condition met: the eval runner's exclusion hook probes use it.
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

// TestProfileCases (S60): each embedded profile has golden hook cases in
// testdata/profiles/<kind>/, so a new profile adds its tests as data, with no Go change.
// <case>.json is a hook start input, and <case>.golden holds the exact bytes that the hook prints
// for it with testdata/brief.txt: the brief in the output form, or output_none when the input is
// not a start or has an unknown shape. The optional noise.txt holds screen lines
// that the profile drops, and keep.txt lines that it keeps.
func TestProfileCases(t *testing.T) {
	brief := string(readFile(t, "brief.txt"))
	for _, kind := range Kinds(nil) {
		t.Run(kind, func(t *testing.T) {
			p, err := Load(kind)
			if err != nil {
				t.Fatal(err)
			}
			dir := filepath.Join("testdata", "profiles", kind)
			inputs, err := filepath.Glob(filepath.Join(dir, "*.json"))
			if err != nil {
				t.Fatal(err)
			}
			var none bytes.Buffer
			if err := p.WriteNone(&none); err != nil {
				t.Fatal(err)
			}
			delivered := false
			for _, in := range inputs {
				name := strings.TrimSuffix(filepath.Base(in), ".json")
				out, err := hook(t, p, readFile(t, filepath.Join("profiles", kind, name+".json")), brief)
				if errors.Is(err, ErrNotStart) || errors.Is(err, ErrRetest) {
					out = none.Bytes()
				} else if err != nil {
					t.Fatalf("%s: hook() = %v", name, err)
				}
				delivered = delivered || !bytes.Equal(out, none.Bytes())
				if golden := readFile(t, filepath.Join("profiles", kind, name+".golden")); !bytes.Equal(out, golden) {
					t.Errorf("%s: output =\n%s\nwant\n%s", name, out, golden)
				}
			}
			if !delivered {
				t.Errorf("%s has no case in which the hook prints the brief", dir)
			}
			for file, want := range map[string]bool{"noise.txt": true, "keep.txt": false} {
				data, err := os.ReadFile(filepath.Join(dir, file))
				if errors.Is(err, fs.ErrNotExist) {
					continue
				}
				if err != nil {
					t.Fatal(err)
				}
				for _, line := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
					if p.Noise(line) != want {
						t.Errorf("%s: Noise(%q) = %t, want %t", file, line, !want, want)
					}
				}
			}
		})
	}
}
