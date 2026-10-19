package harness

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// AC 3 #249: an agent process that started before Brain installed the start entry of its CLI
// started before Brain; one that started after it did not. The test process is the agent.
func TestStartedBefore(t *testing.T) {
	dir := t.TempDir()
	f := Files{Install: filepath.Join(dir, "install.json"), Installed: filepath.Join(dir, "installed")}
	pids := []int{os.Getpid()}
	before := func() bool {
		t.Helper()
		got, err := StartedBefore(context.Background(), f, "claude", pids)
		if err != nil {
			t.Fatalf("StartedBefore() = %v", err)
		}
		return got
	}
	if before() {
		t.Errorf("StartedBefore(no install record) = true, want false")
	}
	// An install before MarkInstalled existed: the install record time stands in.
	if err := os.WriteFile(f.Install, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	later, earlier := time.Now().Add(time.Hour), time.Now().Add(-24*time.Hour)
	if err := os.Chtimes(f.Install, later, later); err != nil {
		t.Fatal(err)
	}
	if !before() {
		t.Errorf("StartedBefore(install record after the start, no install time) = false, want true")
	}
	// The install time of claude wins over a later write of the install record, for example
	// the install of codex (PR #262).
	if err := MarkInstalled(f.Installed, "claude", earlier); err != nil {
		t.Fatal(err)
	}
	if before() {
		t.Errorf("StartedBefore(claude installed before the start, record written later) = true, want false")
	}
	if err := MarkInstalled(f.Installed, "claude", later); err != nil {
		t.Fatal(err)
	}
	if !before() {
		t.Errorf("StartedBefore(claude installed after the start) = false, want true")
	}
	if _, err := StartedBefore(context.Background(), f, "claude", []int{1 << 30}); err == nil {
		t.Errorf("StartedBefore(no such process) = nil error, want a ps error")
	}
}
