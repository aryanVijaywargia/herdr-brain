package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// psLayout is the `ps -o lstart=` time in the C locale, with the spaces collapsed: "Wed Oct 7
// 10:12:03 2026" on macOS and on Linux procps.
const psLayout = "Mon Jan 2 15:04:05 2006"

// installed is the install time of the Brain start entry of one CLI.
type installed struct {
	At time.Time `json:"at"`
}

// MarkInstalled records in dir that Brain installed the start entry of harness kind at time
// at: <dir>/<kind>.json. Setup and doctor call it after each start entry they add (#249). A
// later install of another CLI keeps it. [NEW] the folder and the file.
func MarkInstalled(dir, kind string, at time.Time) error {
	data, err := json.Marshal(installed{At: at})
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("install time: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, kind+".json"), data, 0o600); err != nil {
		return fmt.Errorf("install time: %w", err)
	}
	return nil
}

// installTime returns the install time of the start entry of kind. An install before
// MarkInstalled existed has no file: then the last write of the install record stands in,
// which is that install or later. No install record means no entry, so ok is false.
func installTime(f Files, kind string) (at time.Time, ok bool, err error) {
	path := filepath.Join(f.Installed, kind+".json")
	data, err := os.ReadFile(path)
	if err == nil {
		var in installed
		if err := json.Unmarshal(data, &in); err != nil {
			return time.Time{}, false, fmt.Errorf("install time %s: %w", path, err)
		}
		return in.At, true, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return time.Time{}, false, err
	}
	fi, err := os.Stat(f.Install)
	if errors.Is(err, fs.ErrNotExist) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, err
	}
	return fi.ModTime(), true, nil
}

// StartedBefore reports whether the agent of harness kind with the processes pids started
// before Brain installed the start entry of kind (#249). A CLI reads its hook file when it
// starts, so such an agent runs without the Brain hook and has no receipt; that is no hook
// failure. The signal is the earliest process start against the install time of the entry.
// [NEW] the signal. ps gives whole seconds, so a start in the second of the install counts as
// after.
func StartedBefore(ctx context.Context, f Files, kind string, pids []int) (bool, error) {
	at, ok, err := installTime(f, kind)
	if err != nil || !ok || len(pids) == 0 {
		return false, err
	}
	started, err := processStart(ctx, pids)
	if err != nil {
		return false, err
	}
	return started.Before(at.Truncate(time.Second)), nil
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
