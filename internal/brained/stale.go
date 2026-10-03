package brained

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/aryanVijaywargia/herdr-brain/internal/output"
	"github.com/aryanVijaywargia/herdr-brain/internal/signal"
	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

const (
	// staleEvery is the pause between two stale checks of every workspace (§9.1 "once a day").
	// [NEW] 24 h from the start of brained: any time of day.
	staleEvery = 24 * time.Hour
	// staleTimeout is the deadline of one stale check: its git reads, its write and the build.
	// [NEW] 1 minute.
	staleTimeout = time.Minute
	// outDir is the out folder of a store, as in the hook (§7.5 step 3).
	outDir = "out"
)

// dispute is one item whose anchors no longer match git.
type dispute struct {
	item    store.Item
	changes []signal.AnchorChange
}

// Stale checks the anchors of the active items of workspace key in the store of g, or of every
// workspace when key is "" (§9.1). An item whose anchor changed, moved or was deleted becomes
// disputed, with a proposal in the review queue that holds the anchor diff. It stays delivered
// with a mark, so Stale builds the output. Brain does not change the text of the item or
// retire it: you decide in review. now is the clock.
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
	var ds []dispute
	for _, it := range items {
		changes, _, err := signal.CheckAnchors(ctx, it.Anchors)
		if err != nil {
			return fmt.Errorf("check the anchors of item %d: %w", it.ID, err)
		}
		if len(changes) > 0 {
			ds = append(ds, dispute{item: it, changes: changes})
		}
	}
	if len(ds) == 0 {
		return nil
	}
	n := 0
	err = st.Write(ctx, func(tx *sql.Tx) error {
		for _, d := range ds {
			ok, err := disputeItem(ctx, tx, d, now().UTC())
			if err != nil {
				return err
			}
			if ok {
				n++
			}
		}
		return nil
	})
	if err != nil || n == 0 {
		return err
	}
	_, err = output.Build(ctx, st, filepath.Join(g.Binding.Store, outDir), nil, now)
	// A newer commit made this build stale; the build of that commit holds the marks too.
	if errors.Is(err, output.ErrStale) {
		return nil
	}
	return err
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
