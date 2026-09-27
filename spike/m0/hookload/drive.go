package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"slices"
	"strings"
	"time"
)

// sampleInput is a Claude SessionStart hook input with the keys M0 saw.
const sampleInput = `{"session_id":"00000000-0000-4000-8000-000000000000","transcript_path":"/tmp/x.jsonl",` +
	`"cwd":"/tmp","hook_event_name":"SessionStart","source":"startup"}`

// drive runs the hook as a child process n times and logs the wall time of each run, from
// the fork to the exit. This adds process start, Go runtime start and exit to the hook's own
// ms_total.
func drive(args []string, stderr io.Writer) error {
	fs := flag.NewFlagSet("drive", flag.ContinueOnError)
	fs.SetOutput(stderr)
	n := fs.Int("n", 50, "number of runs")
	out := fs.String("out", "", "JSONL file for the wall times")
	gap := fs.Duration("gap", 50*time.Millisecond, "pause between runs [NEW]")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *out == "" {
		return errors.New("drive: -out is required")
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	f, err := os.OpenFile(*out, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(f)
	for i := range *n {
		var stdout bytes.Buffer
		cmd := exec.Command(self, "hook")
		cmd.Stdin = strings.NewReader(sampleInput)
		cmd.Stdout = &stdout
		t := time.Now()
		err := cmd.Run()
		wall := since(t)
		if err != nil {
			return errors.Join(fmt.Errorf("run %d: %w", i, err), f.Close())
		}
		line := map[string]any{"run": i, "ms_wall": wall, "out_bytes": stdout.Len()}
		if err := enc.Encode(line); err != nil {
			return errors.Join(err, f.Close())
		}
		time.Sleep(*gap)
	}
	return f.Close()
}

// stats prints n, p50, p95 and max of every ms_ field in each JSONL file. Percentiles use
// the nearest-rank method.
func stats(files []string, stdout io.Writer) error {
	var b bytes.Buffer
	for _, file := range files {
		vals, err := readFields(file)
		if err != nil {
			return err
		}
		keys := make([]string, 0, len(vals))
		for k := range vals {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		fmt.Fprintf(&b, "## %s\n", file)
		for _, k := range keys {
			v := vals[k]
			slices.Sort(v)
			fmt.Fprintf(&b, "%-12s n=%-4d p50=%7.2f p95=%7.2f max=%7.2f\n",
				k, len(v), rank(v, 50), rank(v, 95), v[len(v)-1])
		}
	}
	_, err := stdout.Write(b.Bytes())
	return err
}

func readFields(file string) (map[string][]float64, error) {
	f, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	defer f.Close() //nolint:errcheck // read-only: Close cannot lose data
	vals := map[string][]float64{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var line map[string]any
		if err := json.Unmarshal(sc.Bytes(), &line); err != nil {
			return nil, fmt.Errorf("%s: %w", file, err)
		}
		for k, v := range line {
			if x, ok := v.(float64); ok && strings.HasPrefix(k, "ms_") {
				vals[k] = append(vals[k], x)
			}
		}
	}
	return vals, sc.Err()
}

// rank returns the p-th percentile of sorted v by nearest rank.
func rank(v []float64, p float64) float64 {
	i := int(math.Ceil(p/100*float64(len(v)))) - 1
	return v[max(i, 0)]
}
