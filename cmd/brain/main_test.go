package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
)

func TestRun(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cases := []struct {
		args      []string
		code      int
		stdoutHas string
	}{
		{[]string{"version"}, 0, "brain dev"},
		{[]string{"status"}, 0, "not set up"},
		{[]string{"help"}, 0, "usage:"},
		{nil, 2, ""},
		{[]string{"nope"}, 2, ""},
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
