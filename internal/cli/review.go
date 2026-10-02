package cli

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"

	"github.com/aryanVijaywargia/herdr-brain/internal/extract"
	"github.com/aryanVijaywargia/herdr-brain/internal/secret"
	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

const reviewUsage = `usage:
  brain review                                   list the proposals that wait for you
  brain review <id>                              show one proposal
  brain review <id> --approve [--edit "<text>"] [--scope global|workspace] [--pin] [--confirm]
                             [--replace | --exception | --keep-both]   for a proposal with a conflict
  brain review <id> --reject [--reason "<text>"] | --later
  brain review <id> --change "<text>"              ask for a new draft of an extractor proposal
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
	change                 *string // your change request (S56); nil for none
	conflict               string  // replace, exception or keep-both: what approve does with a conflict
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
	if err := r.secrets(); err != nil {
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
	case r.approve || r.reject || r.later || r.change != nil:
		if err := cl.decide(ctx, r); err != nil {
			return err
		}
		return cl.closeReviewPanes(ctx, r.id)
	case r.id != 0:
		return cl.show(ctx, r.id)
	}
	return cl.list(ctx)
}

// decisionFlags adds the text flags of a decision on one proposal to fs, and the approve
// choices for a conflict, of which at most one sets r.conflict.
func decisionFlags(fs *flag.FlagSet, r *review) {
	fs.Func("edit", "", func(s string) error { r.edit = &s; return nil })
	fs.Func("reason", "", func(s string) error { r.reason = &s; return nil })
	fs.Func("change", "", func(s string) error { r.change = &s; return nil })
	for _, c := range []string{conflictReplace, conflictException, conflictKeep} {
		fs.BoolFunc(c, "", func(string) error {
			if r.conflict != "" && r.conflict != c {
				return errors.New("give one of --replace, --exception and --keep-both")
			}
			r.conflict = c
			return nil
		})
	}
}

func parseReview(args []string) (review, error) {
	var r review
	fs := flag.NewFlagSet("review", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.BoolVar(&r.approve, "approve", false, "")
	fs.BoolVar(&r.reject, "reject", false, "")
	fs.BoolVar(&r.later, "later", false, "")
	decisionFlags(fs, &r)
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

// secrets runs the secret filter on each text of r alone too: a line pattern, such as an .env
// line, does not match after the id and the flag in the joined arguments.
func (r review) secrets() error {
	for _, t := range []*string{r.change, r.edit, r.reason} {
		if t == nil {
			continue
		}
		if err := secret.Check(*t); err != nil {
			return err
		}
	}
	return nil
}

// check refuses a mix of flags that has no single meaning.
func (r review) check() error {
	actions := 0
	for _, on := range []bool{r.approve, r.reject, r.later, r.change != nil, r.retired, r.all, r.rejected, r.reopen != 0,
		r.reset != 0, r.restore != 0, r.item != 0} {
		if on {
			actions++
		}
	}
	modifiers := r.scope != "" || r.pin || r.confirm || r.conflict != ""
	switch {
	case actions > 1:
		return errors.New("give one action")
	case r.reason != nil && !r.reject:
		return errors.New("--reason goes with --reject")
	case r.scope != "" && r.scope != store.ScopeGlobal && r.scope != store.ScopeWorkspace:
		return fmt.Errorf("unknown scope %q", r.scope)
	case r.edit != nil && strings.TrimSpace(*r.edit) == "":
		return errors.New("the --edit text is empty")
	case r.change != nil && (strings.TrimSpace(*r.change) == "" || r.id == 0):
		return errors.New("--change needs a proposal id and a text")
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
		return errors.New("--edit, --scope, --pin, --confirm, --replace, --exception and --keep-both go with --approve")
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

// excerptsShown is the most excerpts that a proposal shows (§7.4: 1-3).
const excerptsShown = 3

// show prints one proposal with the §7.4 fields: the text, the claim id, the revision it
// creates, the scope, the target, the diff, the expected benefit, the items it conflicts with,
// and 1-3 source excerpts with sender, session, workspace and date. The excerpts come from the
// evidence links at show time, through the secret filter: the proposal holds no copy.
func (cl caller) show(ctx context.Context, id int64) error {
	var p store.Proposal
	var it store.Item
	var cur store.ItemRevision
	var excerpts, conflicts []string
	var changeable bool
	// The review pane shows a pending proposal only: it closes on one that another surface
	// decided (E5-S1).
	read := cl.proposal
	if cl.surface == store.SurfacePane {
		read = cl.pending
	}
	err := cl.Store.Write(ctx, func(tx *sql.Tx) (err error) {
		if p, it, cur, err = read(ctx, tx, id); err != nil {
			return err
		}
		if excerpts, changeable, err = cl.excerpts(ctx, tx, p); err != nil {
			return err
		}
		conflicts, err = cl.conflicts(ctx, tx, p)
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
	fmt.Fprintf(&b, "proposal %d (%s)\ntext:     %s\n", p.ID, p.Status, text)
	if it.ClaimID != "" {
		fmt.Fprintf(&b, "claim:    %s\n", it.ClaimID)
	}
	fmt.Fprintf(&b, "scope:    %s\ntarget:   %s\n", p.Scope, p.Target)
	fmt.Fprintf(&b, "revision: item %d revision %d, from revision %d (now %d)\n", it.ID, *p.BaseRevision+1, *p.BaseRevision, it.CurrentRevision)
	fmt.Fprintf(&b, "diff:\n  %s\n", strings.ReplaceAll(p.Diff, "\n", "\n  "))
	if p.ExpectedBenefit != "" {
		fmt.Fprintf(&b, "benefit:  %s\n", oneLine(p.ExpectedBenefit))
	}
	for _, c := range conflicts {
		fmt.Fprintf(&b, "conflict: %s\n", c)
	}
	if len(excerpts) > 0 {
		fmt.Fprintf(&b, "excerpts:\n  %s\n", strings.Join(excerpts, "\n  "))
	}
	fmt.Fprintf(&b, "source:   %s, %s\n", source, p.Created.Format("2006-01-02 15:04"))
	if p.ChangeRequest != nil {
		fmt.Fprintf(&b, "change:   %s\n", oneLine(*p.ChangeRequest))
	}
	if warns(p) {
		b.WriteString("warning:  this may change a permission, an allow rule or a safety rule; approve needs --confirm\n")
	}
	if p.Status == store.ProposalPending && cl.surface != store.SurfacePane {
		actions(&b, p, changeable, len(conflicts) > 0)
	}
	_, err = io.WriteString(cl.Stdout, b.String())
	return err
}

// actions writes the action lines of pending proposal p to b: the change request for a
// proposal with evidence (changeable), and the conflict choices when p has an open conflict.
func actions(b *strings.Builder, p store.Proposal, changeable, conflict bool) {
	fmt.Fprintf(b, "actions:  brain review %d --approve [--edit \"<text>\"] [--scope global|workspace] [--pin] | --reject | --later", p.ID)
	if changeable {
		b.WriteString(` | --change "<text>"`)
	}
	b.WriteString("\n")
	if conflict {
		b.WriteString("          on approve, the conflict: --replace (retire the item) | --exception (link as an exception) | --keep-both (the default: the brief asks the agent to ask you)\n")
	}
}

// excerpts are the lines of up to excerptsShown source excerpts of p, the newest first, in a
// scope that the caller sees [NEW], through the secret filter. changeable reports whether p has
// evidence, so it takes a change request.
func (cl caller) excerpts(ctx context.Context, tx *sql.Tx, p store.Proposal) (lines []string, changeable bool, err error) {
	es, changeable, err := evidence(ctx, tx, p)
	if err != nil {
		return nil, false, err
	}
	for _, e := range es {
		if len(lines) == excerptsShown {
			break
		}
		if e.WorkspaceKey == "" || !cl.sees(&e.WorkspaceKey) {
			continue
		}
		lines = append(lines, fmt.Sprintf("%s, session %d (%s), workspace %s, %s: %s", e.Sender, e.SessionID, e.Harness,
			cl.label, e.At.Format("2006-01-02 15:04"), extract.Redact(oneLine(e.Excerpt))))
	}
	return lines, changeable, nil
}

// evidence returns the evidence observations of p that are still in the store, the newest
// first: the evidence of its candidate, or the comebacks of the item revision whose draft p is
// (E15-S1). The draft of an item revision has no candidate, so a reject of it rejects no claim.
// ok is false when p has no such evidence: a draft of your own.
func evidence(ctx context.Context, tx *sql.Tx, p store.Proposal) (es []store.Excerpt, ok bool, err error) {
	if p.CandidateID != nil {
		es, err = store.CandidateExcerpts(ctx, tx, *p.CandidateID)
		return es, err == nil, err
	}
	r, ok, err := store.ItemChangeOf(ctx, tx, p.ID)
	if err != nil || !ok {
		return nil, false, err
	}
	es, err = store.ComebackExcerpts(ctx, tx, r.SourceID, r.Revision)
	return es, err == nil, err
}

// conflicts are the lines of the active items that p conflicts with, when the caller sees
// them.
func (cl caller) conflicts(ctx context.Context, tx *sql.Tx, p store.Proposal) ([]string, error) {
	var lines []string
	for _, id := range p.ConflictsWith {
		it, cur, err := cl.item(ctx, tx, id)
		switch {
		case errors.Is(err, errNotFound):
			continue
		case err != nil:
			return nil, err
		}
		if it.State == store.StateActive {
			lines = append(lines, fmt.Sprintf("item %d (%s): %s", it.ID, cl.target(it.Scope), oneLine(cur.Text)))
		}
	}
	return lines, nil
}

// decide runs the approve, reject or later of r.
func (cl caller) decide(ctx context.Context, r review) error {
	switch {
	case r.approve:
		return cl.approve(ctx, r)
	case r.change != nil:
		return cl.askChange(ctx, r.id, *r.change)
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
	var retired []store.ItemRevision
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
		if retired, err = cl.resolve(ctx, tx, r, p, &next); err != nil {
			return err
		}
		if err := store.DecideProposal(ctx, tx, p.ID, store.ProposalApproved, cl.surface, cl.Now()); err != nil {
			return err
		}
		if err := store.DecideSuggestions(ctx, tx, p.ID, store.SuggestionApproved); err != nil {
			return err
		}
		if _, err = store.WriteRevision(ctx, tx, *p.BaseRevision, next); err != nil {
			return err
		}
		for _, rt := range retired {
			if _, err := store.WriteRevision(ctx, tx, rt.Revision-1, rt); err != nil {
				return err
			}
		}
		return checkSkill(ctx, tx, next.ItemID)
	})
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(cl.Stdout, "proposal %d approved: item %d is %s at revision %d (%s)\n",
		r.id, next.ItemID, next.State, next.Revision, cl.target(next.Scope))
	for _, rt := range retired {
		if err == nil {
			_, err = fmt.Fprintf(cl.Stdout, "item %d retired at revision %d: the new item replaces it\n", rt.ItemID, rt.Revision)
		}
	}
	if err == nil && next.ExceptionToItem != nil {
		_, err = fmt.Fprintf(cl.Stdout, "item %d is an exception to global item %d\n", next.ItemID, *next.ExceptionToItem)
	}
	if err == nil && len(next.ConflictsWith) > 0 {
		_, err = fmt.Fprintf(cl.Stdout, "item %d keeps its conflict with %s: the brief asks the agent to ask you\n",
			next.ItemID, itemList(next.ConflictsWith))
	}
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

// checkSkill runs the store check of a skill item after its revision write, in the same
// transaction (§8.2): a name that another active skill in the scope has refuses the write.
func checkSkill(ctx context.Context, tx *sql.Tx, item int64) error {
	err := store.CheckSkill(ctx, tx, item)
	if errors.Is(err, store.ErrSkillName) {
		return fmt.Errorf("%w: give this skill a new name (approve with --edit and a new name: line); nothing was written", err)
	}
	if err != nil {
		return fmt.Errorf("%w; nothing was written", err)
	}
	return nil
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
// candidate stays rejected in the scope of the candidate, with reason (§7.2), so the other
// pending proposals of the candidate are rejected too: none of them can make the claim active.
func (cl caller) reject(ctx context.Context, id int64, reason string) error {
	var c *store.Candidate
	var rejection int64
	var others []int64
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
			ids, err := store.PendingCandidateProposalIDs(ctx, tx, c.ID)
			if err != nil {
				return err
			}
			others = slices.DeleteFunc(ids, func(other int64) bool { return other == id })
		}
		for _, p := range append([]int64{id}, others...) {
			if err := cl.rejectOne(ctx, tx, p); err != nil {
				return err
			}
		}
		return nil
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
	for _, other := range others {
		if err == nil {
			_, err = fmt.Fprintf(cl.Stdout, "proposal %d of the same claim rejected\n", other)
		}
	}
	return err
}

// rejectOne marks proposal id and its shown suggestions rejected in tx.
func (cl caller) rejectOne(ctx context.Context, tx *sql.Tx, id int64) error {
	if err := store.DecideProposal(ctx, tx, id, store.ProposalRejected, cl.surface, cl.Now()); err != nil {
		return err
	}
	return store.DecideSuggestions(ctx, tx, id, store.SuggestionRejected)
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
// The shown suggestion of the proposal in the calling pane, the review pane or its agent, gets
// result later, which mutes the proposal for the rest of the capture session of that
// suggestion (§5.5). It holds the suggestion lock, so a suggestion that opens now is either
// registered before it or not shown yet.
func (cl caller) later(ctx context.Context, id int64) error {
	unlock, err := lockSuggestions(cl.Binding.Store)
	if err != nil {
		return err
	}
	err = cl.Store.Write(ctx, func(tx *sql.Tx) error {
		if _, _, _, err := cl.pending(ctx, tx, id); err != nil {
			return err
		}
		return store.LaterSuggestions(ctx, tx, id, cl.Binding.Session, cl.PaneID)
	})
	if err := errors.Join(err, unlock()); err != nil {
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
				r.ID, cl.target(r.Scope), r.At.Format("2006-01-02 15:04"), r.New, r.Evidence, r.EvidenceCount, r.ClaimID, text, reason))
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
		if _, err = store.WriteRevision(ctx, tx, it.CurrentRevision, next); err != nil {
			return err
		}
		return checkSkill(ctx, tx, id)
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

// The approve choices for a proposal that conflicts with an item (§7.4) [NEW flags].
const (
	conflictReplace   = "replace"   // retire the conflicting items
	conflictException = "exception" // link the new workspace item as an exception to the global item
	conflictKeep      = "keep-both" // keep both, with the conflict mark
)

// resolve applies your choice r.conflict for the conflicts of p to next, the revision that
// approve writes, and returns the revisions that retire the replaced items (§7.4). Only the
// active conflicting items that the caller sees count. Keep both, and an approve with no
// choice, write the conflicts on next: the brief shows them as "(conflicts with <id>; ask the
// user)" (E6). Replace retires them, as an approved retire does, with --confirm for a
// permission or safety rule. Exception uses the E4 link, with its refusal to relax a global
// permission or safety rule.
func (cl caller) resolve(ctx context.Context, tx *sql.Tx, r review, p store.Proposal, next *store.ItemRevision) ([]store.ItemRevision, error) {
	var its []store.Item
	var curs []store.ItemRevision
	for _, id := range p.ConflictsWith {
		it, cur, err := cl.item(ctx, tx, id)
		switch {
		case errors.Is(err, errNotFound):
			continue
		case err != nil:
			return nil, err
		}
		if it.State == store.StateActive && it.ID != next.ItemID {
			its, curs = append(its, it), append(curs, cur)
		}
	}
	if len(its) == 0 {
		if r.conflict != "" {
			return nil, fmt.Errorf("proposal %d has no open conflict: approve it without --%s; nothing was written", p.ID, r.conflict)
		}
		return nil, nil
	}
	switch r.conflict {
	case conflictReplace:
		retired := make([]store.ItemRevision, len(its))
		for i, it := range its {
			if permissionWords.MatchString(curs[i].Text) && !r.confirm {
				return nil, fmt.Errorf("item %d %w: replace it with --confirm; nothing was written", it.ID, errConfirm)
			}
			retired[i] = nextRevision(curs[i], cl.Now())
			retired[i].Revision, retired[i].State = it.CurrentRevision+1, store.StateRetired
		}
		return retired, nil
	case conflictException:
		if len(its) != 1 || next.Scope != store.ScopeWorkspace || its[0].Scope != store.ScopeGlobal {
			return nil, fmt.Errorf("proposal %d: --exception links one workspace item to one global item; it conflicts with %s; nothing was written",
				p.ID, itemList(p.ConflictsWith))
		}
		linked, err := except(*next, its[0], curs[0])
		*next = linked
		return nil, err
	}
	for _, it := range its {
		if !slices.Contains(next.ConflictsWith, it.ID) {
			next.ConflictsWith = append(next.ConflictsWith, it.ID)
		}
	}
	return nil, nil
}

// itemList names item ids for a message: "item 3" or "items 3, 5".
func itemList(ids []int64) string {
	s := make([]string, len(ids))
	for i, id := range ids {
		s[i] = strconv.FormatInt(id, 10)
	}
	if len(ids) == 1 {
		return "item " + s[0]
	}
	return "items " + strings.Join(s, ", ")
}

// askChange records your change request text on pending proposal id and queues it (S56): the
// proposal becomes changed and is not published, and the extractor drafts a new proposal in
// the background. runReview filtered the text for secrets. [NEW] only an extractor proposal,
// one with a candidate, takes a change request: its draft needs the evidence of the candidate.
// The draft of an item revision takes one too: its request of that revision is queued again
// (E15-S1).
func (cl caller) askChange(ctx context.Context, id int64, text string) error {
	var cr store.ChangeRequest
	err := cl.Store.Write(ctx, func(tx *sql.Tx) error {
		p, _, _, err := cl.pending(ctx, tx, id)
		if err != nil {
			return err
		}
		c, err := cl.candidate(ctx, tx, p)
		if err != nil {
			return err
		}
		item := false
		if c == nil && p.CandidateID == nil {
			if _, item, err = store.ItemChangeOf(ctx, tx, id); err != nil {
				return err
			}
		}
		switch {
		case c != nil:
			cr, err = store.AskChange(ctx, tx, id, *c, text, cl.surface, cl.Now())
		case item:
			cr, err = store.AskItemChange(ctx, tx, id, text, cl.surface, cl.Now())
		default:
			return fmt.Errorf("proposal %d has no extractor evidence: approve it with --edit instead; nothing was written", id)
		}
		if err != nil {
			return err
		}
		return store.DecideSuggestions(ctx, tx, id, store.SuggestionChanged)
	})
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(cl.Stdout, "proposal %d: change asked (request %d); it is not published, and a new draft comes later as a new proposal\n",
		id, cr.ID)
	return err
}
