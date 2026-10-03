package cli

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/aryanVijaywargia/herdr-brain/internal/brief"
	"github.com/aryanVijaywargia/herdr-brain/internal/output"
	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

const promoteUsage = `usage: brain promote <item>`

// Promote runs `brain promote` with args after "promote" (§9.3, E16-S8). It makes one
// proposal to move a workspace item to the global part of the brief. Brain never makes one by
// itself; you approve it in review, where you remove the workspace details from its text.
func Promote(ctx context.Context, e Env, args []string) error {
	return onItem(ctx, e, "promote", promoteUsage, args, caller.promote)
}

// promote writes a promotion proposal of item id: its text is the current text, its scope is
// global. The item stays in its workspace until you approve it.
func (cl caller) promote(ctx context.Context, id int64) error {
	var pid int64
	err := cl.Store.Write(ctx, func(tx *sql.Tx) error {
		it, cur, err := cl.item(ctx, tx, id)
		if err != nil {
			return err
		}
		switch {
		case it.Scope == store.ScopeGlobal:
			return fmt.Errorf("item %d is global already; nothing was written", id)
		case it.State == store.StateDraft || it.State == store.StateRetired:
			return fmt.Errorf("item %d is %s; only an active item can be promoted; nothing was written", id, it.State)
		}
		if err := bar(ctx, tx, it); err != nil {
			return err
		}
		base := it.CurrentRevision
		// [NEW] the diff of a promotion names the scope change; the text is the same until you
		// edit it in review.
		pid, err = store.InsertProposal(ctx, tx, store.Proposal{ItemID: &it.ID, BaseRevision: &base, Text: cur.Text,
			Scope: store.ScopeGlobal, Target: cl.target(store.ScopeGlobal),
			Diff:              fmt.Sprintf("scope: %s -> global", cl.target(cur.Scope)),
			PermissionWarning: permissionWords.MatchString(cur.Text), Status: store.ProposalPending, Created: cl.Now()})
		return err
	})
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(cl.Stdout, "proposal %d: promote item %d to global, waiting for your review (brain review %d); remove the workspace details with --approve --edit \"<text>\"\n",
		pid, id, pid)
	return err
}

// promotion reports whether p moves item it to another scope: a promotion proposal (§9.3).
// Every other proposal has the scope of its item.
func promotion(p store.Proposal, it store.Item) bool { return p.Scope != it.Scope }

// bar checks item it against the bar for the always-on global part of the brief (§9.3). It
// returns the reason of the first check that fails. It runs on brain promote, and again on
// the approve that moves the item, as the item can change in between.
func bar(ctx context.Context, tx *sql.Tx, it store.Item) error {
	switch {
	case it.Kind == store.KindHandoff:
		// [NEW] check: a handoff is a plan for one workspace, not a lasting rule.
		return fmt.Errorf("item %d is a handoff: a plan for one workspace, not a lasting rule; it cannot go global; nothing was written", it.ID)
	case it.State == store.StateDisputed:
		return fmt.Errorf("item %d has an open dispute: decide it in brain review first; nothing was written", it.ID)
	}
	first, err := store.ItemRevisionByKey(ctx, tx, it.ID, 1)
	if err != nil {
		return err
	}
	// You wrote an item that brain remember drafted; the extractor drafts a Brain item.
	if first.Origin == store.OriginRemember {
		return nil
	}
	n, _, err := claimSeen(ctx, tx, it)
	if err != nil {
		return err
	}
	if n < 2 {
		return fmt.Errorf("item %d is a Brain item with %d piece(s) of evidence: the global part needs an item that you wrote, or 2 or more pieces of evidence; nothing was written",
			it.ID, n)
	}
	return nil
}

// claimSeen returns the evidence of the claim of workspace item it in its workspace, and the
// number of other workspaces with a candidate of the same claim id: the "also seen in N
// workspaces" hint (§9.3).
func claimSeen(ctx context.Context, tx *sql.Tx, it store.Item) (evidence, others int, err error) {
	cs, err := store.Candidates(ctx, tx)
	if err != nil {
		return 0, 0, err
	}
	for _, c := range cs {
		switch {
		case c.ClaimID != it.ClaimID || c.Scope != store.ScopeWorkspace:
		case c.WorkspaceKey == *it.WorkspaceKey:
			evidence += c.Count
		default:
			others++
		}
	}
	return evidence, others, nil
}

// checkPromotion checks the approve that moves item it to global: the bar (§9.3), and the
// budget of the global part with the new revision in it (§8.1). It runs after the write in
// the same transaction, so a refusal writes nothing.
func (cl caller) checkPromotion(ctx context.Context, tx *sql.Tx, it store.Item) error {
	if err := bar(ctx, tx, it); err != nil {
		return err
	}
	err := output.CheckGlobal(ctx, tx, filepath.Join(cl.Binding.Store, outDir))
	if be := (*brief.BudgetError)(nil); errors.As(err, &be) {
		return fmt.Errorf("item %d does not fit: %w; or shorten its text with --edit; nothing was written", it.ID, err)
	}
	return err
}
