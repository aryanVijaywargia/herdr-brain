package install

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/aryanVijaywargia/herdr-brain/internal/adapter"
)

// Record is the install record, <data home>/install.json (§11, E8 epic). It holds tokens when
// a snapshot does (owner decision 2026-09-28), so its mode is 0o600.
type Record struct {
	Entries map[string]Entry `json:"entries"` // the start entries, by profile kind, for example "codex"
	// Records are the hook record entries (E11-S5), by profile kind and CLI event, for
	// example "claude PostToolUse" (recordKey).
	Records   map[string]Entry    `json:"records,omitempty"`
	Snapshots map[string]Snapshot `json:"snapshots"` // by hook file path
}

// recordKey is the key of the hook record entry of kind for CLI event event in Record.Records.
func recordKey(kind, event string) string {
	return kind + " " + event
}

// Entry returns the recorded entry of profile kind for hook h.
func (r Record) Entry(kind string, h adapter.Hook) (Entry, bool) {
	if h.Event == adapter.EventStart {
		e, ok := r.Entries[kind]
		return e, ok
	}
	e, ok := r.Records[recordKey(kind, h.CLIEvent)]
	return e, ok
}

// set records entry e of profile kind for hook h.
func (r *Record) set(kind string, h adapter.Hook, e Entry) {
	if h.Event == adapter.EventStart {
		r.Entries[kind] = e
		return
	}
	if r.Records == nil {
		r.Records = map[string]Entry{}
	}
	r.Records[recordKey(kind, h.CLIEvent)] = e
}

// drop removes the recorded entry of profile kind for hook h.
func (r *Record) drop(kind string, h adapter.Hook) {
	if h.Event == adapter.EventStart {
		delete(r.Entries, kind)
		return
	}
	delete(r.Records, recordKey(kind, h.CLIEvent))
}

// Has reports whether the record holds the entry of profile kind for hook h.
func (r Record) Has(kind string, h adapter.Hook) bool {
	_, ok := r.Entry(kind, h)
	return ok
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

// RemoveRecord removes the install record at path when it holds no entry, and reports whether
// it did. It holds the install lock, so an install at the same time either writes its entry
// first, and the record stays, or writes a new record after the removal. The lock file stays:
// another Brain process may wait on it (E8-S5).
func RemoveRecord(path string) (bool, error) {
	lk, err := lock(path)
	if err != nil {
		return false, err
	}
	defer lk.Close() //nolint:errcheck // the close only drops the lock; the file holds no data
	rec, err := Read(path)
	if err != nil {
		return false, err
	}
	if len(rec.Entries) > 0 || len(rec.Records) > 0 {
		return false, nil
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return false, err
	}
	return true, nil
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
