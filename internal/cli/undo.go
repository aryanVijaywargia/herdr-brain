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
func Undo(ctx context.Context, e Env, args []string) error {
	return onItem(ctx, e, "undo", undoUsage, args, caller.undo)
}

// onItem runs a command whose one argument is an item id: it parses the id, opens the store
// and runs fn with a caller.
func onItem(ctx context.Context, e Env, name, usage string, args []string,
	fn func(caller, context.Context, int64) error) (err error) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("%w\n%s", err, usage)
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("want one item id, got %d\n%s", fs.NArg(), usage)
	}
	id, err := strconv.ParseInt(fs.Arg(0), 10, 64)
	if err != nil || id <= 0 {
		return fmt.Errorf("bad item id %q\n%s", fs.Arg(0), usage)
	}
	c, err := e.Open(ctx)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, c.Close()) }()
	return fn(newCaller(e, c), ctx, id)
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
		if forgotten(cur) {
			return fmt.Errorf("item %d is forgotten: undo has no text to restore", id)
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
		if _, err := store.WriteRevision(ctx, tx, it.CurrentRevision, next); err != nil {
			return err
		}
		// Undo of the disputed revision ends the dispute: keep and retire of a dispute need a
		// disputed item, so its pending dispute would stay in the queue for ever.
		if it.State == store.StateDisputed && next.State != store.StateDisputed {
			if err := store.SupersedeDisputes(ctx, tx, it.ID, cl.Now()); err != nil {
				return err
			}
		}
		// The undo of a link move takes the item out of the caller's view: its pending
		// proposals end, as in the link, or no pane could decide them.
		if !cl.sees(next.WorkspaceKey) {
			_, err := endProposals(ctx, tx, []int64{it.ID}, cl.surface, cl.Now())
			return err
		}
		return nil
	})
	if err != nil {
		return err
	}
	if undone != 0 {
		_, err = fmt.Fprintf(cl.Stdout, "item %d: revision %d is undone already; nothing to do\n", id, undone)
		return err
	}
	target := cl.target(next.Scope)
	if !cl.sees(next.WorkspaceKey) {
		target = "back in workspace " + *next.WorkspaceKey // the undo of a link move
	}
	_, err = fmt.Fprintf(cl.Stdout, "item %d: revision %d undone; the item is %s at revision %d (%s)\n",
		id, *next.UndoesRevision, next.State, next.Revision, target)
	// The build runs even when the message fails: the commit is done, and the next hook must
	// print it (E4 post-commit build).
	return errors.Join(err, cl.deliver(ctx, nil))
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
	// workspace that the caller does not see. The one exception is the undo of a link move
	// (§9.4 "Link undo").
	if !cl.sees(prev.WorkspaceKey) {
		back, err := cl.linkUndo(cur, prev)
		if err != nil {
			return store.ItemRevision{}, err
		}
		if !back {
			return store.ItemRevision{}, fmt.Errorf("item %d: %w", cur.ItemID, errNotFound)
		}
	}
	next := nextRevision(prev, cl.Now())
	next.UndoesRevision = &cur.Revision
	return next, nil
}

// linkUndo reports whether undo may return cur, a revision in the caller's workspace, to prev,
// the revision before it, in a workspace that the caller does not see. Only a link moves a
// workspace item to another workspace key, so cur is a link move. The old key must be an entry
// of the caller's herdr session that is not live (§9.4). It need not be in the relink list any
// more: a later close of another workspace can drop it from there.
func (cl caller) linkUndo(cur, prev store.ItemRevision) (bool, error) {
	if cur.Scope != store.ScopeWorkspace || prev.Scope != store.ScopeWorkspace {
		return false, nil
	}
	e, ok, err := cl.Workspace.Entry(*prev.WorkspaceKey)
	return ok && !live(e), err
}
