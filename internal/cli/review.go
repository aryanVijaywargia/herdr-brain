package cli

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/aryanVijaywargia/herdr-brain/internal/secret"
	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

const reviewUsage = `usage:
  brain review                                   list the proposals that wait for you
  brain review <id>                              show one proposal
  brain review <id> --approve [--edit "<text>"] [--scope global|workspace] [--pin] [--confirm]
  brain review <id> --reject [--reason "<text>"] | --later
  brain review --retired                         list the retired items
  brain review --all                             list the candidates too, below the gate as well
  brain review --rejected                        list the rejections, with the new evidence since each
  brain review --reopen <rejection>              end a rejection: its claim can get proposals again
  brain review --reset <rejection>               keep a rejection, and count its new evidence from now
  brain review --restore <item>                  make a retired item active again
  brain review --item <item> --edit "<text>" | --retire   propose a change to an active item
  brain review --item <item> --exception-to <global item> | --conflicts-with <global item>
                                                 link a workspace item to a global item`

// review is the parsed arguments of `brain review`. Each action has one command-line form, so
// the review pane (E5), the plugin popup and E10 run the same code.
type review struct {
	id                     int64 // the proposal; 0 for none
	approve, reject, later bool
	edit                   *string // the new text; nil for none
	reason                 *string // your reason for a reject; nil for none
	scope                  store.Scope
	pin, confirm           bool
	retired                bool
	all                    bool  // list the candidates (E12-S2)
	rejected               bool  // list the rejections (E12-S3)
	reopen, reset          int64 // the rejection to reopen or reset
	restore                int64 // the item to restore
	item                   int64 // the active item to change
	retire                 bool
	exceptionTo            int64 // the global item that item is an exception to
	conflictsWith          int64 // the global item that item conflicts with
}

// Review runs `brain review` with args after "review" (§7.4, E4-S3).
func Review(ctx context.Context, e Env, args []string) error {
	return runReview(ctx, e, store.SurfaceCLI, args)
}

// runReview runs `brain review` args for surface, the one that records a decision.
func runReview(ctx context.Context, e Env, surface store.Surface, args []string) (err error) {
	// As in remember, the filter runs on the raw arguments: an --edit text with a secret
	// writes nothing, and no parse error prints it.
	if err := secret.Check(strings.Join(args, " ")); err != nil {
		return err
	}
	r, err := parseReview(args)
	if err != nil {
		return err
	}
	c, err := e.Open(ctx)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, c.Close()) }()
	cl := newCaller(e, c)
	cl.surface = surface
	switch {
	case r.exceptionTo != 0 || r.conflictsWith != 0:
		return cl.link(ctx, r)
	case r.item != 0:
		return cl.proposeChange(ctx, r)
	case r.retired:
		return cl.listRetired(ctx)
	case r.all:
		return cl.listCandidates(ctx)
	case r.rejected:
		return cl.listRejections(ctx)
	case r.reopen != 0:
		return cl.endRejection(ctx, r.reopen, true)
	case r.reset != 0:
		return cl.endRejection(ctx, r.reset, false)
	case r.restore != 0:
		return cl.restore(ctx, r.restore)
	case r.approve || r.reject || r.later:
		if err := cl.decide(ctx, r); err != nil {
			return err
		}
		return cl.closeReviewPanes(ctx, r.id)
	case r.id != 0:
		return cl.show(ctx, r.id)
	}
	return cl.list(ctx)
}

func parseReview(args []string) (review, error) {
	var r review
	fs := flag.NewFlagSet("review", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.BoolVar(&r.approve, "approve", false, "")
	fs.BoolVar(&r.reject, "reject", false, "")
	fs.BoolVar(&r.later, "later", false, "")
	fs.Func("edit", "", func(s string) error { r.edit = &s; return nil })
	fs.Func("reason", "", func(s string) error { r.reason = &s; return nil })
	scope := fs.String("scope", "", "")
	fs.BoolVar(&r.pin, "pin", false, "")
	fs.BoolVar(&r.confirm, "confirm", false, "")
	fs.BoolVar(&r.retired, "retired", false, "")
	fs.BoolVar(&r.all, "all", false, "")
	fs.BoolVar(&r.rejected, "rejected", false, "")
	fs.Int64Var(&r.reopen, "reopen", 0, "")
	fs.Int64Var(&r.reset, "reset", 0, "")
	fs.Int64Var(&r.restore, "restore", 0, "")
	fs.Int64Var(&r.item, "item", 0, "")
	fs.BoolVar(&r.retire, "retire", false, "")
	fs.Int64Var(&r.exceptionTo, "exception-to", 0, "")
	fs.Int64Var(&r.conflictsWith, "conflicts-with", 0, "")
	// The id can come before the flags, as in `brain review 3 --approve`.
	var ids []string
	for {
		if err := fs.Parse(args); err != nil {
			return review{}, fmt.Errorf("%w\n%s", err, reviewUsage)
		}
		if fs.NArg() == 0 {
			break
		}
		ids = append(ids, fs.Arg(0))
		args = fs.Args()[1:]
	}
	r.scope = store.Scope(*scope)
	if len(ids) > 1 {
		return review{}, fmt.Errorf("want at most one proposal id, got %d\n%s", len(ids), reviewUsage)
	}
	if len(ids) == 1 {
		id, err := strconv.ParseInt(ids[0], 10, 64)
		if err != nil || id <= 0 {
			return review{}, fmt.Errorf("bad proposal id %q\n%s", ids[0], reviewUsage)
		}
		r.id = id
	}
	if err := r.check(); err != nil {
		return review{}, fmt.Errorf("%w\n%s", err, reviewUsage)
	}
	return r, nil
}

// check refuses a mix of flags that has no single meaning.
func (r review) check() error {
	actions := 0
	for _, on := range []bool{r.approve, r.reject, r.later, r.retired, r.all, r.rejected, r.reopen != 0, r.reset != 0,
		r.restore != 0, r.item != 0} {
		if on {
			actions++
		}
	}
	modifiers := r.scope != "" || r.pin || r.confirm
	switch {
	case actions > 1:
		return errors.New("give one action")
	case r.reason != nil && !r.reject:
		return errors.New("--reason goes with --reject")
	case r.scope != "" && r.scope != store.ScopeGlobal && r.scope != store.ScopeWorkspace:
		return fmt.Errorf("unknown scope %q", r.scope)
	case r.edit != nil && strings.TrimSpace(*r.edit) == "":
		return errors.New("the --edit text is empty")
	case r.item != 0:
		changes := 0
		for _, on := range []bool{r.edit != nil, r.retire, r.exceptionTo != 0, r.conflictsWith != 0} {
			if on {
				changes++
			}
		}
		if r.id != 0 || modifiers || changes != 1 {
			return errors.New("--item takes one of --edit, --retire, --exception-to or --conflicts-with, and no proposal id")
		}
	case r.retire || r.exceptionTo != 0 || r.conflictsWith != 0:
		return errors.New("--retire, --exception-to and --conflicts-with need --item")
	case r.approve:
		if r.id == 0 {
			return errors.New("--approve needs a proposal id")
		}
	case modifiers || r.edit != nil:
		return errors.New("--edit, --scope, --pin and --confirm go with --approve")
	case (r.reject || r.later) && r.id == 0:
		return errors.New("--reject and --later need a proposal id")
	case (r.retired || r.all || r.rejected || r.reopen != 0 || r.reset != 0 || r.restore != 0) && r.id != 0:
		return errors.New("--retired, --all, --rejected, --reopen, --reset and --restore take no proposal id")
	}
	return nil
}

// list prints the pending proposals that the caller sees. A proposal of a candidate below its
// gate waits in brain review --all only (§7.2).
func (cl caller) list(ctx context.Context) error {
	gates := store.DefaultGates()
	var lines []string
	err := cl.Store.Write(ctx, func(tx *sql.Tx) error {
		ids, err := store.PendingProposalIDs(ctx, tx)
		if err != nil {
			return err
		}
		if lines, err = cl.staleLinks(ctx, tx); err != nil {
			return err
		}
		for _, id := range ids {
			p, it, _, err := cl.proposal(ctx, tx, id)
			if errors.Is(err, errNotFound) {
				continue
			}
			if err != nil {
				return err
			}
			below, err := belowGate(ctx, tx, gates, p)
			if err != nil {
				return err
			}
			if below {
				continue
			}
			lines = append(lines, fmt.Sprintf("%d  %s  %s%s", p.ID, p.Target, summary(p, it), warningMark(p)))
		}
		return nil
	})
	if err != nil {
		return err
	}
	if len(lines) == 0 {
		_, err = fmt.Fprintln(cl.Stdout, "no proposals wait for your review")
		return err
	}
	_, err = fmt.Fprintf(cl.Stdout, "%s\n\nshow one: brain review <id>\n", strings.Join(lines, "\n"))
	return err
}

// belowGate reports whether p is a proposal of a candidate below the gate of its kind. A
// proposal whose candidate is gone (a forget took its evidence) is in the queue.
func belowGate(ctx context.Context, tx *sql.Tx, gates store.Gates, p store.Proposal) (bool, error) {
	if p.CandidateID == nil {
		return false, nil
	}
	c, err := store.CandidateByID(ctx, tx, *p.CandidateID)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	case err != nil:
		return false, err
	}
	return !gates.Reached(c), nil
}

// summary is the one-line form of p for a list. A proposal with no text retires its item.
func summary(p store.Proposal, it store.Item) string {
	if p.Text == "" {
		return fmt.Sprintf("retire item %d", it.ID)
	}
	return oneLine(p.Text)
}

func warningMark(p store.Proposal) string {
	if warns(p) {
		return "  [permission warning]"
	}
	return ""
}

func warns(p store.Proposal) bool { return p.PermissionWarning || permissionWords.MatchString(p.Text) }

// show prints one proposal: text, scope, target, diff, revision and source (§7.4).
func (cl caller) show(ctx context.Context, id int64) error {
	var p store.Proposal
	var it store.Item
	var cur store.ItemRevision
	// The review pane shows a pending proposal only: it closes on one that another surface
	// decided (E5-S1).
	read := cl.proposal
	if cl.surface == store.SurfacePane {
		read = cl.pending
	}
	err := cl.Store.Write(ctx, func(tx *sql.Tx) (err error) {
		p, it, cur, err = read(ctx, tx, id)
		return err
	})
	if err != nil {
		return err
	}
	text := p.Text
	if text == "" {
		text = "(none: this proposal retires the item)"
	}
	source := fmt.Sprintf("%s, sender unconfirmed", cur.Origin)
	if it.State != store.StateDraft {
		source = fmt.Sprintf("a change to %s item %d, asked in review", it.State, it.ID)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "proposal %d (%s)\ntext:     %s\nscope:    %s\ntarget:   %s\n", p.ID, p.Status, text, p.Scope, p.Target)
	fmt.Fprintf(&b, "revision: item %d revision %d, from revision %d (now %d)\n", it.ID, *p.BaseRevision+1, *p.BaseRevision, it.CurrentRevision)
	fmt.Fprintf(&b, "diff:\n  %s\nsource:   %s, %s\n", strings.ReplaceAll(p.Diff, "\n", "\n  "), source, p.Created.Format("2006-01-02 15:04"))
	if warns(p) {
		b.WriteString("warning:  this may change a permission, an allow rule or a safety rule; approve needs --confirm\n")
	}
	if p.Status == store.ProposalPending && cl.surface != store.SurfacePane {
		fmt.Fprintf(&b, "actions:  brain review %d --approve [--edit \"<text>\"] [--scope global|workspace] [--pin] | --reject | --later\n", p.ID)
	}
	_, err = io.WriteString(cl.Stdout, b.String())
	return err
}

// decide runs the approve, reject or later of r.
func (cl caller) decide(ctx context.Context, r review) error {
	switch {
	case r.approve:
		return cl.approve(ctx, r)
	case r.reject:
		reason := ""
		if r.reason != nil {
			reason = *r.reason
		}
		return cl.reject(ctx, r.id, reason)
	}
	return cl.later(ctx, r.id)
}

// approve makes the proposal's change the next revision of its item, with the edits of r
// (§7.4). The proposal's base revision must still be the item's current one.
func (cl caller) approve(ctx context.Context, r review) error {
	var next store.ItemRevision
	err := cl.Store.Write(ctx, func(tx *sql.Tx) error {
		p, it, cur, err := cl.pending(ctx, tx, r.id)
		if err != nil {
			return err
		}
		if it.CurrentRevision != *p.BaseRevision {
			return fmt.Errorf("item %d changed after proposal %d was made (the item is at revision %d, the proposal is for revision %d); nothing was written: reject it and make a new one",
				it.ID, p.ID, it.CurrentRevision, *p.BaseRevision)
		}
		if next, err = cl.approved(r, p, it, cur); err != nil {
			return err
		}
		if err := store.DecideProposal(ctx, tx, p.ID, store.ProposalApproved, cl.surface, cl.Now()); err != nil {
			return err
		}
		_, err = store.WriteRevision(ctx, tx, *p.BaseRevision, next)
		return err
	})
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(cl.Stdout, "proposal %d approved: item %d is %s at revision %d (%s)\n",
		r.id, next.ItemID, next.State, next.Revision, cl.target(next.Scope))
	// The build runs even when the message fails: the commit is done (§7.5, as in undo).
	return errors.Join(err, cl.deliver(ctx, &r.id))
}

// approved is the revision that approving p with the edits of r writes.
func (cl caller) approved(r review, p store.Proposal, it store.Item, cur store.ItemRevision) (store.ItemRevision, error) {
	next := nextRevision(cur, cl.Now())
	next.Revision, next.ProposalID = *p.BaseRevision+1, &p.ID
	if p.Text == "" {
		if r.edit != nil || r.scope != "" || r.pin {
			return store.ItemRevision{}, fmt.Errorf("proposal %d retires item %d: approve it as it is, or reject it", p.ID, it.ID)
		}
		// A retired revision keeps its text, so that restore can bring it back (§9.2).
		next.State = store.StateRetired
		return next, confirmed(r, p, next)
	}
	next.Text, next.State, next.Pinned = p.Text, store.StateActive, cur.Pinned || r.pin
	if r.edit != nil {
		next.Text = *r.edit
	}
	switch r.scope {
	case store.ScopeGlobal:
		if it.Kind == store.KindHandoff {
			return store.ItemRevision{}, errors.New("a handoff is a workspace item; it cannot go global")
		}
		// An exception link is for a workspace item only (§7.4); undo of the promotion brings it back.
		next.Scope, next.WorkspaceKey, next.ExceptionToItem, next.ExceptionToRevision = store.ScopeGlobal, nil, nil, nil
	case store.ScopeWorkspace:
		// The new target is the caller's own workspace, so it passes the scope check.
		if cl.keyErr != nil {
			return store.ItemRevision{}, cl.keyErr
		}
		next.Scope, next.WorkspaceKey = store.ScopeWorkspace, &cl.key
	}
	return next, confirmed(r, p, next)
}

// errConfirm: approve needs --confirm.
var errConfirm = errors.New("may change a permission, an allow rule or a safety rule")

// confirmed checks that you gave --confirm when approving p may change a permission, an allow
// rule or a safety rule (§7.4). The revision's text counts too, as an --edit can add a keyword.
func confirmed(r review, p store.Proposal, next store.ItemRevision) error {
	if (p.PermissionWarning || permissionWords.MatchString(next.Text)) && !r.confirm {
		return fmt.Errorf("proposal %d %w: read it with brain review %d, then approve with --confirm; nothing was written",
			p.ID, errConfirm, p.ID)
	}
	return nil
}

// reject marks the proposal rejected. The item does not change. The claim of the proposal's
// candidate stays rejected in the scope of the candidate, with reason (§7.2).
func (cl caller) reject(ctx context.Context, id int64, reason string) error {
	var c *store.Candidate
	var rejection int64
	err := cl.Store.Write(ctx, func(tx *sql.Tx) error {
		p, _, _, err := cl.pending(ctx, tx, id)
		if err != nil {
			return err
		}
		if c, err = cl.candidate(ctx, tx, p); err != nil {
			return err
		}
		if c != nil {
			if rejection, err = store.Reject(ctx, tx, *c, reason, cl.Now()); err != nil {
				return err
			}
		}
		return store.DecideProposal(ctx, tx, id, store.ProposalRejected, cl.surface, cl.Now())
	})
	if err != nil {
		return err
	}
	if c == nil {
		_, err = fmt.Fprintf(cl.Stdout, "proposal %d rejected\n", id)
		return err
	}
	_, err = fmt.Fprintf(cl.Stdout, "proposal %d rejected: claim %s stays rejected in %s (rejection %d, brain review --rejected)\n",
		id, c.ClaimID, cl.target(c.Scope), rejection)
	return err
}

// candidate reads the candidate of proposal p, when the caller sees it: nil for a proposal with
// no candidate, or whose candidate is gone (a forget took its evidence).
func (cl caller) candidate(ctx context.Context, tx *sql.Tx, p store.Proposal) (*store.Candidate, error) {
	if p.CandidateID == nil {
		return nil, nil
	}
	c, err := store.CandidateByID(ctx, tx, *p.CandidateID)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, nil
	case err != nil:
		return nil, err
	case !cl.sees(scopeKey(c.Scope, c.WorkspaceKey)):
		return nil, fmt.Errorf("proposal %d: %w", p.ID, errNotFound)
	}
	return &c, nil
}

// scopeKey is the workspace key of a row with scope and key, as sees takes it: nil for global.
func scopeKey(scope store.Scope, key string) *string {
	if scope == store.ScopeGlobal {
		return nil
	}
	return &key
}

// later leaves the proposal pending: it only mutes it for this review (§7.4, §11 proposals).
func (cl caller) later(ctx context.Context, id int64) error {
	err := cl.Store.Write(ctx, func(tx *sql.Tx) error {
		_, _, _, err := cl.pending(ctx, tx, id)
		return err
	})
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(cl.Stdout, "proposal %d stays in the queue (brain review)\n", id)
	return err
}

// listRetired prints the retired items that the caller sees (§9.2).
func (cl caller) listRetired(ctx context.Context) error {
	var lines []string
	err := cl.Store.Write(ctx, func(tx *sql.Tx) error {
		ids, err := store.ItemIDsInState(ctx, tx, store.StateRetired)
		if err != nil {
			return err
		}
		for _, id := range ids {
			_, cur, err := cl.item(ctx, tx, id)
			if errors.Is(err, errNotFound) {
				continue
			}
			if err != nil {
				return err
			}
			lines = append(lines, fmt.Sprintf("item %d  %s  %s", id, cl.target(cur.Scope), oneLine(cur.Text)))
		}
		return nil
	})
	if err != nil {
		return err
	}
	if len(lines) == 0 {
		_, err = fmt.Fprintln(cl.Stdout, "no retired items")
		return err
	}
	_, err = fmt.Fprintf(cl.Stdout, "%s\n\nrestore one: brain review --restore <item>\n", strings.Join(lines, "\n"))
	return err
}

// listCandidates prints the candidates that the caller sees, the ones below their gate too
// (§7.2). It changes nothing: a candidate becomes an item only through a proposal that you
// approve.
func (cl caller) listCandidates(ctx context.Context) error {
	gates := store.DefaultGates()
	var lines []string
	err := cl.Store.Write(ctx, func(tx *sql.Tx) error {
		cs, err := store.Candidates(ctx, tx)
		if err != nil {
			return err
		}
		for _, c := range cs {
			if !cl.sees(scopeKey(c.Scope, c.WorkspaceKey)) {
				continue
			}
			text, err := claimText(ctx, tx, c.ClaimID)
			if err != nil {
				return err
			}
			lines = append(lines, fmt.Sprintf("candidate %d  %s  %s  %d occurrences in %d sessions  %s  last seen %s  %s  %s\n  claim %s: %s",
				c.ID, cl.target(c.Scope), c.Kind, c.Count, c.Sessions, strings.Join(c.Harnesses, ", "),
				c.LastSeen.Format("2006-01-02 15:04"), gateLabel(gates, c), c.Status, c.ClaimID, text))
			ps, err := cl.candidateProposals(ctx, tx, c.ID)
			if err != nil {
				return err
			}
			lines = append(lines, ps...)
		}
		return nil
	})
	if err != nil {
		return err
	}
	if len(lines) == 0 {
		_, err = fmt.Fprintln(cl.Stdout, "no candidates")
		return err
	}
	_, err = fmt.Fprintf(cl.Stdout, "%s\n\nread only: a candidate becomes an item only through a proposal that you approve\n", strings.Join(lines, "\n"))
	return err
}

// candidateProposals are the lines of the pending proposals of candidate id that the caller
// sees. You can approve one below the gate: a rare, costly lesson after one occurrence (§7.2).
// Approve is the E4 approve, with its revision check, permission warning and scope check.
func (cl caller) candidateProposals(ctx context.Context, tx *sql.Tx, id int64) ([]string, error) {
	ids, err := store.PendingCandidateProposalIDs(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	var lines []string
	for _, pid := range ids {
		p, it, _, err := cl.proposal(ctx, tx, pid)
		if errors.Is(err, errNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		lines = append(lines, fmt.Sprintf("  proposal %d: %s%s (brain review %d --approve)", p.ID, summary(p, it), warningMark(p), p.ID))
	}
	return lines, nil
}

// claimText is the one-line claim key text of claim id.
func claimText(ctx context.Context, tx *sql.Tx, id string) (string, error) {
	claim, err := store.ClaimByID(ctx, tx, id)
	if err != nil {
		return "", err
	}
	if claim.Key == (store.ClaimKey{}) {
		return "(text forgotten)", nil
	}
	return oneLine(claim.Key.Situation + ": " + claim.Key.Rule), nil
}

// listRejections prints the rejections that the caller sees, with the new evidence since each
// (§7.2). Brain never reopens one by itself.
func (cl caller) listRejections(ctx context.Context) error {
	var lines []string
	err := cl.Store.Write(ctx, func(tx *sql.Tx) error {
		rs, err := store.Rejections(ctx, tx)
		if err != nil {
			return err
		}
		for _, r := range rs {
			if !cl.sees(scopeKey(r.Scope, r.WorkspaceKey)) {
				continue
			}
			text, err := claimText(ctx, tx, r.ClaimID)
			if err != nil {
				return err
			}
			reason := oneLine(r.Reason)
			if reason == "" {
				reason = "(none)"
			}
			lines = append(lines, fmt.Sprintf("rejection %d  %s  rejected %s  %d new evidence since (%d now, %d then)\n  claim %s: %s\n  reason: %s",
				r.ID, cl.target(r.Scope), r.At.Format("2006-01-02 15:04"), r.New(), r.Evidence, r.EvidenceCount, r.ClaimID, text, reason))
		}
		return nil
	})
	if err != nil {
		return err
	}
	if len(lines) == 0 {
		_, err = fmt.Fprintln(cl.Stdout, "no rejections")
		return err
	}
	_, err = fmt.Fprintf(cl.Stdout, "%s\n\nreopen one: brain review --reopen <rejection>; count its new evidence from now: brain review --reset <rejection>\n",
		strings.Join(lines, "\n"))
	return err
}

// endRejection reopens rejection id, or resets it: it then counts its new evidence from now.
// [NEW] the actions --reopen and --reset (§7.2 "you reopen or reset it").
func (cl caller) endRejection(ctx context.Context, id int64, reopen bool) error {
	var r store.Rejection
	err := cl.Store.Write(ctx, func(tx *sql.Tx) (err error) {
		r, err = store.RejectionByID(ctx, tx, id)
		switch {
		case errors.Is(err, sql.ErrNoRows) || err == nil && !cl.sees(scopeKey(r.Scope, r.WorkspaceKey)):
			return fmt.Errorf("rejection %d: %w", id, errNotFound)
		case err != nil:
			return err
		case !r.Reopened.IsZero():
			return fmt.Errorf("rejection %d: %w: it is reopened already", id, errDecided)
		case reopen:
			return store.ReopenRejection(ctx, tx, id, cl.Now())
		}
		return store.ResetRejection(ctx, tx, id, cl.Now())
	})
	if err != nil {
		return err
	}
	if reopen {
		_, err = fmt.Fprintf(cl.Stdout, "rejection %d reopened: claim %s can get proposals in %s again\n", id, r.ClaimID, cl.target(r.Scope))
		return err
	}
	_, err = fmt.Fprintf(cl.Stdout, "rejection %d reset: claim %s stays rejected, and its new evidence counts from now\n", id, r.ClaimID)
	return err
}

// gateLabel says whether c reached the gate of its kind, and how far it is when not.
func gateLabel(g store.Gates, c store.Candidate) string {
	if g.Reached(c) {
		return "gate reached"
	}
	unit := "occurrences"
	if g[c.Kind].Sessions {
		unit = "sessions"
	}
	return fmt.Sprintf("below gate (%d of %d %s)", g.Support(c), g[c.Kind].Min, unit)
}

// restore makes a retired item active again with a new revision (§9.2).
func (cl caller) restore(ctx context.Context, id int64) error {
	var rev int64
	err := cl.Store.Write(ctx, func(tx *sql.Tx) error {
		it, cur, err := cl.item(ctx, tx, id)
		if err != nil {
			return err
		}
		if it.State != store.StateRetired {
			return fmt.Errorf("item %d is %s, not retired", id, it.State)
		}
		next := nextRevision(cur, cl.Now())
		next.State = store.StateActive
		rev = it.CurrentRevision + 1
		_, err = store.WriteRevision(ctx, tx, it.CurrentRevision, next)
		return err
	})
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(cl.Stdout, "item %d restored at revision %d\n", id, rev)
	return errors.Join(err, cl.deliver(ctx, nil))
}

// proposeChange writes a proposal to edit or retire an active item. The item's current
// revision stays live until you approve the proposal (E4-S3 "change an active item").
func (cl caller) proposeChange(ctx context.Context, r review) error {
	var pid int64
	err := cl.Store.Write(ctx, func(tx *sql.Tx) error {
		it, cur, err := cl.item(ctx, tx, r.item)
		if err != nil {
			return err
		}
		if it.State != store.StateActive {
			return fmt.Errorf("item %d is %s; --edit and --retire change an active item", it.ID, it.State)
		}
		base := it.CurrentRevision
		// [NEW] a proposal with no text retires its item; its diff removes the text.
		p := store.Proposal{ItemID: &it.ID, BaseRevision: &base, Scope: cur.Scope, Target: cl.target(cur.Scope),
			Diff: "- " + cur.Text, Status: store.ProposalPending, Created: cl.Now()}
		if r.edit != nil {
			p.Text, p.Diff = *r.edit, p.Diff+"\n+ "+*r.edit
		}
		// The diff holds the old text too: a retire or an edit that removes a safety rule
		// changes it (§7.4).
		p.PermissionWarning = permissionWords.MatchString(p.Diff)
		pid, err = store.InsertProposal(ctx, tx, p)
		return err
	})
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(cl.Stdout, "proposal %d: a change to item %d, waiting for your review (brain review %d)\n", pid, r.item, pid)
	return err
}
