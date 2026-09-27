package main

import (
	"cmp"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	// totalCap is the HLD §8.3 hook cap.
	totalCap = 300 * time.Millisecond
	// probeLookup is the pane get deadline of this probe, the same as the M0 hook probe. It
	// is wide on purpose, so the probe measures the lookup time and does not cut it.
	probeLookup = 250 * time.Millisecond
	// maxInput bounds the hook input read [NEW].
	maxInput = 1 << 20
)

var (
	errWithheld = errors.New("withheld: inconsistent")
	pointerRe   = regexp.MustCompile(`^gen-([0-9]+)\n$`)
)

// record is one JSONL line of the timing log. All times are in milliseconds from the start
// of main, except the ms_ step times, which are the length of each step.
type record struct {
	Result     string  `json:"result"`
	Reason     string  `json:"reason,omitempty"`
	BriefChars int     `json:"brief_chars,omitempty"`
	MsRead     float64 `json:"ms_read"`
	MsPane     float64 `json:"ms_pane"`
	MsPointer  float64 `json:"ms_pointer"`
	MsManifest float64 `json:"ms_manifest"`
	MsParts    float64 `json:"ms_parts"`
	MsCheck    float64 `json:"ms_check"`
	MsPrint    float64 `json:"ms_print"`
	MsReceipt  float64 `json:"ms_receipt"`
	MsTotal    float64 `json:"ms_total"`
	At         string  `json:"at"`
}

// hook runs HLD §8.3 steps 1, 3, 4 (without the workspace map), 5 and 6, and times each.
func hook(stdin io.Reader, stdout io.Writer, getenv func(string) string) {
	t0 := time.Now()
	rec := record{At: t0.Format(time.RFC3339Nano)}
	defer func() {
		rec.MsTotal = since(t0)
		writeLog(getenv("HOOKLOAD_LOG"), rec)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), totalCap)
	defer cancel()

	step := time.Now()
	session := sessionID(stdin)
	rec.MsRead = since(step)
	if getenv("HERDR_ENV") != "1" {
		rec.Result = "skipped"
		return
	}

	step = time.Now()
	err := checkPane(ctx, getenv)
	rec.MsPane = since(step)
	if err != nil {
		rec.Result, rec.Reason = "withheld", err.Error()
		return
	}

	store, key := getenv("HOOKLOAD_STORE"), cmp.Or(getenv("HOOKLOAD_KEY"), defaultKey)
	brief, hash, err := readBrief(store, key, &rec)
	if err != nil {
		rec.Result, rec.Reason = "withheld", err.Error()
		return
	}
	if ctx.Err() != nil {
		rec.Result, rec.Reason = "withheld", "deadline after check"
		return
	}

	step = time.Now()
	err = emit(stdout, brief)
	rec.MsPrint = since(step)
	if err != nil {
		rec.Result, rec.Reason = "unknown", err.Error()
		return
	}
	rec.BriefChars = len(brief)

	step = time.Now()
	err = writeReceipt(ctx, store, getenv("HERDR_PANE_ID"), session, key, hash)
	rec.MsReceipt = since(step)
	rec.Result = "emitted"
	if err != nil {
		// HLD §8.3 step 7: a failed receipt after the print makes the delivery unknown.
		rec.Result, rec.Reason = "unknown", err.Error()
	}
}

// sessionID reads the hook input. Input that does not parse only leaves the session id
// empty; the receipt allows that.
func sessionID(stdin io.Reader) string {
	var in struct {
		SessionID string `json:"session_id"`
	}
	raw, err := io.ReadAll(io.LimitReader(stdin, maxInput))
	if err != nil {
		return ""
	}
	_ = json.Unmarshal(raw, &in)
	return in.SessionID
}

// emit prints the brief in the Claude and Codex output form (HLD §8.3 step 5).
func emit(stdout io.Writer, brief string) error {
	out, err := json.Marshal(map[string]any{"hookSpecificOutput": map[string]any{
		"hookEventName": "SessionStart", "additionalContext": brief,
	}})
	if err != nil {
		return err
	}
	_, err = stdout.Write(out)
	return err
}

// checkPane asks herdr for the pane, as internal/herdr does: `herdr [--session S] pane get`.
// The pane must exist and be in HERDR_WORKSPACE_ID.
func checkPane(ctx context.Context, getenv func(string) string) error {
	id := getenv("HERDR_PANE_ID")
	if id == "" {
		return errors.New("no pane id")
	}
	args := []string{"pane", "get", id}
	if s := getenv("HERDR_SESSION"); s != "" {
		args = append([]string{"--session", s}, args...)
	}
	ctx, cancel := context.WithTimeout(ctx, probeLookup)
	defer cancel()
	out, err := exec.CommandContext(ctx, cmp.Or(getenv("HERDR_BIN_PATH"), "herdr"), args...).Output()
	if err != nil {
		return fmt.Errorf("pane get: %w", err)
	}
	var reply struct {
		Result struct {
			Pane struct {
				WorkspaceID string `json:"workspace_id"`
			} `json:"pane"`
		} `json:"result"`
	}
	if err := json.Unmarshal(out, &reply); err != nil {
		return fmt.Errorf("pane get: %w", err)
	}
	if ws := reply.Result.Pane.WorkspaceID; ws == "" || ws != getenv("HERDR_WORKSPACE_ID") {
		return fmt.Errorf("pane not in workspace %q", getenv("HERDR_WORKSPACE_ID"))
	}
	return nil
}

// readBrief reads the pointer once, then the manifest and both parts of that generation. It
// hashes the bytes it read and builds the brief from the same bytes.
func readBrief(store, key string, rec *record) (brief, hash string, err error) {
	check := time.Now()
	defer func() { rec.MsCheck = since(check) }()

	step := time.Now()
	p, err := os.ReadFile(filepath.Join(store, "out", "current"))
	rec.MsPointer = since(step)
	if err != nil {
		return "", "", fmt.Errorf("%w: %w", errWithheld, err)
	}
	g := pointerRe.FindSubmatch(p)
	if g == nil {
		return "", "", fmt.Errorf("%w: bad pointer", errWithheld)
	}
	n, err := strconv.Atoi(string(g[1]))
	if err != nil {
		return "", "", fmt.Errorf("%w: bad pointer", errWithheld)
	}
	dir := filepath.Join(store, "out", "gen-"+string(g[1]))

	step = time.Now()
	m, err := readManifest(filepath.Join(dir, "manifest.json"), n)
	rec.MsManifest = since(step)
	if err != nil {
		return "", "", err
	}

	step = time.Now()
	defer func() { rec.MsParts = since(step) }()
	want := []string{"global.md", "workspaces/" + key + ".md"}
	parts := make([]string, len(want))
	for i, name := range want {
		if parts[i], err = readPart(dir, name, m); err != nil {
			return "", "", err
		}
	}
	brief = beginMarker + globalLabel + parts[0] + wsLabel + parts[1] + endMarker
	sum := sha256.Sum256([]byte(brief))
	return brief, hex.EncodeToString(sum[:]), nil
}

func readManifest(file string, gen int) (map[string]manifestFile, error) {
	b, err := os.ReadFile(file)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errWithheld, err)
	}
	var m manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("%w: manifest: %w", errWithheld, err)
	}
	if m.Generation != gen {
		return nil, fmt.Errorf("%w: manifest generation %d, pointer %d", errWithheld, m.Generation, gen)
	}
	files := make(map[string]manifestFile, len(m.Files))
	for _, f := range m.Files {
		if f.Path != path.Clean(f.Path) || path.IsAbs(f.Path) || strings.HasPrefix(f.Path, "..") {
			return nil, fmt.Errorf("%w: bad path %q", errWithheld, f.Path)
		}
		if _, dup := files[f.Path]; dup {
			return nil, fmt.Errorf("%w: duplicate path %q", errWithheld, f.Path)
		}
		files[f.Path] = f
	}
	return files, nil
}

func readPart(dir, name string, m map[string]manifestFile) (string, error) {
	f, ok := m[name]
	if !ok {
		return "", fmt.Errorf("%w: %s not in manifest", errWithheld, name)
	}
	b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(name)))
	if err != nil {
		return "", fmt.Errorf("%w: %w", errWithheld, err)
	}
	sum := sha256.Sum256(b)
	if len(b) != f.Bytes || hex.EncodeToString(sum[:]) != f.SHA256 {
		return "", fmt.Errorf("%w: %s hash mismatch", errWithheld, name)
	}
	return string(b), nil
}

// writeReceipt opens the store and inserts one "emitted" row, as the real hook will.
func writeReceipt(ctx context.Context, store, pane, session, key, hash string) error {
	db, err := sql.Open("sqlite", dsn(store))
	if err != nil {
		return err
	}
	db.SetMaxOpenConns(1)
	_, err = db.ExecContext(ctx, `INSERT INTO deliveries
		(profile, pane_id, agent_session_id, workspace_key, brief_hash, output_generation,
		 cli_version, result, at) VALUES (?, ?, ?, ?, ?, ?, ?, 'emitted', ?)`,
		"claude", pane, session, key, hash, generation, "", time.Now().UTC().Format(time.RFC3339Nano))
	return errors.Join(err, db.Close())
}

func writeLog(file string, rec record) {
	if file == "" {
		return
	}
	b, err := json.Marshal(rec)
	if err != nil {
		return
	}
	f, err := os.OpenFile(file, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	// The log is probe output only; a lost line shows as a missing run in the count.
	_, _ = f.Write(append(b, '\n'))
	_ = f.Close()
}

func since(t time.Time) float64 { return float64(time.Since(t).Microseconds()) / 1000 }
