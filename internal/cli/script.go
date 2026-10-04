package cli

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

// scriptSummary is the one-line form of script proposal p for a list: the second line of the
// script, which names it and its commands.
func scriptSummary(p store.Proposal) string {
	_, rest, _ := strings.Cut(p.Text, "\n")
	line, _, _ := strings.Cut(rest, "\n")
	return "script " + strings.TrimPrefix(line, "# ")
}

// scriptLines are the kind line, the text and the claim of script proposal p in show (§10.4):
// Brain installs no script, so approve records your decision only.
func scriptLines(p store.Proposal, claim string) string {
	return "kind:     script\n" + "text:\n  " + strings.ReplaceAll(strings.TrimSuffix(p.Text, "\n"), "\n", "\n  ") + "\n" +
		"install:  Brain writes no file and changes no permission: approve records your decision, and you install the script yourself\n" +
		fmt.Sprintf("claim:    %s\n", claim)
}

// siblings returns the other pending proposals of the candidate of p that you pick between
// with p [NEW]: the script proposal of a tool draft, or the tool draft of a script proposal,
// from the same command chain (E17-S8).
func siblings(ctx context.Context, tx *sql.Tx, p store.Proposal) ([]store.Proposal, error) {
	if p.CandidateID == nil {
		return nil, nil
	}
	ids, err := store.PendingCandidateProposalIDs(ctx, tx, *p.CandidateID)
	if err != nil {
		return nil, err
	}
	var out []store.Proposal
	for _, id := range ids {
		o, err := store.ProposalByID(ctx, tx, id)
		if err != nil {
			return nil, err
		}
		if o.Script() != p.Script() {
			out = append(out, o)
		}
	}
	return out, nil
}

// pickLines are the lines of show that offer sibs, the siblings of a proposal: you pick one.
func pickLines(sibs []store.Proposal) string {
	var b strings.Builder
	for _, o := range sibs {
		what := "a tool draft"
		if o.Script() {
			what = "a script"
		}
		fmt.Fprintf(&b, "pick one: proposal %d is %s of the same command chain; approve one, and the other is superseded\n", o.ID, what)
	}
	return b.String()
}

// supersede ends siblings sibs of an approved proposal as superseded: you picked the other
// one. It returns their ids.
func (cl caller) supersede(ctx context.Context, tx *sql.Tx, sibs []store.Proposal) ([]int64, error) {
	ids := make([]int64, 0, len(sibs))
	for _, o := range sibs {
		if err := store.DecideProposal(ctx, tx, o.ID, store.ProposalSuperseded, cl.surface, cl.Now()); err != nil {
			return nil, err
		}
		ids = append(ids, o.ID)
	}
	return ids, nil
}

// approveScript records your approval of script proposal p in tx (§10.4): it writes nothing
// outside the store, and no item, as Brain installs no script. The tool draft of the same chain
// is superseded: you picked the script. It returns the ids of the superseded proposals.
func (cl caller) approveScript(ctx context.Context, tx *sql.Tx, r review, p store.Proposal) ([]int64, error) {
	switch {
	case r.edit != nil || r.scope != "" || r.pin || r.conflict != "":
		return nil, fmt.Errorf("proposal %d is a script: approve it as it is and edit it when you install it, or reject it; nothing was written", p.ID)
	case warns(p) && !r.confirm:
		return nil, fmt.Errorf("proposal %d %w: read it with brain review %d, then approve with --confirm; nothing was written",
			p.ID, errConfirm, p.ID)
	}
	sibs, err := siblings(ctx, tx, p)
	if err != nil {
		return nil, err
	}
	if err := store.DecideProposal(ctx, tx, p.ID, store.ProposalApproved, cl.surface, cl.Now()); err != nil {
		return nil, err
	}
	if err := store.DecideSuggestions(ctx, tx, p.ID, store.SuggestionApproved); err != nil {
		return nil, err
	}
	return cl.supersede(ctx, tx, sibs)
}

// supersededLines prints one line for each proposal of ids that an approve superseded.
func (cl caller) supersededLines(ids []int64) error {
	for _, id := range ids {
		if _, err := fmt.Fprintf(cl.Stdout, "proposal %d of the same command chain superseded: you picked this one\n", id); err != nil {
			return err
		}
	}
	return nil
}
