package extract

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

// minScript is the fewest commands of a chain that gets a script proposal: a script is for a
// long command chain, and one command is no chain.
const minScript = 2

// scriptText is the script of chain cmds, an exact repeat that exactDraft accepted: a POSIX sh
// script that stops at the first failure, as the steps of a tool do, and runs the commands as
// they ran in the folder where they ran. The words of each command hold no shell character
// (toolWords), so they need no quotes; the folder gets them. It returns "" for a chain shorter
// than minScript.
func scriptText(cmds []store.CommandRecord) string {
	if len(cmds) < minScript {
		return ""
	}
	runs := make([]string, len(cmds))
	for i, c := range cmds {
		runs[i] = strings.Join(strings.Fields(c.Argv), " ")
	}
	var b strings.Builder
	fmt.Fprintf(&b, "#!/bin/sh\n# %s: run %s\n", draftName(strings.Fields(cmds[0].Argv)), strings.Join(runs, ", then "))
	b.WriteString("# Brain does not install this script: save it where you want it and make it executable.\n")
	fmt.Fprintf(&b, "set -e\ncd '%s'\n%s\n", strings.ReplaceAll(cmds[0].CWD, "'", `'\''`), strings.Join(runs, "\n"))
	return b.String()
}

// proposeScript makes the script proposal with text of candidate id: a proposal with no item,
// as the data model has no script kind, with the script text and its diff from no file. Brain
// offers it beside the pending tool draft of the same chain, and you pick one. It makes nothing
// when the candidate had a script proposal, or when you approved one of its proposals, as then
// you picked already; nor when the candidate has no pending tool draft, so a forget of the draft
// item erases the script too; nor when you rejected its claim in its scope (E12-S3).
func proposeScript(ctx context.Context, tx *sql.Tx, id int64, text string, now time.Time) error {
	offered, err := store.ScriptOffered(ctx, tx, id)
	if err != nil || offered {
		return err
	}
	if draft, err := toolDraftOf(ctx, tx, id); err != nil || draft == 0 {
		return err
	}
	cand, err := store.CandidateByID(ctx, tx, id)
	if err != nil {
		return err
	}
	_, err = store.ProposeCandidate(ctx, tx, id, store.Proposal{Text: text, Scope: cand.Scope, Target: store.ScriptTarget,
		Diff: store.SkillDiff("", text), ExpectedBenefit: "runs these commands with one call of your own script",
		PermissionWarning: store.TouchesPermission(text), Status: store.ProposalPending, Created: now})
	if errors.Is(err, store.ErrRejected) {
		return nil
	}
	return err
}

// toolDraftOf returns the pending proposal of a draft tool item of candidate id, or 0 for none.
func toolDraftOf(ctx context.Context, tx *sql.Tx, id int64) (int64, error) {
	pending, err := store.PendingCandidateProposalIDs(ctx, tx, id)
	if err != nil {
		return 0, err
	}
	for _, pid := range pending {
		p, err := store.ProposalByID(ctx, tx, pid)
		if err != nil {
			return 0, err
		}
		if p.ItemID == nil {
			continue
		}
		it, err := store.ItemByID(ctx, tx, *p.ItemID)
		if err != nil {
			return 0, err
		}
		if it.Kind == store.KindTool && it.State == store.StateDraft {
			return pid, nil
		}
	}
	return 0, nil
}
