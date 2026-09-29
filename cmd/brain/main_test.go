package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRun(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("HERDR_ENV", "") // the test can run in a herdr pane
	cases := []struct {
		args      []string
		code      int
		stdoutHas string
	}{
		{[]string{"version"}, 0, "brain dev"},
		{[]string{"status"}, 0, "not in a herdr pane"},
		{[]string{"status", "x"}, 1, ""},
		{[]string{"doctor"}, 1, "not in a herdr pane"},
		{[]string{"doctor", "x"}, 1, ""},
		{[]string{"help"}, 0, "doctor    check the Brain setup"},
		{[]string{"help"}, 0, "usage:"},
		{nil, 2, ""},
		{[]string{"nope"}, 2, ""},
		{[]string{"remember"}, 1, ""},
		{[]string{"review", "--nope"}, 1, ""},
		{[]string{"pin"}, 1, ""},
		{[]string{"undo"}, 1, ""},
		{[]string{"help"}, 0, "undo      <item>"},
		{[]string{"forget"}, 1, ""},
		{[]string{"help"}, 0, "forget    <item>"},
		{[]string{"repair", "x"}, 1, ""},
		{[]string{"help"}, 0, "repair    [--confirm]"},
		{[]string{"pane", "x"}, 1, ""},
		{[]string{"help"}, 0, "pane      review --proposal <id>"},
		{[]string{"uninstall", "--nope"}, 1, ""},
		{[]string{"help"}, 0, "uninstall [--apply] [--data]"},
		{[]string{"learn"}, 1, ""},
		{[]string{"learn", "off"}, 1, ""}, // not in a herdr pane
		{[]string{"help"}, 0, "learn     on|off [--workspace <id> | --pane <id>]"},
		{[]string{"extract"}, 1, ""},
		{[]string{"help"}, 0, "extract   on|off [--workspace <id>]"},
	}
	for _, c := range cases {
		var out, errOut bytes.Buffer
		if got := run(c.args, strings.NewReader(""), &out, &errOut); got != c.code {
			t.Errorf("run(%v) = %d, want %d", c.args, got, c.code)
		}
		if !strings.Contains(out.String(), c.stdoutHas) {
			t.Errorf("run(%v) stdout = %q, want it to contain %q", c.args, out.String(), c.stdoutHas)
		}
	}
}

// `brain status` from a plugin action and from a CLI hook env (no HERDR_PLUGIN_*) shows one data home.
func TestStatusDataHomeSameInPluginAndHookEnv(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("HERDR_ENV", "")
	dataLine := func() string {
		var out, errOut bytes.Buffer
		if code := run([]string{"status"}, strings.NewReader(""), &out, &errOut); code != 0 {
			t.Fatalf("status = %d, stderr %q", code, errOut.String())
		}
		for _, l := range strings.Split(out.String(), "\n") {
			if strings.HasPrefix(l, "data: ") {
				return l
			}
		}
		t.Fatalf("status output has no data line: %q", out.String())
		return ""
	}

	hook := dataLine()
	t.Setenv("HERDR_PLUGIN_ROOT", filepath.Join(home, "plugin"))
	t.Setenv("HERDR_PLUGIN_STATE_DIR", filepath.Join(home, "state"))
	t.Setenv("HERDR_PLUGIN_CONFIG_DIR", filepath.Join(home, "config"))
	plugin := dataLine()

	want := "data: " + filepath.Join(home, ".local", "share", "brain")
	if hook != want || plugin != want {
		t.Errorf("hook env %q, plugin env %q, want both %q", hook, plugin, want)
	}
}

// `brain hook` prints nothing and exits 0 on any error, so it never fails a session start.
func TestHookErrorsExitZero(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("HERDR_ENV", "1")
	for _, args := range [][]string{
		{"hook"},
		{"hook", "--profile", "nope", "--event", "start"},
		{"hook", "--profile", "claude", "--event", "start"}, // no herdr socket in the environment
	} {
		var out, errOut bytes.Buffer
		if code := run(args, strings.NewReader("{}"), &out, &errOut); code != 0 || out.Len() != 0 || errOut.Len() != 0 {
			t.Errorf("run(%q) = %d, stdout %q, stderr %q; want 0 and no output", args, code, out.String(), errOut.String())
		}
	}
}

// `brain remember` with a token writes nothing and prints the class, not the token (E4-S1 AC).
func TestRememberSecret(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	token := "ghp_" + strings.Repeat("a1B2", 9)
	var out, errOut bytes.Buffer
	if got := run([]string{"remember", "token " + token}, strings.NewReader(""), &out, &errOut); got != 1 {
		t.Errorf("run(remember token) = %d, want 1", got)
	}
	if !strings.Contains(errOut.String(), "github-token") || strings.Contains(errOut.String()+out.String(), token) {
		t.Errorf("run(remember token) stderr = %q, stdout = %q; want the class and no token", errOut.String(), out.String())
	}
	entries, err := os.ReadDir(home)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("run(remember token) wrote %v in HOME, want nothing", entries)
	}
}
