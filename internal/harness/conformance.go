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

// Pass is the result of a completed conformance run that passed. The other results are
// "pending" (the run has not finished), "fail" and "inconclusive"; none of them is verified
// (§8.5, E10-S3).
const Pass = "pass"

// Conformance is <data home>/conformance.json. The eval runner (E10) writes it with
// WriteConformance; Check only reads it. It holds no test item and no receipt.
type Conformance struct {
	// Results holds one record for each harness (E10-S3). A file of an older writer can hold
	// more; the last record of a harness is its current one.
	Results []Result `json:"results"`
}

// Result is one conformance run of one harness (§8.5).
type Result struct {
	Harness       string `json:"harness"`        // the profile kind, for example "codex"
	CLIVersion    string `json:"cli_version"`    // the version word of `<cli> --version`
	ProfileSHA256 string `json:"profile_sha256"` // adapter.Profile.SHA256
	EntrySHA256   string `json:"entry_sha256"`   // install.Entry.SHA256
	RunID         string `json:"run_id"`
	Result        string `json:"result"` // pending, pass, fail or inconclusive
	// At is the time the run completed. A retest marker after it invalidates the pass (S14).
	// [NEW] the field.
	At time.Time `json:"at"`
}

// readConformance reads the conformance results at path. No file is no results: no
// conformance test ran.
func readConformance(path string) (Conformance, error) {
	var c Conformance
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return Conformance{}, err
	}
	if err := json.Unmarshal(data, &c); err != nil {
		return Conformance{}, fmt.Errorf("conformance results %s: %w", path, err)
	}
	return c, nil
}

// last returns the current result of harness.
func last(c Conformance, harness string) (Result, bool) {
	for i := len(c.Results) - 1; i >= 0; i-- {
		if c.Results[i].Harness == harness {
			return c.Results[i], true
		}
	}
	return Result{}, false
}

// WriteConformance makes r the one record of r.Harness in the conformance file at path and
// keeps the records of the other harnesses (E10-S3). Each write replaces the whole file with a
// rename, so a reader never sees half a file. It holds <path>.lock, so two writers do not lose
// a record. [NEW] the lock file name.
func WriteConformance(path string, r Result) (err error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("conformance results: %w", err)
	}
	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("conformance results: %w", err)
	}
	defer func() { err = errors.Join(err, lock.Close()) }() // Close also drops the lock
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("conformance results: lock %s: %w", lock.Name(), err)
	}
	c, err := readConformance(path)
	if err != nil {
		return err
	}
	var out Conformance
	for _, old := range c.Results {
		if old.Harness != r.Harness {
			out.Results = append(out.Results, old)
		}
	}
	out.Results = append(out.Results, r)
	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return fmt.Errorf("conformance results: %w", err)
	}
	if _, err := f.Write(append(data, '\n')); err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return fmt.Errorf("conformance results: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(f.Name())
		return fmt.Errorf("conformance results: %w", err)
	}
	if err := os.Rename(f.Name(), path); err != nil {
		_ = os.Remove(f.Name())
		return fmt.Errorf("conformance results: %w", err)
	}
	return nil
}
