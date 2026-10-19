package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// EvalRun is the results file of the last eval run (E10-S2). The eval runner writes
// it in the test store folder; `brain status` reads it. It is not a table: the runner empties
// the test store before each case, and the file must outlive that reset.
type EvalRun struct {
	Started  time.Time    `json:"started"`
	Finished time.Time    `json:"finished"`
	Results  []EvalResult `json:"results"`
}

// EvalResult is the result of one probe of one case.
type EvalResult struct {
	Case    string `json:"case"`
	Stage   string `json:"stage,omitempty"`
	Source  string `json:"source,omitempty"`
	Control bool   `json:"control,omitempty"` // the run with the seed steps left out
	Harness string `json:"harness"`
	// Status is the run status: delivered, not delivered, reply incomplete or skipped.
	Status string `json:"status"`
	Why    string `json:"why,omitempty"` // why the status is not delivered or skipped
	// Label is PASS, STALE, INVENTED, MISS or UNSURE; "" when the case gets no label.
	Label string `json:"label,omitempty"`
	Rule  string `json:"rule,omitempty"` // the rule that decided the label
	// ControlPass flags a meaning case whose control passed too: the pass may not come from
	// Brain.
	ControlPass    bool              `json:"control_pass,omitempty"`
	Reply          string            `json:"reply,omitempty"`
	Markers        map[string]string `json:"markers,omitempty"`
	ExpectRevision int64             `json:"expect_revision,omitempty"`
	CLIVersion     string            `json:"cli_version,omitempty"`
	Profile        string            `json:"profile,omitempty"`
	ProfileSHA256  string            `json:"profile_sha256,omitempty"`
	Session        string            `json:"session"` // the store binding: its herdr session
	Store          string            `json:"store"`   // and its store folder
	BriefHash      string            `json:"brief_hash,omitempty"`
	Generation     int64             `json:"generation,omitempty"`
	ItemRevisions  []RevisionRef     `json:"item_revisions,omitempty"`
}

// WriteEvalRun replaces the results file at path atomically: it writes a temp file in the same
// folder and renames it over the old file.
func WriteEvalRun(path string, r EvalRun) error {
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	// os.CreateTemp makes the file with mode 0o600; the rename keeps it.
	tmp, err := os.CreateTemp(filepath.Dir(path), ".eval-results-*.tmp")
	if err != nil {
		return err
	}
	if err := writeRename(tmp, path, append(data, '\n')); err != nil {
		_ = os.Remove(tmp.Name()) // the temp file is an orphan now; the write error is the one to report
		return err
	}
	return nil
}

func writeRename(tmp *os.File, path string, data []byte) error {
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close() // the write error is the one to report
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close() // the sync error is the one to report
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// ReadEvalRun reads the results file at path. With no file, the error wraps fs.ErrNotExist.
func ReadEvalRun(path string) (EvalRun, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return EvalRun{}, err
	}
	var r EvalRun
	if err := json.Unmarshal(data, &r); err != nil {
		return EvalRun{}, fmt.Errorf("%s: %w", path, err)
	}
	return r, nil
}
