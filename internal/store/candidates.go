package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"time"
)

// ClaimKey is the claim key that the extractor gives (§7.2, §7.3): a situation and a rule, in
// normal form. It is text; the claim id is not.
type ClaimKey struct {
	Situation, Rule string
}

// SameAsStatus is the state of a same_as link of a claim (§7.2).
type SameAsStatus string

// The same_as states.
const (
	SameAsProposed  SameAsStatus = "proposed"  // waits for review
	SameAsConfirmed SameAsStatus = "confirmed" // the key has the claim id of the link
)

// Claim is one row of claims.
type Claim struct {
	ID           string
	Key          ClaimKey // empty after a forget (§6.7)
	SameAs       string   // the claim id that the key means the same as; "" for none
	SameAsStatus SameAsStatus
	Created      time.Time
}

// ClaimID returns the claim id of key. The same key gives the same id. A key with a confirmed
// same_as link gives the id of that claim (§7.2). A new key gets a new random id with no text.
func ClaimID(ctx context.Context, tx *sql.Tx, key ClaimKey, now time.Time) (string, error) {
	id, err := newClaimID()
	if err != nil {
		return "", err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO claims (id, situation, rule, created) VALUES (?, ?, ?, ?)
		ON CONFLICT (situation, rule) DO NOTHING`, id, key.Situation, key.Rule, timeArg(now))
	if err != nil {
		return "", fmt.Errorf("add claim: %w", err)
	}
	var sameAs, status sql.NullString
	err = tx.QueryRowContext(ctx, `SELECT id, same_as, same_as_status FROM claims WHERE situation = ? AND rule = ?`,
		key.Situation, key.Rule).Scan(&id, &sameAs, &status)
	if err != nil {
		return "", fmt.Errorf("read claim: %w", err)
	}
	if SameAsStatus(status.String) == SameAsConfirmed {
		return sameAs.String, nil
	}
	return id, nil
}

// newClaimID returns a new opaque claim id (§7.2). [NEW] 8 random bytes in hex with a "cl_"
// prefix, as brain remember makes them.
func newClaimID() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("make claim id: %w", err)
	}
	return "cl_" + hex.EncodeToString(b), nil
}

// ClaimByID reads claim id.
func ClaimByID(ctx context.Context, tx *sql.Tx, id string) (Claim, error) {
	var c Claim
	var situation, rule, sameAs, status sql.NullString
	err := tx.QueryRowContext(ctx, `SELECT id, situation, rule, same_as, same_as_status, created FROM claims WHERE id = ?`, id).
		Scan(&c.ID, &situation, &rule, &sameAs, &status, timeDest{&c.Created})
	if err != nil {
		return Claim{}, fmt.Errorf("read claim %s: %w", id, err)
	}
	c.Key = ClaimKey{situation.String, rule.String}
	c.SameAs, c.SameAsStatus = sameAs.String, SameAsStatus(status.String)
	return c, nil
}

// ScopeClaims returns the claims with a key that are open, active or rejected in the scope of
// workspaceKey, or active or rejected in global, by id: the claim keys of the extractor input
// (§7.3). A claim is open with an open or proposed candidate, active with an active item, and
// rejected while its rejection holds. A global candidate is not approved global context: it can
// come from another workspace. A forgotten claim has no key and is not returned.
func ScopeClaims(ctx context.Context, tx *sql.Tx, workspaceKey string) ([]Claim, error) {
	return query(ctx, tx, `SELECT id, situation, rule, created FROM claims WHERE situation IS NOT NULL AND rule IS NOT NULL AND id IN (
			SELECT claim_id FROM candidates WHERE status IN (?2, ?3) AND workspace_key = ?1
			UNION SELECT claim_id FROM items WHERE state = ?4 AND COALESCE(workspace_key, '') IN (?1, '')
			UNION SELECT claim_id FROM rejections WHERE reopened IS NULL AND COALESCE(workspace_key, '') IN (?1, ''))
		ORDER BY id`,
		func(rows *sql.Rows) (Claim, error) {
			var c Claim
			err := rows.Scan(&c.ID, &c.Key.Situation, &c.Key.Rule, timeDest{&c.Created})
			return c, err
		}, workspaceKey, CandidateOpen, CandidateProposed, StateActive)
}

// Match is what Brain knows about a same_as link that the extractor proposes (§7.3).
type Match struct {
	Clear       bool // the extractor gives a clear match
	Permissions bool // the extractor flags both claims as a permission or a safety rule
}

// permissionText finds text that may change a permission or a safety rule: the words of the
// review warning (internal/cli permissionWords).
var permissionText = regexp.MustCompile(`(?i)allow|permission|sandbox|--dangerously|\b(never|always)\s+run`)

// TouchesPermission reports whether text may change a permission or a safety rule. Brain checks
// the permission flag outside the model with it (§7.3), so neither SameAs nor the extractor
// trusts the model flag alone.
func TouchesPermission(text string) bool { return permissionText.MatchString(text) }

func (k ClaimKey) touchesPermission() bool { return TouchesPermission(k.Situation + " " + k.Rule) }

// SameAs links claim id to claim other: the key of id means the same as the key of other. A
// clear match is confirmed. An unclear match, and any match of two permission or safety
// rules, waits for review (§7.2). The link keeps both claim ids and their evidence links.
func SameAs(ctx context.Context, tx *sql.Tx, id, other string, m Match) (SameAsStatus, error) {
	source, err := ClaimByID(ctx, tx, id)
	if err != nil {
		return "", err
	}
	target, err := ClaimByID(ctx, tx, other)
	if err != nil {
		return "", err
	}
	if source.Key.touchesPermission() && target.Key.touchesPermission() {
		m.Permissions = true
	}
	// A link points at the claim id that other has, so a key never needs two hops.
	if target.SameAsStatus == SameAsConfirmed {
		other = target.SameAs
	}
	status := SameAsProposed
	if m.Clear && !m.Permissions {
		status = SameAsConfirmed
	}
	if _, err := tx.ExecContext(ctx, `UPDATE claims SET same_as = ?, same_as_status = ? WHERE id = ?`,
		other, status, id); err != nil {
		return "", fmt.Errorf("link claim %s to %s: %w", id, other, err)
	}
	return status, nil
}

// CandidateKind is the kind of a candidate: a kind of the §7.2 gate table. Its values are the
// signal kinds of the same name (internal/signal).
type CandidateKind string

// The candidate kinds.
const (
	CandidateCorrection   CandidateKind = "correction"
	CandidateFailure      CandidateKind = "failure"
	CandidateProcedure    CandidateKind = "procedure"
	CandidateCommandChain CandidateKind = "command_chain"
)

// CandidateStatus is the status of a candidate (§11).
type CandidateStatus string

// The candidate statuses.
const (
	CandidateOpen     CandidateStatus = "open"
	CandidateProposed CandidateStatus = "proposed"
	CandidateExpired  CandidateStatus = "expired"
)

// Candidate is one row of candidates: one claim in one scope (§7.2).
type Candidate struct {
	ID           int64
	ClaimID      string
	Scope        Scope
	WorkspaceKey string // "" for global
	Kind         CandidateKind
	Harnesses    []string // the harnesses of its evidence, sorted
	Count        int      // occurrences: evidence links
	Sessions     int      // distinct capture sessions of its evidence
	Priority     int      // 1 when the gate of its kind is reached, else 0
	FirstSeen    time.Time
	LastSeen     time.Time
	Status       CandidateStatus
}

// Gate is the queue gate of one candidate kind (§7.2).
type Gate struct {
	Min      int
	Sessions bool // Min counts distinct capture sessions, not occurrences
}

// Gates holds the gate of each candidate kind.
type Gates map[CandidateKind]Gate

// DefaultGates returns the gate defaults of §7.2. [NEW] Brain has no config file yet: a later
// config sets this value.
func DefaultGates() Gates {
	return Gates{
		CandidateCorrection:   {Min: 2},
		CandidateFailure:      {Min: 2, Sessions: true},
		CandidateProcedure:    {Min: 3},
		CandidateCommandChain: {Min: 3, Sessions: true},
	}
}

// Support returns the evidence of c that its gate counts: sessions or occurrences.
func (g Gates) Support(c Candidate) int {
	if g[c.Kind].Sessions {
		return c.Sessions
	}
	return c.Count
}

// Reached reports whether c reached the gate of its kind. The gate orders the queue and sets
// the label; it is not a hard rule (§7.2).
func (g Gates) Reached(c Candidate) bool { return g.Support(c) >= g[c.Kind].Min }

// ErrEvidenceRefused is the error of an evidence link that Brain does not attach: the
// observation is from another scope, its sender cannot support the candidate kind (§6.5), or
// it does not count (§6.4). A command record supports a command chain candidate even when its
// pane read counts instead: the pane read holds no command record, and an assistant sender
// cannot support a command chain, so the occurrence still counts once (E17-S6).
var ErrEvidenceRefused = errors.New("evidence refused")

// Attach adds observation obs as evidence of the candidate of c.ClaimID in the scope of c
// (c.Scope and c.WorkspaceKey), of kind c.Kind, and returns the candidate id. It makes the
// candidate with its first evidence. supports says whether a sender can support a kind
// (signal.Supports). An observation that is already evidence of the candidate adds nothing.
// A new occurrence opens an expired candidate again.
func Attach(ctx context.Context, tx *sql.Tx, c Candidate, obs int64, gates Gates, supports func(CandidateKind, Sender) bool) (int64, error) {
	var l struct {
		identity, workspace, harness string
		sender                       Sender
		session                      int64
		counts, command              bool
		at                           time.Time
	}
	err := tx.QueryRowContext(ctx, `SELECT o.capture_identity, COALESCE(g.workspace_key, ''), s.harness, o.sender,
		s.id, o.counts, EXISTS (SELECT 1 FROM command_records c WHERE c.observation_id = o.id), o.at
		FROM observations o JOIN segments g ON g.id = o.segment_id JOIN sessions s ON s.id = g.session_id
		WHERE o.id = ?`, obs).Scan(&l.identity, &l.workspace, &l.harness, &l.sender, &l.session, &l.counts, &l.command,
		timeDest{&l.at})
	if err != nil {
		return 0, fmt.Errorf("read observation %d: %w", obs, err)
	}
	switch {
	case c.Scope == ScopeWorkspace && l.workspace != c.WorkspaceKey:
		return 0, fmt.Errorf("observation %d is from workspace %q, not %q: %w", obs, l.workspace, c.WorkspaceKey, ErrEvidenceRefused)
	case !supports(c.Kind, l.sender):
		return 0, fmt.Errorf("observation %d: sender %s cannot support a %s: %w", obs, l.sender, c.Kind, ErrEvidenceRefused)
	case !l.counts && (!l.command || c.Kind != CandidateCommandChain):
		return 0, fmt.Errorf("observation %d does not count: %w", obs, ErrEvidenceRefused)
	}
	var id int64
	var kind CandidateKind
	err = tx.QueryRowContext(ctx, `INSERT INTO candidates
		(claim_id, scope, workspace_key, kind, harnesses, count, sessions, priority, first_seen, last_seen, status)
		VALUES (?1, ?2, ?3, ?4, '[]', 0, 0, 0, ?5, ?5, ?6)
		ON CONFLICT (claim_id, COALESCE(workspace_key, '')) DO UPDATE SET id = id RETURNING id, kind`,
		c.ClaimID, c.Scope, nullText(c.WorkspaceKey), c.Kind, timeArg(l.at), CandidateOpen).Scan(&id, &kind)
	if err != nil {
		return 0, fmt.Errorf("add candidate of claim %s: %w", c.ClaimID, err)
	}
	if kind != c.Kind {
		return 0, fmt.Errorf("candidate %d is a %s, not a %s", id, kind, c.Kind)
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO evidence_links
		(candidate_id, observation_id, capture_identity, session_id, workspace_key, harness, sender, at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT DO NOTHING`,
		id, obs, l.identity, l.session, nullText(l.workspace), l.harness, l.sender, timeArg(l.at))
	if err != nil {
		return 0, fmt.Errorf("link observation %d to candidate %d: %w", obs, id, err)
	}
	if n, err := res.RowsAffected(); err != nil || n == 0 {
		return id, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE candidates SET status = ? WHERE id = ? AND status = ?`,
		CandidateOpen, id, CandidateExpired); err != nil {
		return 0, fmt.Errorf("open candidate %d: %w", id, err)
	}
	return id, recount(ctx, tx, id, gates)
}

// recount sets the harnesses, counts, times and priority of candidate id from its evidence
// links. It deletes a candidate that has no evidence left, after dropScripts.
func recount(ctx context.Context, tx *sql.Tx, id int64, gates Gates) error {
	if err := dropScripts(ctx, tx, id); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM candidates WHERE id = ?1
		AND NOT EXISTS (SELECT 1 FROM evidence_links WHERE candidate_id = ?1)`, id)
	if err != nil {
		return fmt.Errorf("delete candidate %d: %w", id, err)
	}
	if n, err := res.RowsAffected(); err != nil || n > 0 {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE candidates SET
		harnesses = (SELECT json_group_array(harness) FROM
			(SELECT DISTINCT harness FROM evidence_links WHERE candidate_id = ?1 ORDER BY harness)),
		count = (SELECT count(*) FROM evidence_links WHERE candidate_id = ?1),
		sessions = (SELECT count(DISTINCT session_id) FROM evidence_links WHERE candidate_id = ?1),
		first_seen = (SELECT min(at) FROM evidence_links WHERE candidate_id = ?1),
		last_seen = (SELECT max(at) FROM evidence_links WHERE candidate_id = ?1)
		WHERE id = ?1`, id)
	if err != nil {
		return fmt.Errorf("count evidence of candidate %d: %w", id, err)
	}
	c, err := CandidateByID(ctx, tx, id)
	if err != nil {
		return err
	}
	priority := 0
	if gates.Reached(c) {
		priority = 1
	}
	if _, err := tx.ExecContext(ctx, `UPDATE candidates SET priority = ? WHERE id = ?`, priority, id); err != nil {
		return fmt.Errorf("set priority of candidate %d: %w", id, err)
	}
	return nil
}

// candidateColumns are the columns that scanCandidate reads, in order.
const candidateColumns = `id, claim_id, scope, COALESCE(workspace_key, ''), kind, harnesses, count, sessions, priority,
	first_seen, last_seen, status`

func scanCandidate(rows interface{ Scan(...any) error }) (Candidate, error) {
	var c Candidate
	err := rows.Scan(&c.ID, &c.ClaimID, &c.Scope, &c.WorkspaceKey, &c.Kind, listDest[string]{&c.Harnesses}, &c.Count,
		&c.Sessions, &c.Priority, timeDest{&c.FirstSeen}, timeDest{&c.LastSeen}, &c.Status)
	return c, err
}

// CandidateByID reads candidate id.
func CandidateByID(ctx context.Context, tx *sql.Tx, id int64) (Candidate, error) {
	c, err := scanCandidate(tx.QueryRowContext(ctx, `SELECT `+candidateColumns+` FROM candidates WHERE id = ?`, id))
	if err != nil {
		return Candidate{}, fmt.Errorf("read candidate %d: %w", id, err)
	}
	return c, nil
}

// Candidates returns every candidate in queue order: the ones that reached their gate first,
// then the newest occurrence first (§7.2).
func Candidates(ctx context.Context, tx *sql.Tx) ([]Candidate, error) {
	return query(ctx, tx, `SELECT `+candidateColumns+` FROM candidates ORDER BY priority DESC, last_seen DESC, id`,
		func(rows *sql.Rows) (Candidate, error) { return scanCandidate(rows) })
}

// PendingCandidateProposalIDs returns the ids of the pending proposals of candidate id, in
// id order.
func PendingCandidateProposalIDs(ctx context.Context, tx *sql.Tx, id int64) ([]int64, error) {
	return query(ctx, tx, `SELECT id FROM proposals WHERE candidate_id = ? AND status = ? ORDER BY id`,
		func(rows *sql.Rows) (int64, error) {
			var id int64
			err := rows.Scan(&id)
			return id, err
		}, id, ProposalPending)
}

// SessionCandidates returns the candidates with evidence from session sessionID, by id: the
// ones that brain forget --session lists (§6.7).
func SessionCandidates(ctx context.Context, tx *sql.Tx, sessionID int64) ([]Candidate, error) {
	return query(ctx, tx, `SELECT `+candidateColumns+` FROM candidates
		WHERE id IN (SELECT candidate_id FROM evidence_links WHERE session_id = ?) ORDER BY id`,
		func(rows *sql.Rows) (Candidate, error) { return scanCandidate(rows) }, sessionID)
}

// expiry is how long an open candidate stays with no new occurrence (§7.2).
const expiry = 30 * 24 * time.Hour

// ExpireCandidates sets the open candidates with no new occurrence in 30 days to expired, and
// returns how many. queued is how long evidence has waited in the extraction queue (E13): that
// time does not count (§7.2). The evidence in the queue is not linked yet and can belong to
// any candidate, so one duration, the wait of the oldest queued evidence, applies to all.
func ExpireCandidates(ctx context.Context, tx *sql.Tx, now time.Time, queued time.Duration) (int64, error) {
	res, err := tx.ExecContext(ctx, `UPDATE candidates SET status = ? WHERE status = ? AND last_seen < ?`,
		CandidateExpired, CandidateOpen, timeArg(now.Add(-expiry-queued)))
	if err != nil {
		return 0, fmt.Errorf("expire candidates: %w", err)
	}
	return res.RowsAffected()
}

// forgetEvidence deletes the evidence links of session sessionID and counts the candidates
// that used them again. A candidate with no evidence left goes (§6.7).
func forgetEvidence(ctx context.Context, tx *sql.Tx, sessionID int64, gates Gates) error {
	cs, err := SessionCandidates(ctx, tx, sessionID)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM evidence_links WHERE session_id = ?`, sessionID); err != nil {
		return fmt.Errorf("delete evidence links of session %d: %w", sessionID, err)
	}
	for _, c := range cs {
		if err := recount(ctx, tx, c.ID, gates); err != nil {
			return err
		}
	}
	return nil
}
