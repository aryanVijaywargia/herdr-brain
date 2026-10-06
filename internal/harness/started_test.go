package harness

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// AC 3 #249: an agent process that started before the last write of the install record
// started before Brain; one that started after it did not. The test process is the agent.
func TestStartedBefore(t *testing.T) {
	record := filepath.Join(t.TempDir(), "install.json")
	pids := []int{os.Getpid()}
	if got, err := StartedBefore(context.Background(), record, pids); err != nil || got {
		t.Errorf("StartedBefore(no install record) = %v, %v, want false", got, err)
	}
	if err := os.WriteFile(record, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name    string
		written time.Time
		want    bool
	}{
		{"install after the start", time.Now().Add(time.Hour), true},
		{"install before the start", time.Now().Add(-24 * time.Hour), false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if err := os.Chtimes(record, tt.written, tt.written); err != nil {
				t.Fatal(err)
			}
			if got, err := StartedBefore(context.Background(), record, pids); err != nil || got != tt.want {
				t.Errorf("StartedBefore() = %v, %v, want %v", got, err, tt.want)
			}
		})
	}
	if _, err := StartedBefore(context.Background(), record, []int{1 << 30}); err == nil {
		t.Errorf("StartedBefore(no such process) = nil error, want a ps error")
	}
}
