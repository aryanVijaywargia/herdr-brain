package harness

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
)

// Pass is the result of a completed conformance run that passed. The other results are
// "pending" (the run has not finished), "fail" and "inconclusive"; none of them is verified
// (§8.5, E10-S3).
const Pass = "pass"

// Conformance is <data home>/conformance.json. The eval runner (E10) writes it; Check only
// reads it. It never reads test items or receipts.
type Conformance struct {
	// Results in run order. The last result of a harness is its current one.
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
