package store

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestEvalRunRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "eval-results.json")
	if _, err := ReadEvalRun(path); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("ReadEvalRun(no file) = %v, want fs.ErrNotExist", err)
	}
	at := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	want := EvalRun{Started: at, Finished: at.Add(time.Minute), Results: []EvalResult{{
		Case: "pnpm-update", Stage: "update", Harness: "claude", Status: "delivered", Label: "PASS",
		Rule: "expected answer", Reply: "bun", Markers: map[string]string{"marker": "kest-1234"},
		Session: "brain-eval", Store: "/tmp/test", BriefHash: "ab", Generation: 2,
		ItemRevisions: []RevisionRef{{Item: 1, Revision: 2}},
	}}}
	if err := WriteEvalRun(path, want); err != nil {
		t.Fatal(err)
	}
	got, err := ReadEvalRun(path)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("ReadEvalRun() = %+v, %v, want %+v", got, err, want)
	}
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("results file mode = %v, %v, want 0600", fi.Mode().Perm(), err)
	}
	if left, _ := filepath.Glob(filepath.Join(filepath.Dir(path), ".eval-results-*")); len(left) != 0 {
		t.Errorf("temp files %v remain", left)
	}
}
