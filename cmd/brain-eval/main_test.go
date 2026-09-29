package main

import (
	"bytes"
	"os"
	"reflect"
	"strings"
	"testing"

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
		{[]string{"--probe", "gemini", "--prompt", "hi"}, nil, "want claude or codex"},
		{[]string{"x"}, nil, "unexpected argument"},
		{[]string{"--probes", "../../evals/probes", "--step", "a"}, nil, "--probes goes with no other flag"},
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
