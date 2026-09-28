// Package paths gives the fixed paths that every Brain entry point uses: the binary path
// for frozen hook commands, the data home and the optional ~/.local/bin link.
// Decision and tests: docs/design/decisions.md (D1, D2, D3).
package paths

import (
	"errors"
	"os"
	"path/filepath"
)

// DataHome returns ~/.local/share/brain (HLD §7.5, §8.1). It reads only HOME. It ignores
// HERDR_PLUGIN_* and XDG_DATA_HOME, so a plugin action, a shell and a CLI hook (which has no
// HERDR_PLUGIN_* vars) all get the same folder.
func DataHome() (string, error) {
	home, err := homeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "share", "brain"), nil
}

// BindingsFile returns ~/.local/share/brain/bindings.json, the session bindings file (HLD §8.3).
// It sits in the data home but outside every store folder.
func BindingsFile() (string, error) {
	dir, err := DataHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "bindings.json"), nil
}

// InstallFile returns ~/.local/share/brain/install.json, the install record (HLD §11, E8). It
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
// (HLD §8.5, E8-S4). The eval runner (E10) writes it; `brain status` reads it. It sits
// outside every store folder, as the install record does.
func ConformanceFile() (string, error) {
	dir, err := DataHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "conformance.json"), nil
}

// LinkPath returns ~/.local/bin/brain, the symlink that `brain setup` offers to make.
func LinkPath() (string, error) {
	home, err := homeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "bin", "brain"), nil
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

// WorkspaceMap returns <store>/workspaces.json, the workspace map of the store folder store
// (HLD §8.3 step 4, §11). `brain` calls write it; the hook only reads it.
func WorkspaceMap(store string) string {
	return filepath.Join(store, "workspaces.json")
}
