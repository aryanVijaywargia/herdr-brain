package harness

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aryanVijaywargia/herdr-brain/internal/herdr"
)

// TestWriteConformanceSequences checks the conformance runs of one harness with the same
// hashes (E10-S3 AC 2 and 3): a pass is verified; a later fail, incomplete reply
// (inconclusive) or interrupted run (pending) is not; a pass after it is verified again. A new
// CLI version after a pass stays verified with a warning.
func TestWriteConformanceSequences(t *testing.T) {
	for _, tc := range []struct{ kind, version string }{
		{"claude", "2.1.283 (Claude Code)"},
		{"codex", "codex-cli 0.154.0"},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			e := newEnv(t)
			p := load(t, tc.kind)
			e.cli(t, tc.kind, tc.version)
			entry := e.install(t, p)
			seen := fakeHerdr{panes: []herdr.Pane{agentPane(tc.kind)}}
			at := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
			n := 0
			write := func(result string) {
				t.Helper()
				n++
				at = at.Add(time.Minute)
				r := Result{Harness: tc.kind, CLIVersion: p.TestedVersion, ProfileSHA256: p.SHA256,
					EntrySHA256: entry.SHA256, RunID: "run-" + strconv.Itoa(n), Result: result, At: at}
				if err := WriteConformance(e.files.Conformance, r); err != nil {
					t.Fatal(err)
				}
			}
			for _, bad := range []string{"fail", "inconclusive", "pending"} {
				write("pending")
				if s := check(t, e, seen, p); s.Verified {
					t.Fatalf("pending run: %+v, want not verified", s)
				}
				write(Pass)
				if s := check(t, e, seen, p); s.State() != Verified {
					t.Fatalf("pass: state %s, want verified", s.State())
				}
				write("pending") // the next run starts
				if bad != "pending" {
					write(bad) // it completes; "pending" is a run that never completes
				}
				if s := check(t, e, seen, p); s.Verified || s.State() != Configured {
					t.Fatalf("pass then %s: %+v, want configured, not verified", bad, s)
				}
			}
			write(Pass)
			if s := check(t, e, seen, p); s.State() != Verified {
				t.Fatalf("a later pass: state %s, want verified", s.State())
			}
			e.cli(t, tc.kind, strings.Replace(tc.version, p.TestedVersion, "9.9.9", 1))
			if s := check(t, e, seen, p); s.State() != Verified || s.VersionNote() == "" {
				t.Fatalf("new CLI version after a pass: %+v, want verified with a version note", s)
			}
		})
	}
}

// TestWriteConformanceKeepsOthers checks that a write keeps one record for each harness and
// the records of the other harnesses, and leaves no temp file.
func TestWriteConformanceKeepsOthers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data", "conformance.json")
	for _, r := range []Result{
		{Harness: "claude", RunID: "r1", Result: "pending"},
		{Harness: "codex", RunID: "r1", Result: "pending"},
		{Harness: "claude", RunID: "r1", Result: Pass},
		{Harness: "claude", RunID: "r2", Result: "fail"},
	} {
		if err := WriteConformance(path, r); err != nil {
			t.Fatal(err)
		}
	}
	c, err := readConformance(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Results) != 2 {
		t.Fatalf("records = %+v, want one for each harness", c.Results)
	}
	if r, _ := last(c, "codex"); r.Result != "pending" || r.RunID != "r1" {
		t.Fatalf("codex = %+v, want its pending record kept", r)
	}
	if r, _ := last(c, "claude"); r.Result != "fail" || r.RunID != "r2" {
		t.Fatalf("claude = %+v, want the r2 fail", r)
	}
	names, err := filepath.Glob(filepath.Join(filepath.Dir(path), "*.tmp"))
	if err != nil || len(names) != 0 {
		t.Fatalf("temp files = %v, %v, want none", names, err)
	}
	if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WriteConformance(path, Result{Harness: "claude"}); err == nil {
		t.Fatal("WriteConformance on a broken file = nil, want an error")
	}
}
