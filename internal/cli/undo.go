package cli

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"strconv"

	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

const undoUsage = `usage: brain undo <item>`

// Undo runs `brain undo` with args after "undo" (§7.5 undo, E4-S5). It undoes the latest
// revision of an item with a new revision.
func Undo(ctx context.Context, e Env, args []string) (err error) {
	fs := flag.NewFlagSet("undo", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("%w\n%s", err, undoUsage)
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("want one item id, got %d\n%s", fs.NArg(), undoUsage)
	}
	id, err := strconv.ParseInt(fs.Arg(0), 10, 64)
	if err != nil || id <= 0 {
		return fmt.Errorf("bad item id %q\n%s", fs.Arg(0), undoUsage)
	}
	c, err := e.Open(ctx)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, c.Close()) }()
	return newCaller(e, c).undo(ctx, id)
}

// undo writes a revision of item id that copies the revision before the latest one, and
// records the latest one in undoes_revision (§7.5). Undo of a creation retires the item. A
// latest revision that is itself an undo is left as it is, so a second undo does nothing.
func (cl caller) undo(ctx context.Context, id int64) error {
	var next store.ItemRevision
	var undone int64
	err := cl.Store.Write(ctx, func(tx *sql.Tx) error {
		it, cur, err := cl.item(ctx, tx, id)
		if err != nil {
			return err
		}
		if it.State == store.StateDraft {
			return fmt.Errorf("item %d is a draft: decide its proposal with brain review", id)
		}
		if cur.UndoesRevision != nil {
			undone = *cur.UndoesRevision
			return nil
		}
		if next, err = cl.undone(ctx, tx, cur); err != nil {
			return err
		}
		next.Revision = it.CurrentRevision + 1
		_, err = store.WriteRevision(ctx, tx, it.CurrentRevision, next)
		return err
	})
	if err != nil {
		return err
	}
	if undone != 0 {
		_, err = fmt.Fprintf(cl.Stdout, "item %d: revision %d is undone already; nothing to do\n", id, undone)
		return err
	}
	if _, err := fmt.Fprintf(cl.Stdout, "item %d: revision %d undone; the item is %s at revision %d (%s)\n",
		id, *next.UndoesRevision, next.State, next.Revision, cl.target(next.Scope)); err != nil {
		return err
	}
	return cl.deliver(ctx, nil)
}

// undone is the revision that undoes cur. It copies the fields of the revision before cur:
// text, scope, workspace key, state, pin and links. When there is none, or it is a draft,
// cur created the item, and the undo retires it.
func (cl caller) undone(ctx context.Context, tx *sql.Tx, cur store.ItemRevision) (store.ItemRevision, error) {
	prev := store.ItemRevision{State: store.StateDraft}
	if cur.Revision > 1 {
		var err error
		if prev, err = store.ItemRevisionByKey(ctx, tx, cur.ItemID, cur.Revision-1); err != nil {
			return store.ItemRevision{}, err
		}
	}
	if prev.State == store.StateDraft {
		// A retired revision keeps its text, so that restore can bring it back (§9.2).
		prev = cur
		prev.State = store.StateRetired
	}
	// The E4 scope check covers the restored target too: undo never moves an item into a
	// workspace that the caller does not see.
	if !cl.sees(prev.WorkspaceKey) {
		return store.ItemRevision{}, fmt.Errorf("item %d: %w", cur.ItemID, errNotFound)
	}
	next := nextRevision(prev, cl.Now())
	next.UndoesRevision = &cur.Revision
	return next, nil
}
