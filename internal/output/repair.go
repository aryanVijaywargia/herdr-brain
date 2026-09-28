package output

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

// hookCap is the total time cap of the hook (§8.3). A reader that read the old pointer is done
// within one cap, so a generation that the pointer left more than one cap ago has no reader
// (§7.5 step 8).
const hookCap = 300 * time.Millisecond

// Repair builds the output when it is behind the store, and then cleans the out folder (§7.5
// steps 6, 8, 9): it deletes every staging folder, and each old generation that no reader can
// still use. Every brain call except the hook runs it, and so do brain doctor and forget. A
// hand-edited current generation stops the build with a *ConflictError; the cleanup still
// runs, so a forget can finish once you resolve the conflict. now is the clock.
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
	cur, err := current(b.out)
	if err != nil {
		return err
	}
	behind, err := behind(ctx, s, cur)
	if err != nil {
		return err
	}
	var buildErr error
	if behind {
		_, buildErr = b.buildLocked(ctx, s, nil)
	}
	return errors.Join(buildErr, cleanup(ctx, s, b.out, b.now))
}

// Confirm resolves the open output conflicts in out: you confirm them with brain repair
// --confirm (§7.5 step 5, D5). Under the lock, it builds a new generation in place of a
// hand-edited current generation, deletes every conflict folder (the current one and the
// retired ones that cleanup kept), and then repairs as Repair does. It returns the conflicts
// that it deleted, in generation order. now is the clock.
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
	// The pointer leaves the hand-edited generation before its folder goes, so it never names
	// a folder that is not there (§7.5 step 9).
	if slices.ContainsFunc(cs, func(c ConflictError) bool { return c.Generation == cur }) {
		b.replace = cur
		if err := s.Write(ctx, func(tx *sql.Tx) error { return newGeneration(ctx, tx, cur) }); err != nil {
			return nil, err
		}
		if _, err := b.buildLocked(ctx, s, nil); err != nil {
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

// newGeneration records one more output generation when the store has none newer than cur, so
// that the build that replaces generation cur has a new number. Only commits record one
// otherwise (§7.5 step 2).
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
	for _, name := range staging {
		if err := os.RemoveAll(filepath.Join(out, name)); err != nil {
			return err
		}
	}
	cur, err := current(out)
	if err != nil {
		return err
	}
	left, err := switchedAt(ctx, s, cur)
	if err != nil || left.IsZero() || now().Sub(left) <= hookCap {
		return err
	}
	for _, gen := range gens {
		if gen == cur {
			continue
		}
		_, err := checkGeneration(out, gen)
		var ce *ConflictError
		if errors.As(err, &ce) {
			continue
		}
		if err != nil {
			return err
		}
		if err := os.RemoveAll(filepath.Join(out, folder(gen))); err != nil {
			return err
		}
	}
	return nil
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
// folder whose files no longer match its manifest (S66). Brain keeps these folders until you
// resolve the conflict. brain status and brain doctor show them.
func Conflicts(out string) ([]ConflictError, error) {
	gens, _, err := folders(out)
	if err != nil {
		return nil, err
	}
	var cs []ConflictError
	for _, gen := range gens {
		_, err := checkGeneration(out, gen)
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

// Holds reports whether any folder in out may still hold the text of item, at any revision
// (§6.7). The forget flow reports "done" only when it is false. A generation counts by the
// (item id, revision) pairs in its manifest, never by a text search. A staging folder, a
// generation with a missing or invalid manifest, or a file that the manifest does not name may
// hold anything, so it counts too, until cleanup removes it. A kept conflict folder counts by
// its manifest like any other.
func Holds(out string, item int64) (bool, error) {
	gens, staging, err := folders(out)
	if err != nil {
		return false, err
	}
	if len(staging) > 0 {
		return true, nil
	}
	for _, gen := range gens {
		g, err := readGeneration(filepath.Join(out, folder(gen)))
		if err != nil && !errors.Is(err, ErrInconsistent) {
			return false, err
		}
		// A manifest that did not read leaves a zero generation number.
		if g.manifest.Generation != gen || len(g.extra) > 0 {
			return true, nil
		}
		for _, f := range g.manifest.Files {
			if slices.ContainsFunc(f.Revisions, func(r store.RevisionRef) bool { return r.Item == item }) {
				return true, nil
			}
		}
	}
	return false, nil
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
