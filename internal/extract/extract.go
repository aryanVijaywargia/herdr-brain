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

	"github.com/aryanVijaywargia/herdr-brain/internal/brief"
	"github.com/aryanVijaywargia/herdr-brain/internal/secret"
	"github.com/aryanVijaywargia/herdr-brain/internal/signal"
	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

const (
	// idle is how long a session with work stays quiet before it is finished (§7.3).
	idle = 10 * time.Minute
	// retryAfter is the wait before a failed or invalid batch is tried again, unless a later
	// call works first (#263). [NEW] 1 hour: the evidence waits, and brain status shows the
	// failure.
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
	Caps         Caps        // the zero value for DefaultCaps
	// Suggest starts the review pane of a new lesson proposal after its commit (#253, §7.2);
	// nil for none.
	Suggest func(ctx context.Context, s Suggestion) error
}

// Suggestion is the reserved suggestion of a new lesson proposal (#253): the review pane opens
// beside the agent pane where you said the lesson, as for a repeated prompt (§5.5).
type Suggestion struct {
	store.Suggestion
	WorkspaceID string // the herdr workspace of the agent pane
}

// ref is one observation of the input of a batch, with its ref id in the prompt.
type ref struct {
	id        string // "e" and the observation id
	obs       int64
	identity  string
	workspace string // the Brain workspace key of the observation
	sender    store.Sender
	excerpt   string
	kind      signal.Kind          // the kind of the signal of the observation
	lesson    bool                 // the observation starts a lesson signal too (§7.1)
	cmd       *store.CommandRecord // the command record of the observation; nil for none
}

// job is one batch to extract: its stable id and start watermark, and its input. The known
// claims and the brief items come from the workspace key of the batch and global (§7.3).
type job struct {
	batch   store.Batch
	session store.Session
	refs    []ref
	known   []store.Claim        // the claims that the input names by claim id
	skills  map[string]string    // the name of each active skill of the scope, by claim id (E20-S5)
	brief   []store.ItemRevision // the current brief items
	change  *change              // a change request instead of a batch; nil for a batch
}

// Run runs the pass: it runs the learned automation case, forms the batches of the finished
// sessions and takes the change requests that wait, and extracts and commits them one after the other in round robin order across workspace keys, so a session has one
// active batch at a time. The caps stop the calls; the rest waits for a later pass (§7.3). A
// failed batch does not stop the others.
func (p Pass) Run(ctx context.Context) error {
	var jobs []job
	err := p.Store.Write(ctx, func(tx *sql.Tx) (err error) {
		// The learned automation case needs no model call (E17-S10).
		if err := p.automations(ctx, tx); err != nil {
			return err
		}
		if jobs, err = p.plan(ctx, tx); err != nil {
			return err
		}
		// A change request is its own entry of the queue (§7.4).
		reqs, err := p.requests(ctx, tx)
		if err != nil {
			return err
		}
		jobs = append(jobs, reqs...)
		last, err := store.LastCalls(ctx, tx)
		jobs = order(jobs, last)
		return err
	})
	if err != nil {
		return err
	}
	var errs []error
	for _, j := range jobs {
		run := p.extract
		if j.change != nil {
			run = p.runChange
		}
		if err := run(ctx, j); err != nil {
			if ctx.Err() != nil {
				return err
			}
			errs = append(errs, err)
		}
	}
	// The exact repeats of command chains need no model call (E17-S6). They run after the
	// batches, so a skill that the extractor made of the same chain stops a tool draft (§10.3).
	errs = append(errs, p.Store.Write(ctx, func(tx *sql.Tx) error { return p.exactTools(ctx, tx) }))
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
	runs, sigs, byID, err := p.finished(ctx, tx)
	if err != nil {
		return nil, err
	}
	var jobs []job
	var oldest time.Time
	for _, r := range runs {
		if oldest.IsZero() || r.obs[0].At.Before(oldest) {
			oldest = r.obs[0].At
		}
		j := p.job(r, sigs, byID)
		ok, err := p.batch(ctx, tx, j)
		if err != nil {
			return nil, err
		}
		if ok {
			jobs = append(jobs, j)
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

// pending is the uncovered part obs of one run of session s, after the watermark start.
type pending struct {
	s     store.Session
	obs   []store.SignalInput
	start int64
}

// finished returns the uncovered runs of the finished sessions of the pass, with the signals of
// the store and its signal inputs by id.
func (p Pass) finished(ctx context.Context, tx *sql.Tx) ([]pending, []signal.Signal, map[int64]store.SignalInput, error) {
	ins, err := store.SignalInputs(ctx, tx)
	if err != nil {
		return nil, nil, nil, err
	}
	byID := map[int64]store.SignalInput{}
	bySession := map[int64][]store.SignalInput{}
	for _, in := range ins {
		byID[in.ID] = in
		bySession[in.SessionID] = append(bySession[in.SessionID], in)
	}
	sessions, err := store.Sessions(ctx, tx)
	if err != nil {
		return nil, nil, nil, err
	}
	var runs []pending
	for _, s := range sessions {
		obs := bySession[s.ID]
		if s.Key.HerdrSession != p.HerdrSession || len(obs) == 0 {
			continue
		}
		slices.SortFunc(obs, func(a, b store.SignalInput) int { return int(a.ID - b.ID) })
		if s.Ended.IsZero() && p.Now().Sub(obs[len(obs)-1].At) < idle {
			continue
		}
		wm, err := store.Watermarks(ctx, tx, s.ID)
		if err != nil {
			return nil, nil, nil, err
		}
		for _, run := range uncovered(obs, wm) {
			runs = append(runs, pending{s: s, obs: run, start: wm[run[0].WorkspaceKey]})
		}
	}
	return runs, signal.Find(ins), byID, nil
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

// job returns the job of run r, cut to the input cap.
func (p Pass) job(r pending, sigs []signal.Signal, byID map[int64]store.SignalInput) job {
	run, rs := p.fit(r.obs, refs(r.obs, sigs, byID), sigs)
	last := run[len(run)-1]
	return job{session: r.s, refs: rs, batch: store.Batch{SessionID: r.s.ID, WorkspaceKey: last.WorkspaceKey,
		FirstCapture: run[0].CaptureIdentity, LastCapture: last.CaptureIdentity, StartWatermark: r.start,
		LastObservation: last.ID}}
}

// batch reports whether job j is extracted now. It returns false when the batch waits:
// extraction or learning is off for its key, no CLI is chosen, or its failed call still waits
// for the retry. A job with no signal is recorded as an empty batch here.
func (p Pass) batch(ctx context.Context, tx *sql.Tx, j job) (bool, error) {
	if on, err := p.allowed(ctx, tx, j); err != nil || !on {
		return false, err
	}
	if len(j.refs) == 0 {
		if err := store.EmptyBatch(ctx, tx, j.batch, p.Now()); err != nil {
			return false, err
		}
		return false, store.MoveWatermark(ctx, tx, j.batch)
	}
	old, err := store.FindBatch(ctx, tx, j.batch)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return true, nil
	case err != nil:
		return false, err
	case old.Done():
		return false, nil
	case old.Status == store.BatchFailed || old.Status == store.BatchInvalid:
		worked, err := store.LastWorkedCall(ctx, tx)
		return err == nil && !p.waits(old.At, worked), err
	}
	return true, nil
}

// waits reports whether a failed or invalid call that ended at at still waits for its retry:
// it ended less than retryAfter ago, and no extractor call worked after it. A call that works
// shows that the CLI works again, so the failed work goes at the next pass (#263).
func (p Pass) waits(at, worked time.Time) bool {
	return p.Now().Sub(at) < retryAfter && !worked.After(at)
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
	// A lesson is a mark beside the kind, so a lesson that is a correction too keeps its
	// correction kind for the comebacks (§5.4).
	lessons := map[int64]bool{}
	for _, s := range sigs {
		if len(s.Observations) == 0 || !in(s.Observations[0]) {
			continue
		}
		if s.Kind == signal.Lesson {
			lessons[s.Observations[0]] = true
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
	for id := range lessons {
		if _, seen := kinds[id]; !seen {
			kinds[id] = signal.Lesson
		}
	}
	var rs []ref
	for id, k := range kinds {
		o := byID[id]
		// The model copies the command of a tool draft whole: the excerpt is cut at 500
		// characters, the command record at 4 KB (§6.7).
		if o.Command != nil && !heldBack(*o.Command) {
			o.Excerpt = o.Command.Argv
		}
		rs = append(rs, ref{id: fmt.Sprintf("e%d", id), obs: id, identity: o.CaptureIdentity, workspace: o.WorkspaceKey,
			sender: o.Sender, excerpt: o.Excerpt, kind: k, lesson: lessons[id], cmd: o.Command})
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
		d, err := p.Day(ctx, tx)
		if err != nil || d.Full() != "" {
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
	var lessons []Suggestion
	err := p.Store.Write(ctx, func(tx *sql.Tx) error {
		lessons = nil
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
			s, err := p.write(ctx, tx, j, c, now)
			if err != nil {
				return err
			}
			if s.ID != 0 {
				lessons = append(lessons, s)
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
	if err != nil || p.Suggest == nil {
		return err
	}
	var errs []error
	for _, s := range lessons {
		errs = append(errs, p.Suggest(ctx, s))
	}
	return errors.Join(errs...)
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

// write stores claim c of batch j: its claim id, a proposed same_as link, and each ref as
// evidence of the candidate of the claim in its scope. A tombstoned claim or ref adds nothing
// (§6.7): a forget erases the claim key, so the tombstone source keys stop the same evidence.
// A known claim (the same claim id) gets the evidence first; propose then drops the duplicate
// proposal (§7.3). A same_as link to an open claim of the same kind in the same scope adds the
// evidence to that claim's candidate (merge). A candidate that reaches its gate gets a proposal,
// but a claim that an active skill covers goes to that skill: it makes no proposal (E20-S5).
func (p Pass) write(ctx context.Context, tx *sql.Tx, j job, c Claim, now time.Time) (Suggestion, error) {
	kept, err := untombstoned(ctx, tx, c.Refs)
	if c.Refs = kept; err != nil || len(kept) == 0 {
		return Suggestion{}, err
	}
	// No tool from a command that held a secret or was cut (§6.7): the claim adds nothing.
	if c.Tool != "" && slices.ContainsFunc(c.Refs, func(r ref) bool { return r.cmd != nil && heldBack(*r.cmd) }) {
		return Suggestion{}, nil
	}
	c = redactClaim(c)
	claim, err := store.ClaimID(ctx, tx, claimKey(c), now)
	if err != nil {
		return Suggestion{}, err
	}
	if ts, err := store.TombstonesByClaim(ctx, tx, claim); err != nil || len(ts) > 0 {
		return Suggestion{}, err
	}
	if err := link(ctx, tx, claim, c); err != nil {
		return Suggestion{}, err
	}
	key := j.batch.WorkspaceKey
	if c.Scope == store.ScopeGlobal {
		key = ""
	}
	kind := candidateKind(c)
	into, err := mergeInto(ctx, tx, claim, c, key, kind)
	if err != nil {
		return Suggestion{}, err
	}
	if kind, err = p.keptKind(ctx, tx, into, key, kind); err != nil {
		return Suggestion{}, err
	}
	cand := store.Candidate{ClaimID: into, Scope: c.Scope, WorkspaceKey: key, Kind: kind}
	if into != claim {
		cand.Via = claim
	}
	id, accepted, err := p.attach(ctx, tx, cand, c.Refs)
	if err != nil {
		return Suggestion{}, err
	}
	// Each outcome names its failure: the claim that got the evidence, after a merge (E20-S5).
	// The skill link comes first: an unconfirmed same_as link to the same skill adds no
	// uncertain outcome over its comeback.
	base := store.Outcome{SessionID: j.session.ID, ClaimID: into, At: now}
	covered, err := skillOutcomes(ctx, tx, j, c.CoveredBy, failures(kind, accepted, c.Refs), base)
	if err != nil {
		return Suggestion{}, err
	}
	if err := outcomes(ctx, tx, j, claim, corrections(kind, accepted), base); err != nil {
		return Suggestion{}, err
	}
	if id == 0 || covered {
		return Suggestion{}, nil
	}
	return p.propose(ctx, tx, id, c, now)
}

// link records the claim kind and the permission flag of claim c on its claim id claim, and
// its same_as link. The model guess alone is not proof (§5.4, §7.2): the link stays proposed,
// so an active item of the other claim gets no comeback from it (outcomes). check found
// c.SameAs in the input.
func link(ctx context.Context, tx *sql.Tx, claim string, c Claim) error {
	if err := store.MarkClaim(ctx, tx, claim, c.Kind, c.TouchesPermission); err != nil {
		return err
	}
	if c.SameAs == "" || c.SameAs == claim {
		return nil
	}
	_, err := store.SameAs(ctx, tx, claim, c.SameAs, store.Match{Permissions: c.TouchesPermission})
	return err
}

// keptKind returns the kind of the candidate of claim in the scope with key for new evidence
// of kind: kind for a new candidate. The kind follows the claim (#280): an explicit lesson
// turns the candidate into a lesson (#253), and your text turns a candidate of tool output
// alone into its kind. Else the candidate keeps its kind: a lesson stays a lesson, and tool
// output alone does not change the kind of your text.
func (p Pass) keptKind(ctx context.Context, tx *sql.Tx, claim, key string, kind store.CandidateKind) (store.CandidateKind, error) {
	old, err := store.CandidateOf(ctx, tx, claim, key)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return kind, nil
	case err != nil:
		return "", err
	case old.Kind == kind:
		return kind, nil
	}
	tool := old.Kind == store.CandidateFailure || old.Kind == store.CandidateCommandChain
	user := kind == store.CandidateCorrection || kind == store.CandidateProcedure
	if kind == store.CandidateLesson || (tool && user) {
		return kind, store.SetCandidateKind(ctx, tx, old.ID, kind, p.gates())
	}
	return old.Kind, nil
}

// attach adds each of refs as evidence of the candidate of cand, and returns its id (0 when
// Brain refused every ref) and the accepted refs. cand.Via marks evidence that a merge brought.
func (p Pass) attach(ctx context.Context, tx *sql.Tx, cand store.Candidate, refs []ref) (int64, []ref, error) {
	var id int64
	var accepted []ref
	for _, r := range refs {
		got, err := store.Attach(ctx, tx, cand, r.obs, p.gates(), signal.Supports)
		switch {
		case errors.Is(err, store.ErrEvidenceRefused):
			continue
		case err != nil:
			return 0, nil, err
		}
		id = got
		accepted = append(accepted, r)
	}
	return id, accepted, nil
}

// mergeInto returns the claim whose candidate gets the evidence of claim c with claim id claim,
// in the scope with key: the claim of its proposed same_as link, else claim (#279, §7.2). The
// link merges only into an open candidate of kind in the same scope, of a claim of the same
// claim kind with no item there, and only when neither claim is a permission or a safety rule
// and the claim of c is not rejected there. Else the link stays a proposed link for review.
// Brain keeps both claim ids and the link, and marks the merged evidence (store.Candidate.Via).
func mergeInto(ctx context.Context, tx *sql.Tx, claim string, c Claim, key string, kind store.CandidateKind) (string, error) {
	cl, err := store.ClaimByID(ctx, tx, claim)
	if err != nil || cl.SameAs == "" || cl.SameAsStatus != store.SameAsProposed || c.TouchesPermission || cl.TouchesPermission {
		return claim, err
	}
	other, err := store.ClaimByID(ctx, tx, cl.SameAs)
	if err != nil || other.Kind != c.Kind || other.TouchesPermission ||
		store.TouchesPermission(other.Key.Situation+" "+other.Key.Rule) {
		return claim, err
	}
	cand, err := store.CandidateOf(ctx, tx, other.ID, key)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return claim, nil
	case err != nil:
		return claim, err
	case cand.Scope != c.Scope || cand.Kind != kind || cand.Status != store.CandidateOpen:
		return claim, nil
	}
	for _, held := range []func(context.Context, *sql.Tx, string, string) (bool, error){store.ClaimHasItem, store.Rejected} {
		if stop, err := held(ctx, tx, other.ID, key); err != nil || stop {
			return claim, err
		}
	}
	if stop, err := store.Rejected(ctx, tx, claim, key); err != nil || stop {
		return claim, err
	}
	if ts, err := store.TombstonesByClaim(ctx, tx, other.ID); err != nil || len(ts) > 0 {
		return claim, err
	}
	return other.ID, nil
}

// corrections returns the new corrections of refs, the evidence that Attach accepted for a
// candidate of kind: all of refs for a correction candidate, else the refs of a correction
// signal. An item of a procedure claim can come back as a correction too.
func corrections(kind store.CandidateKind, refs []ref) []ref {
	if kind == store.CandidateCorrection {
		return refs
	}
	return slices.DeleteFunc(slices.Clone(refs), func(r ref) bool { return r.kind != signal.Correction })
}

// failures returns the new failures of a claim for the skill that covers it (E20-S5): its
// corrections in accepted, the evidence that Attach accepted for its candidate of kind, and each
// tool failure in refs, the checked refs of the claim. A correction candidate refuses tool
// output (§6.5), but a tool failure in the steps of a skill is a failure of the skill.
func failures(kind store.CandidateKind, accepted, refs []ref) []ref {
	fs := corrections(kind, accepted)
	for _, r := range refs {
		if r.kind == signal.Failure && !slices.ContainsFunc(fs, func(f ref) bool { return f.obs == r.obs }) {
			fs = append(fs, r)
		}
	}
	return fs
}

// skillOutcomes records an outcome of each new failure in refs for each active item of skill,
// the claim of the skill that covers them, in the scope of batch j (E20-S5): the skill gets the
// comeback of the revision that the session got, as an item of the claim does (§5.4). base
// holds the session, the failure claim and the time. It reports whether such a skill exists.
func skillOutcomes(ctx context.Context, tx *sql.Tx, j job, skill string, refs []ref, base store.Outcome) (bool, error) {
	if skill == "" {
		return false, nil
	}
	items, err := store.ActiveClaimSkills(ctx, tx, skill, j.batch.WorkspaceKey)
	if err != nil || len(items) == 0 {
		return false, err
	}
	for _, r := range refs {
		d, ok, err := store.TiedReceipt(ctx, tx, r.obs)
		if err != nil {
			return false, err
		}
		for _, it := range items {
			o := base
			o.ItemID, o.ObservationID = it, r.obs
			if err := outcome(ctx, tx, o, d, ok, false); err != nil {
				return false, err
			}
		}
	}
	return true, nil
}

// outcomes records an outcome of each new correction in refs of claim, for each active item of the claim in the scope of batch j, after the evidence (§5.4, §7.3
// order for a known claim). An active item of the claim that an unconfirmed same_as link of
// claim names gets an uncertain outcome instead of a comeback. A second comeback on one
// revision queues the change request of that revision (outcome). base holds the session, the
// failure claim and the time.
func outcomes(ctx context.Context, tx *sql.Tx, j job, claim string, refs []ref, base store.Outcome) error {
	key := j.batch.WorkspaceKey
	items, err := store.ActiveClaimItems(ctx, tx, claim, key)
	if err != nil {
		return err
	}
	cl, err := store.ClaimByID(ctx, tx, claim)
	if err != nil {
		return err
	}
	var guessed []int64
	if cl.SameAs != "" && cl.SameAsStatus == store.SameAsProposed {
		if guessed, err = store.ActiveClaimItems(ctx, tx, cl.SameAs, key); err != nil {
			return err
		}
	}
	for _, r := range refs {
		d, ok, err := store.TiedReceipt(ctx, tx, r.obs)
		if err != nil {
			return err
		}
		o := base
		o.ObservationID = r.obs
		for _, it := range items {
			o.ItemID = it
			if err := outcome(ctx, tx, o, d, ok, false); err != nil {
				return err
			}
		}
		for _, it := range guessed {
			if slices.Contains(items, it) {
				continue
			}
			o.ItemID = it
			if err := outcome(ctx, tx, o, d, ok, true); err != nil {
				return err
			}
		}
	}
	return nil
}

// outcome records outcome o of a correction for item o.ItemID from receipt d (ok is false for
// no tied receipt): d gives its kind and revision, and uncertain replaces a comeback when the
// match is a guess. The second comeback on one revision queues its change request. A skill
// counts the sessions of one failure, not flags: two flags in one session, or two failures in
// two sessions, queue nothing (E20-S5).
func outcome(ctx context.Context, tx *sql.Tx, o store.Outcome, d store.Delivery, ok, guess bool) error {
	kind, rev, write := outcomeOf(d, ok, o.ItemID)
	if !write {
		return nil
	}
	if kind == store.OutcomeComeback && guess {
		kind = store.OutcomeUncertain
	}
	o.Kind, o.Revision = kind, rev
	added, err := store.AddOutcome(ctx, tx, o)
	if err != nil || !added || kind != store.OutcomeComeback {
		return err
	}
	it, err := store.ItemByID(ctx, tx, o.ItemID)
	if err != nil {
		return err
	}
	var n int
	switch it.Kind {
	case store.KindSkill:
		n, err = store.FailureSessions(ctx, tx, o.ItemID, rev, o.ClaimID)
	default:
		n, err = store.Comebacks(ctx, tx, o.ItemID, rev)
	}
	if err != nil || n < 2 {
		return err
	}
	return store.QueueItemChange(ctx, tx, o.ItemID, rev, o.At)
}

// outcomeOf is the outcome for item of a session whose last receipt before the observation is
// d (§5.4): ok is false when no single receipt ties to the session. It returns the revision that
// d names, and false when the session got no revision of the item: no outcome. A revision that
// brain recall returned on d comes first, also on a withheld receipt: recall printed it (§8.6).
func outcomeOf(d store.Delivery, ok bool, item int64) (store.OutcomeKind, int64, bool) {
	for _, r := range d.Recalled {
		if ok && r.Item == item {
			return store.OutcomeComeback, r.Revision, true
		}
	}
	switch {
	case !ok:
		return store.OutcomeUnknown, 0, true
	case d.Result == store.ResultWithheld:
		return store.OutcomeGap, 0, true
	}
	for _, r := range d.ItemRevisions {
		if r.Item == item {
			return store.OutcomeComeback, r.Revision, true
		}
	}
	return "", 0, false
}

// candidateKind is the candidate kind of claim c: the kind follows the claim, not the first
// signal of its evidence (#280, §7.2). A tool is a command chain. A lesson claim with lesson
// evidence is a lesson, as a lesson needs both the local rule and the extractor (§7.1); any
// other lesson and a correction is a correction. A procedure or a skill is a procedure. A
// preference or a fact is a correction. Only tool output can support a failure or a command
// chain (§6.5), so a claim with no text of yours keeps that kind of its evidence: a skill of a
// command chain that needs judgment (§10.3) or a preference from failure output alone.
func candidateKind(c Claim) store.CandidateKind {
	if c.Tool != "" {
		return store.CandidateCommandChain
	}
	has := func(k signal.Kind) bool { return slices.ContainsFunc(c.Refs, func(r ref) bool { return r.kind == k }) }
	yours := slices.ContainsFunc(c.Refs, func(r ref) bool {
		return r.sender == store.SenderYou || r.sender == store.SenderUnconfirmed
	})
	switch c.Kind {
	case kindLesson:
		if slices.ContainsFunc(c.Refs, func(r ref) bool { return r.lesson }) {
			return store.CandidateLesson
		}
		return store.CandidateCorrection
	case "correction":
		return store.CandidateCorrection
	case "procedure", kindSkill:
		if !yours && has(signal.CommandChain) {
			return store.CandidateCommandChain
		}
		return store.CandidateProcedure
	}
	if !yours && has(signal.Failure) {
		return store.CandidateFailure
	}
	return store.CandidateCorrection
}

// propose makes a proposal of candidate id when it reached its gate and its claim has no
// pending proposal, no item and no rejection in its scope (§7.2, §7.3). The proposal goes
// through store.ProposeCandidate. Its draft item gets the scope and workspace key of the
// candidate. It stores the §7.4 fields: the excerpts stay in the evidence links, and the
// proposal holds the hashes of its trigger prompts (store.ProposeCandidate) and the items that
// it conflicts with.
func (p Pass) propose(ctx context.Context, tx *sql.Tx, id int64, c Claim, now time.Time) (Suggestion, error) {
	cand, err := store.CandidateByID(ctx, tx, id)
	if err != nil || !p.gates().Reached(cand) {
		return Suggestion{}, err
	}
	if pending, err := store.PendingCandidateProposalIDs(ctx, tx, id); err != nil || len(pending) > 0 {
		return Suggestion{}, err
	}
	if has, err := store.ClaimHasItem(ctx, tx, cand.ClaimID, cand.WorkspaceKey); err != nil || has {
		return Suggestion{}, err
	}
	if held, err := store.Rejected(ctx, tx, cand.ClaimID, cand.WorkspaceKey); err != nil || held {
		return Suggestion{}, err
	}
	var key *string
	if cand.Scope == store.ScopeWorkspace {
		key = &cand.WorkspaceKey
	}
	target, err := target(ctx, tx, cand)
	switch {
	case errors.Is(err, sql.ErrNoRows): // the item needs the workspace row: the next evidence tries again
		return Suggestion{}, nil
	case err != nil:
		return Suggestion{}, err
	}
	conflicts, err := conflictItems(ctx, tx, cand, c)
	if err != nil {
		return Suggestion{}, err
	}
	item, err := store.InsertItem(ctx, tx, store.Item{ClaimID: cand.ClaimID, CurrentRevision: 1, Kind: itemKind(c),
		Scope: cand.Scope, WorkspaceKey: key, State: store.StateDraft, Created: now})
	if err != nil {
		return Suggestion{}, err
	}
	text, diff := draft(c)
	base := int64(1)
	pid, err := store.ProposeCandidate(ctx, tx, id, store.Proposal{ItemID: &item, BaseRevision: &base, Text: text,
		Scope: cand.Scope, Target: target, Diff: diff, ExpectedBenefit: c.ExpectedBenefit,
		PermissionWarning: c.TouchesPermission, ConflictsWith: conflicts,
		Status: store.ProposalPending, Created: now})
	if err != nil {
		return Suggestion{}, err
	}
	if err := store.InsertItemRevision(ctx, tx, store.ItemRevision{ItemID: item, Revision: 1, Text: text,
		Scope: cand.Scope, WorkspaceKey: key, State: store.StateDraft, Author: store.AuthorBrain,
		Origin: store.OriginExtractor, ProposalID: &pid, Created: now}); err != nil {
		return Suggestion{}, err
	}
	if err := store.MarkProposed(ctx, tx, id); err != nil || cand.Kind != store.CandidateLesson {
		return Suggestion{}, err
	}
	return lessonSuggestion(ctx, tx, cand, pid, now)
}

// lessonSuggestion reserves the suggestion of proposal pid of lesson candidate cand beside the
// agent pane of its newest evidence: the lesson of this batch (#253, §7.2). A suggestion that
// Brain cannot reserve leaves the proposal in brain review, as any proposal.
func lessonSuggestion(ctx context.Context, tx *sql.Tx, cand store.Candidate, pid int64, now time.Time) (Suggestion, error) {
	obs, err := store.NewestEvidence(ctx, tx, cand.ID)
	if err != nil {
		return Suggestion{}, err
	}
	s, err := store.ReserveLessonSuggestion(ctx, tx, obs, pid, now)
	if err != nil || s.ID == 0 {
		return Suggestion{}, err
	}
	w, err := store.WorkspaceByKey(ctx, tx, s.WorkspaceKey)
	if err != nil {
		return Suggestion{}, err
	}
	return Suggestion{Suggestion: s, WorkspaceID: w.HerdrWorkspaceID}, nil
}

// itemKind is the item kind of a proposal of claim c. [NEW] a fact claim makes a fact, a skill
// claim a skill (E16-S4), a tool claim a tool (E17-S6); every other claim kind makes a
// preference, the kind of a lesson.
func itemKind(c Claim) store.Kind {
	switch c.Kind {
	case "fact":
		return store.KindFact
	case kindSkill:
		return store.KindSkill
	case kindTool:
		return store.KindTool
	}
	return store.KindPreference
}

// claimKey is the claim key of claim c, in normal form. A tool keeps its key (toolKey), so the
// case of its commands stays.
func claimKey(c Claim) store.ClaimKey {
	if c.Tool != "" {
		return store.ClaimKey{Situation: c.Situation, Rule: c.Rule}
	}
	return store.ClaimKey{Situation: normal(c.Situation), Rule: normal(c.Rule)}
}

// claimText is the item text of claim c (#284): "When <situation>: <rule>" (brief.ClaimText).
func claimText(c Claim) string { return brief.ClaimText(c.Situation, c.Rule) }

// draft is the text and the diff of a new item of claim c: the skill text for a skill (the
// review shows the whole file, S8), the tool text for a tool (the whole contract, E17-S3), else
// the claim text.
func draft(c Claim) (text, diff string) {
	switch {
	case c.Skill != nil:
		text = c.Skill.Text()
		return text, store.SkillDiff("", text)
	case c.Tool != "":
		return c.Tool, store.SkillDiff("", c.Tool)
	}
	text = claimText(c)
	return text, "+ " + text
}

// redactClaim returns c with each secret of its model text replaced (Redact). checkSkill and
// checkTool filtered the skill and tool fields already.
func redactClaim(c Claim) Claim {
	c.Situation, c.Rule, c.ExpectedBenefit = Redact(c.Situation), Redact(c.Rule), Redact(c.ExpectedBenefit)
	return c
}

// conflictItems returns the conflicts of a proposal of claim c for candidate cand: the active
// items of the claim that c names in conflicts_with, in the scope of cand. [NEW] a claim with
// no active item gives no conflict: there is nothing in the brief to conflict with. The
// trigger prompts come from the prompt index (store.ProposeCandidate, E14-S1).
func conflictItems(ctx context.Context, tx *sql.Tx, cand store.Candidate, c Claim) ([]int64, error) {
	if c.ConflictsWith == "" {
		return nil, nil
	}
	return store.ActiveClaimItems(ctx, tx, c.ConflictsWith, cand.WorkspaceKey)
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

// Redact replaces each secret that the secret filter finds in s with "[secret]" [NEW]: model
// text goes through the filter before Brain stores it, and an excerpt before a proposal shows
// it (§7.4).
func Redact(s string) string {
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

// normal is the normal form of a claim key text (§7.2) [NEW]: lower case with single spaces and
// no end punctuation (#279), so "Writing code." and "writing  code" are one key.
func normal(s string) string {
	return strings.TrimRight(strings.Join(strings.Fields(strings.ToLower(s)), " "), ".!?;:, ")
}
