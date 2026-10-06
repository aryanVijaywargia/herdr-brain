package harness

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// marker is the retest marker of one harness: the time of the last hook input that the
// profile could not parse (S14), and the hash of that profile. It holds no hook input.
type marker struct {
	At time.Time `json:"at"`
	// Profile is the adapter.Profile.SHA256 that could not parse the input. A marker of
	// another profile is stale, so a Brain update with a new profile clears it (#251). A
	// marker of an older writer has none; it stays until a later pass. [NEW] the field.
	Profile string `json:"profile_sha256"`
}

// MarkRetest records in dir that the hook input of harness kind had a shape that the profile
// with hash profile cannot parse, at time at. The hook calls it, so it writes no store: the
// file is <dir>/<kind>.json. [NEW] the folder and the file name. Two hooks can mark at about
// the same time, so it holds <dir>/<kind>.lock and keeps a newer marker of the same profile
// that is already there (#118).
func MarkRetest(dir, kind, profile string, at time.Time) (err error) {
	data, err := json.Marshal(marker{At: at, Profile: profile})
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("retest marker: %w", err)
	}
	lock, err := os.OpenFile(filepath.Join(dir, kind+".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("retest marker: %w", err)
	}
	defer func() { err = errors.Join(err, lock.Close()) }() // Close also drops the lock
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("retest marker: lock %s: %w", lock.Name(), err)
	}
	m, ok, err := readMarker(dir, kind)
	if err != nil {
		return err
	}
	if ok && m.Profile == profile && !m.At.Before(at) {
		return nil
	}
	f, err := os.CreateTemp(dir, kind+".*.tmp")
	if err != nil {
		return fmt.Errorf("retest marker: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return fmt.Errorf("retest marker: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(f.Name())
		return fmt.Errorf("retest marker: %w", err)
	}
	if err := os.Rename(f.Name(), filepath.Join(dir, kind+".json")); err != nil {
		_ = os.Remove(f.Name())
		return fmt.Errorf("retest marker: %w", err)
	}
	return nil
}

// readMarker returns the retest marker of harness kind in dir. A missing file means no marker.
func readMarker(dir, kind string) (marker, bool, error) {
	path := filepath.Join(dir, kind+".json")
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return marker{}, false, nil
	}
	if err != nil {
		return marker{}, false, err
	}
	var m marker
	if err := json.Unmarshal(data, &m); err != nil {
		return marker{}, false, fmt.Errorf("retest marker %s: %w", path, err)
	}
	return m, true, nil
}
