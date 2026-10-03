package cli

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"slices"
	"time"

	"github.com/aryanVijaywargia/herdr-brain/internal/store"
	"github.com/aryanVijaywargia/herdr-brain/internal/workspace"
)

const workspaceUsage = `usage: brain workspace link <old key> <new key> [--confirm]`

// errDryRun ends the transaction of a link with no --confirm: the link runs with every check,
// the skill name check of the store too, and then rolls back, so it writes nothing.
var errDryRun = errors.New("dry run")

// Workspace runs `brain workspace` with args after "workspace" (§9.4, E16-S9). Its one
// subcommand, link, moves the items of an old workspace key to a new one on your request:
// Brain never links by itself and never asks (S20). With no --confirm it prints what moves and
// writes nothing.
func Workspace(ctx context.Context, e Env, args []string) (err error) {
	if len(args) == 0 || args[0] != "link" {
		return fmt.Errorf("want the subcommand link\n%s", workspaceUsage)
	}
	fs := flag.NewFlagSet("workspace link", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	confirm := fs.Bool("confirm", false, "")
	// The keys can come before the flag too, as in `brain workspace link <old> <new> --confirm`.
	var keys []string
	for rest := args[1:]; ; rest = fs.Args()[1:] {
		if err := fs.Parse(rest); err != nil {
			return fmt.Errorf("%w\n%s", err, workspaceUsage)
		}
		if fs.NArg() == 0 {
			break
		}
		keys = append(keys, fs.Arg(0))
	}
	if len(keys) != 2 {
		return fmt.Errorf("want an old and a new workspace key, got %d arguments\n%s", len(keys), workspaceUsage)
	}
	c, err := e.Open(ctx)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, c.Close()) }()
	return newCaller(e, c).linkWorkspace(ctx, keys[0], keys[1], *confirm)
}

// moved is what a link moves: the item ids, and the pending proposals of those items that end.
type moved struct {
	items, ended []int64
}

// linkWorkspace moves the items of key oldKey to key newKey in one transaction, when the scope rule allows it
// (linkScope). With confirm false it prints what moves and writes nothing.
func (cl caller) linkWorkspace(ctx context.Context, oldKey, newKey string, confirm bool) error {
	entry, err := cl.linkScope(oldKey, newKey)
	if err != nil {
		return err
	}
	var m moved
	err = cl.Store.Write(ctx, func(tx *sql.Tx) error {
		var err error
		if m, err = cl.move(ctx, tx, oldKey, newKey); err != nil || confirm {
			return err
		}
		return errDryRun
	})
	// A dry run commits nothing, whether its rollback works or not.
	if err != nil && !errors.Is(err, errDryRun) {
		return skillError(err)
	}
	if len(m.items) == 0 {
		_, err = fmt.Fprintf(cl.Stdout, "workspace %s has no items to move; nothing was written\n", oldKey)
		return err
	}
	if !confirm {
		_, err = fmt.Fprintf(cl.Stdout, "link %s (%s %s) to %s (%s %s) moves %d: %s\n"+
			"pending proposals of these items that end as superseded: %s\n"+
			"observations, candidates, rejections, the other proposals, outcomes, the prompt index and the settings stay with %s\n"+
			"nothing was written; move them: brain workspace link %s %s --confirm\n",
			oldKey, entry.WorkspaceID, entry.Label, newKey, cl.WorkspaceID, cl.label, len(m.items), itemList(m.items),
			listOrNone(m.ended), oldKey, oldKey, newKey)
		return err
	}
	_, err = fmt.Fprintf(cl.Stdout, "linked %s to %s: moved %d, each with a new revision: %s; pending proposals superseded: %s\n"+
		"undo the move of one item: brain undo <item>\n", oldKey, newKey, len(m.items), itemList(m.items), listOrNone(m.ended))
	// The build runs even when the message fails: the commit is done (§7.5, as in undo).
	return errors.Join(err, cl.deliver(ctx, nil))
}

// linkScope checks the scope rule of a link (§9.4 "Link scope", S83): the call comes from a
// pane in new, and old is a relink entry of this herdr session that is not live. It returns the
// map entry of old. An entry that is closed or replaced never becomes current again, so the
// check holds until the transaction of the link.
func (cl caller) linkScope(oldKey, newKey string) (workspace.Entry, error) {
	switch {
	case cl.keyErr != nil:
		return workspace.Entry{}, fmt.Errorf("run the link from a pane in workspace %s: %w; nothing was written", newKey, cl.keyErr)
	case newKey != cl.key:
		return workspace.Entry{}, fmt.Errorf("run the link from a pane in workspace %s; this pane is in %s (%s); nothing was written",
			newKey, cl.key, cl.label)
	}
	e, ok, err := cl.relinkEntry(oldKey)
	switch {
	case err != nil:
		return workspace.Entry{}, err
	case !ok:
		return workspace.Entry{}, fmt.Errorf("workspace %s is not in the relink list of brain status; nothing was written", oldKey)
	case live(e):
		return workspace.Entry{}, fmt.Errorf("workspace %s is live: herdr workspace %s maps to it; close it in herdr first; nothing was written",
			oldKey, e.WorkspaceID)
	}
	return e, nil
}

// relinkEntry returns the map entry of key when it is an old entry of the relink list of the
// caller's herdr session (§9.4), and false when it is not.
func (cl caller) relinkEntry(key string) (workspace.Entry, bool, error) {
	rs, err := cl.Workspace.Relinks()
	if err != nil {
		return workspace.Entry{}, false, err
	}
	for _, r := range rs {
		if r.Old.Key == key {
			return r.Old, true, nil
		}
	}
	return workspace.Entry{}, false, nil
}

// live reports whether e is the current map entry of its herdr workspace (S83): checked by the
// map entry, not by the herdr workspace id, which a later workspace can reuse.
func live(e workspace.Entry) bool { return !e.Closed && e.ReplacedBy == "" }

// move gives each item of key old a new revision with key new, and ends the pending proposals
// of those items: they are for a base revision that the move replaces. A draft stays with its
// proposal in old (§9.4: proposals stay). A disputed item becomes active, as after any new
// revision, and the next stale check in new marks it again when its anchors still differ.
// The store refuses a skill whose name an active skill in new has (§8.2).
func (cl caller) move(ctx context.Context, tx *sql.Tx, oldKey, newKey string) (moved, error) {
	var m moved
	for _, st := range []store.State{store.StateActive, store.StateDisputed, store.StateRetired} {
		ids, err := store.ItemIDsInState(ctx, tx, st)
		if err != nil {
			return moved{}, err
		}
		for _, id := range ids {
			it, err := store.ItemByID(ctx, tx, id)
			if err != nil {
				return moved{}, err
			}
			if it.WorkspaceKey == nil || *it.WorkspaceKey != oldKey {
				continue
			}
			cur, err := store.ItemRevisionByKey(ctx, tx, id, it.CurrentRevision)
			if err != nil {
				return moved{}, err
			}
			next := nextRevision(cur, cl.Now())
			next.WorkspaceKey = &newKey
			if _, err := store.WriteRevision(ctx, tx, it.CurrentRevision, next); err != nil {
				return moved{}, err
			}
			m.items = append(m.items, id)
		}
	}
	slices.Sort(m.items)
	ended, err := endProposals(ctx, tx, m.items, cl.surface, cl.Now())
	m.ended = ended
	return m, err
}

// endProposals ends the pending proposals of items as superseded, and returns their ids. A link
// and its undo move an item to another key: the proposals are for a base revision that the move
// replaces. [NEW] superseded, made in surface: the link is your action.
func endProposals(ctx context.Context, tx *sql.Tx, items []int64, surface store.Surface, now time.Time) ([]int64, error) {
	pending, err := store.PendingProposalIDs(ctx, tx)
	if err != nil {
		return nil, err
	}
	var ended []int64
	for _, id := range pending {
		p, err := store.ProposalByID(ctx, tx, id)
		if err != nil {
			return nil, err
		}
		if p.ItemID == nil || !slices.Contains(items, *p.ItemID) {
			continue
		}
		if err := store.DecideProposal(ctx, tx, id, store.ProposalSuperseded, surface, now); err != nil {
			return nil, err
		}
		ended = append(ended, id)
	}
	return ended, nil
}

// listOrNone is itemList of ids, or "none".
func listOrNone(ids []int64) string {
	if len(ids) == 0 {
		return "none"
	}
	return itemList(ids)
}
