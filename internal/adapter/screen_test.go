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
// the CLI versions of the real-use test (claude 2.1.292, codex 0.160.1), or rows of the
// recorded reads (claude-logo, claude-tip-nbsp); its .golden holds the kept lines. #281: the
// approval, trust, welcome and diff screens are made from the stored excerpts of the M2 eval
// run (2026-10-07). claude-approval-top is the dialog of claude-s31-2-blocked at the top of a
// read: the trust dialog rule keeps it. #316: the placeholder, mcp, shell, fold, hooks and stop
// screens are made from the lines of the M2 eval rerun report (2026-10-08, claude 2.1.293,
// codex 0.160.1), with the hooks texts of the Codex source; the words-keep screens hold real
// prompts with the same words. An empty golden keeps no line.
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
			var want []string // an empty golden: the screen keeps no line
			if golden := strings.TrimSuffix(string(readFile(t, filepath.Join("screen", name+".golden"))), "\n"); golden != "" {
				want = strings.Split(golden, "\n")
			}
			if got := p.Screen(string(readFile(t, filepath.Join("screen", name+".txt")))); !slices.Equal(got, want) {
				t.Errorf("Screen() =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
			}
		})
	}
}

// #258 AC 3: the screen rules are profile data, so a new CLI or CLI version needs no Go
// change: a fake profile gets its own noise blocks and side panel. A bad rule is an error.
// #291: a blank line ends a block, unless its next matches the empty line. #316: a noise_above
// line is noise only when the next line that is not blank matches its below, and a block with
// after_blank starts only under a blank line.
func TestScreenRules(t *testing.T) {
	good := string(readFile(t, "fake/fake.toml"))
	rules := `
noise_lines = ['== status .*']
noise_blocks = [{ start = '\+-+\+', next = '\|.*\||\+-+\+' }, { start = '<open>', next = '  .*|' },
  { start = '<shut>', next = '  .*' }, { start = '<late>', next = '  .*', after_blank = true }]
noise_above = [{ line = '> try', below = '== status .*' }]
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
		"<open>",
		"",
		"  dropped",
		"<shut>",
		"",
		"  kept",
		"> try",
		"",
		"== status ok",
		"> try",
		"kept below",
		"<late>",
		"  kept late",
		"",
		"<late>",
		"  dropped late",
		"",
	}, "\n")
	// The wide runes take two columns each, so the cut keeps the reply up to column 14.
	want := []string{"> hello", "漢字 reply", "| kept |", "  kept", "> try", "kept below", "<late>", "  kept late"}
	if got := p.Screen(screen); !slices.Equal(got, want) {
		t.Errorf("Screen() = %q, want %q", got, want)
	}
	for _, bad := range []string{
		"noise_blocks = [{ start = '(', next = 'x' }]",
		"noise_blocks = [{ start = 'x', next = '(' }]",
		"noise_above = [{ line = '(', below = 'x' }]",
		"noise_above = [{ line = 'x', below = '(' }]",
		"side_panel = { header = '(', min_column = 10 }",
		"side_panel = { header = 'x' }",
	} {
		if _, err := LoadFS(fstest.MapFS{"fake.toml": {Data: []byte(good + "\n" + bad + "\n")}}, "fake"); err == nil {
			t.Errorf("LoadFS with %s = nil, want an error", bad)
		}
	}
}
