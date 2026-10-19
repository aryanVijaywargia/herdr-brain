package brained

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path/filepath"
	"time"

	"github.com/aryanVijaywargia/herdr-brain/internal/bindings"
	"github.com/aryanVijaywargia/herdr-brain/internal/brief"
	"github.com/aryanVijaywargia/herdr-brain/internal/extract"
	"github.com/aryanVijaywargia/herdr-brain/internal/output"
	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

const (
	// mergeEvery is the pause between two merge passes: once a day. [NEW] 24 h from the
	// start of brained, as staleEvery.
	mergeEvery = 24 * time.Hour
	// mergeFull is the share of its budget, in percent, that a brief part fills before the merge
	// pass calls the model for it.
	mergeFull = 80
)

// mergePass runs the merge pass of the store of g: each part of the current output
// generation that is at least mergeFull percent of its budget goes to extract.Pass.Merge, which
// checks the rest of the trigger. The global part goes to any pass, and a workspace part to the
// pass of its herdr session, as a change request does. The caps come from the config file
// config, read at each pass; r runs the extractor CLI, and now is the clock. A failed part does
// not stop the others.
func mergePass(ctx context.Context, g Gate, config string, log io.Writer, r extract.Runner, now func() time.Time) (err error) {
	_, caps, err := extract.ReadConfig(config)
	if err != nil {
		return fmt.Errorf("merge: %w", err)
	}
	parts, err := output.Parts(filepath.Join(g.Binding.Store, outDir))
	if errors.Is(err, fs.ErrNotExist) {
		return nil // no generation is built yet: there is no part to merge
	}
	if err != nil {
		return fmt.Errorf("merge: %w", err)
	}
	st, err := store.Open(ctx, g.Binding.Store, busyTimeout)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, st.Close()) }()
	p := extract.Pass{Store: st, Runner: r, HerdrSession: g.Binding.Session, DefaultOn: g.Binding.Kind != bindings.KindTest,
		Now: now, Caps: caps, Note: func(line string) {
			_, _ = fmt.Fprintf(log, "brained: merge: %s\n", line) // a failed log write has no other place to go
		}}
	var errs []error
	for _, part := range parts {
		ok, err := ownPart(ctx, st, g, part)
		if err != nil {
			errs = append(errs, err)
		}
		if !ok {
			continue
		}
		if err := p.Merge(ctx, extract.MergePart{Key: part.Key, SHA256: part.SHA256}); err != nil {
			errs = append(errs, fmt.Errorf("merge part %q: %w", part.Key, err))
		}
	}
	return errors.Join(errs...)
}

// ownPart reports whether the pass of g takes part: it fills at least mergeFull percent of its
// budget, and it is the global part or a part of a workspace of the herdr session of g.
func ownPart(ctx context.Context, st *store.Store, g Gate, part output.Part) (bool, error) {
	size, limit := brief.GlobalSize(part.Text)
	if part.Key != "" {
		size, limit = brief.WorkspaceSize(part.Text)
	}
	if size*100 < limit*mergeFull {
		return false, nil
	}
	if part.Key == "" {
		return true, nil
	}
	var w store.Workspace
	err := st.Write(ctx, func(tx *sql.Tx) (err error) {
		w, err = store.WorkspaceByKey(ctx, tx, part.Key)
		return err
	})
	return err == nil && w.HerdrSession == g.Binding.Session, err
}
