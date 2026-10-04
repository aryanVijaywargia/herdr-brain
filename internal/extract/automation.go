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
// gives one automation proposal through the queue checks: learning on, the daily proposal cap
// in the round robin order of the workspaces (§7.3), no pending proposal, no item and no
// rejection of the claim in its scope. It makes no model call. It is no general automation
// detector.
func (p Pass) automations(ctx context.Context, tx *sql.Tx) error {
	calls, err := store.StopCalls(ctx, tx, p.HerdrSession)
	if err != nil {
		return err
	}
	var gated []bindingCandidate
	seen := map[int64]bool{}
	for _, c := range calls {
		bc, err := p.stopCall(ctx, tx, c)
		if err != nil {
			return err
		}
		if bc.id != 0 && !seen[bc.id] {
			seen[bc.id] = true
			gated = append(gated, bc)
		}
	}
	last, err := store.LastCalls(ctx, tx)
	if err != nil {
		return err
	}
	for _, bc := range roundRobin(gated, func(bc bindingCandidate) string { return bc.key }, last) {
		if err := p.proposeBinding(ctx, tx, bc.id, bc.b); err != nil {
			return err
		}
	}
	return nil
}

// bindingCandidate is a candidate of the learned case and the binding that it proposes.
type bindingCandidate struct {
	id  int64
	key string // the workspace key
	b   store.Binding
}

// stopCall attaches occurrence c to the candidate of its claim, and returns the candidate with
// its binding when the candidate reached its gate. Only a call that ran the current approved
// contract of the tool counts: its cwd is the folder of the binding, so a call of an older
// contract (another cwd, maybe) is no evidence. A tool that a binding cannot take (it has a
// required input, or it is not approved now), or a CLI with no tested stop condition, gives
// nothing.
func (p Pass) stopCall(ctx context.Context, tx *sql.Tx, c store.StopCall) (bindingCandidate, error) {
	trigger, ok := store.StopTrigger(c.Harness)
	if !ok || c.WorkspaceKey == "" {
		return bindingCandidate{}, nil
	}
	on, err := store.LearningOn(ctx, tx, p.DefaultOn, p.HerdrSession, c.WorkspaceKey, c.PaneID)
	if err != nil || !on {
		return bindingCandidate{}, err
	}
	t, err := store.ToolByID(ctx, tx, c.ToolID)
	if err != nil {
		return bindingCandidate{}, err
	}
	if t.ApprovedHash == nil || *t.ApprovedHash != t.ContractHash || c.ContractHash != t.ContractHash {
		return bindingCandidate{}, nil
	}
	ct, err := tool.Parse(t.Contract)
	if err != nil || len(ct.Inputs) > 0 {
		return bindingCandidate{}, nil //nolint:nilerr // a forgotten or refused contract is no tool that a binding can take
	}
	b := store.Binding{Tool: t.Name, ToolID: t.ID, ContractHash: t.ContractHash, Folder: filepath.Clean(ct.Cwd),
		CLI: c.Harness, Trigger: trigger}
	// [NEW] the claim key of the learned case.
	key := store.ClaimKey{Situation: normal(fmt.Sprintf("the %s agent stops working in %s", b.CLI, b.Folder)),
		Rule: normal("run brain tool " + b.Tool)}
	claim, err := store.ClaimID(ctx, tx, key, p.Now())
	if err != nil {
		return bindingCandidate{}, err
	}
	if ts, err := store.TombstonesByClaim(ctx, tx, claim); err != nil || len(ts) > 0 {
		return bindingCandidate{}, err
	}
	id, err := store.Attach(ctx, tx, store.Candidate{ClaimID: claim, Scope: store.ScopeWorkspace,
		WorkspaceKey: c.WorkspaceKey, Kind: store.CandidateCommandChain}, c.Stop, p.gates(), stopSupports)
	switch {
	case errors.Is(err, store.ErrEvidenceRefused):
		return bindingCandidate{}, nil
	case err != nil:
		return bindingCandidate{}, err
	}
	cand, err := store.CandidateByID(ctx, tx, id)
	if err != nil || !p.gates().Reached(cand) {
		return bindingCandidate{}, err
	}
	return bindingCandidate{id: id, key: c.WorkspaceKey, b: b}, nil
}

// stopSupports says that any sender of a stop observation supports the learned case: the tool
// call is the act, and the stop only times it.
func stopSupports(store.CandidateKind, store.Sender) bool { return true }

// proposeBinding makes the automation proposal of candidate id with binding b, when the checks
// of propose pass, and no active binding of the same tool, folder and CLI is in the workspace.
// The daily cap of new proposals holds it back too.
func (p Pass) proposeBinding(ctx context.Context, tx *sql.Tx, id int64, b store.Binding) error {
	cand, err := store.CandidateByID(ctx, tx, id)
	if err != nil {
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
