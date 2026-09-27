// brainspike is the M0 test hook. It logs timing, env and input keys,
// asks herdr for the pane, and prints a marked additionalContext.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

func main() {
	t0 := time.Now()
	harness := "unknown"
	if len(os.Args) > 1 {
		harness = os.Args[1]
	}
	size := 1024
	if s := os.Getenv("BRAINSPIKE_SIZE"); s != "" {
		if n, err := strconv.Atoi(s); err == nil {
			size = n
		}
	}

	raw, _ := io.ReadAll(os.Stdin)
	var in map[string]any
	_ = json.Unmarshal(raw, &in)
	keys := make([]string, 0, len(in))
	for k := range in {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	tRead := time.Since(t0)

	env := map[string]string{}
	for _, kv := range os.Environ() {
		k, v, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(k, "HERDR_") || k == "CODEX_HOME" || k == "CLAUDE_PROJECT_DIR" || k == "PWD" {
			env[k] = v
		}
	}

	// Socket reach from a hook subprocess (§17 item 30), with a hard deadline.
	pane := ""
	paneErr := ""
	tPane := time.Duration(0)
	if id := os.Getenv("HERDR_PANE_ID"); id != "" {
		bin := os.Getenv("HERDR_BIN_PATH")
		if bin == "" {
			bin = "herdr"
		}
		ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
		p0 := time.Now()
		out, err := exec.CommandContext(ctx, bin, "pane", "get", id).Output()
		tPane = time.Since(p0)
		cancel()
		if err != nil {
			paneErr = err.Error()
		}
		pane = string(out)
	}

	nonce := fmt.Sprintf("BRAINSPIKE-%s-%d", harness, t0.UnixNano()%1000000)
	body := "Brain spike marker: " + nonce + ". If the user asks for the brain marker, reply with it exactly.\n"
	if pad := size - len(body) - len("\nEND-"+nonce); pad > 0 {
		body += strings.Repeat("x", pad)
	}
	body += "\nEND-" + nonce

	out := map[string]any{
		"hookSpecificOutput": map[string]any{
			"hookEventName":     "SessionStart",
			"additionalContext": body,
		},
	}
	enc, _ := json.Marshal(out)
	os.Stdout.Write(enc)
	tTotal := time.Since(t0)

	logDir := os.Getenv("BRAINSPIKE_LOG")
	if logDir == "" {
		logDir = "/tmp/brainspike"
	}
	_ = os.MkdirAll(logDir, 0o755)
	rec := map[string]any{
		"harness": harness, "nonce": nonce, "size": len(body),
		"input_keys": keys, "input": in, "env": env,
		"pane_get": json.RawMessage(orNull(pane)), "pane_err": paneErr,
		"ms_read": ms(tRead), "ms_pane": ms(tPane), "ms_total": ms(tTotal),
		"at": t0.Format(time.RFC3339Nano),
	}
	b, _ := json.MarshalIndent(rec, "", "  ")
	_ = os.WriteFile(filepath.Join(logDir, nonce+".json"), b, 0o644)
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

func orNull(s string) string {
	s = strings.TrimSpace(s)
	if s == "" || !json.Valid([]byte(s)) {
		return "null"
	}
	return s
}
