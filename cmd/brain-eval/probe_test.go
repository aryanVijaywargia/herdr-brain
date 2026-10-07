package main

import (
	"path/filepath"
	"strings"
	"testing"
)

const goodProbe = `
[[probe]]
id = "pm-update"
stage = "update"
source = "S12"
setup = ["echo {{marker}}"]
expect_revision = 2
ask = "Which package manager? {{marker2}}"
expect = ["bun"]
stale = ["pnpm"]
harnesses = ["claude", "codex"]
`

// A probe file gives each probe as a case with one probe of each CLI, and then its control:
// the same case with no setup steps.
func TestLoadProbes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "p.toml")
	write(t, path, goodProbe)
	cases, err := loadProbes(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cases) != 2 {
		t.Fatalf("got %d cases, want the probe and its control", len(cases))
	}
	c, ctl := cases[0], cases[1]
	if c.ID != "pm-update" || c.Stage != "update" || c.Source != "S12" || c.Control || len(c.Setup) != 1 ||
		c.ExpectRevision != 2 || len(c.Probes) != 2 || c.Probes[1] != (Probe{Harness: "codex", Prompt: "Which package manager? {{marker2}}"}) ||
		c.Check.Expect[0] != "bun" || c.Check.Stale[0] != "pnpm" {
		t.Errorf("case %+v", c)
	}
	if ctl.ID != c.ID || !ctl.Control || ctl.Setup != nil || len(ctl.Probes) != 2 {
		t.Errorf("control %+v, want the same case with no setup", ctl)
	}
}

// The example probe file loads.
func TestExampleProbes(t *testing.T) {
	cases, err := loadProbes(filepath.Join("..", "..", "evals", "probes"))
	if err != nil {
		t.Fatal(err)
	}
	if len(cases) == 0 {
		t.Error("no case in evals/probes")
	}
}

// The loader names the file, the probe and each fault.
func TestLoadProbesErrors(t *testing.T) {
	tests := []struct {
		name, text string
		want       []string
	}{
		{"bad toml", "[[probe]\n", []string{"p.toml"}},
		{"unknown key", strings.Replace(goodProbe, "stale", "stail", 1), []string{`unknown key "probe.stail"`}},
		{"no probe", "", []string{"no [[probe]]"}},
		{"bad fields", `
[[probe]]
id = "Bad Id"
stage = "memory"
ask = " "
expect_revision = -1
harnesses = ["claude", "gemini", "claude"]
setup = [""]
`, []string{`probe 1 ("Bad Id")`, "id: want", `stage "memory"`, "ask is empty", "expect is empty",
			"expect_revision -1", `harness "gemini"`, `harness "claude" is listed twice`, "an empty entry"}},
		{"no harness", strings.Replace(goodProbe, `harnesses = ["claude", "codex"]`, "", 1), []string{"harnesses is empty"}},
		{"bad placeholder", strings.Replace(goodProbe, "{{marker2}}", "{{name}}", 1), []string{"placeholder {{name}}"}},
		{"duplicate id", goodProbe + goodProbe, []string{`id "pm-update" is also in`}},
		{"bad bug", `
[[bug]]
issue = 0
tests = ["internal/herdr", "./internal/herdr:TestA", "internal/herdr:Check", "internal/herdr:TestA", "internal/herdr:TestA"]
`, []string{"bug 1 (issue 0)", "issue 0: want", `test "internal/herdr": want`, `test "./internal/herdr:TestA": want`,
			`test "internal/herdr:Check": want`, `test "internal/herdr:TestA" is listed twice`}},
		{"bug with no test", "[[bug]]\nissue = 7\n", []string{"tests is empty"}},
		{"duplicate bug", strings.Repeat("[[bug]]\nissue = 7\ntests = [\"internal/herdr:TestA\"]\n", 2), []string{`id "bug-7" is also in`}},
		{"regression probe", strings.Replace(goodProbe, `stage = "update"`, `stage = "regression"`, 1),
			[]string{"stage regression: give its cases as [[bug]]"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "p.toml")
			write(t, path, tt.text)
			_, err := loadProbes(path)
			if err == nil {
				t.Fatal("loadProbes() = nil, want an error")
			}
			for _, w := range tt.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error %q, want %q", err, w)
				}
			}
		})
	}
}

// A decline probe needs no expected answer; a folder with no probe file is an error.
func TestLoadProbesDeclineAndFolder(t *testing.T) {
	dir := t.TempDir()
	if _, err := loadProbes(dir); err == nil || !strings.Contains(err.Error(), "no probe file") {
		t.Errorf("empty folder: %v, want no probe file", err)
	}
	write(t, filepath.Join(dir, "d.toml"), `
[[probe]]
id = "never-told"
stage = "restraint"
ask = "What is the code word?"
decline = true
stale = ["{{marker}}"]
harnesses = ["codex"]
`)
	cases, err := loadProbes(dir)
	if err != nil || len(cases) != 2 || !cases[0].Check.Decline {
		t.Errorf("loadProbes() = %+v, %v; want the decline case and its control", cases, err)
	}
}
