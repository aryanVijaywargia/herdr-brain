package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite"
)

// The assumed E6 output shape (E6 is not built yet). HLD §7.5 gives the folder, the pointer
// and "one manifest with the hash of each file"; the file names and the JSON form are [NEW]:
//
//	<store>/out/current                       "gen-<n>\n", replaced by a rename
//	<store>/out/gen-<n>/manifest.json         {"generation":n,"files":[{"path","bytes","sha256"}]}
//	<store>/out/gen-<n>/global.md             the global part
//	<store>/out/gen-<n>/workspaces/<key>.md   one workspace part
//	<store>/brain.db                          SQLite, WAL, with a deliveries table (§11)
const (
	defaultKey   = "ws-0001"
	generation   = 1
	briefBudget  = 9500 // HLD §8.1: the whole additionalContext text
	globalBudget = 3800 // HLD §8.1: the global part
	beginMarker  = "<!-- brain:begin v1 -->\n"
	endMarker    = "<!-- brain:end -->"
	globalLabel  = "## Global (all workspaces)\n"
	wsLabel      = "\n## This workspace\n"
)

type manifest struct {
	Generation int            `json:"generation"`
	Files      []manifestFile `json:"files"`
}

type manifestFile struct {
	Path   string `json:"path"`
	Bytes  int    `json:"bytes"`
	SHA256 string `json:"sha256"`
}

// schema is a cut-down deliveries table of HLD §11, enough for one realistic insert.
const schema = `CREATE TABLE IF NOT EXISTS deliveries (
	id INTEGER PRIMARY KEY,
	profile TEXT NOT NULL, pane_id TEXT NOT NULL, agent_session_id TEXT,
	workspace_key TEXT, brief_hash TEXT, output_generation INTEGER,
	cli_version TEXT, result TEXT NOT NULL, withheld_reason TEXT, at TEXT NOT NULL)`

// gen writes one generation whose brief is exactly briefBudget characters: the largest brief
// that HLD §8.1 allows.
func gen(store, key string) error {
	dir := filepath.Join(store, "out", fmt.Sprintf("gen-%d", generation))
	if err := os.MkdirAll(filepath.Join(dir, "workspaces"), 0o755); err != nil {
		return err
	}
	wrapper := len(beginMarker) + len(globalLabel) + len(wsLabel) + len(endMarker)
	parts := []struct{ path, body string }{
		{"global.md", filler("global", globalBudget)},
		{"workspaces/" + key + ".md", filler("workspace", briefBudget-wrapper-globalBudget)},
	}
	m := manifest{Generation: generation}
	for _, p := range parts {
		if err := os.WriteFile(filepath.Join(dir, p.path), []byte(p.body), 0o644); err != nil {
			return err
		}
		sum := sha256.Sum256([]byte(p.body))
		m.Files = append(m.Files, manifestFile{p.path, len(p.body), hex.EncodeToString(sum[:])})
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), b, 0o644); err != nil {
		return err
	}
	pointer := filepath.Join(store, "out", "current")
	if err := os.WriteFile(pointer+".tmp", []byte(fmt.Sprintf("gen-%d\n", generation)), 0o644); err != nil {
		return err
	}
	if err := os.Rename(pointer+".tmp", pointer); err != nil {
		return err
	}
	return initDB(store)
}

// filler returns n characters of brief-like lines (3-line entries of about 100 characters).
func filler(scope string, n int) string {
	var b strings.Builder
	for i := 1; b.Len() < n; i++ {
		fmt.Fprintf(&b, "- [%s %03d] Prefer the repo's own test helper; run go test ./... before a commit. ok\n", scope, i)
	}
	return b.String()[:n]
}

func initDB(store string) error {
	db, err := sql.Open("sqlite", dsn(store))
	if err != nil {
		return err
	}
	ctx := context.Background()
	_, err = db.ExecContext(ctx, "PRAGMA journal_mode=WAL")
	if err == nil {
		_, err = db.ExecContext(ctx, schema)
	}
	return errors.Join(err, db.Close())
}

// dsn matches internal/store: a busy timeout, foreign keys and BEGIN IMMEDIATE. The hook's
// busy timeout of 50 ms is [NEW]; E3-S1 leaves it to the caller.
func dsn(store string) string {
	q := url.Values{}
	q.Add("_pragma", "busy_timeout(50)")
	q.Add("_pragma", "foreign_keys(1)")
	q.Set("_txlock", "immediate")
	u := url.URL{Scheme: "file", Path: filepath.Join(store, "brain.db"), RawQuery: q.Encode()}
	return u.String()
}
