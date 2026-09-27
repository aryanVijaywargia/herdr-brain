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
