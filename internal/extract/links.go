package extract

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/aryanVijaywargia/herdr-brain/internal/brief"
	"github.com/aryanVijaywargia/herdr-brain/internal/signal"
	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

// setDraft records claim c, checked and through the secret filter, as the newest telling of its
// claim id claim (store.SetDraft): a merge in review proposes it (#358). The refs stay out:
// they are evidence links.
func setDraft(ctx context.Context, tx *sql.Tx, claim string, c Claim, now time.Time) error {
	c.Refs = nil
	b, err := json.Marshal(c)
	if err != nil {
		return fmt.Errorf("encode the draft of claim %s: %w", claim, err)
	}
	return store.SetDraft(ctx, tx, claim, string(b), now)
}

// telling returns the claim of the newest telling of claim cl and its time (setDraft). ok is
// false for a claim with no draft, of an older Brain or forgotten: the claim then holds its key,
// kind and permission flag only.
func telling(ctx context.Context, tx *sql.Tx, cl store.Claim) (c Claim, at time.Time, ok bool, err error) {
	d, at, err := store.ClaimDraft(ctx, tx, cl.ID)
	if err != nil || d == "" {
		return Claim{Kind: cl.Kind, Situation: cl.Key.Situation, Rule: cl.Key.Rule,
			TouchesPermission: cl.TouchesPermission}, time.Time{}, false, err
	}
	if err := json.Unmarshal([]byte(d), &c); err != nil {
		return Claim{}, time.Time{}, false, fmt.Errorf("decode the draft of claim %s: %w", cl.ID, err)
	}
	return c, at, true, nil
}

// Link is a same_as link that waits for review (#358, §7.2): the claim of the open candidate
// Candidate means the same as claim Other, the extractor says, and Brain did not merge their
// evidence. Text and OtherText are the texts of the two claims. Permission says that Brain held
// the link back as a permission or a safety rule (permissionPair); else the match is unclear.
type Link struct {
	Candidate        store.Candidate
	Claim, Other     store.Claim
	Text, OtherText  string
	Permission       bool
	OtherCount       int   // the occurrences of the candidate of Other in the scope of Candidate
	OtherCandidateID int64 // that candidate; 0 for none
	// Stuck says why a merge would move no evidence (stuck), so the review offers no merge; ""
	// when it moves some.
	Stuck string
}

// Links returns the same_as links that wait for review, by candidate id
// (store.LinkCandidates). It only reads.
func Links(ctx context.Context, tx *sql.Tx) ([]Link, error) {
	cs, err := store.LinkCandidates(ctx, tx)
	if err != nil {
		return nil, err
	}
	links := make([]Link, 0, len(cs))
	for _, cand := range cs {
		l := Link{Candidate: cand}
		if l.Claim, err = store.ClaimByID(ctx, tx, cand.ClaimID); err != nil {
			return nil, err
		}
		if l.Other, err = store.ClaimByID(ctx, tx, l.Claim.SameAs); err != nil {
			return nil, err
		}
		c, _, _, err := telling(ctx, tx, l.Claim)
		if err != nil {
			return nil, err
		}
		if l.Permission, err = permissionPair(ctx, tx, l.Claim, l.Other, c); err != nil {
			return nil, err
		}
		if l.Text, err = linkText(ctx, tx, l.Claim); err != nil {
			return nil, err
		}
		if l.OtherText, err = linkText(ctx, tx, l.Other); err != nil {
			return nil, err
		}
		other, err := store.CandidateOf(ctx, tx, l.Other.ID, cand.WorkspaceKey)
		switch {
		case err == nil:
			l.OtherCandidateID, l.OtherCount = other.ID, other.Count
		case !errors.Is(err, sql.ErrNoRows):
			return nil, err
		}
		if l.Stuck, err = stuck(ctx, tx, cand, l.Other.ID); err != nil {
			return nil, err
		}
		links = append(links, l)
	}
	return links, nil
}

// linkText is the text of claim cl for the review: "When <situation>: <rule>", or the text of
// the brain remember item of a claim with no key (#312).
func linkText(ctx context.Context, tx *sql.Tx, cl store.Claim) (string, error) {
	if cl.Key != (store.ClaimKey{}) {
		return brief.ClaimText(cl.Key.Situation, cl.Key.Rule), nil
	}
	return store.ClaimText(ctx, tx, cl.ID)
}

// Merged is what MergeLink did: the candidates that got the evidence, and the proposals of the
// merged claim that it superseded.
type Merged struct {
	Candidates, Superseded []int64
}

// MergeLink merges the same_as link that waits for review of candidate id (#358, §7.2): you say
// that its claim means the same as the claim of the link. The link is confirmed
// (store.ConfirmLink): the key gets that claim id from now on. The evidence of each candidate of
// the claim whose link waits (store.LinkWaits) moves to the candidate of the other claim in the
// same scope (moveEvidence), as a merge of the extractor does (#279), so it counts together; a
// proposal of such a candidate that waits for your review is superseded. A merge approves
// nothing: a candidate at its gate gets a normal proposal for your review, with the text of the
// newest telling of the two claims (#348) and no model call. A claim with no draft proposes at
// its next telling. A link whose merge would move no evidence of candidate id is refused (stuck,
// #361): the review never says that evidence counts together when none moved.
func MergeLink(ctx context.Context, tx *sql.Tx, id int64, gates store.Gates, now time.Time) (Merged, error) {
	cand, err := linkCandidate(ctx, tx, id)
	if err != nil {
		return Merged{}, err
	}
	cl, err := store.ClaimByID(ctx, tx, cand.ClaimID)
	if err != nil {
		return Merged{}, err
	}
	cs, err := store.ClaimCandidates(ctx, tx, cl.ID)
	if err != nil {
		return Merged{}, err
	}
	into, err := store.ConfirmLink(ctx, tx, cl.ID)
	if err != nil {
		return Merged{}, err
	}
	if err := mergeable(ctx, tx, cand, into); err != nil {
		return Merged{}, err
	}
	c, ok, err := newest(ctx, tx, cl, into)
	if err != nil {
		return Merged{}, err
	}
	p := Pass{Gates: gates, Now: func() time.Time { return now }}
	var m Merged
	for _, from := range cs {
		if waits, err := store.LinkWaits(ctx, tx, from.ID); err != nil || !waits {
			if err != nil {
				return Merged{}, err
			}
			continue
		}
		ids, err := store.SupersedePending(ctx, tx, from.ID, now)
		if err != nil {
			return Merged{}, err
		}
		m.Superseded = append(m.Superseded, ids...)
		to, err := p.moveEvidence(ctx, tx, from, into)
		if err != nil || to == 0 {
			if err != nil {
				return Merged{}, err
			}
			continue
		}
		m.Candidates = append(m.Candidates, to)
		if !ok {
			continue
		}
		if _, _, err := p.proposal(ctx, tx, to, c, now); err != nil {
			return Merged{}, err
		}
	}
	return m, nil
}

// mergeable returns an error that says why when a merge of candidate cand into claim into would
// move none of its evidence (stuck), else nil.
func mergeable(ctx context.Context, tx *sql.Tx, cand store.Candidate, into string) error {
	why, err := stuck(ctx, tx, cand, into)
	if err != nil || why == "" {
		return err
	}
	return fmt.Errorf("link of candidate %d: a merge moves no evidence: %s; keep it apart (brain review --apart %d)",
		cand.ID, why, cand.ID)
}

// stuck returns why a merge of candidate from into claim into would move none of its evidence,
// or "" when it moves some: no evidence of from can count for the candidate that would get it
// (mergeTarget), as the agent's own commands cannot count for a rule (§6.5, #361). The review
// then offers no merge, and MergeLink refuses it.
func stuck(ctx context.Context, tx *sql.Tx, from store.Candidate, into string) (string, error) {
	to, _, err := mergeTarget(ctx, tx, from, into)
	if err != nil {
		return "", err
	}
	es, err := store.CandidateEvidence(ctx, tx, from.ID)
	if err != nil {
		return "", err
	}
	for _, e := range es {
		err := store.CheckEvidence(ctx, tx, to, e.Observation, signal.Supports)
		switch {
		case err == nil:
			return "", nil
		case !errors.Is(err, store.ErrEvidenceRefused):
			return "", err
		}
	}
	what := "your text"
	if from.Kind == store.CandidateCommandChain || from.Kind == store.CandidateFailure {
		what = "the agent's own commands and tool output"
	}
	return fmt.Sprintf("the evidence of candidate %d is %s, which cannot count for a %s", from.ID, what,
		strings.ReplaceAll(string(to.Kind), "_", " ")), nil
}

// mergeTarget returns the candidate that gets the evidence of candidate from in a merge into
// claim into (moveEvidence), in the scope of from, with its kind, and the id of the candidate of
// into whose kind your text turns (0 for none): the candidate of into keeps its kind, unless it
// holds tool output alone and from is your text (overTool, #361). A new candidate takes the kind
// of from.
func mergeTarget(ctx context.Context, tx *sql.Tx, from store.Candidate, into string) (store.Candidate, int64, error) {
	to := store.Candidate{ClaimID: into, Scope: from.Scope, WorkspaceKey: from.WorkspaceKey, Kind: from.Kind}
	old, err := store.CandidateOf(ctx, tx, into, from.WorkspaceKey)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return to, 0, nil
	case err != nil:
		return store.Candidate{}, 0, err
	case overTool(old.Kind, from.Kind):
		return to, old.ID, nil
	}
	to.Kind = old.Kind
	return to, 0, nil
}

// newest returns the claim of the newest telling of claim cl and of claim into, the claim that
// cl now means (telling). ok is false when neither has a draft.
func newest(ctx context.Context, tx *sql.Tx, cl store.Claim, into string) (Claim, bool, error) {
	other, err := store.ClaimByID(ctx, tx, into)
	if err != nil {
		return Claim{}, false, err
	}
	c, at, ok, err := telling(ctx, tx, cl)
	if err != nil {
		return Claim{}, false, err
	}
	d, dat, dok, err := telling(ctx, tx, other)
	if err != nil || !dok || ok && !dat.After(at) {
		return c, ok, err
	}
	return d, true, nil
}

// moveEvidence moves the evidence of open candidate from to the candidate of claim into in the
// same scope (mergeTarget): of that candidate's kind, of the kind of from for a new one, or of
// the kind of from when your text turns a candidate of tool output alone (store.SetCandidateKind,
// #361). Each evidence link keeps the claim that brought it, else it is marked as brought by the
// claim of from (store.Candidate.Via). Attach checks each one again: evidence that the other
// kind refuses stays with from, except tool output that counts for neither kind, which goes
// along uncounted. A candidate with no evidence left goes. It returns the candidate that got the
// evidence, or 0 when it took none.
func (p Pass) moveEvidence(ctx context.Context, tx *sql.Tx, from store.Candidate, into string) (int64, error) {
	to, turn, err := mergeTarget(ctx, tx, from, into)
	if err != nil {
		return 0, err
	}
	if turn != 0 {
		if err := store.SetCandidateKind(ctx, tx, turn, to.Kind, p.gates()); err != nil {
			return 0, err
		}
	}
	es, err := store.CandidateEvidence(ctx, tx, from.ID)
	if err != nil {
		return 0, err
	}
	var id int64
	var moved, kept []int64
	for _, e := range es {
		// Tool output that does not count for from, the agent's commands after your tellings
		// (store.SetCandidateKind), counts for neither kind: it moves uncounted
		// (store.CopyEvidence).
		if e.Sender == store.SenderTool && !signal.Supports(from.Kind, e.Sender) {
			kept = append(kept, e.Observation)
			continue
		}
		to.Via = cmp.Or(e.Via, from.ClaimID)
		got, err := store.Attach(ctx, tx, to, e.Observation, p.gates(), signal.Supports)
		switch {
		case errors.Is(err, store.ErrEvidenceRefused):
			continue
		case err != nil:
			return 0, err
		}
		id, moved = got, append(moved, e.Observation)
	}
	if id != 0 && len(kept) > 0 {
		if err := store.CopyEvidence(ctx, tx, from.ID, id, kept, from.ClaimID, p.gates()); err != nil {
			return 0, err
		}
		moved = append(moved, kept...)
	}
	return id, store.DropEvidence(ctx, tx, from.ID, moved, p.gates())
}

// KeepApart keeps the claim of open candidate id apart from the claim of its same_as link that
// waits for review (#358, store.KeepApart): the pair does not come back for review, and both
// keep their own evidence. It returns the other claim, and the proposals that the link held
// (proposeFreed).
func KeepApart(ctx context.Context, tx *sql.Tx, id int64, gates store.Gates, now time.Time) (string, []int64, error) {
	cand, err := linkCandidate(ctx, tx, id)
	if err != nil {
		return "", nil, err
	}
	other, err := store.KeepApart(ctx, tx, cand.ClaimID)
	if err != nil {
		return "", nil, err
	}
	ps, err := Pass{Gates: gates, Now: func() time.Time { return now }}.proposeFreed(ctx, tx, cand.ClaimID, now)
	return other, ps, err
}

// proposeFreed makes the proposal of each candidate of claim at its gate that the rejection of
// its link held (linkRejected, #365): keep apart frees it, so it needs no new telling. The draft
// is the newest telling of the claim (telling), with no model call; a claim with no draft
// proposes at its next telling. It returns the new proposals.
func (p Pass) proposeFreed(ctx context.Context, tx *sql.Tx, claim string, now time.Time) ([]int64, error) {
	cl, err := store.ClaimByID(ctx, tx, claim)
	if err != nil {
		return nil, err
	}
	c, _, ok, err := telling(ctx, tx, cl)
	if err != nil || !ok {
		return nil, err
	}
	cs, err := store.ClaimCandidates(ctx, tx, claim)
	if err != nil {
		return nil, err
	}
	var ps []int64
	for _, cand := range cs {
		_, pid, err := p.proposal(ctx, tx, cand.ID, c, now)
		if err != nil {
			return nil, err
		}
		if pid != 0 {
			ps = append(ps, pid)
		}
	}
	return ps, nil
}

// linkCandidate reads candidate id of a link to review (store.LinkWaits). The store checks the
// link.
func linkCandidate(ctx context.Context, tx *sql.Tx, id int64) (store.Candidate, error) {
	cand, err := store.CandidateByID(ctx, tx, id)
	if err != nil {
		return store.Candidate{}, err
	}
	waits, err := store.LinkWaits(ctx, tx, id)
	if err == nil && !waits {
		err = fmt.Errorf("candidate %d is %s with no proposal that waits: it has no link to review", id, cand.Status)
	}
	return cand, err
}
