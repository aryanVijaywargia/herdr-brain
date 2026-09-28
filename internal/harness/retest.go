package harness

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// marker is the retest marker of one harness: the time of the last hook input that the
// profile could not parse (S14). It holds no hook input.
type marker struct {
	At time.Time `json:"at"`
}

// MarkRetest records in dir that the hook input of harness kind had an unknown shape at time
// at. The hook calls it, so it writes no store: the file is <dir>/<kind>.json. [NEW] the
// folder and the file name.
func MarkRetest(dir, kind string, at time.Time) error {
	data, err := json.Marshal(marker{At: at})
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("retest marker: %w", err)
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
