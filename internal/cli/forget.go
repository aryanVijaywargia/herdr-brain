package cli

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/aryanVijaywargia/herdr-brain/internal/output"
	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

const forgetUsage = `usage: brain forget <item> | --session <id>`

// settle is how long forget waits after its build before it deletes the old generations: a
// generation that the pointer left less than one hook time cap ago (300 ms, §8.3) may still
// have a reader (§7.5 step 8). [NEW] 350 ms, the cap and a margin.
const settle = 350 * time.Millisecond

// Forget runs `brain forget` with args after "forget" (§6.7, E4-S6). It removes the text of
// one item and its derivatives from the store and the output, and keeps a tombstone with no
// text. A second call on the same item only reports whether forget is done.
func Forget(ctx context.Context, e Env, args []string) error {
	for _, a := range args {
		if a == "--session" || a == "-session" || strings.HasPrefix(a, "--session=") || strings.HasPrefix(a, "-session=") {
			return forgetSession(ctx, e, args)
		}
	}
	return onItem(ctx, e, "forget", forgetUsage, args, caller.forget)
}

// forgetSession runs `brain forget --session <id>` (§6.7, E11-S6). id is a capture session id
// or an agent session id in the caller's herdr session. It lists the evidence of the session
// and the candidates that use it first, then deletes its observations, command records and
// evidence links in one transaction that raises the capture generation, so a pane read or
// hook record that collected the text before the forget stores nothing. The candidates are
// counted again. New text after the forget is captured as usual.
func forgetSession(ctx context.Context, e Env, args []string) (err error) {
	fs := flag.NewFlagSet("forget", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	id := fs.String("session", "", "")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("%w\n%s", err, forgetUsage)
	}
	if *id == "" || fs.NArg() != 0 {
		return fmt.Errorf("want one session id and no item\n%s", forgetUsage)
	}
	c, err := e.Open(ctx)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, c.Close()) }()
	var ss []store.Session
	var ev []store.Evidence
	var cs []store.Candidate
	err = c.Store.Write(ctx, func(tx *sql.Tx) (err error) {
		if ss, err = store.SessionsByID(ctx, tx, c.Binding.Session, *id); err != nil {
			return err
		}
		ev = make([]store.Evidence, len(ss))
		for i, s := range ss {
			if ev[i], err = store.SessionEvidence(ctx, tx, s.ID); err != nil {
				return err
			}
		}
		cs, err = newCandidates(ctx, tx, ss, nil)
		return err
	})
	if err != nil {
		return err
	}
	if len(ss) == 0 {
		return fmt.Errorf("no capture session %q in herdr session %s", *id, c.Binding.Session)
	}
	// The list comes first, before anything is deleted (§6.7).
	w := e.Stdout
	for i, s := range ss {
		agent := s.AgentSessionID
		if s.Key.Kind == store.KeyAgent {
			agent = s.Key.ID
		}
		if _, err := fmt.Fprintf(w, "session %d: %s in pane %s, %s id %s, agent session %q: %d observations, %d command records, %d evidence links\n",
			s.ID, s.Key.Harness, s.PaneID, s.Key.Kind, s.Key.ID, agent, ev[i].Observations, ev[i].CommandRecords, ev[i].EvidenceLinks); err != nil {
			return err
		}
	}
	if err := listCandidates(w, cs); err != nil {
		return err
	}
	// E13 (extraction batches) adds the items and proposals that use this evidence, and the
	// choice to keep them or forget them too; E14 adds the prompt index rows.
	if _, err := fmt.Fprintf(w, "%d candidates use this evidence; no item uses capture evidence yet: nothing else to keep or forget\n", len(cs)); err != nil {
		return err
	}
	err = c.Store.Write(ctx, func(tx *sql.Tx) error {
		// Evidence can link to a new candidate after the list: that candidate is listed too,
		// still before the delete.
		late, err := newCandidates(ctx, tx, ss, cs)
		if err != nil {
			return err
		}
		if err := listCandidates(w, late); err != nil {
			return err
		}
		for _, s := range ss {
			if err := store.ForgetSession(ctx, tx, s.ID); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "forget session %s: done; its text is deleted, and new text is captured as usual\n", *id)
	return err
}

// listCandidates prints the candidates that use the evidence of a session to forget.
func listCandidates(w io.Writer, cs []store.Candidate) error {
	for _, c := range cs {
		if _, err := fmt.Fprintf(w, "candidate %d: %s %s of claim %s, %d occurrences in %d sessions: its links to this evidence go, and it is counted again\n",
			c.ID, c.Scope, c.Kind, c.ClaimID, c.Count, c.Sessions); err != nil {
			return err
		}
	}
	return nil
}

// newCandidates returns the candidates with evidence from sessions ss that are not in listed,
// each once.
func newCandidates(ctx context.Context, tx *sql.Tx, ss []store.Session, listed []store.Candidate) ([]store.Candidate, error) {
	var cs []store.Candidate
	for _, s := range ss {
		more, err := store.SessionCandidates(ctx, tx, s.ID)
		if err != nil {
			return nil, err
		}
		for _, m := range more {
			same := func(c store.Candidate) bool { return c.ID == m.ID }
			if !slices.ContainsFunc(listed, same) && !slices.ContainsFunc(cs, same) {
				cs = append(cs, m)
			}
		}
	}
	return cs, nil
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
	// The erased claim key gives the same text a new claim id, so the tombstone also holds the
	// capture identities of the evidence: the extractor does not bring it back (E13-S2).
	keys, err := store.ClaimSourceKeys(ctx, tx, it.ClaimID)
	if err != nil {
		return err
	}
	if err := store.EraseItem(ctx, tx, id, cl.Now()); err != nil {
		return err
	}
	return store.InsertTombstone(ctx, tx, store.Tombstone{ClaimID: it.ClaimID, SourceKeys: append([]string{}, keys...),
		ForgottenAt: cl.Now()})
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
