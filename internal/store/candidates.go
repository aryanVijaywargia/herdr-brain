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
	// Kind is the claim kind of the extractor (#279); "" for a claim made before it was kept.
	Kind string
	// TouchesPermission is true once the extractor or the Brain check flagged the claim as a
	// permission or a safety rule (§7.3).
	TouchesPermission bool
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
	var situation, rule, sameAs, status, kind sql.NullString
	err := tx.QueryRowContext(ctx, `SELECT id, situation, rule, same_as, same_as_status, created, kind, touches_permission
		FROM claims WHERE id = ?`, id).
		Scan(&c.ID, &situation, &rule, &sameAs, &status, timeDest{&c.Created}, &kind, &c.TouchesPermission)
	if err != nil {
		return Claim{}, fmt.Errorf("read claim %s: %w", id, err)
	}
	c.Key = ClaimKey{situation.String, rule.String}
	c.SameAs, c.SameAsStatus, c.Kind = sameAs.String, SameAsStatus(status.String), kind.String
	return c, nil
}

// MarkClaim records the claim kind of claim id, if it has none yet, and a permission flag: once
// set, the flag stays (#279). The merge check reads both (§7.2).
func MarkClaim(ctx context.Context, tx *sql.Tx, id, kind string, permission bool) error {
	if _, err := tx.ExecContext(ctx, `UPDATE claims SET kind = COALESCE(kind, ?), touches_permission = MAX(touches_permission, ?)
		WHERE id = ?`, kind, permission, id); err != nil {
		return fmt.Errorf("mark claim %s: %w", id, err)
	}
	return nil
}

// ScopeClaims returns the claims with a key that are open, active or rejected in the scope of
// workspaceKey, or active or rejected in global, by id: the claim keys of the extractor input
// (§7.3). A claim is open with an open or proposed candidate, active with an active item, and
// rejected while its rejection holds. A global candidate is not approved global context: it can
// come from another workspace. A forgotten claim has no key and is not returned.
//
// The claim of an item that brain remember made has no key (#312): its key is the text of the
// current revision of its first item, with no situation. It is returned while the item is
// active in that scope or in global, or waits for review in that scope. An automation states no
// rule, so its claim is not returned.
func ScopeClaims(ctx context.Context, tx *sql.Tx, workspaceKey string) ([]Claim, error) {
	return query(ctx, tx, `SELECT id, situation, rule, created FROM claims WHERE situation IS NOT NULL AND rule IS NOT NULL AND id IN (
			SELECT claim_id FROM candidates WHERE status IN (?2, ?3) AND workspace_key = ?1
			UNION SELECT claim_id FROM items WHERE state = ?4 AND COALESCE(workspace_key, '') IN (?1, '')
			UNION SELECT claim_id FROM rejections WHERE reopened IS NULL AND COALESCE(workspace_key, '') IN (?1, ''))
		UNION ALL SELECT i.claim_id, '', r.text, i.created FROM items i
			JOIN item_revisions r ON r.item_id = i.id AND r.revision = i.current_revision
			WHERE i.id IN (SELECT MIN(id) FROM items WHERE kind != ?5
				AND NOT EXISTS (SELECT 1 FROM claims k WHERE k.id = items.claim_id AND k.situation IS NOT NULL)
				AND (state = ?4 AND COALESCE(workspace_key, '') IN (?1, '')
					OR state = ?6 AND workspace_key = ?1
						AND EXISTS (SELECT 1 FROM proposals p WHERE p.item_id = items.id AND p.status = ?7))
				GROUP BY claim_id)
		ORDER BY 1`,
		func(rows *sql.Rows) (Claim, error) {
			var c Claim
			err := rows.Scan(&c.ID, &c.Key.Situation, &c.Key.Rule, timeDest{&c.Created})
			return c, err
		}, workspaceKey, CandidateOpen, CandidateProposed, StateActive, KindAutomation, StateDraft, ProposalPending)
}

// ClaimHeld reports whether claim claimID is proposed or approved in the scope of workspaceKey
// ("" for global): an item of the claim in that scope or in global is active, or waits for
// review with a pending proposal. A claim that means the same then adds no proposal of its own
// (#312, §7.3 order for a known claim).
func ClaimHeld(ctx context.Context, tx *sql.Tx, claimID, workspaceKey string) (bool, error) {
	var held bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM items i WHERE i.claim_id = ?1
		AND COALESCE(i.workspace_key, '') IN (?2, '')
		AND (i.state = ?3 OR EXISTS (SELECT 1 FROM proposals p WHERE p.item_id = i.id AND p.status = ?4)))`,
		claimID, workspaceKey, StateActive, ProposalPending).Scan(&held)
	if err != nil {
		return false, fmt.Errorf("read held state of claim %s: %w", claimID, err)
	}
	return held, nil
}

// ClaimText returns the key text of claim id for the permission check (§7.3): its situation and
// rule. The claim of a brain remember item has no key: its text is the current text of its
// first item, as in ScopeClaims (#312). A forgotten claim gives "".
func ClaimText(ctx context.Context, tx *sql.Tx, id string) (string, error) {
	var text string
	err := tx.QueryRowContext(ctx, `SELECT COALESCE(
		(SELECT situation || ' ' || rule FROM claims WHERE id = ?1 AND situation IS NOT NULL AND rule IS NOT NULL),
		(SELECT r.text FROM items i JOIN item_revisions r ON r.item_id = i.id AND r.revision = i.current_revision
			WHERE i.claim_id = ?1 ORDER BY i.id LIMIT 1), '')`, id).Scan(&text)
	if err != nil {
		return "", fmt.Errorf("read text of claim %s: %w", id, err)
	}
	return text, nil
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
// The claim of an item that brain remember made has no row yet: the link adds it, with no key
// text, so the link has its target (#312).
func SameAs(ctx context.Context, tx *sql.Tx, id, other string, m Match) (SameAsStatus, error) {
	source, err := ClaimByID(ctx, tx, id)
	if err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO claims (id, created) SELECT claim_id, created FROM items
		WHERE claim_id = ? ORDER BY id LIMIT 1 ON CONFLICT (id) DO NOTHING`, other); err != nil {
		return "", fmt.Errorf("add claim %s of its item: %w", other, err)
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

// The candidate kinds. A lesson is an explicit lesson in plain words (#253).
const (
	CandidateCorrection   CandidateKind = "correction"
	CandidateLesson       CandidateKind = "lesson"
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
	// Via is the claim whose key brings new evidence through a same_as link (#279): Attach
	// records it on each new evidence link only, so the merge can be undone. "" for own evidence.
	Via string
}

// Gate is the queue gate of one candidate kind (§7.2).
type Gate struct {
	Min      int
	Sessions bool // Min counts distinct capture sessions, not occurrences
}

// Gates holds the gate of each candidate kind.
type Gates map[CandidateKind]Gate

// DefaultGates returns the gate defaults of §7.2. The config can change them
// (extract.ReadConfig).
func DefaultGates() Gates {
	return Gates{
		CandidateCorrection:   {Min: 2},
		CandidateLesson:       {Min: 1},
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
	l, err := readEvidence(ctx, tx, c, obs, supports)
	if err != nil {
		return 0, err
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
	// Only a new link records the merge: evidence that the candidate had already stays its own.
	if c.Via != "" {
		if _, err := tx.ExecContext(ctx, `UPDATE evidence_links SET via_claim = ? WHERE candidate_id = ? AND observation_id = ?`,
			c.Via, id, obs); err != nil {
			return 0, fmt.Errorf("mark evidence %d of candidate %d as merged: %w", obs, id, err)
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE candidates SET status = ? WHERE id = ? AND status = ?`,
		CandidateOpen, id, CandidateExpired); err != nil {
		return 0, fmt.Errorf("open candidate %d: %w", id, err)
	}
	return id, recount(ctx, tx, id, gates)
}

// observed is what Attach reads of an observation: the fields of its evidence link, and what
// the checks need.
type observed struct {
	identity, workspace, harness string
	sender                       Sender
	session                      int64
	counts, command              bool
	at                           time.Time
}

// readEvidence reads observation obs for Attach and checks that it can be evidence of c. A
// refused observation gives an error that wraps ErrEvidenceRefused.
func readEvidence(ctx context.Context, tx *sql.Tx, c Candidate, obs int64, supports func(CandidateKind, Sender) bool) (observed, error) {
	var l observed
	err := tx.QueryRowContext(ctx, `SELECT o.capture_identity, COALESCE(g.workspace_key, ''), s.harness, o.sender,
		s.id, o.counts, EXISTS (SELECT 1 FROM command_records c WHERE c.observation_id = o.id), o.at
		FROM observations o JOIN segments g ON g.id = o.segment_id JOIN sessions s ON s.id = g.session_id
		WHERE o.id = ?`, obs).Scan(&l.identity, &l.workspace, &l.harness, &l.sender, &l.session, &l.counts, &l.command,
		timeDest{&l.at})
	if err != nil {
		return observed{}, fmt.Errorf("read observation %d: %w", obs, err)
	}
	switch {
	case c.Scope == ScopeWorkspace && l.workspace != c.WorkspaceKey:
		return observed{}, fmt.Errorf("observation %d is from workspace %q, not %q: %w", obs, l.workspace, c.WorkspaceKey, ErrEvidenceRefused)
	case !supports(c.Kind, l.sender):
		return observed{}, fmt.Errorf("observation %d: sender %s cannot support a %s: %w", obs, l.sender, c.Kind, ErrEvidenceRefused)
	case !l.counts && (!l.command || c.Kind != CandidateCommandChain):
		return observed{}, fmt.Errorf("observation %d does not count: %w", obs, ErrEvidenceRefused)
	}
	return l, nil
}

// CheckEvidence runs the checks of Attach for observation obs as evidence of c, and writes
// nothing: an error that wraps ErrEvidenceRefused when Attach would refuse it. Evidence with no
// candidate to attach to still counts for a comeback (#312, §5.4).
func CheckEvidence(ctx context.Context, tx *sql.Tx, c Candidate, obs int64, supports func(CandidateKind, Sender) bool) error {
	_, err := readEvidence(ctx, tx, c, obs, supports)
	return err
}

// EvidenceElsewhere reports whether observation obs is evidence of a candidate other than
// candidate id (#314).
func EvidenceElsewhere(ctx context.Context, tx *sql.Tx, obs, id int64) (bool, error) {
	var found bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM evidence_links
		WHERE observation_id = ? AND candidate_id != ?)`, obs, id).Scan(&found)
	if err != nil {
		return false, fmt.Errorf("read evidence of observation %d: %w", obs, err)
	}
	return found, nil
}

// SetCandidateKind sets the kind of candidate id and counts its gate again: the kind follows
// the claim (#280, §7.2).
func SetCandidateKind(ctx context.Context, tx *sql.Tx, id int64, kind CandidateKind, gates Gates) error {
	if _, err := tx.ExecContext(ctx, `UPDATE candidates SET kind = ? WHERE id = ?`, kind, id); err != nil {
		return fmt.Errorf("set kind of candidate %d: %w", id, err)
	}
	return recount(ctx, tx, id, gates)
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

// NewestEvidence returns the observation of the newest evidence link of candidate id.
func NewestEvidence(ctx context.Context, tx *sql.Tx, id int64) (int64, error) {
	var obs int64
	err := tx.QueryRowContext(ctx, `SELECT observation_id FROM evidence_links WHERE candidate_id = ?
		ORDER BY at DESC, observation_id DESC LIMIT 1`, id).Scan(&obs)
	if err != nil {
		return 0, fmt.Errorf("read newest evidence of candidate %d: %w", id, err)
	}
	return obs, nil
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
