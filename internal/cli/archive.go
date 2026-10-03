package cli

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

const archiveUsage = `usage: brain archive [--all | --keep]`

// Archive runs `brain archive` with args after "archive" (E16-S7, §9.2). With no flag it
// lists the archive candidates that the caller sees (E4): the global ones and those of its
// workspace. --all retires all of them in one action; --keep keeps all of them and starts a
// new 90-day period. Archive never deletes: brain review --restore brings an item back. The
// items of a closed workspace are in no pane's view, so you act on them after a link (§9.4).
func Archive(ctx context.Context, e Env, args []string) (err error) {
	fs := flag.NewFlagSet("archive", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	all := fs.Bool("all", false, "")
	keep := fs.Bool("keep", false, "")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("%w\n%s", err, archiveUsage)
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected argument %q\n%s", fs.Arg(0), archiveUsage)
	}
	if *all && *keep {
		return fmt.Errorf("--all and --keep exclude each other\n%s", archiveUsage)
	}
	c, err := e.Open(ctx)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, c.Close()) }()
	cl := newCaller(e, c)
	if *all || *keep {
		return cl.archive(ctx, *all)
	}
	return cl.listArchive(ctx)
}

// archiveCandidates reads the archive candidates that the caller sees, the oldest first.
func (cl caller) archiveCandidates(ctx context.Context, tx *sql.Tx) ([]store.ArchiveCandidate, error) {
	cs, err := store.ArchiveCandidates(ctx, tx, cl.Now().Add(-store.ArchiveAge))
	if err != nil {
		return nil, err
	}
	var seen []store.ArchiveCandidate
	for _, c := range cs {
		if cl.sees(c.WorkspaceKey) {
			seen = append(seen, c)
		}
	}
	return seen, nil
}

// listArchive prints the archive candidates that the caller sees, with their text and idle
// time. It changes nothing.
func (cl caller) listArchive(ctx context.Context) error {
	var lines []string
	err := cl.Store.Write(ctx, func(tx *sql.Tx) error {
		cs, err := cl.archiveCandidates(ctx, tx)
		if err != nil {
			return err
		}
		for _, c := range cs {
			_, cur, err := cl.item(ctx, tx, c.ItemID)
			if err != nil {
				return err
			}
			lines = append(lines, fmt.Sprintf("item %d  %s  idle %s  %s", c.ItemID, cl.target(cur.Scope),
				age(cl.Now().Sub(c.Since)), oneLine(cur.Text)))
		}
		return nil
	})
	if err != nil {
		return err
	}
	if len(lines) == 0 {
		_, err = fmt.Fprintln(cl.Stdout, "no archive candidates")
		return err
	}
	_, err = fmt.Fprintf(cl.Stdout, "archive candidates: no relevant session for 90 days\n%s\n\n"+
		"retire all: brain archive --all; keep all for 90 more days: brain archive --keep\n", strings.Join(lines, "\n"))
	return err
}

// archive retires all the archive candidates that the caller sees, each with a new revision,
// or keeps them all, in one transaction.
func (cl caller) archive(ctx context.Context, retire bool) error {
	n := 0
	err := cl.Store.Write(ctx, func(tx *sql.Tx) error {
		cs, err := cl.archiveCandidates(ctx, tx)
		if err != nil {
			return err
		}
		n = len(cs)
		for _, c := range cs {
			if !retire {
				if err := store.KeepItem(ctx, tx, c.ItemID, cl.Now()); err != nil {
					return err
				}
				continue
			}
			it, cur, err := cl.item(ctx, tx, c.ItemID)
			if err != nil {
				return err
			}
			next := nextRevision(cur, cl.Now())
			next.State = store.StateRetired
			if _, err := store.WriteRevision(ctx, tx, it.CurrentRevision, next); err != nil {
				return err
			}
		}
		return nil
	})
	switch {
	case err != nil:
		return err
	case n == 0:
		_, err = fmt.Fprintln(cl.Stdout, "no archive candidates")
		return err
	case !retire:
		_, err = fmt.Fprintf(cl.Stdout, "kept %d items; a new 90-day period starts now\n", n)
		return err
	}
	_, err = fmt.Fprintf(cl.Stdout, "retired %d items; brain review --retired lists them, brain review --restore <item> brings one back\n", n)
	return errors.Join(err, cl.deliver(ctx, nil))
}
