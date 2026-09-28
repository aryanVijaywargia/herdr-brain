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

const pinUsage = `usage: brain pin [--off] <item>`

// Pin runs `brain pin` with args after "pin" (E4-S3). It pins an active item, or unpins it
// with --off, in a new revision. A pinned item never ages (§9.2).
func Pin(ctx context.Context, e Env, args []string) (err error) {
	fs := flag.NewFlagSet("pin", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	off := fs.Bool("off", false, "")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("%w\n%s", err, pinUsage)
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("want one item id, got %d\n%s", fs.NArg(), pinUsage)
	}
	id, err := strconv.ParseInt(fs.Arg(0), 10, 64)
	if err != nil || id <= 0 {
		return fmt.Errorf("bad item id %q\n%s", fs.Arg(0), pinUsage)
	}
	c, err := e.Open(ctx)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, c.Close()) }()
	return newCaller(e, c).pin(ctx, id, !*off)
}

// pin writes a revision of item id with pinned set to on. An item that is already so is left
// as it is.
func (cl caller) pin(ctx context.Context, id int64, on bool) error {
	word := map[bool]string{true: "pinned", false: "not pinned"}[on]
	var rev int64
	err := cl.Store.Write(ctx, func(tx *sql.Tx) error {
		it, cur, err := cl.item(ctx, tx, id)
		if err != nil {
			return err
		}
		if it.State != store.StateActive {
			return fmt.Errorf("item %d is %s; only an active item can be pinned", id, it.State)
		}
		if it.Pinned == on {
			return nil
		}
		next := nextRevision(cur, cl.Now())
		next.Pinned = on
		rev = it.CurrentRevision + 1
		_, err = store.WriteRevision(ctx, tx, it.CurrentRevision, next)
		return err
	})
	if err != nil {
		return err
	}
	if rev == 0 {
		_, err = fmt.Fprintf(cl.Stdout, "item %d is %s already\n", id, word)
		return err
	}
	_, err = fmt.Fprintf(cl.Stdout, "item %d is %s at revision %d\n", id, word, rev)
	return errors.Join(err, cl.deliver(ctx, nil))
}
