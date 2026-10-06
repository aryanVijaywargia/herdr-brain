package adapter

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
)

// #258: the shipped profiles drop the CLI screen chrome (tips, start banners, footer and
// status lines) and cut the side panel, and keep the real prompts and replies, also those
// with the words of the chrome. Each testdata/screen/<harness>-<case>.txt is a made screen of
// the CLI versions of the real-use test (claude 2.1.292, codex 0.160.1); its .golden holds
// the kept lines.
func TestScreen(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("testdata", "screen", "*.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no screen fixtures")
	}
	for _, f := range files {
		name := strings.TrimSuffix(filepath.Base(f), ".txt")
		t.Run(name, func(t *testing.T) {
			harness, _, _ := strings.Cut(name, "-")
			p, err := Load(harness)
			if err != nil {
				t.Fatal(err)
			}
			want := strings.Split(strings.TrimSuffix(string(readFile(t, filepath.Join("screen", name+".golden"))), "\n"), "\n")
			if got := p.Screen(string(readFile(t, filepath.Join("screen", name+".txt")))); !slices.Equal(got, want) {
				t.Errorf("Screen() =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
			}
		})
	}
}

// #258 AC 3: the screen rules are profile data, so a new CLI or CLI version needs no Go
// change: a fake profile gets its own noise blocks and side panel. A bad rule is an error.
func TestScreenRules(t *testing.T) {
	good := string(readFile(t, "fake/fake.toml"))
	rules := `
noise_lines = ['== status .*']
noise_blocks = [{ start = '\+-+\+', next = '\|.*\||\+-+\+' }]
side_panel = { header = '\[panel\]', min_column = 10 }
`
	p, err := LoadFS(fstest.MapFS{"fake.toml": {Data: []byte(good + rules)}}, "fake")
	if err != nil {
		t.Fatalf("LoadFS() = %v", err)
	}
	screen := strings.Join([]string{
		"+------+",
		"| fake |",
		"",
		"+------+",
		"> hello       [panel]",
		"漢字 reply    panel text",
		"| kept |",
		"== status ok",
		"",
	}, "\n")
	// The wide runes take two columns each, so the cut keeps the reply up to column 14.
	want := []string{"> hello", "漢字 reply", "| kept |"}
	if got := p.Screen(screen); !slices.Equal(got, want) {
		t.Errorf("Screen() = %q, want %q", got, want)
	}
	for _, bad := range []string{
		"noise_blocks = [{ start = '(', next = 'x' }]",
		"noise_blocks = [{ start = 'x', next = '(' }]",
		"side_panel = { header = '(', min_column = 10 }",
		"side_panel = { header = 'x' }",
	} {
		if _, err := LoadFS(fstest.MapFS{"fake.toml": {Data: []byte(good + "\n" + bad + "\n")}}, "fake"); err == nil {
			t.Errorf("LoadFS with %s = nil, want an error", bad)
		}
	}
}
