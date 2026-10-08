package store

import (
	"context"
	"database/sql"
	"fmt"
	"reflect"
	"testing"
	"time"
)

// handoffStore holds the rows of TestReadHandoff: the receipts of two sessions in workspace ka,
// items approved around the start of session "a", and its capture with hook records.
type handoffStore struct {
	s   *Store
	t0  time.Time // the first receipt of agent session "a"
	obs int       // the hook records so far, for their capture identities
}

// approve adds item id, approved at revision rev at at, after rev-1 earlier revisions. A
// proposal makes it an approval; with proposal false the revision is a pin or an undo.
func (h *handoffStore) approve(ctx context.Context, tx *sql.Tx, id, rev int64, key *string, at time.Time, proposal bool) error {
	scope := ScopeGlobal
	if key != nil {
		scope = ScopeWorkspace
	}
	if rev == 1 {
		if _, err := InsertItem(ctx, tx, Item{ID: id, ClaimID: fmt.Sprint("c", id), CurrentRevision: 1, Kind: KindFact,
			Scope: scope, WorkspaceKey: key, State: StateActive, Created: at}); err != nil {
			return err
		}
	}
	r := ItemRevision{ItemID: id, Revision: rev, Text: "rule", Scope: scope, WorkspaceKey: key, State: StateActive,
		Author: AuthorYou, Origin: OriginReview, Created: at}
	if proposal {
		p, err := InsertProposal(ctx, tx, Proposal{ItemID: &id, Text: "rule", Scope: scope, Target: "fact", Diff: "+rule",
			ExpectedBenefit: "b", Status: ProposalApproved, Created: at})
		if err != nil {
			return err
		}
		r.ProposalID = &p
	}
	return InsertItemRevision(ctx, tx, r)
}

// command adds a hook record of argv with exit status exit (nil: none) in segment seg.
func (h *handoffStore) command(ctx context.Context, tx *sql.Tx, seg int64, argv string, exit *int, brain, secret bool) error {
	h.obs++
	o, err := AddObservation(ctx, tx, Observation{CaptureIdentity: fmt.Sprint("hook-", h.obs), Source: SourceHook,
		SegmentID: seg, Event: "tool", Excerpt: argv, Status: ObservationComplete, Sender: SenderTool, Counts: true,
		BrainOrigin: brain, At: h.t0.Add(time.Duration(h.obs) * time.Minute)})
	if err != nil {
		return err
	}
	c := CommandRecord{ObservationID: o, Argv: argv, CWD: "/w", ExitStatus: exit, SecretFound: secret}
	if secret {
		c.Argv = ""
	}
	return AddCommandRecord(ctx, tx, c)
}

// AC #273 1 and 2: the approved items are the items of the brief whose revision there you
// approved at or after the first receipt of the last session; the failed commands are those
// whose newest record in that session failed, newest first, at most 3, in its segments of the
// workspace only.
func TestReadHandoff(t *testing.T) {
	ctx := context.Background()
	h := &handoffStore{s: openM1(t), t0: testNow}
	ka, kb := "ka", "kb"
	exit := func(n int) *int { return &n }
	var last Delivery
	write(t, h.s, func(tx *sql.Tx) error {
		for _, k := range []string{ka, kb} {
			if err := InsertWorkspace(ctx, tx, Workspace{Key: k, HerdrSession: "default", HerdrWorkspaceID: "w-" + k,
				Label: k, Status: WorkspaceLive, LastSeen: h.t0}); err != nil {
				return err
			}
		}
		// Session "a" has two receipts, as after a compact. An older session "z" came before it.
		for _, r := range []struct {
			agent string
			at    time.Time
		}{{"z", h.t0.Add(-time.Hour)}, {"a", h.t0}, {"a", h.t0.Add(20 * time.Minute)}} {
			if _, err := InsertDelivery(ctx, tx, Delivery{Profile: "claude", HerdrSession: ptr("default"), PaneID: "w1:p1",
				AgentSessionID: ptr(r.agent), WorkspaceKey: &ka, Result: ResultEmitted, At: r.at,
				TranscriptPath: ptr("/t/" + r.agent)}); err != nil {
				return err
			}
		}
		var err error
		if last, _, err = LastSession(ctx, tx, ka, "new"); err != nil {
			return err
		}
		for _, a := range []struct {
			id, rev  int64
			key      *string
			at       time.Duration // after t0
			proposal bool
		}{
			{1, 1, nil, -time.Minute, true},      // before the start
			{2, 1, nil, 10 * time.Minute, true},  // between the two receipts of "a"
			{3, 1, &ka, 30 * time.Minute, true},  // after the last receipt
			{4, 1, nil, 10 * time.Minute, true},  // approved, then undone: the brief has revision 2
			{4, 2, nil, 11 * time.Minute, false}, //
			{5, 1, &kb, 10 * time.Minute, true},  // another workspace: not in the brief
			{6, 1, nil, 0, true},                 // at the start
			{7, 1, nil, -time.Minute, true},      // approved before, pinned after
			{7, 2, nil, 10 * time.Minute, false}, //
			{8, 1, nil, 10 * time.Minute, true},  // a newer revision than the brief holds
			{8, 2, nil, -2 * time.Minute, true},  //
		} {
			if err := h.approve(ctx, tx, a.id, a.rev, a.key, h.t0.Add(a.at), a.proposal); err != nil {
				return err
			}
		}
		// The capture of "a": a segment in ka, then one in kb after a pane move. Session "b" is
		// another agent in the same pane.
		key := SessionKey{HerdrSession: "default", Harness: "claude", Kind: KeyAgent, ID: "a"}
		sa, err := OpenSession(ctx, tx, key, "w1:p1", ka, h.t0)
		if err != nil {
			return err
		}
		segA, err := StartSegment(ctx, tx, sa, "w1:p1", ka, "visible", h.t0)
		if err != nil {
			return err
		}
		for _, c := range []struct {
			argv          string
			exit          *int
			brain, secret bool
		}{
			{"go test ./x", exit(1), false, false},
			{"go test ./x", nil, false, false}, // a Claude PostToolUse: it passed
			{"make lint", exit(2), false, false},
			{"npm test", exit(1), false, false},
			{"npm test", exit(1), false, false},
			{"", exit(1), false, true},               // a secret: no argv
			{"brain recall x", exit(1), true, false}, // Brain text
			{"cargo build", exit(101), false, false},
			{"git push", exit(128), false, false},
		} {
			if err := h.command(ctx, tx, segA, c.argv, c.exit, c.brain, c.secret); err != nil {
				return err
			}
		}
		segB, err := StartSegment(ctx, tx, sa, "w2:p1", kb, "visible", h.t0.Add(time.Hour))
		if err != nil {
			return err
		}
		if err := h.command(ctx, tx, segB, "rm -rf build", exit(1), false, false); err != nil {
			return err
		}
		sb, err := OpenSession(ctx, tx, SessionKey{HerdrSession: "default", Harness: "claude", Kind: KeyAgent, ID: "b"},
			"w1:p1", ka, h.t0)
		if err != nil {
			return err
		}
		segOther, err := StartSegment(ctx, tx, sb, "w1:p1", ka, "visible", h.t0)
		if err != nil {
			return err
		}
		return h.command(ctx, tx, segOther, "ls", exit(2), false, false)
	})
	brief := []RevisionRef{{1, 1}, {2, 1}, {3, 1}, {4, 2}, {6, 1}, {7, 2}, {8, 1}}
	want := Handoff{Approved: []int64{2, 3, 6, 8},
		Failed: []FailedCommand{{"git push", 128}, {"cargo build", 101}, {"npm test", 1}}}
	noBinding := last
	noBinding.HerdrSession = nil
	for _, tc := range []struct {
		name string
		last Delivery
		want Handoff
	}{
		{"last session", last, want},
		// The receipt of an older store has no binding, so no capture session matches.
		{"receipt with no binding", noBinding, Handoff{Approved: want.Approved}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			write(t, h.s, func(tx *sql.Tx) error {
				got, err := ReadHandoff(ctx, tx, ka, tc.last, brief)
				if err != nil || !reflect.DeepEqual(got, tc.want) {
					t.Errorf("ReadHandoff() = %+v, %v, want %+v", got, err, tc.want)
				}
				return nil
			})
		})
	}
}

// AC #273 1 (Sol review of PR #307): the start of a session is its oldest receipt in the
// workspace, also when that receipt has no transcript path; a receipt of the same agent session
// in another workspace, or of another session, does not count.
func TestSessionStart(t *testing.T) {
	ctx := context.Background()
	s := openM1(t)
	at := func(h, m int) time.Time { return time.Date(2026, 10, 8, h, m, 0, 0, time.UTC) }
	ids := map[string]int64{}
	write(t, s, func(tx *sql.Tx) error {
		for _, k := range []string{"ka", "kb"} {
			if err := InsertWorkspace(ctx, tx, Workspace{Key: k, HerdrSession: "default", HerdrWorkspaceID: "w-" + k,
				Label: k, Status: WorkspaceLive, LastSeen: at(7, 0)}); err != nil {
				return err
			}
		}
		for _, r := range []struct {
			name, agent, key string
			at               time.Time
			path             *string
		}{
			{"b", "b", "ka", at(7, 0), ptr("/t/b")},
			{"a in kb", "a", "kb", at(8, 0), ptr("/t/a")},
			{"a first", "a", "ka", at(9, 0), nil},
			{"a", "a", "ka", at(9, 30), ptr("/t/a")},
			{"a last", "a", "ka", at(10, 0), ptr("/t/a")},
			{"c", "c", "ka", at(11, 0), ptr("/t/c")},
		} {
			id, err := InsertDelivery(ctx, tx, Delivery{Profile: "claude", HerdrSession: ptr("default"), PaneID: "w1:p1",
				AgentSessionID: ptr(r.agent), WorkspaceKey: ptr(r.key), Result: ResultEmitted, At: r.at, TranscriptPath: r.path})
			if err != nil {
				return err
			}
			ids[r.name] = id
		}
		return nil
	})
	for _, tc := range []struct {
		name, last, agent string
		want              time.Time
	}{
		{"first receipt with no transcript path", "a last", "a", at(9, 0)},
		{"the only receipt of the session", "c", "c", at(11, 0)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			write(t, s, func(tx *sql.Tx) error {
				got, err := sessionStart(ctx, tx, "ka", Delivery{ID: ids[tc.last], AgentSessionID: ptr(tc.agent)})
				if err != nil || !got.Equal(tc.want) {
					t.Errorf("sessionStart(%s) = %v, %v, want %v", tc.last, got, err, tc.want)
				}
				return nil
			})
		})
	}
}
