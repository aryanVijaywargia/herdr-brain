package main

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/aryanVijaywargia/herdr-brain/internal/adapter"
	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

func TestParse(t *testing.T) {
	tests := []struct {
		args []string
		want []Case
		err  string
	}{
		{nil, nil, ""},
		{[]string{"--step", "a", "--step", "b"}, []Case{{ID: "adhoc", Setup: []string{"a", "b"}}}, ""},
		{[]string{"--probe", "claude", "--probe", "codex", "--prompt", "hi {{marker}}"}, []Case{{ID: "adhoc",
			Probes: []Probe{{Harness: "claude", Prompt: "hi {{marker}}"}, {Harness: "codex", Prompt: "hi {{marker}}"}}}}, ""},
		{[]string{"--probe", "claude"}, nil, "go together"},
		{[]string{"--prompt", "hi"}, nil, "go together"},
		{[]string{"--probe", "gemini", "--prompt", "hi"}, nil, "want " + oneOf(adapter.Kinds(nil))},
		{[]string{"x"}, nil, "unexpected argument"},
		{[]string{"--probes", "../../evals/probes", "--step", "a"}, nil, "--probes goes only with --stage and --harness"},
		{[]string{"--stage", "delivery"}, nil, "go with --probes"},
		{[]string{"--probes", "../../evals/probes", "--stage", "nope"}, nil, `--stage "nope"`},
		{[]string{"--probes", "../../evals/probes", "--harness", "gemini"}, nil, `--harness "gemini"`},
		{[]string{"--probes", "no-such-folder"}, nil, "no-such-folder"},
	}
	for _, tt := range tests {
		got, err := parse(tt.args)
		if tt.err != "" {
			if err == nil || !strings.Contains(err.Error(), tt.err) {
				t.Errorf("parse(%q) = %v, want %q", tt.args, err, tt.err)
			}
			continue
		}
		if err != nil || !reflect.DeepEqual(got, tt.want) {
			t.Errorf("parse(%q) = %+v, %v, want %+v", tt.args, got, err, tt.want)
		}
	}
}

func TestParseProbes(t *testing.T) {
	cases, err := parse([]string{"--probes", "../../evals/probes"})
	if err != nil || len(cases) < 2 || !cases[1].Control {
		t.Errorf("parse(--probes) = %+v, %v; want the probes of evals/probes and their controls", cases, err)
	}
}

func TestParseFilter(t *testing.T) {
	cases, err := parse([]string{"--probes", "../../evals/probes", "--stage", "delivery", "--harness", "codex"})
	if err != nil || len(cases) == 0 {
		t.Fatalf("parse(--stage delivery --harness codex) = %v, %v", cases, err)
	}
	for _, c := range cases {
		if c.Stage != "delivery" {
			t.Errorf("case %s has stage %q, want delivery", c.ID, c.Stage)
		}
		for _, p := range c.Probes {
			if p.Harness != "codex" {
				t.Errorf("case %s has a %s probe, want codex only", c.ID, p.Harness)
			}
		}
	}
}

// The regression stage keeps only the regression cases; a harness leaves them out (E20-S1).
func TestParseRegression(t *testing.T) {
	cases, err := parse([]string{"--probes", "../../evals/probes", "--stage", "regression"})
	if err != nil || len(cases) == 0 {
		t.Fatalf("parse(--stage regression) = %v, %v", cases, err)
	}
	for _, c := range cases {
		if c.Stage != regressionStage || c.Control || len(c.Probes) > 0 || len(c.Tests) == 0 {
			t.Errorf("case %+v, want a regression case with tests and no probe", c)
		}
	}
	cases, err = parse([]string{"--probes", "../../evals/probes", "--harness", "claude"})
	if err != nil || slices.ContainsFunc(cases, func(c Case) bool { return c.Stage == regressionStage }) {
		t.Errorf("parse(--harness claude) = %v; want no regression case", err)
	}
	if _, err := parse([]string{"--probes", "../../evals/probes", "--stage", "regression", "--harness", "claude"}); err == nil ||
		!strings.Contains(err.Error(), "no probe matches") {
		t.Errorf("parse(--stage regression --harness claude) = %v, want no probe matches", err)
	}
}

// AC 3 (E20-S1): a run of regression cases only prints one line for each case, then the
// summary line (#318), and exits 1 when one fails. It needs no Brain setup: with a scratch HOME it reaches no runner check.
// The go command writes nothing in that HOME (#322).
func TestRunRegressionOnly(t *testing.T) {
	goEnv := goEnv(t) // the Go caches stay where they are, out of the scratch HOME
	home := t.TempDir()
	t.Setenv("HOME", home)
	mode := goTelemetryOff(t)
	path := filepath.Join(t.TempDir(), "bugs.toml")
	write(t, path, "[[bug]]\nissue = 9\ntests = [\"no-such-folder:TestGone\"]\n")
	var out, errOut bytes.Buffer
	if code := run([]string{"--probes", path, "--stage", "regression"}, append(os.Environ(), goEnv...), &out, &errOut); code != 1 {
		t.Errorf("run() = %d, want 1 (stderr %q)", code, errOut.String())
	}
	if lines := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n"); len(lines) != 2 ||
		!strings.HasPrefix(lines[0], "bug-9: fail; no-such-folder TestGone: not run (") ||
		lines[1] != "regression: 0 of 1 pass" || errOut.Len() > 0 {
		t.Errorf("run() stdout %q, stderr %q; want one fail line of bug-9, the summary and no error", out.String(), errOut.String())
	}
	for path, data := range tree(t, home) {
		if data != "dir" && path != mode {
			t.Errorf("run() wrote %s in the scratch HOME, want only the telemetry mode file", path)
		}
	}
}

// goTelemetryOff turns off Go telemetry for a go command with the HOME of the test, and
// returns the mode file (#322). In its default mode, local, each go command starts a telemetry
// process that outlives it and writes in the Go config folder of HOME, so the cleanup of a
// scratch HOME can find a new file there. The folder is in HOME on Linux only with no
// XDG_CONFIG_HOME, so the test clears it: the test never writes a mode file outside HOME.
func goTelemetryOff(t *testing.T) string {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", "")
	dir, err := os.UserConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	mode := filepath.Join(dir, "go", "telemetry", "mode")
	write(t, mode, "off\n")
	return mode
}

func TestResultLine(t *testing.T) {
	res := store.EvalResult{Case: "pm", Control: true, Harness: "codex", Status: "not delivered", Why: "no receipt of the pane",
		Label: "PASS", Rule: "the expected answer"}
	want := "pm (control) codex: not delivered, PASS (the expected answer); no receipt of the pane"
	if got := resultLine(res); got != want {
		t.Errorf("resultLine() = %q, want %q", got, want)
	}
	res = store.EvalResult{Case: "pm", Harness: "claude", Status: "delivered", Label: "PASS", Rule: "r", ControlPass: true}
	if got := resultLine(res); got != "pm claude: delivered, PASS (r), control_pass" {
		t.Errorf("resultLine() = %q", got)
	}
}

func TestRun(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	tests := []struct {
		args      []string
		code      int
		outHas    string
		stderrHas string
	}{
		{[]string{"-h"}, 0, "usage: brain-eval", ""},
		{[]string{"--nope"}, 2, "", "usage: brain-eval"},
		{nil, 1, "", "run `brain setup` first"}, // a scratch HOME has no install
	}
	for _, tt := range tests {
		var out, errOut bytes.Buffer
		if code := run(tt.args, os.Environ(), &out, &errOut); code != tt.code {
			t.Errorf("run(%q) = %d, want %d (stderr %q)", tt.args, code, tt.code, errOut.String())
		}
		if !strings.Contains(out.String(), tt.outHas) || !strings.Contains(errOut.String(), tt.stderrHas) {
			t.Errorf("run(%q) stdout %q, stderr %q; want %q and %q", tt.args, out.String(), errOut.String(), tt.outHas, tt.stderrHas)
		}
	}
	if entries, err := os.ReadDir(home); err != nil || len(entries) != 0 {
		t.Errorf("run wrote %v in HOME (%v), want nothing", entries, err)
	}
}
