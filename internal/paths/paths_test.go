package paths

import (
	"os"
	"path/filepath"
	"testing"
)

// The plugin env and the hook env (no HERDR_PLUGIN_*) resolve to the same data home.
func TestDataHomeSameInPluginAndHookEnv(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", "")

	hook, err := DataHome()
	if err != nil {
		t.Fatal(err)
	}

	t.Setenv("HERDR_PLUGIN_ROOT", filepath.Join(home, ".config/herdr/plugins/github/brain-bbbf7a6412d6"))
	t.Setenv("HERDR_PLUGIN_STATE_DIR", filepath.Join(home, ".local/state/herdr/plugins/brain"))
	t.Setenv("HERDR_PLUGIN_CONFIG_DIR", filepath.Join(home, ".config/herdr/plugins/config/brain"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "xdg"))
	plugin, err := DataHome()
	if err != nil {
		t.Fatal(err)
	}

	want := filepath.Join(home, ".local", "share", "brain")
	if hook != want || plugin != want {
		t.Errorf("hook env = %q, plugin env = %q, want both %q", hook, plugin, want)
	}
}

func TestDataHomeNeedsAbsoluteHome(t *testing.T) {
	t.Setenv("HOME", "relative/home")
	if _, err := DataHome(); err == nil {
		t.Error("DataHome with a relative HOME: want an error")
	}
}

func TestBindingsFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	got, err := BindingsFile()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(home, ".local", "share", "brain", "bindings.json"); got != want {
		t.Errorf("BindingsFile = %q, want %q", got, want)
	}
}

func TestInstallFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	got, err := InstallFile()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(home, ".local", "share", "brain", "install.json"); got != want {
		t.Errorf("InstallFile = %q, want %q", got, want)
	}
}

func TestConformanceFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	got, err := ConformanceFile()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(home, ".local", "share", "brain", "conformance.json"); got != want {
		t.Errorf("ConformanceFile = %q, want %q", got, want)
	}
}

func TestEvalLock(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	got, err := EvalLock()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(home, ".local", "share", "brain", "eval.lock"); got != want {
		t.Errorf("EvalLock = %q, want %q", got, want)
	}
}

func TestLinkPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	got, err := LinkPath()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(home, ".local", "bin", "brain"); got != want {
		t.Errorf("LinkPath = %q, want %q", got, want)
	}
}

// A binary started through the ~/.local/bin link reports the real checkout path.
func TestResolveFollowsLink(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "plugin", "bin", "brain")
	link := filepath.Join(dir, "local", "bin", "brain")
	for _, d := range []string{filepath.Dir(bin), filepath.Dir(link)} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(bin, link); err != nil {
		t.Fatal(err)
	}
	got, err := resolve(link)
	if err != nil {
		t.Fatal(err)
	}
	if got != bin {
		t.Errorf("resolve(link) = %q, want %q", got, bin)
	}
}

func TestBinaryPathIsAbsolute(t *testing.T) {
	got, err := BinaryPath()
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(got) {
		t.Errorf("BinaryPath = %q, want an absolute path", got)
	}
}

// The workspace map sits in the store folder, next to the store that holds its keys.
func TestWorkspaceMap(t *testing.T) {
	if got, want := WorkspaceMap("/data/brain"), "/data/brain/workspaces.json"; got != want {
		t.Errorf("WorkspaceMap(/data/brain) = %q, want %q", got, want)
	}
}

// The eval results sit in the test store folder (E10-S2).
func TestEvalResults(t *testing.T) {
	if got, want := EvalResults("/data/test"), "/data/test/eval-results.json"; got != want {
		t.Errorf("EvalResults(/data/test) = %q, want %q", got, want)
	}
}

func TestPluginDirs(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	config, state, err := PluginDirs()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(home, ".config", "herdr", "plugins", "config", "brain"); config != want {
		t.Errorf("config = %q, want %q", config, want)
	}
	if want := filepath.Join(home, ".local", "state", "herdr", "plugins", "brain"); state != want {
		t.Errorf("state = %q, want %q", state, want)
	}
}
