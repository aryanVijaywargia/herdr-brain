// Package extract runs the extractor of the learning pipeline (HLD §7.3): it forms the batches
// of the finished sessions, calls the extractor CLI outside any transaction, and commits each
// result with the new watermark in one transaction (G3).
package extract

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/aryanVijaywargia/herdr-brain/internal/secret"
	"github.com/aryanVijaywargia/herdr-brain/internal/signal"
	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

const (
	// idle is how long a session with work stays quiet before it is finished (§7.3).
	idle = 10 * time.Minute
	// retryAfter is the wait before a failed or invalid batch is tried again. [NEW] 1 hour: the
	// evidence waits, and brain status shows the failure.
	retryAfter = time.Hour
)

// Pass is one extraction pass over the sessions of one herdr session in one store.
type Pass struct {
	Store        *store.Store
	Runner       Runner
	HerdrSession string // the binding: the pass reads only its sessions
	DefaultOn    bool   // learning with no store setting: false for a test store (§12.1)
	Now          func() time.Time
	Gates        store.Gates // nil for store.DefaultGates
}

// ref is one observation of the input of a batch, with its ref id in the prompt.
type ref struct {
	id        string // "e" and the observation id
	obs       int64
	identity  string
	workspace string // the Brain workspace key of the observation
	sender    store.Sender
	excerpt   string
	kind      signal.Kind // the kind of the signal of the observation
}

// job is one batch to extract: its stable id and start watermark, and its input. The known
// claims and the brief items come from the workspace key of the batch and global (§7.3).
type job struct {
	batch   store.Batch
	session store.Session
	refs    []ref
	known   []store.Claim        // the claims that the input names by claim id
	brief   []store.ItemRevision // the current brief items
}

// Run runs the pass: it forms the batches of the finished sessions, and extracts and commits
// them one after the other, so a session has one active batch at a time. A failed batch does
// not stop the others.
func (p Pass) Run(ctx context.Context) error {
	var jobs []job
	err := p.Store.Write(ctx, func(tx *sql.Tx) (err error) {
		jobs, err = p.plan(ctx, tx)
		return err
	})
	if err != nil {
		return err
	}
	var errs []error
	for _, j := range jobs {
		if err := p.extract(ctx, j); err != nil {
			if ctx.Err() != nil {
				return err
			}
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (p Pass) gates() store.Gates {
	if p.Gates == nil {
		return store.DefaultGates()
	}
	return p.Gates
}

// plan returns the batches to extract now. A session is finished when it ended (pane.exited),
// or when its newest observation is idle old. Its batch for a workspace key is the part after
// the watermark of the first run of observations with that key: a session that moved from A to
// B gives a batch for A and one for B. A run with no signal needs no call: plan records an
// empty batch and moves the watermark. The candidates expire with the wait of the oldest
// evidence that waits for extraction (§7.2).
func (p Pass) plan(ctx context.Context, tx *sql.Tx) ([]job, error) {
	now := p.Now()
	ins, err := store.SignalInputs(ctx, tx)
	if err != nil {
		return nil, err
	}
	sigs := signal.Find(ins)
	byID := map[int64]store.SignalInput{}
	bySession := map[int64][]store.SignalInput{}
	for _, in := range ins {
		byID[in.ID] = in
		bySession[in.SessionID] = append(bySession[in.SessionID], in)
	}
	sessions, err := store.Sessions(ctx, tx)
	if err != nil {
		return nil, err
	}
	var jobs []job
	var oldest time.Time
	for _, s := range sessions {
		obs := bySession[s.ID]
		if s.Key.HerdrSession != p.HerdrSession || len(obs) == 0 {
			continue
		}
		slices.SortFunc(obs, func(a, b store.SignalInput) int { return int(a.ID - b.ID) })
		if s.Ended.IsZero() && now.Sub(obs[len(obs)-1].At) < idle {
			continue
		}
		wm, err := store.Watermarks(ctx, tx, s.ID)
		if err != nil {
			return nil, err
		}
		for _, run := range uncovered(obs, wm) {
			if oldest.IsZero() || run[0].At.Before(oldest) {
				oldest = run[0].At
			}
			j, ok, err := p.batch(ctx, tx, s, run, wm[run[0].WorkspaceKey], sigs, byID)
			if err != nil {
				return nil, err
			}
			if ok {
				jobs = append(jobs, j)
			}
		}
	}
	var queued time.Duration
	if !oldest.IsZero() {
		queued = now.Sub(oldest)
	}
	if _, err := store.ExpireCandidates(ctx, tx, now, queued); err != nil {
		return nil, err
	}
	return jobs, nil
}

// uncovered returns, for each workspace key of obs (in id order), the part after the watermark
// of its first run that has one. Observations with no Brain key belong to no batch.
func uncovered(obs []store.SignalInput, wm map[string]int64) [][]store.SignalInput {
	var runs [][]store.SignalInput
	seen := map[string]bool{}
	for i := 0; i < len(obs); {
		key := obs[i].WorkspaceKey
		var run []store.SignalInput
		for ; i < len(obs) && obs[i].WorkspaceKey == key; i++ {
			if obs[i].ID > wm[key] {
				run = append(run, obs[i])
			}
		}
		if key != "" && len(run) > 0 && !seen[key] {
			seen[key] = true
			runs = append(runs, run)
		}
	}
	return runs
}

// batch returns the job of run, the uncovered part of one run of session s, with start
// watermark start. It returns false when the batch waits: extraction or learning is off for
// its key, no CLI is chosen, or it failed less than retryAfter ago. A run with no signal is
// recorded as an empty batch here.
func (p Pass) batch(ctx context.Context, tx *sql.Tx, s store.Session, run []store.SignalInput, start int64,
	sigs []signal.Signal, byID map[int64]store.SignalInput) (job, bool, error) {
	last := run[len(run)-1]
	j := job{session: s, batch: store.Batch{SessionID: s.ID, WorkspaceKey: last.WorkspaceKey,
		FirstCapture: run[0].CaptureIdentity, LastCapture: last.CaptureIdentity, StartWatermark: start,
		LastObservation: last.ID}}
	if on, err := p.allowed(ctx, tx, j); err != nil || !on {
		return job{}, false, err
	}
	j.refs = refs(run, sigs, byID)
	if len(j.refs) == 0 {
		if err := store.EmptyBatch(ctx, tx, j.batch, p.Now()); err != nil {
			return job{}, false, err
		}
		return job{}, false, store.MoveWatermark(ctx, tx, j.batch)
	}
	old, err := store.FindBatch(ctx, tx, j.batch)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return j, true, nil
	case err != nil:
		return job{}, false, err
	case old.Done():
		return job{}, false, nil
	case (old.Status == store.BatchFailed || old.Status == store.BatchInvalid) && p.Now().Sub(old.At) < retryAfter:
		return job{}, false, nil
	}
	return j, true, nil
}

// allowed reports whether batch j may call the extractor: extraction is on for its workspace
// key, a CLI is chosen, and learning is on for its session, key and pane (§7.3).
func (p Pass) allowed(ctx context.Context, tx *sql.Tx, j job) (bool, error) {
	key := j.batch.WorkspaceKey
	if on, err := store.ExtractionOn(ctx, tx, key); err != nil || !on {
		return false, err
	}
	s, err := store.Extractor(ctx, tx)
	if err != nil || s.CLI == "" {
		return false, err
	}
	return store.LearningOn(ctx, tx, p.DefaultOn, j.session.Key.HerdrSession, key, j.session.PaneID)
}

// refs returns the input of run: the observations in run of each signal that starts in run,
// by id. An observation outside run is the input of its own batch, so nothing is extracted
// twice (§7.3); its claim id still joins the evidence of both batches.
func refs(run []store.SignalInput, sigs []signal.Signal, byID map[int64]store.SignalInput) []ref {
	in := func(id int64) bool {
		return slices.ContainsFunc(run, func(o store.SignalInput) bool { return o.ID == id })
	}
	kinds := map[int64]signal.Kind{}
	for _, s := range sigs {
		if len(s.Observations) == 0 || !in(s.Observations[0]) {
			continue
		}
		for i, id := range s.Observations {
			if in(id) {
				if _, seen := kinds[id]; !seen || i == 0 {
					kinds[id] = s.Kind
				}
			}
		}
	}
	var rs []ref
	for id, k := range kinds {
		o := byID[id]
		rs = append(rs, ref{id: fmt.Sprintf("e%d", id), obs: id, identity: o.CaptureIdentity, workspace: o.WorkspaceKey,
			sender: o.Sender, excerpt: o.Excerpt, kind: k})
	}
	slices.SortFunc(rs, func(a, b ref) int { return int(a.obs - b.obs) })
	return rs
}

// extract makes j the active batch of its session, calls the extractor outside any
// transaction and commits the result. A cancel during the call leaves the batch active: the
// next pass tries it again, as after a crash.
func (p Pass) extract(ctx context.Context, j job) error {
	var b store.Batch
	var s store.ExtractorSetting
	skip := false
	err := p.Store.Write(ctx, func(tx *sql.Tx) error {
		on, err := p.allowed(ctx, tx, j)
		if err != nil || !on {
			skip = true
			return err
		}
		if wm, err := store.Watermark(ctx, tx, j.batch.SessionID, j.batch.WorkspaceKey); err != nil || wm != j.batch.StartWatermark {
			skip = true
			return err
		}
		if s, err = store.Extractor(ctx, tx); err != nil {
			return err
		}
		if j, err = scope(ctx, tx, j); err != nil {
			return err
		}
		b, err = store.StartBatch(ctx, tx, j.batch, p.Now())
		skip = b.Done()
		return err
	})
	if err != nil || skip {
		return err
	}
	out, err := call(ctx, p.Runner, s, input(j))
	if ctx.Err() != nil {
		return ctx.Err()
	}
	var claims []Claim
	if err == nil {
		claims, err = check(out, j)
	}
	return p.commit(ctx, j, b.ID, claims, err)
}

// commit is the one transaction of batch id (G3, §7.3). A batch that is no longer active adds
// nothing: a retry of a committed batch, or a batch that a newer one replaced. A failure or an
// invalid response keeps the watermark. Else it checks the settings, the watermark and the
// input again; a forget that touched the input discards the whole result. Then it writes the
// evidence links, candidates and proposals, and moves the watermark.
func (p Pass) commit(ctx context.Context, j job, id int64, claims []Claim, callErr error) error {
	return p.Store.Write(ctx, func(tx *sql.Tx) error {
		now := p.Now()
		b, err := store.BatchByID(ctx, tx, id)
		if err != nil || b.Status != store.BatchActive {
			return err
		}
		if callErr != nil {
			status := store.BatchFailed
			if errors.Is(callErr, ErrInvalid) {
				status = store.BatchInvalid
			}
			_, err := store.EndBatch(ctx, tx, id, status, callErr.Error(), now)
			return err
		}
		reason, err := p.stale(ctx, tx, j)
		if err != nil {
			return err
		}
		if reason != "" {
			_, err := store.EndBatch(ctx, tx, id, store.BatchDiscarded, reason, now)
			return err
		}
		for _, c := range claims {
			if err := p.write(ctx, tx, j, c, now); err != nil {
				return err
			}
		}
		status := store.BatchCommitted
		if len(claims) == 0 {
			status = store.BatchEmpty
		}
		if _, err := store.EndBatch(ctx, tx, id, status, "", now); err != nil {
			return err
		}
		return store.MoveWatermark(ctx, tx, j.batch)
	})
}

// stale returns why the result of j must be discarded, or "": extraction or learning went off,
// the watermark moved, a forget removed an observation of the input, or the claim keys or the
// brief items of the input changed, as a forget of an item does (§6.7, §7.3).
func (p Pass) stale(ctx context.Context, tx *sql.Tx, j job) (string, error) {
	if on, err := p.allowed(ctx, tx, j); err != nil || !on {
		return "extraction or learning is off", err
	}
	wm, err := store.Watermark(ctx, tx, j.batch.SessionID, j.batch.WorkspaceKey)
	if err != nil {
		return "", err
	}
	if wm != j.batch.StartWatermark {
		return "the watermark moved", nil
	}
	for _, r := range j.refs {
		held, err := store.ObservationHeld(ctx, tx, r.obs, r.identity)
		if err != nil {
			return "", err
		}
		if !held {
			return "a forget removed input of the batch", nil
		}
	}
	cur, err := scope(ctx, tx, j)
	if err != nil {
		return "", err
	}
	if input(cur) != input(j) {
		return "the claim keys or the brief of the input changed", nil
	}
	return "", nil
}

// untombstoned returns the refs whose evidence no tombstone names.
func untombstoned(ctx context.Context, tx *sql.Tx, refs []ref) ([]ref, error) {
	var kept []ref
	for _, r := range refs {
		stopped, err := store.Tombstoned(ctx, tx, r.identity)
		if err != nil {
			return nil, err
		}
		if !stopped {
			kept = append(kept, r)
		}
	}
	return kept, nil
}

// write stores claim c of batch j: its claim id, and each ref as evidence of the candidate of
// the claim in its scope. A tombstoned claim or ref adds nothing (§6.7): a
// forget erases the claim key, so the tombstone source keys stop the same evidence. A known
// claim (same_as, or the same claim id) gets the evidence first; propose then drops the
// duplicate proposal (§7.3). A candidate that reaches its gate gets a proposal.
func (p Pass) write(ctx context.Context, tx *sql.Tx, j job, c Claim, now time.Time) error {
	kept, err := untombstoned(ctx, tx, c.Refs)
	if c.Refs = kept; err != nil || len(kept) == 0 {
		return err
	}
	c.Situation, c.Rule, c.ExpectedBenefit = redact(c.Situation), redact(c.Rule), redact(c.ExpectedBenefit)
	// A same_as claim is a known claim of the input: its evidence goes to the candidate of that
	// claim id. Its reworded key is not stored, so a forget of the known claim's item leaves no
	// copy of the key text (§6.7).
	claim := c.SameAs
	if claim == "" {
		claim, err = store.ClaimID(ctx, tx, store.ClaimKey{Situation: normal(c.Situation), Rule: normal(c.Rule)}, now)
		if err != nil {
			return err
		}
	}
	if ts, err := store.TombstonesByClaim(ctx, tx, claim); err != nil || len(ts) > 0 {
		return err
	}
	key := j.batch.WorkspaceKey
	if c.Scope == store.ScopeGlobal {
		key = ""
	}
	kind := candidateKind(c)
	switch old, err := store.CandidateOf(ctx, tx, claim, key); {
	case err == nil:
		kind = old.Kind
	case !errors.Is(err, sql.ErrNoRows):
		return err
	}
	var cand int64
	for _, r := range c.Refs {
		id, err := store.Attach(ctx, tx, store.Candidate{ClaimID: claim, Scope: c.Scope, WorkspaceKey: key, Kind: kind},
			r.obs, p.gates(), signal.Supports)
		switch {
		case errors.Is(err, store.ErrEvidenceRefused):
			continue
		case err != nil:
			return err
		}
		cand = id
	}
	if cand == 0 {
		return nil
	}
	return p.propose(ctx, tx, cand, c, now)
}

// candidateKind is the candidate kind of claim c: the kind of the first signal of its evidence
// that is a candidate kind. Else [NEW] a procedure claim is a procedure, and any other claim a
// correction.
func candidateKind(c Claim) store.CandidateKind {
	for _, r := range c.Refs {
		switch r.kind {
		case signal.Correction, signal.Procedure, signal.Failure, signal.CommandChain:
			return store.CandidateKind(r.kind)
		}
	}
	if c.Kind == "procedure" {
		return store.CandidateProcedure
	}
	return store.CandidateCorrection
}

// propose makes a proposal of candidate id when it reached its gate and its claim has no
// pending proposal, no item and no rejection in its scope (§7.2, §7.3). The proposal goes
// through store.ProposeCandidate. Its draft item gets the scope and workspace key of the
// candidate. E13-S5 adds the rest of the §7.4 fields.
func (p Pass) propose(ctx context.Context, tx *sql.Tx, id int64, c Claim, now time.Time) error {
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
	var key *string
	if cand.Scope == store.ScopeWorkspace {
		key = &cand.WorkspaceKey
	}
	target, err := target(ctx, tx, cand)
	switch {
	case errors.Is(err, sql.ErrNoRows): // the item needs the workspace row: the next evidence tries again
		return nil
	case err != nil:
		return err
	}
	kind := store.KindPreference // [NEW] E13-S5 sets the item kind
	if c.Kind == "fact" {
		kind = store.KindFact
	}
	item, err := store.InsertItem(ctx, tx, store.Item{ClaimID: cand.ClaimID, CurrentRevision: 1, Kind: kind,
		Scope: cand.Scope, WorkspaceKey: key, State: store.StateDraft, Created: now})
	if err != nil {
		return err
	}
	text := c.Situation + ": " + c.Rule // [NEW] E13-S5 sets the proposal text
	base := int64(1)
	pid, err := store.ProposeCandidate(ctx, tx, id, store.Proposal{ItemID: &item, BaseRevision: &base, Text: text,
		Scope: cand.Scope, Target: target, Diff: "+ " + text, ExpectedBenefit: c.ExpectedBenefit,
		PermissionWarning: c.TouchesPermission, TriggerPrompts: []string{}, Status: store.ProposalPending, Created: now})
	if err != nil {
		return err
	}
	if err := store.InsertItemRevision(ctx, tx, store.ItemRevision{ItemID: item, Revision: 1, Text: text,
		Scope: cand.Scope, WorkspaceKey: key, State: store.StateDraft, Author: store.AuthorBrain,
		Origin: store.OriginExtractor, ProposalID: &pid, Created: now}); err != nil {
		return err
	}
	return store.MarkProposed(ctx, tx, id)
}

// target names the brief part of candidate c, as brain remember does. It returns
// sql.ErrNoRows when the workspace of c has no row.
func target(ctx context.Context, tx *sql.Tx, c store.Candidate) (string, error) {
	if c.Scope == store.ScopeGlobal {
		return "global", nil
	}
	w, err := store.WorkspaceByKey(ctx, tx, c.WorkspaceKey)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("workspace %s (%s)", w.HerdrWorkspaceID, w.Label), nil
}

// redact replaces each secret that the secret filter finds in s with "[secret]" [NEW]: model
// text goes through the filter before Brain stores it.
func redact(s string) string {
	var b strings.Builder
	end := 0
	for _, h := range secret.Find(s) {
		if h.Start < end {
			end = max(end, h.End)
			continue
		}
		b.WriteString(s[end:h.Start])
		b.WriteString("[secret]")
		end = h.End
	}
	b.WriteString(s[end:])
	return b.String()
}

// normal is the normal form of a claim key text [NEW]: lower case with single spaces.
func normal(s string) string { return strings.Join(strings.Fields(strings.ToLower(s)), " ") }
