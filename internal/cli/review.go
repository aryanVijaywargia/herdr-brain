package cli

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/aryanVijaywargia/herdr-brain/internal/brief"
	"github.com/aryanVijaywargia/herdr-brain/internal/extract"
	"github.com/aryanVijaywargia/herdr-brain/internal/secret"
	"github.com/aryanVijaywargia/herdr-brain/internal/signal"
	"github.com/aryanVijaywargia/herdr-brain/internal/store"
	"github.com/aryanVijaywargia/herdr-brain/internal/tool"
)

const reviewUsage = `usage:
  brain review                                   list the proposals that wait for you
  brain review <id> [--detail]                   show one proposal; --detail adds its claim id and revision
  brain review <id> --evidence                   show the stored text around each source excerpt
  brain review <id> --approve [--edit "<text>"] [--scope global|workspace] [--pin] [--confirm]
                             [--replace | --exception | --keep-both]   for a proposal with a conflict
  brain review <id> --reject [--reason "<text>"] | --later
  brain review <id> --change "<text>"              ask for a new draft of an extractor proposal
  brain review <id> --keep | --retire              decide a disputed item: keep it as it is, or retire it
                                                 (edit it: brain review <id> --approve --edit "<text>")
  brain review --retired                         list the retired items
  brain review --all                             list the candidates too, below the gate as well
  brain review --tool <candidate>                make this a tool: a tool draft from one example of a
                                                 command candidate
  brain review --rejected                        list the rejections, with the new evidence since each
  brain review --reopen <rejection>              end a rejection: its claim can get proposals again
  brain review --reset <rejection>               keep a rejection, and count its new evidence from now
  brain review --restore <item>                  make a retired item active again
  brain review --item <item> --edit "<text>" | --retire   propose a change to an active item
  brain review --item <item> --scope global      propose to promote a workspace item (as brain promote)
  brain review --item <item> --exception-to <global item> | --conflicts-with <global item>
                                                 link a workspace item to a global item`

// review is the parsed arguments of `brain review`. Each action has one command-line form, so
// the review pane (E5), the plugin popup and E10 run the same code.
type review struct {
	id                     int64 // the proposal; 0 for none
	approve, reject, later bool
	keep                   bool    // keep a disputed item (E16-S6)
	edit                   *string // the new text; nil for none
	change                 *string // your change request (S56); nil for none
	conflict               string  // replace, exception or keep-both: what approve does with a conflict
	reason                 *string // your reason for a reject; nil for none
	scope                  store.Scope
	pin, confirm           bool
	detail                 bool // show adds the claim id and the revision (#254)
	evidence               bool // show the stored text around each source excerpt (E20-S4)
	retired                bool
	all                    bool  // list the candidates (E12-S2)
	makeTool               int64 // the command candidate to make a tool draft of (E17-S6)
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
	case r.item != 0 && r.scope != "":
		// The review action "change scope" of an active item (§9.3).
		return cl.promote(ctx, r.item)
	case r.item != 0:
		return cl.proposeChange(ctx, r)
	case r.retired:
		return cl.listRetired(ctx)
	case r.all:
		return cl.listCandidates(ctx)
	case r.makeTool != 0:
		return cl.makeTool(ctx, r.makeTool)
	case r.rejected:
		return cl.listRejections(ctx)
	case r.reopen != 0 || r.reset != 0:
		return cl.endRejection(ctx, cmp.Or(r.reopen, r.reset), r.reopen != 0)
	case r.restore != 0:
		return cl.restore(ctx, r.restore)
	case r.approve || r.reject || r.later || r.change != nil || r.keep || r.retire && r.item == 0:
		if err := cl.decide(ctx, r); err != nil {
			return err
		}
		return cl.closeReviewPanes(ctx, r.id)
	case r.id != 0:
		return cl.view(ctx, r)
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

// queueFlags adds the flags of the actions on the lists, candidates and rejections to fs.
func queueFlags(fs *flag.FlagSet, r *review) {
	fs.BoolVar(&r.retired, "retired", false, "")
	fs.BoolVar(&r.all, "all", false, "")
	fs.Int64Var(&r.makeTool, "tool", 0, "")
	fs.BoolVar(&r.rejected, "rejected", false, "")
	fs.Int64Var(&r.reopen, "reopen", 0, "")
	fs.Int64Var(&r.reset, "reset", 0, "")
	fs.Int64Var(&r.restore, "restore", 0, "")
}

func parseReview(args []string) (review, error) {
	var r review
	fs := flag.NewFlagSet("review", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.BoolVar(&r.approve, "approve", false, "")
	fs.BoolVar(&r.reject, "reject", false, "")
	fs.BoolVar(&r.later, "later", false, "")
	fs.BoolVar(&r.keep, "keep", false, "")
	decisionFlags(fs, &r)
	scope := fs.String("scope", "", "")
	fs.BoolVar(&r.pin, "pin", false, "")
	fs.BoolVar(&r.confirm, "confirm", false, "")
	fs.BoolVar(&r.detail, "detail", false, "")
	fs.BoolVar(&r.evidence, "evidence", false, "")
	queueFlags(fs, &r)
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
	if err := cmp.Or(r.views(), r.check()); err != nil {
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
	for _, on := range []bool{r.approve, r.reject, r.later, r.change != nil, r.keep, r.retire && r.item == 0, r.retired, r.all,
		r.rejected, r.reopen != 0, r.reset != 0, r.restore != 0, r.item != 0, r.makeTool != 0} {
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
		for _, on := range []bool{r.edit != nil, r.retire, r.exceptionTo != 0, r.conflictsWith != 0, r.scope != ""} {
			if on {
				changes++
			}
		}
		if r.id != 0 || r.pin || r.confirm || r.conflict != "" || changes != 1 || r.scope == store.ScopeWorkspace {
			return errors.New("--item takes one of --edit, --retire, --scope global, --exception-to or --conflicts-with, and no proposal id")
		}
	case r.exceptionTo != 0 || r.conflictsWith != 0:
		return errors.New("--exception-to and --conflicts-with need --item")
	case r.retire && r.id == 0:
		return errors.New("--retire needs --item, or the proposal id of a disputed item")
	case r.keep && r.id == 0:
		return errors.New("--keep needs the proposal id of a disputed item")
	case r.approve:
		if r.id == 0 {
			return errors.New("--approve needs a proposal id")
		}
	case modifiers || r.edit != nil:
		return errors.New("--edit, --scope, --pin, --confirm, --replace, --exception and --keep-both go with --approve")
	case (r.reject || r.later) && r.id == 0:
		return errors.New("--reject and --later need a proposal id")
	case (r.retired || r.all || r.rejected || r.reopen != 0 || r.reset != 0 || r.restore != 0 || r.makeTool != 0) && r.id != 0:
		return errors.New("--retired, --all, --rejected, --reopen, --reset, --restore and --tool take no proposal id")
	}
	return nil
}

// gates returns the gates of the config file, as the extractor reads them (§7.2, #282). A
// config that does not read gives the default gates with a warning, so the lists still show.
func (cl caller) gates() store.Gates {
	g, _, err := extract.ReadConfig(cl.Config)
	if err != nil {
		if cl.Stderr != nil {
			_, _ = fmt.Fprintf(cl.Stderr, "warning: %v; the review lists use the default gates\n", err)
		}
		return store.DefaultGates()
	}
	return g
}

// list prints the pending proposals that the caller sees. A proposal of a candidate below its
// gate waits in brain review --all only (§7.2).
func (cl caller) list(ctx context.Context) error {
	gates := cl.gates()
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
	if len(p.Dispute) > 0 {
		return fmt.Sprintf("item %d disputed: %s: %s", it.ID, gone(p), oneLine(p.Text))
	}
	if p.Script() {
		return scriptSummary(p)
	}
	if p.Text == "" {
		return fmt.Sprintf("retire item %d", it.ID)
	}
	if promotion(p, it) {
		return fmt.Sprintf("promote item %d to %s: %s", it.ID, p.Scope, oneLine(p.Text))
	}
	if b, err := store.ParseBinding(p.Text); err == nil && it.Kind == store.KindAutomation {
		return fmt.Sprintf("automation: run tool %s in %s after each stop of a %s agent", b.Tool, b.Folder, b.CLI)
	}
	if c, err := tool.Parse(p.Text); err == nil && it.Kind == store.KindTool {
		return fmt.Sprintf("tool %s: %s", c.Name, oneLine(c.Description))
	}
	if sk, err := store.ParseSkill(p.Text); err == nil && it.Kind == store.KindSkill {
		if patch(p, it) {
			return fmt.Sprintf("patch of skill %s (item %d)", sk.Name, it.ID)
		}
		return fmt.Sprintf("skill %s: %s", sk.Name, oneLine(sk.Description))
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

// show prints one proposal with the §7.4 fields. What you decide on comes first: the text, the
// scope and the effect (the target and the expected benefit); then the diff, the items it
// conflicts with, and 1-3 source excerpts with sender, session, workspace and local date. The
// claim id and the revision it creates show with detail only (#254). The excerpts come from the
// evidence links at show time, through the secret filter: the proposal holds no copy.
func (cl caller) show(ctx context.Context, id int64, detail bool) error {
	var p store.Proposal
	var it store.Item
	var cur store.ItemRevision
	var excerpts, conflicts []string
	var changeable bool
	var others int                  // the workspaces of the "also seen" hint of a promotion; -1 for none
	var binding, from, match string // match: the match line of the suggestion of p, if any
	var claim string                // the claim of a script proposal
	var sibs []store.Proposal       // the proposals that you pick between with p
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
		if from, err = origin(ctx, tx, p, it, cur); err != nil {
			return err
		}
		if others, err = hint(ctx, tx, p, it); err != nil {
			return err
		}
		if binding, err = cl.bindingLines(ctx, tx, p, it); err != nil {
			return err
		}
		if sibs, err = siblings(ctx, tx, p); err != nil {
			return err
		}
		if match, err = matchLine(ctx, tx, p.ID, cl.PaneID); err != nil {
			return err
		}
		if p.Script() {
			// cl.proposal read the candidate of a script proposal: it is there.
			c, err := cl.candidate(ctx, tx, p)
			if err != nil {
				return err
			}
			claim = c.ClaimID
		}
		conflicts, err = cl.conflicts(ctx, tx, p)
		return err
	})
	if err != nil {
		return err
	}
	var b strings.Builder
	var more string
	if detail {
		more = detailLines(p, it, claim)
	}
	showHead(&b, p, it, binding, more)
	fmt.Fprintf(&b, "diff:\n  %s\n", strings.ReplaceAll(p.Diff, "\n", "\n  "))
	if len(p.Dispute) > 0 {
		b.WriteString(disputeLine)
	}
	b.WriteString(promotionLines(others))
	for _, c := range conflicts {
		fmt.Fprintf(&b, "conflict: %s\n", c)
	}
	if len(excerpts) > 0 {
		fmt.Fprintf(&b, "excerpts:\n  %s\n", strings.Join(excerpts, "\n  "))
	}
	fmt.Fprintf(&b, "source:   %s, %s\n", from, localTime(p.Created))
	b.WriteString(match)
	if p.ChangeRequest != nil {
		fmt.Fprintf(&b, "change:   %s\n", oneLine(*p.ChangeRequest))
	}
	if p.Status == store.ProposalPending {
		b.WriteString(pickLines(sibs))
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

// matchLine is the line of show from pane paneID that says why the suggestion of proposal
// matched (E20-S9, store.ProposalSuggestion), or "" when the proposal has none.
func matchLine(ctx context.Context, tx *sql.Tx, proposal int64, paneID string) (string, error) {
	s, ok, err := store.ProposalSuggestion(ctx, tx, proposal, paneID)
	if err != nil || !ok {
		return "", err
	}
	return fmt.Sprintf("match:    %s, suggestion %d\n", reasonWords(s.Reason), s.ID), nil
}

// reasonWords is suggestion reason r in words (E20-S9): a row of an older Brain has none.
func reasonWords(r store.SuggestionReason) string {
	switch r {
	case store.ReasonExactPrompt:
		return "exact prompt"
	case store.ReasonLesson:
		return "plain-words lesson"
	}
	return "unknown"
}

// showHead writes the first lines of p in show, what you decide on (§7.4): the status and what
// p does, the kind, the text, the scope and the effect, the target and the expected benefit. A
// text of many lines (a skill, a tool, an automation, a script) comes after the scope and the
// effect, so that a short pane shows them too (#254). binding holds the lines of an automation;
// detail holds the detail lines, which come after the effect.
func showHead(b *strings.Builder, p store.Proposal, it store.Item, binding, detail string) {
	fmt.Fprintf(b, "proposal %d (%s): %s\n", p.ID, p.Status, what(p, it))
	var text string
	if p.Script() {
		b.WriteString(scriptLine)
		text = scriptText(p)
	} else {
		b.WriteString(kindLine(it))
		text = itemText(p, it) + binding
	}
	effect := fmt.Sprintf("scope:    %s\ntarget:   %s\n", p.Scope, p.Target)
	if p.ExpectedBenefit != "" {
		effect += fmt.Sprintf("benefit:  %s\n", oneLine(p.ExpectedBenefit))
	}
	effect += detail
	if strings.Count(text, "\n") > 1 {
		text, effect = effect, text
	}
	b.WriteString(text + effect)
}

// what says what p does to its item: a draft item is new, any other item changes. A script
// proposal has no item (E17-S8).
func what(p store.Proposal, it store.Item) string {
	switch {
	case p.Script():
		return "new script"
	case it.State == store.StateDraft:
		return "new item"
	}
	return fmt.Sprintf("changes item %d", it.ID)
}

// detailLines are the detail lines of p in show (#254): the claim id and the revision it
// creates. A script proposal has no item: claim is its claim, and it has no revision (E17-S8).
func detailLines(p store.Proposal, it store.Item, claim string) string {
	var b strings.Builder
	if c := cmp.Or(it.ClaimID, claim); c != "" {
		fmt.Fprintf(&b, "claim:    %s\n", c)
	}
	if !p.Script() {
		fmt.Fprintf(&b, "revision: item %d revision %d, from revision %d (now %d)\n", it.ID, *p.BaseRevision+1, *p.BaseRevision, it.CurrentRevision)
	}
	return b.String()
}

// localTime is t in local time, to the minute, as review shows times (#254).
func localTime(t time.Time) string { return t.Local().Format("2006-01-02 15:04") }

// skillLine is the line of a skill proposal in show. The review pane finds a skill by it: its
// one-line edit cannot hold a skill file.
const skillLine = "kind:     skill\n"

// kindLine is the kind line of item it in show; a fact has none.
func kindLine(it store.Item) string {
	switch it.Kind {
	case store.KindSkill:
		return skillLine
	case store.KindTool:
		return toolLine
	case store.KindAutomation:
		return automationLine
	}
	return ""
}

// itemText is the text line and, for a tool, the contract lines of p in show.
func itemText(p store.Proposal, it store.Item) string {
	var b strings.Builder
	b.WriteString(proposalText(p, it))
	if it.Kind == store.KindTool && p.Text != "" {
		b.WriteString(contractLines(p.Text))
	}
	return b.String()
}

// toolLine is the line of a tool proposal in show.
const toolLine = "kind:     tool\n"

// automationLine is the line of an automation proposal in show.
const automationLine = "kind:     automation\n"

// bindingLines are the lines of the binding of automation proposal p of item it in show
// (§10.4): the exact tool with its contract, the trigger and the folder, what your approval
// permits, and what undo cannot do. A binding that the approval refuses shows why.
func (cl caller) bindingLines(ctx context.Context, tx *sql.Tx, p store.Proposal, it store.Item) (string, error) {
	if it.Kind != store.KindAutomation || p.Text == "" {
		return "", nil
	}
	b, err := store.ParseBinding(p.Text)
	if err != nil {
		return fmt.Sprintf("binding:  refused: %v\n", err), nil
	}
	t, err := store.ToolByID(ctx, tx, b.ToolID)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return fmt.Sprintf("binding:  refused: tool %d does not exist\n", b.ToolID), nil
	case err != nil:
		return "", err
	}
	var s strings.Builder
	fmt.Fprintf(&s, "binding:\n  tool:     %s (item %d), contract hash %s\n", t.Name, t.ID, b.ContractHash)
	s.WriteString("  " + strings.ReplaceAll(strings.TrimSuffix(contractLines(t.Contract), "\n"), "\n", "\n  ") + "\n")
	fmt.Fprintf(&s, "  trigger:  %s of a %s agent in this workspace\n  folder:   %s\n", b.Trigger, b.CLI, b.Folder)
	s.WriteString("  approval: your approval of this exact tool, trigger and folder is the permission: the tool runs after each stop, with no prompt\n")
	s.WriteString("  undo:     pause, removal and undo stop future runs; they cannot reverse effects that already happened\n")
	key := ""
	if it.WorkspaceKey != nil {
		key = *it.WorkspaceKey
	}
	switch err := store.CheckBinding(ctx, tx, b, key, lookupEnv(cl.Environ, "HOME")); {
	case errors.Is(err, store.ErrBindingRefused):
		fmt.Fprintf(&s, "  refused:  %v; approve refuses it\n", err)
	case err != nil:
		return "", err
	}
	return s.String(), nil
}

// proposalText is the text line of p in show. A skill draft shows the whole file (S8). A
// skill patch shows no text: its diff holds the changed lines, and approve writes the whole
// new file (§9.2). A tool shows its whole contract text, a change too: your approval covers
// all of it (§10.1).
func proposalText(p store.Proposal, it store.Item) string {
	switch {
	case p.Text == "":
		return "text:     (none: this proposal retires the item)\n"
	case patch(p, it):
		return "text:     (a skill patch: the diff shows the changed lines)\n"
	case it.Kind == store.KindSkill || it.Kind == store.KindTool || it.Kind == store.KindAutomation:
		return "text:\n  " + strings.ReplaceAll(strings.TrimSuffix(p.Text, "\n"), "\n", "\n  ") + "\n"
	}
	return fmt.Sprintf("text:     %s\n", brief.Clean(p.Text))
}

// contractLines are the lines of the contract of tool text in show (§10.1): the command bytes
// of each step, quoted, the input schema, the cwd, the timeout, the stated effects and the
// read-only restriction. The scope line shows the scope. A contract that breaks a rule shows
// why: approve refuses it.
func contractLines(text string) string {
	c, err := tool.Parse(text)
	if err != nil {
		return fmt.Sprintf("contract: refused: %v\n", err)
	}
	var b strings.Builder
	b.WriteString("contract:\n")
	for i, argv := range c.Commands() {
		q := make([]string, len(argv))
		for j, a := range argv {
			q[j] = strconv.Quote(a)
		}
		fmt.Fprintf(&b, "  step %d:    %s\n", i+1, strings.Join(q, " "))
	}
	if c.Script != "" {
		fmt.Fprintf(&b, "  %s:    the text after the --- %s --- line; inputs reach it as \"$1\", \"$2\"\n", tool.ScriptName, tool.ScriptName)
	}
	for _, n := range slices.Sorted(maps.Keys(c.Inputs)) {
		fmt.Fprintf(&b, "  input:     %s: %s, pattern %s\n", n, c.Inputs[n].Type, c.Inputs[n].Pattern)
	}
	readOnly := "no"
	if c.ReadOnly {
		readOnly = "yes"
	}
	fmt.Fprintf(&b, "  cwd:       %s\n  timeout:   %d s\n  effects:   %s\n  read-only: %s\n", c.Cwd, c.TimeoutS,
		oneLine(c.Effects), readOnly)
	return b.String()
}

// patch reports whether p is a patch of an active or disputed skill it: a change with a new
// text. A dispute is not a patch: it keeps the text, and its diff is the anchor diff. A
// promotion is not one either: it shows the whole text, for you to edit.
func patch(p store.Proposal, it store.Item) bool {
	return it.Kind == store.KindSkill && it.State != store.StateDraft && p.Text != "" && len(p.Dispute) == 0 && !promotion(p, it)
}

// source says where proposal p of item it, at revision cur, comes from.
func source(p store.Proposal, it store.Item, cur store.ItemRevision) string {
	switch {
	case p.Script():
		return "the exact repeats of a command chain, sender unconfirmed"
	case len(p.Dispute) > 0:
		return fmt.Sprintf("the anchor check of item %d: %s", it.ID, gone(p))
	case it.State != store.StateDraft:
		return fmt.Sprintf("a change to %s item %d, asked in review", it.State, it.ID)
	case learned(p, it):
		return "the learned automation rule (a brain run of this tool after a stop of the agent; no model call)"
	}
	return fmt.Sprintf("%s, sender unconfirmed", cur.Origin)
}

// learned reports whether p, a proposal of item it, comes from the learned automation rule
// (§10.4, E17-S10): an automation proposal of a candidate. brain automation add makes one with
// no candidate.
func learned(p store.Proposal, it store.Item) bool {
	return p.CandidateID != nil && it.Kind == store.KindAutomation
}

// origin says where proposal p of item it, at revision cur, comes from: for the draft of the
// change request of an item revision, its comebacks and their sessions (E20-S5), else source.
func origin(ctx context.Context, tx *sql.Tx, p store.Proposal, it store.Item, cur store.ItemRevision) (string, error) {
	r, ok, err := store.ItemChangeOf(ctx, tx, p.ID)
	if err != nil || !ok {
		return source(p, it, cur), err
	}
	n, err := store.ComebackSessions(ctx, tx, r.SourceID, r.Revision)
	if err != nil {
		return "", err
	}
	unit := "sessions"
	if n == 1 {
		unit = "session"
	}
	return fmt.Sprintf("the comebacks of revision %d in %d %s that got it", r.Revision, n, unit), nil
}

// gone is the text of the anchors of dispute p, each moved or deleted (§9.1), as the brief mark
// says it.
func gone(p store.Proposal) string {
	return brief.Dispute{Anchors: p.Dispute, Moved: signal.Moved(p.Diff)}.Gone()
}

// disputeLine is the line of a dispute in show. The review pane finds a dispute by it, and
// then offers the keys of a dispute.
const disputeLine = "dispute:  the item stays in the brief with a mark until you keep, edit or retire it\n"

// actions writes the action lines of pending proposal p to b: the change request for a
// proposal with evidence (changeable), and the conflict choices when p has an open conflict.
func actions(b *strings.Builder, p store.Proposal, changeable, conflict bool) {
	switch {
	case len(p.Dispute) > 0:
		fmt.Fprintf(b, "actions:  brain review %d --keep | --approve --edit \"<text>\" | --retire | --later\n", p.ID)
		return
	case p.Script():
		fmt.Fprintf(b, "actions:  brain review %d --approve | --reject | --later\n", p.ID)
		return
	}
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
	if p.ItemID != nil {
		it, err := store.ItemByID(ctx, tx, *p.ItemID)
		if err != nil {
			return nil, false, err
		}
		if learned(p, it) {
			// A binding that does not parse names no tool (0): the lines then show the stops only,
			// and bindingLines shows why it is refused.
			b, _ := store.ParseBinding(p.Text)
			lines, err = cl.stopLines(ctx, tx, es, b.ToolID)
			return lines, changeable, err
		}
	}
	for _, e := range cl.shown(es) {
		lines = append(lines, cl.excerptHead(e)+": "+extract.Redact(oneLine(e.Excerpt)))
	}
	return lines, changeable, nil
}

// stopLines are the lines of up to excerptsShown evidence stops es of a learned automation
// proposal of tool item toolID (#340), the newest of each session first (bySession): the stop of
// the agent and the first brain run call of the tool after it (store.StopCalls). The screen
// text at the stop says nothing of the call, so it does not show; brain review --evidence shows
// it.
func (cl caller) stopLines(ctx context.Context, tx *sql.Tx, es []store.Excerpt, toolID int64) ([]string, error) {
	calls := map[int64]store.StopCall{} // by stop observation
	read := map[string]bool{}           // the herdr sessions whose calls are in calls
	var lines []string
	for _, e := range cl.shown(bySession(es)) {
		if !read[e.HerdrSession] {
			read[e.HerdrSession] = true
			cs, err := store.StopCalls(ctx, tx, e.HerdrSession)
			if err != nil {
				return nil, err
			}
			for _, c := range cs {
				if _, ok := calls[c.Stop]; !ok && c.ToolID == toolID {
					calls[c.Stop] = c
				}
			}
		}
		line := fmt.Sprintf("session %d (%s), workspace %s, %s: the agent stopped (%s)", e.SessionID, e.Harness, cl.label,
			localTime(e.At), stopMark(e))
		if c, ok := calls[e.ObservationID]; ok {
			line += fmt.Sprintf(", then it ran brain run %s at %s", c.Tool, localTime(c.At))
		}
		lines = append(lines, line)
	}
	return lines, nil
}

// stopMark says what marked stop e (store.StopCall): the Stop hook of the CLI, or a change of
// the agent status that herdr gives from working.
func stopMark(e store.Excerpt) string {
	if e.Source == store.SourceHook {
		return "Stop hook"
	}
	return "herdr status working → " + e.Event
}

// shown are the first excerptsShown excerpts of es in a scope that the caller sees.
func (cl caller) shown(es []store.Excerpt) []store.Excerpt {
	var shown []store.Excerpt
	for _, e := range es {
		if len(shown) == excerptsShown {
			break
		}
		if e.WorkspaceKey != "" && cl.sees(&e.WorkspaceKey) {
			shown = append(shown, e)
		}
	}
	return shown
}

// excerptHead says where excerpt e comes from: its sender, session, workspace and local date.
func (cl caller) excerptHead(e store.Excerpt) string {
	return fmt.Sprintf("%s, session %d (%s), workspace %s, %s", e.Sender, e.SessionID, e.Harness, cl.label, localTime(e.At))
}

// evidence returns the evidence observations of p that are still in the store, the newest
// first: the evidence of its candidate, or the comebacks of the item revision whose draft p is
// (E15-S1), the newest of each session first (bySession). The draft of an item revision has no
// candidate, so a reject of it rejects no claim. ok is false when p has no such evidence: a
// draft of your own.
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
	return bySession(es), err == nil, err
}

// bySession puts the newest excerpt of each session of es first, then the rest, each part in
// the order of es: the review shows each session of the comebacks as evidence (E20-S5), also
// when one session has more flags than the review shows.
func bySession(es []store.Excerpt) []store.Excerpt {
	seen := map[int64]bool{}
	var first, rest []store.Excerpt
	for _, e := range es {
		if seen[e.SessionID] {
			rest = append(rest, e)
			continue
		}
		seen[e.SessionID] = true
		first = append(first, e)
	}
	return append(first, rest...)
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
	case r.keep || r.retire:
		return cl.settle(ctx, r.id, r.keep)
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
	var script bool
	var superseded []int64 // the siblings of the proposal: you picked it (E17-S8)
	err := cl.Store.Write(ctx, func(tx *sql.Tx) error {
		p, it, cur, err := cl.pending(ctx, tx, r.id)
		if err != nil {
			return err
		}
		if script = p.Script(); script {
			superseded, err = cl.approveScript(ctx, tx, r, p)
			return err
		}
		sibs, err := siblings(ctx, tx, p)
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
		if err := cl.checkBinding(ctx, tx, it, next); err != nil {
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
		if superseded, err = cl.supersede(ctx, tx, sibs); err != nil {
			return err
		}
		// The replaced items retire first, so a skill that replaces one with its name passes
		// the name check of its own write (§8.2).
		for _, rt := range retired {
			if _, err := store.WriteRevision(ctx, tx, rt.Revision-1, rt); err != nil {
				return err
			}
		}
		if _, err := store.WriteRevision(ctx, tx, *p.BaseRevision, next); err != nil {
			return err
		}
		// The approval of another proposal of a disputed item ends its dispute: the new
		// revision gets fresh anchors (§9.1).
		if it.State == store.StateDisputed && len(p.Dispute) == 0 {
			if err := store.SupersedeDisputes(ctx, tx, it.ID, cl.Now()); err != nil {
				return err
			}
		}
		if err := anchor(ctx, tx, next); err != nil {
			return err
		}
		// A promotion, or the "change scope" of approve (§9.3). The check comes after anchor,
		// which drops the workspace anchors: the budget counts the room of their mark.
		if cur.Scope == store.ScopeWorkspace && next.Scope == store.ScopeGlobal {
			return cl.checkPromotion(ctx, tx, it)
		}
		return nil
	})
	if err != nil {
		return skillError(err)
	}
	if script {
		if _, err := fmt.Fprintf(cl.Stdout, "proposal %d approved: Brain wrote no file; install the script yourself (brain review %d shows it)\n",
			r.id, r.id); err != nil {
			return err
		}
		return cl.superseded(ctx, superseded)
	}
	_, err = fmt.Fprintf(cl.Stdout, "proposal %d approved: item %d is %s at revision %d (%s)\n",
		r.id, next.ItemID, next.State, next.Revision, cl.target(next.Scope))
	if err == nil {
		err = cl.superseded(ctx, superseded)
	}
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
		return next, confirmed(r, p, cur, next)
	}
	next.Text, next.State, next.Pinned = p.Text, store.StateActive, cur.Pinned || r.pin
	if r.edit != nil {
		next.Text = *r.edit
	}
	scope := r.scope
	if scope == "" && promotion(p, it) {
		scope = p.Scope
	}
	switch scope {
	case store.ScopeGlobal:
		// An exception link is for a workspace item only (§7.4); undo of the promotion brings it back.
		next.Scope, next.WorkspaceKey, next.ExceptionToItem, next.ExceptionToRevision = store.ScopeGlobal, nil, nil, nil
	case store.ScopeWorkspace:
		// The new target is the caller's own workspace, so it passes the scope check.
		if cl.keyErr != nil {
			return store.ItemRevision{}, cl.keyErr
		}
		next.Scope, next.WorkspaceKey = store.ScopeWorkspace, &cl.key
	}
	return next, confirmed(r, p, cur, next)
}

// skillError asks for a new name when the store refused a write because another active skill
// or tool in the scope has the name (§8.2, §10.1). The transaction wrote nothing.
func skillError(err error) error {
	switch {
	case errors.Is(err, store.ErrSkillName):
		return fmt.Errorf("%w: give this skill a new name (approve with --edit and a new name: line); nothing was written", err)
	case errors.Is(err, store.ErrToolName):
		return fmt.Errorf("%w: give this tool a new name (approve with --edit and a new name = line); nothing was written", err)
	case errors.Is(err, store.ErrBindingRefused):
		return fmt.Errorf("%w; nothing was written", err)
	}
	return err
}

// checkBinding refuses next, the revision that makes automation it active, when its binding
// breaks a rule of §10.4 (store.CheckBinding): your approval covers this exact binding only.
func (cl caller) checkBinding(ctx context.Context, tx *sql.Tx, it store.Item, next store.ItemRevision) error {
	if it.Kind != store.KindAutomation || next.State != store.StateActive {
		return nil
	}
	b, err := store.ParseBinding(next.Text)
	if err != nil {
		return fmt.Errorf("%w: %w", store.ErrBindingRefused, err)
	}
	if next.Scope != store.ScopeWorkspace || next.WorkspaceKey == nil {
		return fmt.Errorf("an automation is a workspace item: %w", store.ErrBindingRefused)
	}
	return store.CheckBinding(ctx, tx, b, *next.WorkspaceKey, lookupEnv(cl.Environ, "HOME"))
}

// errConfirm: approve needs --confirm.
var errConfirm = errors.New("may change a permission, an allow rule or a safety rule")

// confirmed checks that you gave --confirm when approving p may change a permission, an allow
// rule or a safety rule (§7.4). The revision's text counts too, as an --edit can add a keyword
// or remove the read-only restriction of the tool at revision cur.
func confirmed(r review, p store.Proposal, cur, next store.ItemRevision) error {
	warn := p.PermissionWarning || permissionWords.MatchString(next.Text) || tool.DropsRestriction(cur.Text, next.Text)
	if warn && !r.confirm {
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
		if len(p.Dispute) > 0 {
			return fmt.Errorf("proposal %d is a dispute: decide it with --keep, --approve --edit \"<text>\" or --retire; nothing was written", id)
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
	gates := cl.gates()
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
				localTime(c.LastSeen), gateLabel(gates, c), c.Status, c.ClaimID, text))
			ps, err := cl.candidateProposals(ctx, tx, c.ID)
			if err != nil {
				return err
			}
			lines = append(lines, ps...)
			hint, err := makeToolHint(ctx, tx, c, len(ps))
			if err != nil {
				return err
			}
			lines = append(lines, hint...)
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

// makeToolHint is the line of the review action "make this a tool" (§10.1) for candidate c with
// n pending proposals that the caller sees: only for an open command candidate with commands in
// its evidence, so not for the learned automation case (E17-S10), whose evidence is stops.
func makeToolHint(ctx context.Context, tx *sql.Tx, c store.Candidate, n int) ([]string, error) {
	if c.Kind != store.CandidateCommandChain || c.Status != store.CandidateOpen || n > 0 {
		return nil, nil
	}
	cmds, err := store.CandidateCommands(ctx, tx, c.ID)
	if err != nil || len(cmds) == 0 {
		return nil, err
	}
	return []string{fmt.Sprintf("  make this a tool: brain review --tool %d", c.ID)}, nil
}

// makeTool runs the review action "make this a tool" on command candidate id that the caller
// sees (extract.MakeTool): a tool draft from one example, for your review.
func (cl caller) makeTool(ctx context.Context, id int64) error {
	var pid int64
	err := cl.Store.Write(ctx, func(tx *sql.Tx) error {
		c, err := store.CandidateByID(ctx, tx, id)
		switch {
		case errors.Is(err, sql.ErrNoRows) || err == nil && !cl.sees(scopeKey(c.Scope, c.WorkspaceKey)):
			return fmt.Errorf("candidate %d: %w", id, errNotFound)
		case err != nil:
			return err
		}
		pid, err = extract.MakeTool(ctx, tx, id, cl.Now())
		return err
	})
	if err != nil {
		return fmt.Errorf("%w; nothing was written", err)
	}
	_, err = fmt.Fprintf(cl.Stdout, "proposal %d: a tool draft from candidate %d, waiting for your review (brain review %d)\n"+
		"for an input in place of a word, ask for a new draft: brain review %d --change \"<text>\"\n", pid, id, pid, pid)
	return err
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
	return oneLine(brief.ClaimText(claim.Key.Situation, claim.Key.Rule)), nil
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
				r.ID, cl.target(r.Scope), localTime(r.At), r.New, r.Evidence, r.EvidenceCount, r.ClaimID, text, reason))
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
	err := cl.Store.Write(ctx, func(tx *sql.Tx) (err error) {
		rev, err = cl.restoreItem(ctx, tx, id)
		return err
	})
	if err != nil {
		return skillError(err)
	}
	_, err = fmt.Fprintf(cl.Stdout, "item %d restored at revision %d\n", id, rev)
	return errors.Join(err, cl.deliver(ctx, nil))
}

// restoreItem makes retired item id active again with a new revision, and returns it.
func (cl caller) restoreItem(ctx context.Context, tx *sql.Tx, id int64) (int64, error) {
	it, cur, err := cl.item(ctx, tx, id)
	if err != nil {
		return 0, err
	}
	if it.State != store.StateRetired {
		return 0, fmt.Errorf("item %d is %s, not retired", id, it.State)
	}
	next := nextRevision(cur, cl.Now())
	next.State = store.StateActive
	if err := cl.checkBinding(ctx, tx, it, next); err != nil {
		return 0, err
	}
	if _, err := store.WriteRevision(ctx, tx, it.CurrentRevision, next); err != nil {
		return 0, err
	}
	return it.CurrentRevision + 1, anchor(ctx, tx, next)
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
		switch {
		case r.edit != nil && it.Kind == store.KindSkill:
			// A skill patch: its diff holds only the changed lines (§9.2). Approve writes the
			// whole text, so a text that is not a skill would only wait in the queue.
			if _, err := store.ParseSkill(*r.edit); err != nil {
				return fmt.Errorf("item %d is a skill: %w; nothing was written", it.ID, err)
			}
			p.Text, p.Diff = *r.edit, store.SkillDiff(cur.Text, *r.edit)
		case r.edit != nil && it.Kind == store.KindTool:
			// A tool change: approve writes the whole contract, so a contract that breaks a
			// rule would only wait in the queue (§10.1).
			if _, err := tool.Parse(*r.edit); err != nil {
				return fmt.Errorf("item %d is a tool: %w; nothing was written", it.ID, err)
			}
			p.Text, p.Diff = *r.edit, store.SkillDiff(cur.Text, *r.edit)
		case r.edit != nil:
			p.Text, p.Diff = *r.edit, p.Diff+"\n+ "+*r.edit
		}
		// The diff holds the old text too: a retire or an edit that removes a safety rule
		// changes it (§7.4).
		p.PermissionWarning = permissionWords.MatchString(p.Diff) || tool.DropsRestriction(cur.Text, p.Text)
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
		if p.Script() {
			return fmt.Errorf("proposal %d is a script: approve it and edit it when you install it, or reject it; nothing was written", id)
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
