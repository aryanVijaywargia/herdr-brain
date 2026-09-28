package cli

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/aryanVijaywargia/herdr-brain/internal/output"
	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

const forgetUsage = `usage: brain forget <item>`

// settle is how long forget waits after its build before it deletes the old generations: a
// generation that the pointer left less than one hook time cap ago (300 ms, §8.3) may still
// have a reader (§7.5 step 8). [NEW] 350 ms, the cap and a margin.
const settle = 350 * time.Millisecond

// Forget runs `brain forget` with args after "forget" (§6.7, E4-S6). It removes the text of
// one item and its derivatives from the store and the output, and keeps a tombstone with no
// text. A second call on the same item only reports whether forget is done.
func Forget(ctx context.Context, e Env, args []string) error {
	return onItem(ctx, e, "forget", forgetUsage, args, caller.forget)
}

// forgotten reports whether forget removed the text of the item whose current revision is cur:
// remember and review refuse an empty text.
func forgotten(cur store.ItemRevision) bool { return cur.Text == "" && cur.State == store.StateRetired }

// forget lists the copies of item id's claim in other scopes, then forgets the item in one
// transaction and builds the output, and reports "done" or "pending" (§6.7).
func (cl caller) forget(ctx context.Context, id int64) error {
	var copies []store.Item
	var was bool
	err := cl.Store.Write(ctx, func(tx *sql.Tx) error {
		_, cur, err := cl.item(ctx, tx, id)
		if err != nil {
			return err
		}
		was = forgotten(cur)
		copies, err = store.ClaimCopies(ctx, tx, id)
		return err
	})
	if err != nil {
		return err
	}
	// The copies come first, before anything is erased; forget keeps them (§6.7).
	for _, c := range copies {
		if _, err := fmt.Fprintf(cl.Stdout, "kept: item %d, a copy of the same claim (%s)\n", c.ID, cl.where(c)); err != nil {
			return err
		}
	}
	out := filepath.Join(cl.Binding.Store, outDir)
	if !was {
		if err := output.Forget(ctx, cl.Store, out, id, func(tx *sql.Tx) error { return cl.erase(ctx, tx, id) }); err != nil {
			return err
		}
		_, err := fmt.Fprintf(cl.Stdout, "item %d: its text is removed from the store\n", id)
		// The build runs even when the message fails: the forget is committed (E4 post-commit
		// build).
		if err := errors.Join(err, cl.deliver(ctx, nil)); err != nil {
			return err
		}
		if err := cl.clean(ctx, out, id); err != nil {
			return err
		}
	}
	return cl.report(ctx, out, id)
}

// erase is the forget transaction of item id: a retired revision with no text raises the
// output generation, the text of the item and its derivatives goes from every text field, and
// a tombstone with the claim id stays (§6.7).
func (cl caller) erase(ctx context.Context, tx *sql.Tx, id int64) error {
	it, cur, err := cl.item(ctx, tx, id)
	if err != nil {
		return err
	}
	next := nextRevision(cur, cl.Now())
	next.Text, next.State = "", store.StateRetired
	if _, err := store.WriteRevision(ctx, tx, it.CurrentRevision, next); err != nil {
		return err
	}
	if err := store.EraseItem(ctx, tx, id, cl.Now()); err != nil {
		return err
	}
	// M1 has no evidence, so the tombstone has no source keys yet.
	return store.InsertTombstone(ctx, tx, store.Tombstone{ClaimID: it.ClaimID, SourceKeys: []string{}, ForgottenAt: cl.Now()})
}

// clean waits until no hook can still read the generations that hold item id, and then
// repairs the output, which deletes them (§7.5 step 8). A conflict stops the repair; report
// names it.
func (cl caller) clean(ctx context.Context, out string, id int64) error {
	holds, err := output.Holds(out, id)
	if err != nil || !holds {
		return err
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(settle):
	}
	err = output.Repair(ctx, cl.Store, out, cl.Now)
	var ce *output.ConflictError
	if errors.As(err, &ce) {
		return nil
	}
	return err
}

// report prints "done" when no store field and no output folder holds the text of item id,
// and "pending" with what still holds it otherwise (§6.7). A copy in another scope does not
// count.
func (cl caller) report(ctx context.Context, out string, id int64) error {
	var left []store.Field
	err := cl.Store.Write(ctx, func(tx *sql.Tx) (err error) {
		left, err = store.ItemTextLeft(ctx, tx, id)
		return err
	})
	if err != nil {
		return err
	}
	holds, err := output.Holds(out, id)
	if err != nil {
		return err
	}
	var why []string
	for _, f := range left {
		why = append(why, f.Table+"."+f.Column)
	}
	if holds {
		why = append(why, "old output generations")
	}
	if len(why) == 0 {
		_, err := fmt.Fprintf(cl.Stdout, "forget item %d: done\n", id)
		return err
	}
	if _, err := fmt.Fprintf(cl.Stdout, "forget item %d: pending: %s still hold its text; the hook withholds the brief until then\n",
		id, strings.Join(why, ", ")); err != nil {
		return err
	}
	cs, err := output.Conflicts(out)
	if err != nil || len(cs) == 0 {
		_, werr := fmt.Fprintln(cl.Stdout, "the next brain call deletes them; run brain forget again to check")
		return errors.Join(err, werr)
	}
	return printConflicts(cl.Stdout, cs, "conflict: %v\n", "brain repair --confirm deletes them\n")
}

// where names the scope of item it for a list, without the name of a workspace that the
// caller does not see.
func (cl caller) where(it store.Item) string {
	if cl.sees(it.WorkspaceKey) {
		return cl.target(it.Scope)
	}
	return "another workspace"
}
