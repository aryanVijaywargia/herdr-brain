// Package output is the compiler: it builds the store's active items into one output
// generation, a folder with the brief parts and a manifest, and publishes it with one
// pointer file. A reader follows the pointer once and sees a whole generation or nothing.
package output

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/aryanVijaywargia/herdr-brain/internal/store"
	"github.com/aryanVijaywargia/herdr-brain/internal/tool"
)

// The names in the out folder and in each generation folder.
const (
	pointerName   = "current"
	lockName      = "lock"
	stagingPrefix = "staging-" // out/staging-<n>/
	manifestName  = "manifest.json"
	globalName    = "global.md"
	workspaceDir  = "workspaces"
	// skillsName is the skills folder beside the out folder: <store>/skills.
	skillsName    = "skills"
	skillFileName = "SKILL.md"
	// toolsName is the tools folder beside the out folder: <store>/tools.
	toolsName = "tools"
)

// ErrInconsistent means that a file of a generation is missing or does not match the hash in
// the generation's manifest. The hook withholds the brief then ("withheld: inconsistent").
var ErrInconsistent = errors.New("output files do not match their manifest")

// Manifest describes one generation. Its file names use "/" and are relative to the
// generation folder.
type Manifest struct {
	Generation int64           `json:"generation"`
	BuiltAt    time.Time       `json:"built_at"`
	Files      map[string]File `json:"files"`
	// Skills lists each skill file that the parts name. A generation built before E16-S3
	// has none.
	Skills []RevisionFile `json:"skills"`
	// Tools lists each file of each active tool. A generation built before E17-S3 has none.
	Tools []RevisionFile `json:"tools"`
}

// RevisionFile is one skill or tool file that a generation names: its exact path, its hash
// and its (item id, revision) pair.
type RevisionFile struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	store.RevisionRef
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
	// Workspace is empty when Read got no workspace key (an unmapped workspace).
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

// folder is the folder of generation gen in the out folder, the same as outputs.folder
// (store.nextGeneration).
func folder(gen int64) string {
	return "gen-" + strconv.FormatInt(gen, 10)
}

// workspaceName is the manifest name of the part of workspace key. Brain makes the keys, so a
// key is a safe file name.
func workspaceName(key string) string {
	return workspaceDir + "/" + key + ".md"
}

// skillsDir is the skills folder of the store whose out folder is out.
func skillsDir(out string) string {
	return filepath.Join(filepath.Dir(out), skillsName)
}

// skillPath is the file of revision rev of skill item in the skills folder dir:
// <dir>/<item id>/r<revision>/SKILL.md.
func skillPath(dir string, item, rev int64) string {
	return revisionPath(dir, item, rev, skillFileName)
}

// toolsDir is the tools folder of the store whose out folder is out.
func toolsDir(out string) string {
	return filepath.Join(filepath.Dir(out), toolsName)
}

// revisionPath is the file name of revision rev of item in the skills or tools folder dir:
// <dir>/<item id>/r<revision>/<name>.
func revisionPath(dir string, item, rev int64, name string) string {
	return filepath.Join(dir, strconv.FormatInt(item, 10), "r"+strconv.FormatInt(rev, 10), name)
}

// fileKind is one kind of revision files that a generation lists: the skill files or the
// tool files.
type fileKind struct {
	dir    func(out string) string
	listed func(m Manifest) []RevisionFile
	active func(ctx context.Context, tx *sql.Tx) ([]store.ItemRevision, error)
	// files are the files of one revision text, by name.
	files func(text string) map[string]string
	names []string
}

// fileKinds are the skill files and the tool files.
var fileKinds = []fileKind{
	{dir: skillsDir, listed: func(m Manifest) []RevisionFile { return m.Skills }, active: store.ActiveSkills,
		files: func(text string) map[string]string { return map[string]string{skillFileName: text} },
		names: []string{skillFileName}},
	{dir: toolsDir, listed: func(m Manifest) []RevisionFile { return m.Tools }, active: store.ActiveTools,
		files: tool.Files, names: []string{tool.TomlName, tool.ScriptName}},
}

// path is the file name of revision rev of item in the folder of k.
func (k fileKind) path(out string, item, rev int64, name string) string {
	return revisionPath(k.dir(out), item, rev, name)
}

// changedFiles returns the skill and tool files that m lists and that are missing or no
// longer match their hash, in list order: each is a conflict. A listed path that is not a
// path of its pair counts too: the manifest is damaged.
func changedFiles(out string, m Manifest) ([]string, error) {
	var changed []string
	for _, k := range fileKinds {
		for _, f := range k.listed(m) {
			name := filepath.Base(f.Path)
			path := k.path(out, f.Item, f.Revision, name)
			b, err := os.ReadFile(path)
			switch {
			case errors.Is(err, fs.ErrNotExist):
			case err != nil:
				return nil, err
			case f.Path == path && slices.Contains(k.names, name) && hash(b) == f.SHA256:
				continue
			}
			changed = append(changed, path)
		}
	}
	return changed, nil
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
	if err := checkManifest(m); err != nil {
		return Manifest{}, nil, fmt.Errorf("%s: %w: %w", filepath.Join(dir, manifestName), ErrInconsistent, err)
	}
	return m, b, nil
}

// checkManifest checks the structure that every build writes: a generation number, the global
// part, and a hash and a revision list for each file. Valid JSON that lacks it is damaged, and
// cannot show that a file holds no item revision.
func checkManifest(m Manifest) error {
	if m.Generation < 1 {
		return fmt.Errorf("generation %d", m.Generation)
	}
	if _, ok := m.Files[globalName]; !ok {
		return fmt.Errorf("no %s", globalName)
	}
	for _, f := range slices.Concat(m.Skills, m.Tools) {
		if _, err := hex.DecodeString(f.SHA256); err != nil || len(f.SHA256) != 2*sha256.Size {
			return fmt.Errorf("%s: bad sha256", f.Path)
		}
	}
	for name, f := range m.Files {
		if _, err := hex.DecodeString(f.SHA256); err != nil || len(f.SHA256) != 2*sha256.Size {
			return fmt.Errorf("%s: bad sha256", name)
		}
		if f.Revisions == nil {
			return fmt.Errorf("%s: no revisions", name)
		}
	}
	return nil
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

// Part is one brief part of a generation: its workspace key, "" for the global part, its text
// and the hash of its file in the manifest.
type Part struct {
	Key, Text, SHA256 string
}

// Parts reads the pointer in out once and returns the brief parts of that generation, each
// checked against its manifest: the global part, then the workspace parts by key. With no
// pointer yet, the error wraps fs.ErrNotExist.
func Parts(out string) ([]Part, error) {
	gen, err := readPointer(out)
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(out, folder(gen))
	m, _, err := readManifest(dir)
	if err != nil {
		return nil, err
	}
	var ps []Part
	for _, name := range slices.Sorted(maps.Keys(m.Files)) {
		key, ok := strings.CutPrefix(strings.TrimSuffix(name, ".md"), workspaceDir+"/")
		switch {
		case name == globalName:
			key = ""
		case !ok:
			continue
		}
		b, err := readChecked(dir, m, name)
		if err != nil {
			return nil, err
		}
		ps = append(ps, Part{Key: key, Text: string(b), SHA256: m.Files[name].SHA256})
	}
	return ps, nil
}
