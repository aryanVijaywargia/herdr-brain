package harness

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// psLayout is the `ps -o lstart=` time in the C locale, with the spaces collapsed: "Wed Oct 7
// 10:12:03 2026" on macOS and on Linux procps.
const psLayout = "Mon Jan 2 15:04:05 2006"

// StartedBefore reports whether the agent with the processes pids started before Brain wrote
// its install record (#249). A CLI reads its hook file when it starts, so such an agent runs
// without the Brain hook and has no receipt; that is no hook failure. The signal is the
// earliest process start against the time of the last write of the install record: every
// recorded hook entry is that old or older. [NEW] the signal. ps gives whole seconds, so a
// start in the second of the write counts as after. No install record means no hook entry.
func StartedBefore(ctx context.Context, record string, pids []int) (bool, error) {
	fi, err := os.Stat(record)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil || len(pids) == 0 {
		return false, err
	}
	started, err := processStart(ctx, pids)
	if err != nil {
		return false, err
	}
	return started.Before(fi.ModTime().Truncate(time.Second)), nil
}

// processStart returns the earliest start time of the processes pids, to the second.
func processStart(ctx context.Context, pids []int) (time.Time, error) {
	ids := make([]string, len(pids))
	for i, pid := range pids {
		ids[i] = strconv.Itoa(pid)
	}
	cmd := exec.CommandContext(ctx, "ps", "-o", "lstart=", "-p", strings.Join(ids, ","))
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	out, err := cmd.Output()
	if err != nil {
		return time.Time{}, fmt.Errorf("ps: %w", err)
	}
	var first time.Time
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		t, err := time.ParseInLocation(psLayout, strings.Join(strings.Fields(line), " "), time.Local)
		if err != nil {
			return time.Time{}, fmt.Errorf("ps start time: %w", err)
		}
		if first.IsZero() || t.Before(first) {
			first = t
		}
	}
	return first, nil
}
