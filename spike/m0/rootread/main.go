// rootread is the E2-S5 probe. It reads one workspace's identity_cwd from a
// herdr session.json and times the read. Any shape it does not know withholds.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// knownVersion is the session.json version seen on herdr 0.9.1 [M0].
const knownVersion = 3

type sessionFile struct {
	Version    *int `json:"version"`
	Workspaces []struct {
		ID          *string `json:"id"`
		IdentityCwd *string `json:"identity_cwd"`
	} `json:"workspaces"`
}

var errWithheld = errors.New("withheld: root unreadable")

func readRoot(path, workspaceID string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("%w: %v", errWithheld, err)
	}
	var f sessionFile
	if err := json.Unmarshal(b, &f); err != nil {
		return "", fmt.Errorf("%w: %v", errWithheld, err)
	}
	if f.Version == nil || *f.Version != knownVersion {
		return "", fmt.Errorf("%w: unknown version", errWithheld)
	}
	for _, w := range f.Workspaces {
		if w.ID == nil || w.IdentityCwd == nil {
			return "", fmt.Errorf("%w: missing field", errWithheld)
		}
		if *w.ID == workspaceID {
			if !filepath.IsAbs(*w.IdentityCwd) {
				return "", fmt.Errorf("%w: relative root", errWithheld)
			}
			return *w.IdentityCwd, nil
		}
	}
	return "", fmt.Errorf("%w: workspace not in file", errWithheld)
}

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: rootread <session.json> <workspace id>")
		os.Exit(2)
	}
	t0 := time.Now()
	root, err := readRoot(os.Args[1], os.Args[2])
	d := time.Since(t0)
	if err != nil {
		fmt.Printf("%v (%.3f ms)\n", err, float64(d.Microseconds())/1000)
		return
	}
	fmt.Printf("root=%s (%.3f ms)\n", root, float64(d.Microseconds())/1000)
}
