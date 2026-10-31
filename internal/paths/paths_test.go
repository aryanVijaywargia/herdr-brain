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
	if got, want := EvalReport("/data/test"), "/data/test/eval-report.md"; got != want {
		t.Errorf("EvalReport(/data/test) = %q, want %q", got, want)
	}
}

// The plugin folders are where herdr 0.9.5 puts them: under XDG_CONFIG_HOME and XDG_STATE_HOME
// when they are set, else under HOME (#470). Brain cannot know the working folder of herdr, so
// a relative value counts as not set.
func TestPluginDirs(t *testing.T) {
	home := t.TempDir()
	xdg := t.TempDir()
	homeConfig := filepath.Join(home, ".config", "herdr", "plugins", "config", "brain")
	homeState := filepath.Join(home, ".local", "state", "herdr", "plugins", "brain")
	for _, tt := range []struct {
		name                  string
		configHome, stateHome string
		wantConfig, wantState string
	}{
		{"no XDG", "", "", homeConfig, homeState},
		{"XDG", filepath.Join(xdg, "c"), filepath.Join(xdg, "s"),
			filepath.Join(xdg, "c", "herdr", "plugins", "config", "brain"), filepath.Join(xdg, "s", "herdr", "plugins", "brain")},
		{"XDG config only", filepath.Join(xdg, "c"), "", filepath.Join(xdg, "c", "herdr", "plugins", "config", "brain"), homeState},
		{"XDG state only", "", filepath.Join(xdg, "s"), homeConfig, filepath.Join(xdg, "s", "herdr", "plugins", "brain")},
		{"relative XDG", "rel/c", "rel/s", homeConfig, homeState},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("HOME", home)
			t.Setenv("XDG_CONFIG_HOME", tt.configHome)
			t.Setenv("XDG_STATE_HOME", tt.stateHome)
			config, state, err := PluginDirs()
			if err != nil {
				t.Fatal(err)
			}
			if config != tt.wantConfig || state != tt.wantState {
				t.Errorf("PluginDirs() = %q, %q, want %q, %q", config, state, tt.wantConfig, tt.wantState)
			}
			file, err := ConfigFile()
			if err != nil {
				t.Fatal(err)
			}
			if want := filepath.Join(tt.wantConfig, "config.toml"); file != want {
				t.Errorf("ConfigFile() = %q, want %q", file, want)
			}
		})
	}
}

// A relative HOME is refused also when the XDG variables are set: the fallback of the other
// folder needs it.
func TestPluginDirsRelativeHome(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("HOME", "rel/home")
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(xdg, "c"))
	t.Setenv("XDG_STATE_HOME", "")
	if _, _, err := PluginDirs(); err == nil {
		t.Error("PluginDirs() with a relative HOME = nil error, want an error")
	}
}
