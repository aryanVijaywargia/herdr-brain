package output

import (
	"context"
	"database/sql"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

// hookCap is the total time cap of the hook. A reader that read the old pointer is done
// within one cap, so a generation that the pointer left more than one cap ago has no reader.
const hookCap = 300 * time.Millisecond

// Repair builds the output when it is behind the store, and then cleans the out folder: it
// deletes every staging folder, and each old generation that no reader can still use. Every
// brain call except the hook runs it, and so do brain doctor and forget. A hand-edited
// current generation stops the build with a *ConflictError; the cleanup still runs, so a
// forget can finish once you resolve the conflict. now is the clock.
func Repair(ctx context.Context, s *store.Store, out string, now func() time.Time) (err error) {
	b := builder{out: out, now: now, writeFile: writeFile, afterRender: func() {}}
	unlock, err := b.lock()
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, unlock()) }()
	return b.repair(ctx, s)
}

// repair is Repair for a caller that holds the lock.
func (b builder) repair(ctx context.Context, s *store.Store) error {
	return errors.Join(b.catchUp(ctx, s), cleanup(ctx, s, b.out, b.now))
}

// catchUp builds the output when it is behind the store. The caller holds the lock.
func (b builder) catchUp(ctx context.Context, s *store.Store) error {
	cur, err := current(b.out)
	if err != nil {
		return err
	}
	behind, err := behind(ctx, s, cur)
	if err != nil || !behind {
		return err
	}
	_, err = b.buildLocked(ctx, s, nil)
	return err
}

// RepairConflicts runs brain repair: it repairs as Repair does, and then returns the open
// output conflicts under the same lock, so no build changes the folders between the two. A
// conflict is not an error here: it is in the list. now is the clock.
func RepairConflicts(ctx context.Context, s *store.Store, out string, now func() time.Time) (_ []ConflictError, err error) {
	b := builder{out: out, now: now, writeFile: writeFile, afterRender: func() {}}
	unlock, err := b.lock()
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, unlock()) }()
	buildErr := b.catchUp(ctx, s)
	var ce *ConflictError
	if errors.As(buildErr, &ce) {
		buildErr = nil
	}
	if err := errors.Join(buildErr, cleanup(ctx, s, b.out, b.now)); err != nil {
		return nil, err
	}
	beforeScan()
	return Conflicts(out)
}

// beforeScan runs just before RepairConflicts reads the conflicts; a test checks the lock
// there.
var beforeScan = func() {}

// Confirm resolves the open output conflicts in out: you confirm them with brain repair
// --confirm. Under the lock, it builds a new generation in place of a hand-edited current
// generation, deletes every conflict folder (the current one and the retired ones that
// cleanup kept), and then repairs as Repair does. Before the deletes, it waits until the
// pointer left them more than one hookCap ago, so a hook that read the old pointer still
// reads its folder. It returns the conflicts that it deleted, in generation order. now is
// the clock.
func Confirm(ctx context.Context, s *store.Store, out string, now func() time.Time) (_ []ConflictError, err error) {
	b := builder{out: out, now: now, writeFile: writeFile, afterRender: func() {}}
	unlock, err := b.lock()
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, unlock()) }()

	cs, err := Conflicts(out)
	if err != nil {
		return nil, err
	}
	cur, err := current(out)
	if err != nil {
		return nil, err
	}
	// Every conflict counts as no source, also one that the outputs rows still record as current
	// after a build that switched the pointer and stopped before its record.
	for _, c := range cs {
		b.replace = append(b.replace, c.Generation)
	}
	if err := restoreFiles(ctx, s, out, cur, cs); err != nil {
		return nil, err
	}
	// The pointer leaves the hand-edited generation before its folder goes, so it never names
	// a folder that is not there.
	if slices.Contains(b.replace, cur) {
		if err := s.Write(ctx, func(tx *sql.Tx) error { return newGeneration(ctx, tx, cur) }); err != nil {
			return nil, err
		}
		if _, err := b.buildLocked(ctx, s, nil); err != nil {
			return nil, err
		}
	}
	if len(cs) > 0 {
		if err := waitReaders(ctx, s, out, now); err != nil {
			return nil, err
		}
	}
	for _, c := range cs {
		if err := os.RemoveAll(filepath.Join(out, folder(c.Generation))); err != nil {
			return nil, err
		}
	}
	return cs, b.repair(ctx, s)
}

// restoreFiles writes each skill and tool file of the current generation cur that you
// confirmed in cs again, from the text of its revision in the store. A running session may
// still read an older revision, so this covers every listed revision, not only the newest. A
// revision whose stored text no longer has the listed hash (forget erased it) is not written:
// its item is no active skill or tool, so cleanup removes the file.
func restoreFiles(ctx context.Context, s *store.Store, out string, cur int64, cs []ConflictError) error {
	if !slices.ContainsFunc(cs, func(c ConflictError) bool { return c.Generation == cur }) {
		return nil
	}
	m, _, err := readManifest(filepath.Join(out, folder(cur)))
	if errors.Is(err, ErrInconsistent) {
		// A damaged manifest lists no file that Brain can trust; the rebuild writes the files
		// of the new generation.
		return nil
	}
	if err != nil {
		return err
	}
	changed, err := changedFiles(out, m)
	if err != nil {
		return err
	}
	for _, k := range fileKinds {
		for _, f := range k.listed(m) {
			name := filepath.Base(f.Path)
			path := k.path(out, f.Item, f.Revision, name)
			if !slices.Contains(changed, path) {
				continue
			}
			var r store.ItemRevision
			err := s.Write(ctx, func(tx *sql.Tx) (err error) {
				r, err = store.ItemRevisionByKey(ctx, tx, f.Item, f.Revision)
				return err
			})
			if err != nil {
				return err
			}
			text, ok := k.files(r.Text)[name]
			if !ok || hash([]byte(text)) != f.SHA256 {
				continue
			}
			if err := writeAtomic(path, text, writeFile); err != nil {
				return err
			}
		}
	}
	return nil
}

// sleep is time.Sleep; a test replaces it.
var sleep = time.Sleep

// waitReaders waits until the pointer left every generation that is not current more than
// hookCap ago, so no hook still reads their folders. With no record of the last switch it
// waits one full hookCap.
func waitReaders(ctx context.Context, s *store.Store, out string, now func() time.Time) error {
	cur, err := current(out)
	if err != nil {
		return err
	}
	left, err := switchedAt(ctx, s, cur)
	if err != nil {
		return err
	}
	wait := hookCap
	if !left.IsZero() {
		wait = min(hookCap, hookCap-now().Sub(left))
	}
	if wait > 0 {
		sleep(wait)
	}
	return nil
}

// newGeneration records one more output generation when the store has none newer than cur, so
// that the build that replaces generation cur has a new number. Only commits record one
// otherwise.
func newGeneration(ctx context.Context, tx *sql.Tx, cur int64) error {
	newest, err := store.NewestGeneration(ctx, tx)
	if err != nil || newest > cur {
		return err
	}
	return store.InsertOutput(ctx, tx, store.Output{Generation: newest + 1, Folder: folder(newest + 1)})
}

// behind reports whether the store records a generation newer than cur, the pointer, or the
// outputs rows do not record cur as current yet.
func behind(ctx context.Context, s *store.Store, cur int64) (bool, error) {
	var newest, recorded int64
	err := s.Write(ctx, func(tx *sql.Tx) (err error) {
		if newest, err = store.NewestGeneration(ctx, tx); err != nil {
			return err
		}
		recorded, err = store.CurrentGeneration(ctx, tx)
		return err
	})
	return newest > cur || recorded != cur, err
}

// cleanup deletes every staging folder in out, and each generation that is not current and
// that the pointer left more than hookCap ago. The caller holds the lock, so no build writes a
// staging folder now, and a staging folder was never published: it has no reader. A
// generation whose files no longer match its manifest stays as a conflict (Conflicts).
func cleanup(ctx context.Context, s *store.Store, out string, now func() time.Time) error {
	gens, staging, err := folders(out)
	if err != nil {
		return err
	}
	if err := dropStaging(out, staging); err != nil {
		return err
	}
	cur, err := current(out)
	if err != nil {
		return err
	}
	left, err := switchedAt(ctx, s, cur)
	if err != nil {
		return err
	}
	var kept []int64
	for _, gen := range gens {
		if gen == cur || left.IsZero() || now().Sub(left) <= hookCap {
			kept = append(kept, gen)
			continue
		}
		_, err := checkGeneration(out, gen)
		var ce *ConflictError
		if errors.As(err, &ce) {
			kept = append(kept, gen)
			continue
		}
		if err != nil {
			return err
		}
		if err := os.RemoveAll(filepath.Join(out, folder(gen))); err != nil {
			return err
		}
	}
	if err := cleanFiles(ctx, s, out, cur, left, now); err != nil {
		return err
	}
	// A deleted generation holds no forgotten text any more, so the hook stops withholding for
	// it.
	return s.Write(ctx, func(tx *sql.Tx) error { return store.ClearForgotten(ctx, tx, kept) })
}

// cleanFiles deletes the skill and tool files of each item that is not an active skill or
// tool, when the current generation cur lists none of them and the pointer switched to cur
// more than hookCap ago, the time left. All revision files of an active skill or tool stay,
// so a running session can still read the path in its brief. A folder name that is no item
// id stays. The caller holds the lock, so no build writes such a file now.
func cleanFiles(ctx context.Context, s *store.Store, out string, cur int64, left time.Time, now func() time.Time) error {
	if cur == 0 || left.IsZero() || now().Sub(left) <= hookCap {
		return nil
	}
	m, _, err := readManifest(filepath.Join(out, folder(cur)))
	if errors.Is(err, ErrInconsistent) {
		// With a damaged current manifest, Brain cannot see which files it lists: all stay
		// until you resolve the conflict.
		return nil
	}
	if err != nil {
		return err
	}
	for _, k := range fileKinds {
		if err := cleanKind(ctx, s, out, m, k); err != nil {
			return err
		}
	}
	return nil
}

// cleanKind deletes the folder of each item in the folder of k that the manifest m does not
// list and that is no active item of k.
func cleanKind(ctx context.Context, s *store.Store, out string, m Manifest, k fileKind) error {
	dir := k.dir(out)
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	keep := make(map[int64]bool)
	for _, f := range k.listed(m) {
		keep[f.Item] = true
	}
	// The deletes run inside the store write, so no restore or undo can make one of these
	// items active between the check and the delete.
	return s.Write(ctx, func(tx *sql.Tx) error {
		active, err := k.active(ctx, tx)
		if err != nil {
			return err
		}
		for _, r := range active {
			keep[r.ItemID] = true
		}
		for _, e := range entries {
			id, err := strconv.ParseInt(e.Name(), 10, 64)
			if err != nil || keep[id] {
				continue
			}
			if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
				return err
			}
		}
		return nil
	})
}

// switchedAt returns the time of the last pointer switch, to generation cur: its outputs row
// records it. Every other generation was left at that time or before. It returns the zero
// time when there is no pointer yet, or when the rows do not record cur as current yet.
func switchedAt(ctx context.Context, s *store.Store, cur int64) (time.Time, error) {
	if cur == 0 {
		return time.Time{}, nil
	}
	var o store.Output
	err := s.Write(ctx, func(tx *sql.Tx) (err error) {
		o, err = store.OutputByGeneration(ctx, tx, cur)
		return err
	})
	if err != nil || !o.Current {
		return time.Time{}, err
	}
	return o.BuiltAt, nil
}

// Conflicts returns the open output conflicts in out, in generation order: each generation
// folder whose files no longer match its manifest, and the current generation when a skill
// file that it lists no longer matches. Brain keeps these folders until you resolve the
// conflict. brain status and brain doctor show them.
func Conflicts(out string) ([]ConflictError, error) {
	gens, _, err := folders(out)
	if err != nil {
		return nil, err
	}
	cur, err := current(out)
	if err != nil {
		return nil, err
	}
	var cs []ConflictError
	for _, gen := range gens {
		check := checkGeneration
		if gen == cur {
			check = checkCurrent
		}
		_, err := check(out, gen)
		var ce *ConflictError
		if errors.As(err, &ce) {
			cs = append(cs, *ce)
			continue
		}
		if err != nil {
			return nil, err
		}
	}
	return cs, nil
}

// Holds reports whether any folder in out may still hold the text of item, at any revision, or
// a skill or tool file of item is still there. The forget flow reports "done" only when it is
// false. A generation counts by the (item id, revision) pairs in its manifest, never by a text
// search. A staging folder, a generation with a missing or invalid manifest, or a file that the
// manifest does not name may hold anything, so it counts too, until cleanup removes it. A kept
// conflict folder counts too: a file that fails its hash may hold any text, whatever its
// manifest names.
func Holds(out string, item int64) (bool, error) {
	gens, staging, err := holding(out, item)
	if err != nil || staging || len(gens) > 0 {
		return staging || len(gens) > 0, err
	}
	return revisionFiles(out, item)
}

// revisionFiles reports whether a skill or tool file of item, of any revision, is still on
// disk: forget of a skill or tool stays pending until every revision file is gone.
func revisionFiles(out string, item int64) (bool, error) {
	for _, k := range fileKinds {
		_, err := os.Stat(filepath.Join(k.dir(out), strconv.FormatInt(item, 10)))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		return err == nil, err
	}
	return false, nil
}

// holding returns the generations in out that may hold the text of item, as Holds counts
// them, and whether a staging folder is there.
func holding(out string, item int64) (gens []int64, staging bool, err error) {
	all, stagings, err := folders(out)
	if err != nil {
		return nil, false, err
	}
	for _, gen := range all {
		g, err := readGeneration(filepath.Join(out, folder(gen)))
		if err != nil && !errors.Is(err, ErrInconsistent) {
			return nil, false, err
		}
		// err is ErrInconsistent here. A manifest that did not read leaves a zero generation number.
		if err != nil || g.manifest.Generation != gen || len(g.extra) > 0 || holdsItem(g.manifest, item) {
			gens = append(gens, gen)
		}
	}
	return gens, len(stagings) > 0, nil
}

func holdsItem(m Manifest, item int64) bool {
	for _, f := range m.Files {
		if slices.ContainsFunc(f.Revisions, func(r store.RevisionRef) bool { return r.Item == item }) {
			return true
		}
	}
	return false
}

// Forget runs erase, the store write that forgets item, under the build lock. The same write
// marks each generation in out that holds item with outputs.holds_forgotten_text, so the hook
// withholds the brief until cleanup deletes them, and a build from one of them logs no old
// bytes. With the lock held, no build writes a generation between the check and the mark.
// Forget first deletes the staging folders: no outputs row can mark them, so a staging folder
// left after the commit would hold the text while the hook delivers.
func Forget(ctx context.Context, s *store.Store, out string, item int64, erase func(tx *sql.Tx) error) (err error) {
	b := builder{out: out}
	unlock, err := b.lock()
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, unlock()) }()
	_, staging, err := folders(out)
	if err != nil {
		return err
	}
	if err := dropStaging(out, staging); err != nil {
		return err
	}
	gens, _, err := holding(out, item)
	if err != nil {
		return err
	}
	return s.Write(ctx, func(tx *sql.Tx) error {
		if err := erase(tx); err != nil {
			return err
		}
		return store.MarkForgotten(ctx, tx, gens)
	})
}

// dropStaging deletes the staging folders names in out. The caller holds the lock, so no
// build writes one now, and a staging folder was never published: it has no reader.
func dropStaging(out string, names []string) error {
	for _, name := range names {
		if err := os.RemoveAll(filepath.Join(out, name)); err != nil {
			return err
		}
	}
	return nil
}

// folders lists the generation folders in out, in generation order, and the names of the
// staging folders. An out folder that is not there yet holds neither.
func folders(out string) (gens []int64, staging []string, err error) {
	entries, err := os.ReadDir(out)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, stagingPrefix) {
			staging = append(staging, name)
			continue
		}
		num, ok := strings.CutPrefix(name, "gen-")
		if gen, err := strconv.ParseInt(num, 10, 64); ok && err == nil && e.IsDir() {
			gens = append(gens, gen)
		}
	}
	slices.Sort(gens)
	return gens, staging, nil
}
