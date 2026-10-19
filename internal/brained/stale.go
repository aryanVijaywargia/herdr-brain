package brained

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"time"

	"github.com/aryanVijaywargia/herdr-brain/internal/output"
	"github.com/aryanVijaywargia/herdr-brain/internal/signal"
	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

const (
	// staleEvery is the pause between two stale checks of every workspace: once a day.
	// 24 h from the start of brained: any time of day.
	staleEvery = 24 * time.Hour
	// staleTimeout is the deadline of one stale check: its git reads, its write and the build.
	// 1 minute.
	staleTimeout = time.Minute
	// outDir is the out folder of a store, as in the hook.
	outDir = "out"
)

// dispute is one item whose anchor files moved or were deleted.
type dispute struct {
	item    store.Item
	changes []signal.AnchorChange
}

// check is what the stale check does with one item (#252): dispute it, give it a new
// baseline after a normal edit, or make it active again.
type check struct {
	item store.Item
	// gone holds the anchors whose path is gone: moved or deleted.
	gone []signal.AnchorChange
	// now holds the anchors as they are now.
	now []store.Anchor
}

// Stale checks the anchors of the active and disputed items of workspace key in the store of
// g, or of every workspace when key is "". An active item whose anchor file moved or
// was deleted becomes disputed, with a proposal in the review queue that holds the anchor
// diff. It stays delivered with a mark, so Stale builds the output. A normal edit of an anchor
// file does not dispute the item: the item gets the new blob as its baseline. A disputed item
// whose anchor paths are all still there, and whose dispute is of a normal edit, is active
// again: an older build disputed it (#252). Brain does not change the text of the item or retire it: you decide in
// review. now is the clock.
func Stale(ctx context.Context, g Gate, key string, now func() time.Time) (err error) {
	ctx, cancel := context.WithTimeout(ctx, staleTimeout)
	defer cancel()
	st, err := store.Open(ctx, g.Binding.Store, busyTimeout)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, st.Close()) }()
	var items []store.Item
	err = st.Write(ctx, func(tx *sql.Tx) (err error) {
		items, err = store.AnchoredItems(ctx, tx, key)
		return err
	})
	if err != nil {
		return err
	}
	// git runs outside the transaction, so the store lock does not wait for it.
	var cs []check
	for _, it := range items {
		changes, cur, err := signal.CheckAnchors(ctx, it.Anchors)
		if err != nil {
			return fmt.Errorf("check the anchors of item %d: %w", it.ID, err)
		}
		gone := slices.DeleteFunc(changes, func(c signal.AnchorChange) bool { return c.Blob != "" })
		edited := len(cur) == len(it.Anchors) && !slices.Equal(cur, it.Anchors)
		switch {
		case it.State == store.StateActive && (len(gone) > 0 || edited),
			it.State == store.StateDisputed && len(gone) == 0:
			cs = append(cs, check{item: it, gone: gone, now: cur})
		}
	}
	if len(cs) == 0 {
		return nil
	}
	build := false
	err = st.Write(ctx, func(tx *sql.Tx) error {
		for _, c := range cs {
			ok, err := apply(ctx, tx, c, now().UTC())
			if err != nil {
				return err
			}
			build = build || ok
		}
		return nil
	})
	if err != nil || !build {
		return err
	}
	_, err = output.Build(ctx, st, filepath.Join(g.Binding.Store, outDir), nil, now)
	// A newer commit made this build stale; the build of that commit holds the marks too.
	if errors.Is(err, output.ErrStale) {
		return nil
	}
	return err
}

// apply does what c says. build is true when the brief changed: an item became disputed or
// active again. An item that changed after the check read it is left to the next check.
func apply(ctx context.Context, tx *sql.Tx, c check, now time.Time) (build bool, err error) {
	it := c.item
	switch {
	case it.State == store.StateDisputed:
		return store.Undispute(ctx, tx, it.ID, it.CurrentRevision, c.now, now)
	case len(c.gone) > 0:
		return disputeItem(ctx, tx, dispute{item: it, changes: c.gone}, now)
	}
	return false, store.RebaseAnchors(ctx, tx, it.ID, it.CurrentRevision, c.now)
}

// disputeItem makes the item of d disputed with its review proposal. ok is false when the
// item changed after the check read it: the next check reads it again.
func disputeItem(ctx context.Context, tx *sql.Tx, d dispute, now time.Time) (ok bool, err error) {
	it := d.item
	cur, err := store.ItemRevisionByKey(ctx, tx, it.ID, it.CurrentRevision)
	if err != nil {
		return false, err
	}
	ws, err := store.WorkspaceByKey(ctx, tx, *it.WorkspaceKey)
	if err != nil {
		return false, err
	}
	paths := make([]string, len(d.changes))
	for i, c := range d.changes {
		paths[i] = c.Anchor.Path
	}
	base := it.CurrentRevision
	p := store.Proposal{ItemID: &it.ID, BaseRevision: &base, Text: cur.Text, Scope: cur.Scope,
		Target: fmt.Sprintf("workspace %s (%s)", ws.HerdrWorkspaceID, ws.Label), Diff: signal.AnchorDiff(d.changes),
		Dispute: paths, Status: store.ProposalPending, Created: now}
	id, err := store.Dispute(ctx, tx, p)
	return id != 0, err
}
