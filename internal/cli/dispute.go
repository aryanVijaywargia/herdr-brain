package cli

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/aryanVijaywargia/herdr-brain/internal/signal"
	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

// anchor stores the anchors of r, a revision that approval or restore just wrote: each
// path in its text that git tracks in the root folder of its workspace. A global item has no
// one workspace root and gets no anchor (E16-S6 open point), nor does a workspace whose root
// is not in git. A revision that is not active keeps the anchors of the item.
func anchor(ctx context.Context, tx *sql.Tx, r store.ItemRevision) error {
	if r.State != store.StateActive {
		return nil
	}
	var anchors []store.Anchor
	if r.Scope == store.ScopeWorkspace {
		ws, err := store.WorkspaceByKey(ctx, tx, *r.WorkspaceKey)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if ws.RootFolder != nil {
			if anchors, err = signal.FindAnchors(ctx, *ws.RootFolder, r.Text); err != nil {
				return err
			}
		}
	}
	return store.SetAnchors(ctx, tx, r.ItemID, anchors)
}

// settle decides the dispute proposal id: keep makes the item active again with a new
// revision of the same text, and stores its anchors as they are now: the new blob of a changed
// path, no anchor for a path that is gone. Retire retires the item with a new revision, which
// keeps the history: brain review --restore brings it back.
func (cl caller) settle(ctx context.Context, id int64, keep bool) error {
	var next store.ItemRevision
	err := cl.Store.Write(ctx, func(tx *sql.Tx) error {
		p, it, cur, err := cl.pending(ctx, tx, id)
		if err != nil {
			return err
		}
		switch {
		case len(p.Dispute) == 0:
			return fmt.Errorf("proposal %d is not a dispute: --keep and --retire with a proposal id decide a disputed item", id)
		case it.State != store.StateDisputed || it.CurrentRevision != *p.BaseRevision:
			return fmt.Errorf("item %d changed after proposal %d was made; nothing was written", it.ID, id)
		}
		next = nextRevision(cur, cl.Now())
		next.Revision, next.ProposalID, next.State = it.CurrentRevision+1, &p.ID, store.StateRetired
		if keep {
			next.State = store.StateActive
			_, now, err := signal.CheckAnchors(ctx, it.Anchors)
			if err != nil {
				return err
			}
			if err := store.SetAnchors(ctx, tx, it.ID, now); err != nil {
				return err
			}
		}
		if err := store.DecideProposal(ctx, tx, p.ID, store.ProposalApproved, cl.surface, cl.Now()); err != nil {
			return err
		}
		_, err = store.WriteRevision(ctx, tx, it.CurrentRevision, next)
		return err
	})
	if err != nil {
		return skillError(err)
	}
	what := "kept: it is active again, with its anchors as they are now"
	if !keep {
		what = "retired: brain review --retired lists it, and brain review --restore brings it back"
	}
	_, err = fmt.Fprintf(cl.Stdout, "proposal %d: item %d %s (revision %d)\n", id, next.ItemID, what, next.Revision)
	return errors.Join(err, cl.deliver(ctx, &id))
}
