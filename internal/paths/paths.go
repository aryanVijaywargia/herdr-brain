// Package paths gives the fixed paths that every Brain entry point uses: the binary path
// for frozen hook commands, the data home and the optional ~/.local/bin link.
// Story and tests: E1-S2.
package paths

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
)

// DataHome returns ~/.local/share/brain. It reads only HOME. It ignores
// HERDR_PLUGIN_* and XDG_DATA_HOME, so a plugin action, a shell and a CLI hook (which has no
// HERDR_PLUGIN_* vars) all get the same folder.
func DataHome() (string, error) {
	home, err := homeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "share", "brain"), nil
}

// BindingsFile returns ~/.local/share/brain/bindings.json, the session bindings file.
// It sits in the data home but outside every store folder.
func BindingsFile() (string, error) {
	dir, err := DataHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "bindings.json"), nil
}

// MainStore returns ~/.local/share/brain/main, the folder of the main store that every real
// herdr session shares. It sits beside the bindings file, not around it.
func MainStore() (string, error) {
	dir, err := DataHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "main"), nil
}

// InstallFile returns ~/.local/share/brain/install.json, the install record (E8). It
// sits in the data home but outside every store folder, because the CLI hook files are shared
// by all bindings.
func InstallFile() (string, error) {
	dir, err := DataHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "install.json"), nil
}

// ConformanceFile returns ~/.local/share/brain/conformance.json, the conformance results
// (E8-S4). The eval runner (E10) writes it; `brain status` reads it. It sits
// outside every store folder, as the install record does.
func ConformanceFile() (string, error) {
	dir, err := DataHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "conformance.json"), nil
}

// EvalLock returns ~/.local/share/brain/eval.lock, the lock of the eval runner (E10-S1). One
// run at a time holds it.
func EvalLock() (string, error) {
	dir, err := DataHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "eval.lock"), nil
}

// LinkPath returns ~/.local/bin/brain, the symlink that `brain setup` offers to make.
func LinkPath() (string, error) {
	home, err := homeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "bin", "brain"), nil
}

// Home returns HOME. It must be an absolute path.
func Home() (string, error) {
	return homeDir()
}

// PluginDirs returns the config and state folders that herdr gives the plugin, as herdr 0.9.1 to
// 0.9.5 make them (#470): $XDG_CONFIG_HOME/herdr/plugins/config/brain and
// $XDG_STATE_HOME/herdr/plugins/brain, else ~/.config/herdr/plugins/config/brain and
// ~/.local/state/herdr/plugins/brain. `herdr plugin uninstall` leaves them on disk (E8 epic),
// so `brain uninstall --data` removes them (E8-S5).
func PluginDirs() (config, state string, err error) {
	configHome, err := xdgDir("XDG_CONFIG_HOME", ".config")
	if err != nil {
		return "", "", err
	}
	stateHome, err := xdgDir("XDG_STATE_HOME", filepath.Join(".local", "state"))
	if err != nil {
		return "", "", err
	}
	return filepath.Join(configHome, "herdr", "plugins", "config", "brain"),
		filepath.Join(stateHome, "herdr", "plugins", "brain"), nil
}

// xdgDir returns the folder of the XDG variable key, else ~/<fallback>. herdr takes a relative
// value relative to its own working folder, which Brain cannot know; so Brain takes only an
// absolute value, as brained.StateDir does, and the XDG spec says.
func xdgDir(key, fallback string) (string, error) {
	if dir := os.Getenv(key); filepath.IsAbs(dir) {
		return dir, nil
	}
	home, err := homeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, fallback), nil
}

// ConfigFile returns config.toml in the plugin config folder (PluginDirs), the Brain config
// file. It holds the live_suggestions switch.
func ConfigFile() (string, error) {
	config, _, err := PluginDirs()
	if err != nil {
		return "", err
	}
	return filepath.Join(config, "config.toml"), nil
}

// BinaryPath returns the absolute path of the running brain binary with all symlinks resolved.
// A hook entry gets this path, so it names the managed checkout (<plugin_root>/bin/brain)
// and not the ~/.local/bin/brain link.
func BinaryPath() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	return resolve(exe)
}

func resolve(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(abs)
}

func homeDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(home) {
		return "", errors.New("paths: HOME is not an absolute path")
	}
	return home, nil
}

// WorkspaceMap returns <store>/workspaces.json, the workspace map of the store folder store.
// `brain` calls write it; the hook only reads it.
func WorkspaceMap(store string) string {
	return filepath.Join(store, "workspaces.json")
}

// Launches returns <store>/launches, the folder of the launch folders of the store folder store
// (E24-S3).
func Launches(store string) string {
	return filepath.Join(store, "launches")
}

// Launch returns <store>/launches/<id>, the private folder of launch record id in the store
// folder store (E24-S3). The folder name is the record id, not the launch ID: launch flags name
// the folder, and other processes can read the flags.
func Launch(store string, id int64) string {
	return filepath.Join(Launches(store), strconv.FormatInt(id, 10))
}

// EvalResults returns <store>/eval-results.json, the results of the last eval run in the test
// store folder store (E10-S2). The eval runner writes it and keeps it through its
// store reset; `brain status` reads it.
func EvalResults(store string) string {
	return filepath.Join(store, "eval-results.json")
}

// EvalReport returns <store>/eval-report.md, the short report of the last eval run in the test
// store folder store: its score, the UNSURE results for your review and the usefulness records
// (E10-S3). The eval runner writes it and keeps it through its store reset.
func EvalReport(store string) string {
	return filepath.Join(store, "eval-report.md")
}
