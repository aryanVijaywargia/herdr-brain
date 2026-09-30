package cli

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"

	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

// link links workspace item r.item to global item r.exceptionTo as an exception, or marks a
// conflict between it and global item r.conflictsWith, in a new revision (§7.4 G2). M1 has no
// automatic marks: you make them here. A link that is set already is left as it is.
func (cl caller) link(ctx context.Context, r review) error {
	global := max(r.exceptionTo, r.conflictsWith)
	var rev int64
	err := cl.Store.Write(ctx, func(tx *sql.Tx) error {
		it, cur, err := cl.item(ctx, tx, r.item)
		if err != nil {
			return err
		}
		if it.State != store.StateActive || it.Scope != store.ScopeWorkspace {
			return fmt.Errorf("item %d is a %s %s item; a link starts from an active workspace item", it.ID, it.State, it.Scope)
		}
		g, gcur, err := cl.item(ctx, tx, global)
		if err != nil {
			return err
		}
		if g.State != store.StateActive || g.Scope != store.ScopeGlobal {
			return fmt.Errorf("item %d is a %s %s item; a link goes to an active global item", g.ID, g.State, g.Scope)
		}
		next, err := cl.linked(r, cur, g, gcur)
		if err != nil {
			return err
		}
		if sameLinks(cur, next) {
			return nil
		}
		rev = it.CurrentRevision + 1
		_, err = store.WriteRevision(ctx, tx, it.CurrentRevision, next)
		return err
	})
	if err != nil {
		return err
	}
	what := fmt.Sprintf("an exception to global item %d", global)
	if r.conflictsWith != 0 {
		what = fmt.Sprintf("in conflict with item %d", global)
	}
	if rev == 0 {
		_, err = fmt.Fprintf(cl.Stdout, "item %d is %s already\n", r.item, what)
		return err
	}
	_, err = fmt.Fprintf(cl.Stdout, "item %d is %s at revision %d\n", r.item, what, rev)
	return errors.Join(err, cl.deliver(ctx, nil))
}

// linked is the revision of cur that r's link or conflict mark to global item g writes.
func (cl caller) linked(r review, cur store.ItemRevision, g store.Item, gcur store.ItemRevision) (store.ItemRevision, error) {
	next := nextRevision(cur, cl.Now())
	next.ConflictsWith = slices.Clone(cur.ConflictsWith)
	if r.conflictsWith != 0 {
		// An unresolved conflict replaces an exception between the two items.
		if cur.ExceptionToItem != nil && *cur.ExceptionToItem == g.ID {
			next.ExceptionToItem, next.ExceptionToRevision = nil, nil
		}
		if !slices.Contains(next.ConflictsWith, g.ID) {
			next.ConflictsWith = append(next.ConflictsWith, g.ID)
		}
		return next, nil
	}
	return except(next, g, gcur)
}

// except links revision next as an exception to global item g, whose current revision is
// gcur, and drops its conflict mark with g. A workspace item never relaxes a global permission
// or safety rule (§7.4, §8.1): except refuses such a g.
func except(next store.ItemRevision, g store.Item, gcur store.ItemRevision) (store.ItemRevision, error) {
	if permissionWords.MatchString(gcur.Text) {
		return store.ItemRevision{}, fmt.Errorf("item %d may be a permission, an allow rule or a safety rule: a workspace item cannot be an exception to it; mark a conflict with --conflicts-with %d; nothing was written",
			g.ID, g.ID)
	}
	// The exception resolves a conflict between the two items.
	next.ExceptionToItem, next.ExceptionToRevision = &g.ID, &g.CurrentRevision
	next.ConflictsWith = slices.DeleteFunc(slices.Clone(next.ConflictsWith), func(id int64) bool { return id == g.ID })
	return next, nil
}

// sameLinks reports whether a and b have the same exception link and conflict marks.
func sameLinks(a, b store.ItemRevision) bool {
	same := func(x, y *int64) bool { return (x == nil) == (y == nil) && (x == nil || *x == *y) }
	return same(a.ExceptionToItem, b.ExceptionToItem) && same(a.ExceptionToRevision, b.ExceptionToRevision) &&
		slices.Equal(a.ConflictsWith, b.ConflictsWith)
}

// staleLinks lists the exception links that went back to review because their global item
// has a new revision (§7.4), for the items that the caller sees.
func (cl caller) staleLinks(ctx context.Context, tx *sql.Tx) ([]string, error) {
	revs, err := store.StaleExceptions(ctx, tx)
	if err != nil {
		return nil, err
	}
	var lines []string
	for _, r := range revs {
		if !cl.sees(r.WorkspaceKey) {
			continue
		}
		lines = append(lines, fmt.Sprintf("item %d  was an exception to item %d revision %d, which changed: link again with brain review --item %d --exception-to %d, or mark a conflict with --conflicts-with %d",
			r.ItemID, *r.ExceptionToItem, *r.ExceptionToRevision, r.ItemID, *r.ExceptionToItem, *r.ExceptionToItem))
	}
	return lines, nil
}
