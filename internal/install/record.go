package install

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// Record is the install record, <data home>/install.json (§11, E8 epic). It holds tokens when
// a snapshot does (owner decision 2026-09-28), so its mode is 0o600.
type Record struct {
	Entries   map[string]Entry    `json:"entries"`   // by profile kind, for example "codex"
	Snapshots map[string]Snapshot `json:"snapshots"` // by hook file path
}

// Entry is one installed Brain entry.
type Entry struct {
	Target  string `json:"target"`       // the hook file
	Command string `json:"command"`      // the frozen hook command
	Text    string `json:"entry"`        // the exact bytes of the matcher group
	SHA256  string `json:"entry_sha256"` // hex SHA-256 of Text
	Wrap    string `json:"wrap"`         // what install created around the entry
}

// Snapshot holds the whole hook file before Brain's first edit. It is recovery material only:
// uninstall never restores it, because you may have changed the file since (§8.1).
type Snapshot struct {
	Missing bool   `json:"missing,omitempty"` // the file did not exist
	Data    []byte `json:"data,omitempty"`    // the original bytes, base64 in the JSON
}

// Read reads the install record at path. No file is an empty record: Brain installed nothing.
func Read(path string) (Record, error) {
	rec := Record{Entries: map[string]Entry{}, Snapshots: map[string]Snapshot{}}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return rec, nil
	}
	if err != nil {
		return Record{}, err
	}
	if err := json.Unmarshal(data, &rec); err != nil {
		return Record{}, fmt.Errorf("install record %s: %w", path, err)
	}
	return rec, nil
}

// Files returns the install record at path and its lock file: the files that
// `brain uninstall` removes after the hook entries (E8-S5).
func Files(path string) []string {
	return []string{path, lockFile(path)}
}

func lockFile(record string) string {
	return record + ".lock"
}

// write replaces the install record at path atomically.
func write(path string, rec Record) error {
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	// 0o700, as the bindings file: the data home holds your items, so only you may list it.
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return writeFile(file{path: path, mode: 0o600}, append(data, '\n'))
}
