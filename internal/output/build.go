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
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

// ErrStale means that a commit recorded a newer generation while this build ran. The build
// stops before the switch, so the pointer never goes back. The build of the newer generation
// publishes it.
var ErrStale = errors.New("a newer output generation exists")

// ConflictError means that files of the current generation no longer match its manifest: you
// edited them by hand, or they are gone. The build stops before the switch.
type ConflictError struct {
	Generation int64
	Files      []string
}

func (e *ConflictError) Error() string {
	return fmt.Sprintf("output generation %d changed outside Brain: %s", e.Generation, strings.Join(e.Files, ", "))
}

// Build builds the newest generation that the store recorded into the out folder and makes it
// current. E4 calls it right after a commit, and repair calls it too. proposalID is the
// proposal whose approval made the generation, or nil. It returns the current generation, or 0
// when the store has none yet. A build that a newer commit overtook returns ErrStale; a
// hand-edited current generation returns a *ConflictError. now is the clock.
func Build(ctx context.Context, s *store.Store, out string, proposalID *int64, now func() time.Time) (int64, error) {
	b := builder{out: out, now: now, writeFile: writeFile, afterRender: func() {}}
	return b.build(ctx, s, proposalID)
}

// builder holds the seams that the tests use to stop a build at a chosen point.
type builder struct {
	out         string
	now         func() time.Time
	writeFile   func(path string, data []byte) error
	afterRender func()
	// replace lists the hand-edited generations that you confirmed to replace (Confirm).
	replace []int64
}

func (b builder) build(ctx context.Context, s *store.Store, proposalID *int64) (_ int64, err error) {
	unlock, err := b.lock()
	if err != nil {
		return 0, err
	}
	defer func() { err = errors.Join(err, unlock()) }()
	return b.buildLocked(ctx, s, proposalID)
}

// lock makes the out folder and takes its build lock.
func (b builder) lock() (unlock func() error, err error) {
	// 0o700: the data home holds your items, the same as internal/bindings.
	if err := os.MkdirAll(b.out, 0o700); err != nil {
		return nil, err
	}
	return lock(filepath.Join(b.out, lockName))
}

// buildLocked is build for a caller that holds the lock.
func (b builder) buildLocked(ctx context.Context, s *store.Store, proposalID *int64) (int64, error) {
	cur, err := current(b.out)
	if err != nil {
		return 0, err
	}
	// The first check of the current generation: a hand edit stops the build before it
	// writes anything. publish checks again just before the switch.
	src, err := b.source(cur)
	if err != nil {
		return 0, err
	}
	gen, parts, rfs, err := render(ctx, s, b.out)
	if err != nil || gen == 0 {
		return 0, err
	}
	// With gen == cur, an earlier build switched the pointer and then stopped before its record.
	// publish finds that from the outputs rows and records it now.
	var g generation
	if gen != cur {
		b.afterRender()
		if g, err = b.publishFolder(gen, parts, rfs, src.manifest, b.now()); err != nil {
			return 0, err
		}
	}
	// One store write holds the write lock from the newest check to the record, so no commit (a
	// forget too) lands between them.
	err = s.Write(ctx, func(tx *sql.Tx) error { return b.publish(ctx, tx, gen, g, proposalID) })
	if err != nil {
		return 0, err
	}
	return gen, nil
}

// publish makes generation gen current and records it, unless the outputs rows record it as
// current already. g is the generation folder as written, or zero when gen was current
// already; then publish reads the folder again.
func (b builder) publish(ctx context.Context, tx *sql.Tx, gen int64, g generation, proposalID *int64) error {
	newest, err := store.NewestGeneration(ctx, tx)
	if err != nil {
		return err
	}
	if newest > gen {
		return fmt.Errorf("build generation %d, newest %d: %w", gen, newest, ErrStale)
	}
	o, err := store.OutputByGeneration(ctx, tx, gen)
	if err != nil || o.Current {
		return err
	}
	if g.manifest.Generation == 0 {
		if g, err = readSource(b.out, gen); err != nil {
			return err
		}
	}
	// The source is the generation that the rows record as current: the pointer can be on gen
	// already after a stopped build.
	was, err := store.CurrentGeneration(ctx, tx)
	if err != nil {
		return err
	}
	src, err := b.source(was)
	if err != nil {
		return err
	}
	if err := switchPointer(b.out, gen); err != nil {
		return err
	}
	// The clock is read after the switch: cleanup counts the reader grace period from this
	// time, and a slow build must not shorten it.
	return record(ctx, tx, src, g, o.HoldsForgottenText, proposalID, b.now())
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

// publishFolder writes the skill and tool files of gen first, then the parts and the manifest
// of gen into out/staging-<gen>/, checks them against the manifest, and renames the folder to
// out/gen-<gen>/. It returns the generation as read back. A build that stops before the rename
// leaves only the staging folder, which the next build of gen replaces, and skill and
// tool files that no generation lists yet, which the next build uses. src is the manifest of
// the source generation, the current one: a skill or tool file that it lists is never written
// over.
func (b builder) publishFolder(gen int64, parts []part, rfs files, src Manifest, now time.Time) (generation, error) {
	m := Manifest{Generation: gen, BuiltAt: now.UTC(), Files: make(map[string]File, len(parts)),
		Skills: make([]RevisionFile, 0, len(rfs.skills)), Tools: make([]RevisionFile, 0, len(rfs.tools))}
	listed := make(map[string]bool, len(src.Skills)+len(src.Tools))
	for _, f := range slices.Concat(src.Skills, src.Tools) {
		listed[f.Path] = true
	}
	for _, l := range []struct {
		files []revFile
		to    *[]RevisionFile
	}{{rfs.skills, &m.Skills}, {rfs.tools, &m.Tools}} {
		for _, f := range l.files {
			if err := b.writeFileOf(f, listed[f.path], src.Generation); err != nil {
				return generation{}, err
			}
			*l.to = append(*l.to, RevisionFile{Path: f.path, SHA256: hash([]byte(f.text)), RevisionRef: f.ref})
		}
	}
	// The staging folder form (issue #30).
	staging := filepath.Join(b.out, stagingPrefix+strconv.FormatInt(gen, 10))
	if err := os.RemoveAll(staging); err != nil {
		return generation{}, err
	}
	if err := os.MkdirAll(filepath.Join(staging, workspaceDir), 0o700); err != nil {
		return generation{}, err
	}
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

// writeFileOf makes the file of skill or tool revision f hold its text. A file that matches is
// used as it is. A file that is missing, or that does not match, is written with a temp file
// and a rename. When the current generation cur lists the file (listed), a mismatch is your
// edit since the source check: it stays, as a conflict.
func (b builder) writeFileOf(f revFile, listed bool, cur int64) error {
	data, err := os.ReadFile(f.path)
	switch {
	case err == nil && string(data) == f.text:
		return nil
	case err != nil && !errors.Is(err, fs.ErrNotExist):
		return err
	case listed:
		return &ConflictError{Generation: cur, Files: []string{f.path}}
	}
	return writeAtomic(f.path, f.text, b.writeFile)
}

// writeAtomic writes text to path with a temp file and a rename, so a reader sees the whole
// file or none. write writes the temp file.
func writeAtomic(path, text string, write func(path string, data []byte) error) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := write(tmp, []byte(text)); err != nil {
		return err
	}
	return os.Rename(tmp, path)
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
	// changed lists the files that are missing or do not match the manifest, and the files
	// that the manifest does not name, in name order.
	changed []string
	// extra lists the files in the folder that the manifest does not name.
	extra []string
}

// readGeneration reads the manifest in dir and every file that it names. A file that does not
// match, or that the manifest does not name, goes into changed; any other read error is
// returned.
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
	err = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		name, err := filepath.Rel(dir, path)
		name = filepath.ToSlash(name)
		if _, ok := m.Files[name]; err == nil && !ok && name != manifestName {
			g.extra = append(g.extra, name)
		}
		return err
	})
	if err != nil {
		return generation{}, err
	}
	g.changed = append(g.changed, g.extra...)
	slices.Sort(g.changed)
	if len(g.changed) > 0 {
		return g, fmt.Errorf("%s: %s: %w", dir, strings.Join(g.changed, ", "), ErrInconsistent)
	}
	return g, nil
}

// readSource reads generation cur and checks its files against its manifest: the generation
// folder and the skill files that the manifest lists. It returns a zero generation for cur
// 0, before the first build.
func readSource(out string, cur int64) (generation, error) {
	if cur == 0 {
		return generation{}, nil
	}
	return checkCurrent(out, cur)
}

// checkCurrent checks generation gen as checkGeneration does, and also the skill files that
// its manifest lists. Only the current generation counts its skill files: an older generation
// that lists an edited file is no conflict.
func checkCurrent(out string, gen int64) (generation, error) {
	g, err := checkGeneration(out, gen)
	if err != nil {
		return generation{}, err
	}
	changed, err := changedFiles(out, g.manifest)
	if err != nil {
		return generation{}, err
	}
	if len(changed) > 0 {
		return generation{}, &ConflictError{Generation: gen, Files: changed}
	}
	return g, nil
}

// source reads generation gen, the source of a build, as readSource does. A generation that
// you confirmed to replace counts as no generation: its files are your edit, so the log of the
// build keeps none of them.
func (b builder) source(gen int64) (generation, error) {
	if slices.Contains(b.replace, gen) {
		return generation{}, nil
	}
	return readSource(b.out, gen)
}

// checkGeneration reads generation gen and checks its files against its manifest. A file that
// does not match gives a *ConflictError.
func checkGeneration(out string, gen int64) (generation, error) {
	g, err := readGeneration(filepath.Join(out, folder(gen)))
	if errors.Is(err, ErrInconsistent) {
		files := g.changed
		if files == nil {
			// With no changed file, the manifest itself is gone or damaged.
			files = []string{manifestName}
		}
		return generation{}, &ConflictError{Generation: gen, Files: files}
	}
	if err == nil && g.manifest.Generation != gen {
		return generation{}, &ConflictError{Generation: gen, Files: []string{manifestName}}
	}
	return g, err
}

// switchPointer makes gen current: it writes the pointer to a temp file and renames it over
// out/current, so a reader sees the old pointer or the new one. Only the lock holder writes
// the temp file.
func switchPointer(out string, gen int64) error {
	tmp := filepath.Join(out, pointerName+".tmp")
	if err := writeFile(tmp, []byte(folder(gen)+"\n")); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(out, pointerName))
}

// record copies the switch into the outputs rows and logs one applied edit for each file that
// changed from src to the new generation g. forgetAfter reports whether g holds the text of
// an item with an open forget.
func record(ctx context.Context, tx *sql.Tx, src, g generation, forgetAfter bool, proposalID *int64,
	now time.Time) error {
	forgetBefore, err := openForget(ctx, tx, src)
	if err != nil {
		return err
	}
	for _, e := range edits(src, g, forgetBefore, forgetAfter) {
		e.ProposalID, e.Applied = proposalID, now
		if _, err := store.InsertAppliedEdit(ctx, tx, e); err != nil {
			return err
		}
	}
	return store.PublishOutput(ctx, tx, g.manifest.Generation, g.manifestHash, now)
}

// openForget reports whether the source generation holds the text of an item with an open
// forget. The forget flow marks such a generation with outputs.holds_forgotten_text.
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
// removes from src, in name order. With an open forget on a side, a file that holds items on
// that side keeps those bytes out of the log: the row has the file, the generation, the hash
// and the revisions only. The content hash is the hash of the new bytes.
func edits(src, g generation, forgetBefore, forgetAfter bool) []store.AppliedEdit {
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
		if had && !withheld(forgetBefore, was) {
			e.ContentBefore = ptr(string(src.files[name]))
		}
		if has && !withheld(forgetAfter, is) {
			e.ContentAfter = ptr(string(g.files[name]))
		}
		es = append(es, e)
	}
	return es
}

// withheld reports whether the log keeps the bytes of f out: f holds items and forget is open.
func withheld(forget bool, f File) bool { return forget && len(f.Revisions) > 0 }

func ptr[T any](v T) *T { return &v }
