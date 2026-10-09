package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// SetDraft records draft, the claim of a telling of claim id as the extractor gave it (JSON),
// and at, the time of that telling: the newest telling of the key. A merge in review proposes
// it with no model call (#358). The caller filtered its text for secrets.
func SetDraft(ctx context.Context, tx *sql.Tx, id, draft string, at time.Time) error {
	if _, err := tx.ExecContext(ctx, `UPDATE claims SET draft = ?, drafted = ? WHERE id = ?`,
		draft, timeArg(at), id); err != nil {
		return fmt.Errorf("record the draft of claim %s: %w", id, err)
	}
	return nil
}

// ClaimDraft returns the draft of the newest telling of claim id and its time (SetDraft): ""
// and the zero time for a claim of an older Brain, and for a forgotten one.
func ClaimDraft(ctx context.Context, tx *sql.Tx, id string) (string, time.Time, error) {
	var draft sql.NullString
	var at time.Time
	err := tx.QueryRowContext(ctx, `SELECT draft, drafted FROM claims WHERE id = ?`, id).Scan(&draft, timeDest{&at})
	if err != nil {
		return "", time.Time{}, fmt.Errorf("read the draft of claim %s: %w", id, err)
	}
	if !draft.Valid {
		return "", time.Time{}, nil
	}
	return draft.String, at, nil
}

// LinkCandidates returns the candidates of the same_as links that wait for review (#358, §7.2):
// the candidates of the claims with a proposed link that wait for evidence or for your review
// (LinkWaits), by id. A link whose evidence Brain merged leaves no candidate of its claim: the
// evidence is in the candidate of the other claim. A forgotten claim at either end gives none
// (§6.7).
func LinkCandidates(ctx context.Context, tx *sql.Tx) ([]Candidate, error) {
	return query(ctx, tx, `SELECT `+candidateColumns+` FROM candidates c WHERE `+linkWaits+` AND claim_id IN
		(SELECT id FROM claims k WHERE same_as_status = ?3
			AND NOT EXISTS (SELECT 1 FROM tombstones t WHERE t.claim_id IN (k.id, k.same_as))) ORDER BY id`,
		func(rows *sql.Rows) (Candidate, error) { return scanCandidate(rows) }, CandidateOpen, ProposalPending, SameAsProposed)
}

// linkWaits is the SQL test of LinkWaits on candidate c, with ?1 open and ?2 pending.
const linkWaits = `(c.status = ?1 OR EXISTS (SELECT 1 FROM proposals p WHERE p.candidate_id = c.id AND p.status = ?2))`

// LinkWaits reports whether the same_as link of the claim of candidate id waits for review in
// its scope: the candidate is open, or its proposal waits for your review (a lesson or a
// procedure that reached its gate on its own). A candidate with an item or a rejection has
// its decision.
func LinkWaits(ctx context.Context, tx *sql.Tx, id int64) (bool, error) {
	var waits bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM candidates c WHERE c.id = ?3 AND `+linkWaits+`)`,
		CandidateOpen, ProposalPending, id).Scan(&waits)
	if err != nil {
		return false, fmt.Errorf("read candidate %d: %w", id, err)
	}
	return waits, nil
}

// SupersedePending ends the pending proposals of candidate id as superseded at now, and
// returns their ids: a merge in review gave its evidence to the candidate of another claim,
// whose proposal you review instead (#358). Their shown suggestions get result rejected, as
// for a sibling that you did not take.
func SupersedePending(ctx context.Context, tx *sql.Tx, id int64, now time.Time) ([]int64, error) {
	ids, err := PendingCandidateProposalIDs(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	for _, p := range ids {
		if _, err := tx.ExecContext(ctx, `UPDATE proposals SET status = ?, decided = ? WHERE id = ?`,
			ProposalSuperseded, timeArg(now), p); err != nil {
			return nil, fmt.Errorf("supersede proposal %d: %w", p, err)
		}
		if err := DecideSuggestions(ctx, tx, p, SuggestionRejected); err != nil {
			return nil, err
		}
	}
	return ids, nil
}

// ClaimCandidates returns the candidates of claim id in every scope, by id.
func ClaimCandidates(ctx context.Context, tx *sql.Tx, id string) ([]Candidate, error) {
	return query(ctx, tx, `SELECT `+candidateColumns+` FROM candidates WHERE claim_id = ? ORDER BY id`,
		func(rows *sql.Rows) (Candidate, error) { return scanCandidate(rows) }, id)
}

// proposedLink reads claim id, which must have a same_as link that waits for review.
func proposedLink(ctx context.Context, tx *sql.Tx, id string) (Claim, error) {
	c, err := ClaimByID(ctx, tx, id)
	if err != nil {
		return Claim{}, err
	}
	if c.SameAs == "" || c.SameAsStatus != SameAsProposed {
		return Claim{}, fmt.Errorf("claim %s has no same_as link that waits for review", id)
	}
	return c, nil
}

// ConfirmLink confirms the proposed same_as link of claim id (#358): its key gets the claim id
// of the link from now on (ClaimID), and it returns that claim id. A link points at the claim
// id that the other claim has (SameAs), and so do the links that pointed at id: a key never
// needs two hops. A link of the other claim back to id ends: the two claims are one now. A
// rejection of id in a scope holds for that claim too, so a reworded telling stays rejected
// there (§7.2); your reason stays on the rejection of id only, so a forget of its item finds it.
func ConfirmLink(ctx context.Context, tx *sql.Tx, id string) (string, error) {
	c, err := proposedLink(ctx, tx, id)
	if err != nil {
		return "", err
	}
	target, err := ClaimByID(ctx, tx, c.SameAs)
	if err != nil {
		return "", err
	}
	to := c.SameAs
	if target.SameAsStatus == SameAsConfirmed {
		to = target.SameAs
	}
	for _, q := range []string{
		`UPDATE claims SET same_as = ?2, same_as_status = ?3 WHERE id = ?1`,
		`UPDATE claims SET same_as = NULL, same_as_status = NULL WHERE id = ?2 AND same_as = ?1`,
		`UPDATE claims SET same_as = ?2 WHERE same_as = ?1 AND id != ?2`,
		`INSERT INTO rejections (claim_id, scope, workspace_key, reason, evidence_count, at)
			SELECT ?2, scope, workspace_key, '', evidence_count, at FROM rejections WHERE claim_id = ?1 AND reopened IS NULL
			ON CONFLICT (claim_id, COALESCE(workspace_key, '')) WHERE reopened IS NULL DO NOTHING`,
	} {
		if _, err := tx.ExecContext(ctx, q, id, to, SameAsConfirmed); err != nil {
			return "", fmt.Errorf("confirm the link of claim %s to %s: %w", id, to, err)
		}
	}
	return to, nil
}

// KeepApart ends the proposed same_as link of claim id, and a proposed link of the other claim
// back to id, and records the pair as kept apart (#358): SameAs links the pair no more, so it
// does not come back for review. It returns the claim that the link named. Both claims keep
// their candidates and evidence.
func KeepApart(ctx context.Context, tx *sql.Tx, id string) (string, error) {
	c, err := proposedLink(ctx, tx, id)
	if err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO claims_apart (claim_id, other_id) VALUES (?, ?)
		ON CONFLICT DO NOTHING`, id, c.SameAs); err != nil {
		return "", fmt.Errorf("keep claims %s and %s apart: %w", id, c.SameAs, err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE claims SET same_as = NULL, same_as_status = NULL
		WHERE id = ?1 OR id = ?2 AND same_as = ?1 AND same_as_status = ?3`, id, c.SameAs, SameAsProposed); err != nil {
		return "", fmt.Errorf("end the link of claim %s: %w", id, err)
	}
	return c.SameAs, nil
}

// EvidenceLink is one evidence link of a candidate: its observation, the claim whose key
// brought it through a same_as merge ("" for the candidate's own evidence), and its sender.
type EvidenceLink struct {
	Observation int64
	Via         string
	Sender      Sender
}

// CandidateEvidence returns the evidence links of candidate id, by observation id.
func CandidateEvidence(ctx context.Context, tx *sql.Tx, id int64) ([]EvidenceLink, error) {
	return query(ctx, tx, `SELECT observation_id, COALESCE(via_claim, ''), sender FROM evidence_links
		WHERE candidate_id = ? ORDER BY observation_id`, func(rows *sql.Rows) (EvidenceLink, error) {
		var e EvidenceLink
		err := rows.Scan(&e.Observation, &e.Via, &e.Sender)
		return e, err
	}, id)
}

// CopyEvidence copies the evidence links of the observations obs of candidate from to candidate
// to, in the same scope, with no sender check, and counts to again. A merge carries tool output
// that counts for neither kind (recount) with it: the agent's own commands of a skill claim stay
// the evidence of the claim, so the exact tool path still sees the skill (§10.3, #361). Each link
// keeps the claim that brought it, else via.
func CopyEvidence(ctx context.Context, tx *sql.Tx, from, to int64, obs []int64, via string, gates Gates) error {
	ids, err := json.Marshal(obs)
	if err != nil {
		return fmt.Errorf("encode observation ids: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO evidence_links
		(candidate_id, observation_id, capture_identity, session_id, workspace_key, harness, sender, at, via_claim)
		SELECT ?1, observation_id, capture_identity, session_id, workspace_key, harness, sender, at, COALESCE(via_claim, ?2)
		FROM evidence_links WHERE candidate_id = ?3 AND observation_id IN (SELECT value FROM json_each(?4))
		ON CONFLICT DO NOTHING`, to, via, from, string(ids)); err != nil {
		return fmt.Errorf("copy evidence of candidate %d to %d: %w", from, to, err)
	}
	return recount(ctx, tx, to, gates)
}

// DropEvidence deletes the evidence of the observations obs from candidate id and counts it
// again: a candidate with no evidence left goes (recount).
func DropEvidence(ctx context.Context, tx *sql.Tx, id int64, obs []int64, gates Gates) error {
	ids, err := json.Marshal(obs)
	if err != nil {
		return fmt.Errorf("encode observation ids: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM evidence_links WHERE candidate_id = ?
		AND observation_id IN (SELECT value FROM json_each(?))`, id, string(ids)); err != nil {
		return fmt.Errorf("drop evidence of candidate %d: %w", id, err)
	}
	return recount(ctx, tx, id, gates)
}
