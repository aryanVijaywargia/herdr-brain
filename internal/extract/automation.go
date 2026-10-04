package extract

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"github.com/aryanVijaywargia/herdr-brain/internal/store"
	"github.com/aryanVijaywargia/herdr-brain/internal/tool"
)

// automations runs the minimal learned automation case (§10.4, E17-S10): the same approved tool
// runs in its folder after a stop of the agent (store.StopCalls). Each occurrence is evidence of
// one claim in the workspace of the stop: this tool, this folder and the stop of this CLI. The
// claim is a command chain candidate, so its gate is 3 sessions (§7.2, E12-S2). At the gate it
// gives one automation proposal through the queue checks: learning on, the daily proposal cap,
// no pending proposal, no item and no rejection of the claim in its scope. It makes no model
// call. It is no general automation detector.
func (p Pass) automations(ctx context.Context, tx *sql.Tx) error {
	calls, err := store.StopCalls(ctx, tx, p.HerdrSession)
	if err != nil {
		return err
	}
	for _, c := range calls {
		if err := p.stopCall(ctx, tx, c); err != nil {
			return err
		}
	}
	return nil
}

// stopCall attaches occurrence c to the candidate of its claim, and proposes the binding when
// the candidate reaches its gate. A tool that a binding cannot take (it has a required input,
// or it is not approved now), or a CLI with no tested stop condition, gives nothing.
func (p Pass) stopCall(ctx context.Context, tx *sql.Tx, c store.StopCall) error {
	trigger, ok := store.StopTrigger(c.Harness)
	if !ok || c.WorkspaceKey == "" {
		return nil
	}
	on, err := store.LearningOn(ctx, tx, p.DefaultOn, p.HerdrSession, c.WorkspaceKey, c.PaneID)
	if err != nil || !on {
		return err
	}
	t, err := store.ToolByID(ctx, tx, c.ToolID)
	if err != nil {
		return err
	}
	ct, err := tool.Parse(t.Contract)
	if err != nil || len(ct.Inputs) > 0 || t.ApprovedHash == nil || *t.ApprovedHash != t.ContractHash {
		return nil //nolint:nilerr // a forgotten or refused contract is no tool that a binding can take
	}
	b := store.Binding{Tool: t.Name, ToolID: t.ID, ContractHash: t.ContractHash, Folder: filepath.Clean(ct.Cwd),
		CLI: c.Harness, Trigger: trigger}
	// [NEW] the claim key of the learned case.
	key := store.ClaimKey{Situation: normal(fmt.Sprintf("the %s agent stops working in %s", b.CLI, b.Folder)),
		Rule: normal("run brain tool " + b.Tool)}
	claim, err := store.ClaimID(ctx, tx, key, p.Now())
	if err != nil {
		return err
	}
	if ts, err := store.TombstonesByClaim(ctx, tx, claim); err != nil || len(ts) > 0 {
		return err
	}
	cand, err := store.Attach(ctx, tx, store.Candidate{ClaimID: claim, Scope: store.ScopeWorkspace,
		WorkspaceKey: c.WorkspaceKey, Kind: store.CandidateCommandChain}, c.Stop, p.gates(), stopSupports)
	switch {
	case errors.Is(err, store.ErrEvidenceRefused):
		return nil
	case err != nil:
		return err
	}
	return p.proposeBinding(ctx, tx, cand, b)
}

// stopSupports says that any sender of a stop observation supports the learned case: the tool
// call is the act, and the stop only times it.
func stopSupports(store.CandidateKind, store.Sender) bool { return true }

// proposeBinding makes the automation proposal of candidate id with binding b, when the
// candidate reached its gate and the checks of propose pass, and no active binding of the same
// tool, folder and CLI is in the workspace. The daily cap of new proposals holds it back too.
func (p Pass) proposeBinding(ctx context.Context, tx *sql.Tx, id int64, b store.Binding) error {
	cand, err := store.CandidateByID(ctx, tx, id)
	if err != nil || !p.gates().Reached(cand) {
		return err
	}
	if pending, err := store.PendingCandidateProposalIDs(ctx, tx, id); err != nil || len(pending) > 0 {
		return err
	}
	if has, err := store.ClaimHasItem(ctx, tx, cand.ClaimID, cand.WorkspaceKey); err != nil || has {
		return err
	}
	if held, err := store.Rejected(ctx, tx, cand.ClaimID, cand.WorkspaceKey); err != nil || held {
		return err
	}
	as, err := store.ActiveAutomations(ctx, tx)
	if err != nil {
		return err
	}
	if slices.ContainsFunc(as, func(a store.Automation) bool {
		return a.WorkspaceKey == cand.WorkspaceKey && a.ToolID == b.ToolID && a.Folder == b.Folder && a.CLI == b.CLI
	}) {
		return nil
	}
	if d, err := p.Day(ctx, tx); err != nil || d.Proposals >= d.Caps.Proposals {
		return err
	}
	target, err := target(ctx, tx, cand)
	switch {
	case errors.Is(err, sql.ErrNoRows): // the item needs the workspace row: the next pass tries again
		return nil
	case err != nil:
		return err
	}
	now := p.Now()
	item, err := store.InsertItem(ctx, tx, store.Item{ClaimID: cand.ClaimID, CurrentRevision: 1,
		Kind: store.KindAutomation, Scope: store.ScopeWorkspace, WorkspaceKey: &cand.WorkspaceKey,
		State: store.StateDraft, Created: now})
	if err != nil {
		return err
	}
	text, base := b.Text(), int64(1)
	pid, err := store.ProposeCandidate(ctx, tx, id, store.Proposal{ItemID: &item, BaseRevision: &base, Text: text,
		Scope: store.ScopeWorkspace, Target: target, Diff: diffOf(text),
		ExpectedBenefit: b.Benefit(), Status: store.ProposalPending, Created: now})
	if err != nil {
		return err
	}
	if err := store.InsertItemRevision(ctx, tx, store.ItemRevision{ItemID: item, Revision: 1, Text: text,
		Scope: store.ScopeWorkspace, WorkspaceKey: &cand.WorkspaceKey, State: store.StateDraft,
		Author: store.AuthorBrain, Origin: store.OriginExtractor, ProposalID: &pid, Created: now}); err != nil {
		return err
	}
	return store.MarkProposed(ctx, tx, id)
}

// diffOf is the diff of a new item with text: each line with "+ ".
func diffOf(text string) string {
	return "+ " + strings.ReplaceAll(strings.TrimSuffix(text, "\n"), "\n", "\n+ ")
}
