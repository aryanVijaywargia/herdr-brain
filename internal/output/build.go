package output

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

// ErrStale means that a commit recorded a newer generation while this build ran. The build
// stops before the switch, so the pointer never goes back (§7.5 step 4, S43). The build of the
// newer generation publishes it.
var ErrStale = errors.New("a newer output generation exists")

// ConflictError means that files of the current generation no longer match its manifest: you
// edited them by hand, or they are gone. The build stops before the switch (§7.5 step 5).
type ConflictError struct {
	Generation int64
	Files      []string
}

func (e *ConflictError) Error() string {
	return fmt.Sprintf("output generation %d changed outside Brain: %s", e.Generation, strings.Join(e.Files, ", "))
}

// Build builds the newest generation that the store recorded into the out folder and makes it
// current (§7.5 steps 3-4, 7, 9). E4 calls it right after a commit, and repair calls it too.
// proposalID is the proposal whose approval made the generation, or nil. It returns the current
// generation, or 0 when the store has none yet. A build that a newer commit overtook returns
// ErrStale; a hand-edited current generation returns a *ConflictError.
func Build(ctx context.Context, s *store.Store, out string, proposalID *int64, now time.Time) (int64, error) {
	b := builder{out: out, writeFile: writeFile, afterRender: func() {}}
	return b.build(ctx, s, proposalID, now)
}

// builder holds the seams that the tests use to stop a build at a chosen point.
type builder struct {
	out         string
	writeFile   func(path string, data []byte) error
	afterRender func()
}

func (b builder) build(ctx context.Context, s *store.Store, proposalID *int64, now time.Time) (_ int64, err error) {
	// [NEW] 0o700: the data home holds your items, the same as internal/bindings.
	if err := os.MkdirAll(b.out, 0o700); err != nil {
		return 0, err
	}
	unlock, err := lock(filepath.Join(b.out, lockName))
	if err != nil {
		return 0, err
	}
	defer func() { err = errors.Join(err, unlock()) }()

	gen, parts, err := render(ctx, s)
	if err != nil {
		return 0, err
	}
	cur, err := current(b.out)
	if err != nil || gen == cur {
		return cur, err
	}
	b.afterRender()
	g, err := b.publishFolder(gen, parts, now)
	if err != nil {
		return 0, err
	}
	if err := checkNewest(ctx, s, gen); err != nil {
		return 0, err
	}
	src, err := readSource(b.out, cur)
	if err != nil {
		return 0, err
	}
	if err := switchPointer(b.out, gen); err != nil {
		return 0, err
	}
	return gen, record(ctx, s, src, g, proposalID, now)
}

// lock takes the build lock of the out folder and waits while another build holds it. The lock
// is released when the file closes, also when the process dies.
func lock(path string) (unlock func() error, err error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return nil, errors.Join(fmt.Errorf("lock %s: %w", path, err), f.Close())
	}
	return f.Close, nil
}

// current returns the generation that the pointer names, or 0 before the first build.
func current(out string) (int64, error) {
	gen, err := readPointer(out)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	return gen, err
}

// publishFolder writes the parts and the manifest of gen into out/staging-<gen>/, checks them
// against the manifest, and renames the folder to out/gen-<gen>/. It returns the generation as
// read back. A build that stops before the rename leaves only the staging folder, which the
// next build of gen replaces (S65).
func (b builder) publishFolder(gen int64, parts []part, now time.Time) (generation, error) {
	// [NEW] the staging folder form (issue #30).
	staging := filepath.Join(b.out, fmt.Sprintf("staging-%d", gen))
	if err := os.RemoveAll(staging); err != nil {
		return generation{}, err
	}
	if err := os.MkdirAll(filepath.Join(staging, workspaceDir), 0o700); err != nil {
		return generation{}, err
	}
	m := Manifest{Generation: gen, BuiltAt: now.UTC(), Files: make(map[string]File, len(parts))}
	for _, p := range parts {
		if err := b.writeFile(filepath.Join(staging, filepath.FromSlash(p.name)), []byte(p.text)); err != nil {
			return generation{}, err
		}
		m.Files[p.name] = File{SHA256: hash([]byte(p.text)), Revisions: p.revisions}
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return generation{}, fmt.Errorf("encode manifest: %w", err)
	}
	if err := b.writeFile(filepath.Join(staging, manifestName), append(data, '\n')); err != nil {
		return generation{}, err
	}
	g, err := readGeneration(staging)
	if err != nil {
		return generation{}, fmt.Errorf("check staging folder: %w", err)
	}
	// A gen-<gen> folder that is here already is from a build that stopped before its switch.
	// It is not current, so no reader follows it.
	dir := filepath.Join(b.out, folder(gen))
	if err := os.RemoveAll(dir); err != nil {
		return generation{}, err
	}
	return g, os.Rename(staging, dir)
}

func writeFile(path string, data []byte) error {
	return os.WriteFile(path, data, 0o600)
}

// generation is a generation folder as read from disk.
type generation struct {
	manifest     Manifest
	manifestHash string
	// files holds the bytes of each file that matched the manifest.
	files map[string][]byte
	// changed lists the files that are missing or do not match the manifest, in name order.
	changed []string
}

// readGeneration reads the manifest in dir and every file that it names. A file that does not
// match goes into changed; any other read error is returned.
func readGeneration(dir string) (generation, error) {
	m, data, err := readManifest(dir)
	if err != nil {
		return generation{}, err
	}
	g := generation{manifest: m, manifestHash: hash(data), files: make(map[string][]byte, len(m.Files))}
	for _, name := range slices.Sorted(maps.Keys(m.Files)) {
		b, err := readChecked(dir, m, name)
		if errors.Is(err, ErrInconsistent) {
			g.changed = append(g.changed, name)
			continue
		}
		if err != nil {
			return generation{}, err
		}
		g.files[name] = b
	}
	if len(g.changed) > 0 {
		return g, fmt.Errorf("%s: %s: %w", dir, strings.Join(g.changed, ", "), ErrInconsistent)
	}
	return g, nil
}

// checkNewest stops the build when a commit recorded a newer generation than gen.
func checkNewest(ctx context.Context, s *store.Store, gen int64) error {
	var newest int64
	err := s.Write(ctx, func(tx *sql.Tx) (err error) {
		newest, err = store.NewestGeneration(ctx, tx)
		return err
	})
	if err != nil {
		return err
	}
	if newest > gen {
		return fmt.Errorf("build generation %d, newest %d: %w", gen, newest, ErrStale)
	}
	return nil
}

// readSource reads the current generation cur, the source of this build, and checks its files
// against its manifest. It returns a zero generation before the first build.
func readSource(out string, cur int64) (generation, error) {
	if cur == 0 {
		return generation{}, nil
	}
	src, err := readGeneration(filepath.Join(out, folder(cur)))
	if errors.Is(err, ErrInconsistent) {
		files := src.changed
		if files == nil {
			// With no changed file, the manifest itself is gone or damaged.
			files = []string{manifestName}
		}
		return generation{}, &ConflictError{Generation: cur, Files: files}
	}
	return src, err
}

// switchPointer makes gen current: it writes the pointer to a temp file and renames it over
// out/current, so a reader sees the old pointer or the new one (§7.5 step 3). Only the lock
// holder writes the temp file.
func switchPointer(out string, gen int64) error {
	tmp := filepath.Join(out, pointerName+".tmp")
	if err := writeFile(tmp, []byte(folder(gen)+"\n")); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(out, pointerName))
}

// record copies the switch into the outputs row and logs one applied edit for each file that
// changed from src to the new generation g (§7.5 steps 7, 9).
func record(ctx context.Context, s *store.Store, src, g generation, proposalID *int64, now time.Time) error {
	return s.Write(ctx, func(tx *sql.Tx) error {
		forget, err := openForget(ctx, tx, src)
		if err != nil {
			return err
		}
		for _, e := range edits(src, g, forget) {
			e.ProposalID, e.Applied = proposalID, now
			if _, err := store.InsertAppliedEdit(ctx, tx, e); err != nil {
				return err
			}
		}
		return store.PublishOutput(ctx, tx, g.manifest.Generation, g.manifestHash, now)
	})
}

// openForget reports whether the source generation holds the text of an item with an open
// forget. The forget flow marks such a generation with outputs.holds_forgotten_text (§6.7).
func openForget(ctx context.Context, tx *sql.Tx, src generation) (bool, error) {
	if src.manifest.Generation == 0 {
		return false, nil
	}
	o, err := store.OutputByGeneration(ctx, tx, src.manifest.Generation)
	if err != nil {
		return false, err
	}
	return o.HoldsForgottenText, nil
}

// edits lists one applied edit for each file that the new generation g adds, changes or
// removes from src, in name order. With an open forget, a file that held items keeps its old
// bytes out of the log: the row has the file, the generation, the hash and the revisions
// only (§6.7). The content hash is the hash of the new bytes.
func edits(src, g generation, forget bool) []store.AppliedEdit {
	names := slices.Sorted(maps.Keys(g.manifest.Files))
	for _, name := range slices.Sorted(maps.Keys(src.manifest.Files)) {
		if _, ok := g.manifest.Files[name]; !ok {
			names = append(names, name)
		}
	}
	var es []store.AppliedEdit
	for _, name := range names {
		was, had := src.manifest.Files[name]
		is, has := g.manifest.Files[name]
		if had && has && was.SHA256 == is.SHA256 {
			continue
		}
		e := store.AppliedEdit{File: name, Generation: g.manifest.Generation, ContentHash: hash(g.files[name]),
			RevisionsBefore: was.Revisions, RevisionsAfter: is.Revisions}
		withheld := forget && len(was.Revisions) > 0
		if had && !withheld {
			e.ContentBefore = ptr(string(src.files[name]))
		}
		if has {
			e.ContentAfter = ptr(string(g.files[name]))
		}
		es = append(es, e)
	}
	return es
}

func ptr[T any](v T) *T { return &v }
