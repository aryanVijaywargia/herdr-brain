// Package output is the compiler (HLD §7.5, §8.1): it builds the store's active items into one
// output generation, a folder with the brief parts and a manifest, and publishes it with one
// pointer file. A reader follows the pointer once and sees a whole generation or nothing.
package output

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

// The names in the out folder and in each generation folder. [NEW] all names except
// out/gen-<n>/ and out/current, which come from §7.5 step 3.
const (
	pointerName  = "current"
	lockName     = "lock"
	manifestName = "manifest.json"
	globalName   = "global.md"
	workspaceDir = "workspaces"
)

// ErrInconsistent means that a file of a generation is missing or does not match the hash in
// the generation's manifest. The hook withholds the brief then ("withheld: inconsistent", §8.3).
var ErrInconsistent = errors.New("output files do not match their manifest")

// Manifest describes one generation. Its file names use "/" and are relative to the
// generation folder.
type Manifest struct {
	Generation int64           `json:"generation"`
	BuiltAt    time.Time       `json:"built_at"`
	Files      map[string]File `json:"files"`
}

// File is one file of a generation: its hash and the item revisions that its text holds. The
// revisions come from the same store read as the text.
type File struct {
	SHA256    string              `json:"sha256"`
	Revisions []store.RevisionRef `json:"revisions"`
}

// Brief is what the hook reads from the current generation: the checked bytes of the global
// part and of one workspace part, and the item revisions in them.
type Brief struct {
	Generation int64
	Global     string
	// Workspace is empty when Read got no workspace key (an unmapped workspace, §8.3 step 4).
	Workspace string
	// Revisions are the (item id, revision) pairs of both parts, from the manifest. E7 records
	// these in the receipt, never the store's current revisions.
	Revisions []store.RevisionRef
}

// Read reads the pointer in out once, then reads the global part and the part of workspace
// key from that one generation, and checks each against the generation's manifest. key ""
// reads the global part only. When the manifest has no part for key, the generation was built
// before the workspace existed, so it holds no items of it: Read returns the global part and an
// empty workspace part. A missing file or a hash mismatch of a part that the manifest lists
// wraps ErrInconsistent. With no pointer yet, the error wraps fs.ErrNotExist.
func Read(out, key string) (Brief, error) {
	gen, err := readPointer(out)
	if err != nil {
		return Brief{}, err
	}
	dir := filepath.Join(out, folder(gen))
	m, _, err := readManifest(dir)
	if err != nil {
		return Brief{}, err
	}
	if m.Generation != gen {
		return Brief{}, fmt.Errorf("%s: manifest names generation %d: %w", dir, m.Generation, ErrInconsistent)
	}
	b := Brief{Generation: gen}
	global, err := readChecked(dir, m, globalName)
	if err != nil {
		return Brief{}, err
	}
	b.Global = string(global)
	b.Revisions = append(b.Revisions, m.Files[globalName].Revisions...)
	if key == "" {
		return b, nil
	}
	name := workspaceName(key)
	if _, ok := m.Files[name]; !ok {
		return b, nil
	}
	ws, err := readChecked(dir, m, name)
	if err != nil {
		return Brief{}, err
	}
	b.Workspace = string(ws)
	b.Revisions = append(b.Revisions, m.Files[name].Revisions...)
	return b, nil
}

// folder is the folder of generation gen in the out folder. [NEW] the form, the same as
// outputs.folder (store.nextGeneration).
func folder(gen int64) string {
	return "gen-" + strconv.FormatInt(gen, 10)
}

// workspaceName is the manifest name of the part of workspace key. Brain makes the keys, so a
// key is a safe file name.
func workspaceName(key string) string {
	return workspaceDir + "/" + key + ".md"
}

// readPointer returns the generation that out/current names.
func readPointer(out string) (int64, error) {
	b, err := os.ReadFile(filepath.Join(out, pointerName))
	if err != nil {
		return 0, err
	}
	num, ok := strings.CutPrefix(strings.TrimSuffix(string(b), "\n"), "gen-")
	gen, err := strconv.ParseInt(num, 10, 64)
	if !ok || err != nil || gen < 1 {
		return 0, fmt.Errorf("output pointer %q: want gen-<n>", b)
	}
	return gen, nil
}

// readManifest reads the manifest of the generation in dir. It returns the bytes too, whose
// hash outputs.manifest_hash records.
func readManifest(dir string) (Manifest, []byte, error) {
	b, err := os.ReadFile(filepath.Join(dir, manifestName))
	if err != nil {
		return Manifest{}, nil, errors.Join(err, ErrInconsistent)
	}
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return Manifest{}, nil, fmt.Errorf("%s: %w: %w", filepath.Join(dir, manifestName), ErrInconsistent, err)
	}
	return m, b, nil
}

// readChecked reads the file name of the generation in dir and checks it against m.
func readChecked(dir string, m Manifest, name string) ([]byte, error) {
	f, ok := m.Files[name]
	if !ok {
		return nil, fmt.Errorf("%s: %s is not in the manifest: %w", dir, name, ErrInconsistent)
	}
	b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(name)))
	if err != nil {
		return nil, errors.Join(err, ErrInconsistent)
	}
	if hash(b) != f.SHA256 {
		return nil, fmt.Errorf("%s: %s changed: %w", dir, name, ErrInconsistent)
	}
	return b, nil
}

func hash(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
