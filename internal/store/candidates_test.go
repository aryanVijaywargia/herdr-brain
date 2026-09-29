package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

// supportsForTest is the §6.5 table of the gated kinds, as signal.Supports gives it: the store
// cannot import internal/signal. The signal tests check Attach with signal.Supports.
func supportsForTest(k CandidateKind, s Sender) bool {
	if k == CandidateFailure || k == CandidateCommandChain {
		return s == SenderTool
	}
	return s == SenderYou || s == SenderUnconfirmed
}

// evidenceStore is a store with capture sessions for candidate tests.
type evidenceStore struct {
	*Store
	sessions map[string]int64 // by harness and key id
	n        int
}

func newEvidenceStore(t *testing.T) *evidenceStore {
	t.Helper()
	return &evidenceStore{Store: openM1(t), sessions: map[string]int64{}}
}

// observe adds one counting observation of sender at at, in the session keyID of harness in
// workspace ws, and returns its id.
func (e *evidenceStore) observe(t *testing.T, harness, keyID, ws string, sender Sender, at time.Time) int64 {
	t.Helper()
	ctx := context.Background()
	var id int64
	write(t, e.Store, func(tx *sql.Tx) error {
		sid, ok := e.sessions[harness+"/"+keyID]
		if !ok {
			var err error
			key := SessionKey{HerdrSession: "default", Harness: harness, Kind: KeyAgent, ID: keyID}
			if sid, err = OpenSession(ctx, tx, key, "w1:p1", ws, testNow); err != nil {
				return err
			}
			e.sessions[harness+"/"+keyID] = sid
		}
		seg, err := StartSegment(ctx, tx, sid, "w1:p1", ws, "visible", testNow)
		if err != nil {
			return err
		}
		e.n++
		id, err = AddObservation(ctx, tx, Observation{CaptureIdentity: fmt.Sprint("c", e.n), Source: SourceHerdr,
			SegmentID: seg, Event: "working", Excerpt: "no, use pnpm", Status: ObservationComplete, Sender: sender,
			Counts: true, At: at})
		return err
	})
	return id
}

// claim returns the claim id of a key with rule.
func (e *evidenceStore) claim(t *testing.T, rule string) string {
	t.Helper()
	var id string
	write(t, e.Store, func(tx *sql.Tx) (err error) {
		id, err = ClaimID(context.Background(), tx, ClaimKey{"in this repo", rule}, testNow)
		return err
	})
	return id
}

// attach attaches obs to the candidate of c and returns the candidate.
func (e *evidenceStore) attach(t *testing.T, c Candidate, obs int64) (Candidate, error) {
	t.Helper()
	var got Candidate
	err := e.Write(context.Background(), func(tx *sql.Tx) error {
		id, err := Attach(context.Background(), tx, c, obs, DefaultGates(), supportsForTest)
		if err != nil {
			return err
		}
		got, err = CandidateByID(context.Background(), tx, id)
		return err
	})
	return got, err
}

func (e *evidenceStore) mustAttach(t *testing.T, c Candidate, obs int64) Candidate {
	t.Helper()
	got, err := e.attach(t, c, obs)
	if err != nil {
		t.Fatalf("Attach(%+v, %d) = %v", c, obs, err)
	}
	return got
}

func (e *evidenceStore) links(t *testing.T) int {
	t.Helper()
	return queryInt(t, e.Store, `SELECT count(*) FROM evidence_links`)
}

// The same claim key gives the same claim id, and the id holds no text (§7.2).
func TestClaimID(t *testing.T) {
	e := newEvidenceStore(t)
	a, again, b := e.claim(t, "use pnpm"), e.claim(t, "use pnpm"), e.claim(t, "run go vet")
	if a != again {
		t.Errorf("the same key gave %s and %s, want one id", a, again)
	}
	if a == b {
		t.Errorf("two keys gave the same id %s", a)
	}
	if !strings.HasPrefix(a, "cl_") || strings.Contains(a, "pnpm") || strings.Contains(a, "repo") {
		t.Errorf("claim id %q, want an opaque cl_ id with no text", a)
	}
}

// A same_as link is a proposal: only a clear match of claims that are not both permission or
// safety rules is confirmed, and the old claim ids and their evidence stay (§7.2).
func TestSameAs(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		m    Match
		want SameAsStatus
	}{
		{"clear", Match{Clear: true}, SameAsConfirmed},
		{"unclear goes to review", Match{}, SameAsProposed},
		{"two permission rules go to review", Match{Clear: true, Permissions: true}, SameAsProposed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEvidenceStore(t)
			old := e.claim(t, "use pnpm")
			c := e.mustAttach(t, Candidate{ClaimID: old, Scope: ScopeGlobal, Kind: CandidateCorrection},
				e.observe(t, "claude", "a1", "ws-a", SenderUnconfirmed, testNow))
			reworded := e.claim(t, "always use pnpm")
			write(t, e.Store, func(tx *sql.Tx) error {
				got, err := SameAs(ctx, tx, reworded, old, tc.m)
				if err != nil || got != tc.want {
					return fmt.Errorf("SameAs() = %s, %w, want %s", got, err, tc.want)
				}
				return nil
			})
			wantID := reworded
			if tc.want == SameAsConfirmed {
				wantID = old
			}
			if got := e.claim(t, "always use pnpm"); got != wantID {
				t.Errorf("the reworded key has claim id %s, want %s", got, wantID)
			}
			if got := e.claim(t, "use pnpm"); got != old {
				t.Errorf("the old key has claim id %s, want %s", got, old)
			}
			write(t, e.Store, func(tx *sql.Tx) error {
				if _, err := CandidateByID(ctx, tx, c.ID); err != nil {
					return err
				}
				cl, err := ClaimByID(ctx, tx, reworded)
				if err == nil && (cl.SameAs != old || cl.SameAsStatus != tc.want) {
					err = fmt.Errorf("claim %+v, want same_as %s %s", cl, old, tc.want)
				}
				return err
			})
		})
	}
}

// AC: cross-harness count. One Claude and one Codex correction of one claim are one pattern,
// and they reach the gate of 2.
func TestCrossHarnessCount(t *testing.T) {
	e := newEvidenceStore(t)
	c := Candidate{ClaimID: e.claim(t, "use pnpm"), Scope: ScopeWorkspace, WorkspaceKey: "ws-a", Kind: CandidateCorrection}
	first := e.mustAttach(t, c, e.observe(t, "claude", "a1", "ws-a", SenderUnconfirmed, testNow))
	if first.Count != 1 || first.Priority != 0 || DefaultGates().Reached(first) {
		t.Errorf("after one correction: %+v, want count 1 below the gate", first)
	}
	second := e.mustAttach(t, c, e.observe(t, "codex", "b1", "ws-a", SenderUnconfirmed, testNow.Add(time.Hour)))
	if second.ID != first.ID {
		t.Errorf("the Codex correction made candidate %d, want the Claude one %d", second.ID, first.ID)
	}
	if second.Count != 2 || second.Priority != 1 || !DefaultGates().Reached(second) ||
		!slices.Equal(second.Harnesses, []string{"claude", "codex"}) {
		t.Errorf("after two corrections: %+v, want count 2 from claude and codex at the gate", second)
	}
	if !second.FirstSeen.Equal(testNow) || !second.LastSeen.Equal(testNow.Add(time.Hour)) || second.Status != CandidateOpen {
		t.Errorf("seen %v to %v, status %s, want %v to %v, open", second.FirstSeen, second.LastSeen, second.Status,
			testNow, testNow.Add(time.Hour))
	}
	// One observation counts once, also on a retry (§6.4).
	if again := e.mustAttach(t, c, 1); again.Count != 2 {
		t.Errorf("a second attach of one observation gave count %d, want 2", again.Count)
	}
}

// AC: evidence from workspace B cannot attach to a candidate of workspace A. A global
// candidate takes evidence from any workspace. A sender that cannot support the kind, and an
// observation that does not count, attach nothing either.
func TestAttachChecks(t *testing.T) {
	e := newEvidenceStore(t)
	claim := e.claim(t, "use pnpm")
	inA := Candidate{ClaimID: claim, Scope: ScopeWorkspace, WorkspaceKey: "ws-a", Kind: CandidateCorrection}
	fromB := e.observe(t, "claude", "b1", "ws-b", SenderUnconfirmed, testNow)
	if _, err := e.attach(t, inA, fromB); !errors.Is(err, ErrEvidenceRefused) {
		t.Errorf("Attach of workspace B evidence to a workspace A candidate = %v, want ErrEvidenceRefused", err)
	}
	e.mustAttach(t, Candidate{ClaimID: claim, Scope: ScopeGlobal, Kind: CandidateCorrection}, fromB)
	e.mustAttach(t, Candidate{ClaimID: claim, Scope: ScopeGlobal, Kind: CandidateCorrection},
		e.observe(t, "claude", "a1", "ws-a", SenderUnconfirmed, testNow))

	assistant := e.observe(t, "claude", "a1", "ws-a", SenderAssistant, testNow)
	tool := e.observe(t, "claude", "a1", "ws-a", SenderTool, testNow)
	replay := e.observe(t, "claude", "a1", "ws-a", SenderUnconfirmed, testNow)
	write(t, e.Store, func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE observations SET counts = 0 WHERE id = ?`, replay)
		return err
	})
	for _, tc := range []struct {
		name string
		kind CandidateKind
		obs  int64
	}{
		{"assistant text for a correction", CandidateCorrection, assistant},
		{"tool output for a correction", CandidateCorrection, tool},
		{"a captured message for a failure", CandidateFailure, e.observe(t, "claude", "a1", "ws-a", SenderUnconfirmed, testNow)},
		{"an observation that does not count", CandidateCorrection, replay},
	} {
		c := Candidate{ClaimID: e.claim(t, tc.name), Scope: ScopeWorkspace, WorkspaceKey: "ws-a", Kind: tc.kind}
		if _, err := e.attach(t, c, tc.obs); !errors.Is(err, ErrEvidenceRefused) {
			t.Errorf("%s: Attach = %v, want ErrEvidenceRefused", tc.name, err)
		}
	}
	if got := e.links(t); got != 2 {
		t.Errorf("evidence links = %d, want the 2 of the global candidate", got)
	}
	if got := queryInt(t, e.Store, `SELECT count(*) FROM candidates`); got != 1 {
		t.Errorf("candidates = %d, want 1: a refused link makes none", got)
	}
}

// AC: two failures in one session do not pass the 2-session gate; one more in another
// session does.
func TestSessionGate(t *testing.T) {
	e := newEvidenceStore(t)
	c := Candidate{ClaimID: e.claim(t, "run go mod tidy first"), Scope: ScopeWorkspace, WorkspaceKey: "ws-a",
		Kind: CandidateFailure}
	e.mustAttach(t, c, e.observe(t, "codex", "s1", "ws-a", SenderTool, testNow))
	got := e.mustAttach(t, c, e.observe(t, "codex", "s1", "ws-a", SenderTool, testNow.Add(time.Minute)))
	if got.Count != 2 || got.Sessions != 1 || got.Priority != 0 || DefaultGates().Reached(got) {
		t.Errorf("two failures in one session: %+v, want 2 occurrences in 1 session, below the gate", got)
	}
	got = e.mustAttach(t, c, e.observe(t, "codex", "s2", "ws-a", SenderTool, testNow.Add(time.Hour)))
	if got.Sessions != 2 || got.Priority != 1 || !DefaultGates().Reached(got) {
		t.Errorf("a failure in a second session: %+v, want 2 sessions at the gate", got)
	}
}

// The §7.2 defaults: correction 2, failure 2 sessions, procedure 3, command chain 3 sessions.
func TestDefaultGates(t *testing.T) {
	want := Gates{
		CandidateCorrection:   {Min: 2},
		CandidateFailure:      {Min: 2, Sessions: true},
		CandidateProcedure:    {Min: 3},
		CandidateCommandChain: {Min: 3, Sessions: true},
	}
	got := DefaultGates()
	for k, g := range want {
		if got[k] != g {
			t.Errorf("gate of %s = %+v, want %+v", k, got[k], g)
		}
	}
	if len(got) != len(want) {
		t.Errorf("DefaultGates() = %v, want %v", got, want)
	}
}

// AC: expiry with a fake clock, with and without queue time. A candidate expires after 30
// days with no new occurrence; time in the extraction queue does not count. A new occurrence
// opens it again.
func TestExpireCandidates(t *testing.T) {
	ctx := context.Background()
	e := newEvidenceStore(t)
	c := Candidate{ClaimID: e.claim(t, "use pnpm"), Scope: ScopeWorkspace, WorkspaceKey: "ws-a", Kind: CandidateCorrection}
	id := e.mustAttach(t, c, e.observe(t, "claude", "a1", "ws-a", SenderUnconfirmed, testNow)).ID
	day := 24 * time.Hour
	for _, step := range []struct {
		name   string
		now    time.Time
		queued time.Duration
		want   CandidateStatus
	}{
		{"29 days", testNow.Add(29 * day), 0, CandidateOpen},
		{"30 days", testNow.Add(30 * day), 0, CandidateOpen},
		{"31 days, 2 of them in the queue", testNow.Add(31 * day), 2 * day, CandidateOpen},
		{"31 days", testNow.Add(31 * day), 0, CandidateExpired},
	} {
		write(t, e.Store, func(tx *sql.Tx) error {
			if _, err := ExpireCandidates(ctx, tx, step.now, step.queued); err != nil {
				return err
			}
			got, err := CandidateByID(ctx, tx, id)
			if err == nil && got.Status != step.want {
				t.Errorf("%s: status %s, want %s", step.name, got.Status, step.want)
			}
			return err
		})
	}
	got := e.mustAttach(t, c, e.observe(t, "claude", "a1", "ws-a", SenderUnconfirmed, testNow.Add(40*day)))
	if got.ID != id || got.Status != CandidateOpen || got.Count != 2 {
		t.Errorf("a new occurrence after expiry: %+v, want candidate %d open with 2 occurrences", got, id)
	}
}

// Forget of a session deletes its evidence links and counts the candidates again; a
// candidate with no evidence left goes (§6.7).
func TestForgetSessionEvidence(t *testing.T) {
	ctx := context.Background()
	e := newEvidenceStore(t)
	shared := Candidate{ClaimID: e.claim(t, "use pnpm"), Scope: ScopeWorkspace, WorkspaceKey: "ws-a", Kind: CandidateCorrection}
	only := Candidate{ClaimID: e.claim(t, "run go vet"), Scope: ScopeWorkspace, WorkspaceKey: "ws-a", Kind: CandidateCorrection}
	e.mustAttach(t, shared, e.observe(t, "claude", "a1", "ws-a", SenderUnconfirmed, testNow))
	s1 := e.sessions["claude/a1"]
	e.mustAttach(t, only, e.observe(t, "claude", "a1", "ws-a", SenderUnconfirmed, testNow))
	kept := e.mustAttach(t, shared, e.observe(t, "codex", "b1", "ws-a", SenderUnconfirmed, testNow.Add(time.Hour)))
	write(t, e.Store, func(tx *sql.Tx) error {
		cs, err := SessionCandidates(ctx, tx, s1)
		if err != nil {
			return err
		}
		if len(cs) != 2 {
			t.Errorf("SessionCandidates = %+v, want both candidates", cs)
		}
		ev, err := SessionEvidence(ctx, tx, s1)
		if err == nil && ev.EvidenceLinks != 2 {
			t.Errorf("SessionEvidence = %+v, want 2 evidence links", ev)
		}
		return err
	})
	write(t, e.Store, func(tx *sql.Tx) error { return ForgetSession(ctx, tx, s1) })
	if got := queryInt(t, e.Store, fmt.Sprintf(`SELECT count(*) FROM evidence_links WHERE session_id = %d`, s1)); got != 0 {
		t.Errorf("%d evidence links of the forgotten session stay", got)
	}
	write(t, e.Store, func(tx *sql.Tx) error {
		got, err := CandidateByID(ctx, tx, kept.ID)
		if err != nil {
			return err
		}
		if got.Count != 1 || got.Priority != 0 || !slices.Equal(got.Harnesses, []string{"codex"}) ||
			!got.FirstSeen.Equal(testNow.Add(time.Hour)) {
			t.Errorf("after the forget: %+v, want 1 codex occurrence below the gate", got)
		}
		return nil
	})
	if n := queryInt(t, e.Store, `SELECT count(*) FROM candidates`); n != 1 {
		t.Errorf("candidates = %d, want 1: the one with no evidence left goes", n)
	}
}

// Forget of an item removes the claim key text of its claim, and the claim id stays (§6.7).
func TestEraseItemClaimText(t *testing.T) {
	ctx := context.Background()
	e := newEvidenceStore(t)
	claim := e.claim(t, "use pnpm")
	write(t, e.Store, func(tx *sql.Tx) error {
		id, err := InsertItem(ctx, tx, Item{ClaimID: claim, CurrentRevision: 1, Kind: KindFact, Scope: ScopeGlobal,
			State: StateActive, Anchors: []string{}, Created: testNow})
		if err != nil {
			return err
		}
		if err := EraseItem(ctx, tx, id, testNow); err != nil {
			return err
		}
		left, err := ItemTextLeft(ctx, tx, id)
		if err != nil {
			return err
		}
		if len(left) != 0 {
			t.Errorf("ItemTextLeft = %v, want none", left)
		}
		c, err := ClaimByID(ctx, tx, claim)
		if err == nil && c.Key != (ClaimKey{}) {
			t.Errorf("claim %s keeps its key %+v after the forget", claim, c.Key)
		}
		return err
	})
}
