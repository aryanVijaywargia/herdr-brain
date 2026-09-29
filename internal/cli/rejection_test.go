package cli

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/aryanVijaywargia/herdr-brain/internal/herdr"
	"github.com/aryanVijaywargia/herdr-brain/internal/signal"
	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

// reviewBlog runs `brain review args` from pane w15:p1 of workspace w15 ("blog").
func (f *fixture) reviewBlog(t *testing.T, args ...string) (string, error) {
	t.Helper()
	f.panes["w15:p1"] = herdr.Pane{ID: "w15:p1", WorkspaceID: "w15"}
	var out bytes.Buffer
	err := Review(context.Background(), f.env(f.sock, "w15:p1", "w15", &out), args)
	return out.String(), err
}

// claim returns the claim id of the key "installing packages: use pnpm".
func (f *fixture) claim(t *testing.T) string {
	t.Helper()
	s := openStore(t, f.store)
	defer closeStore(t, s)
	var id string
	write(t, s, func(tx *sql.Tx) (err error) {
		id, err = store.ClaimID(context.Background(), tx, store.ClaimKey{Situation: "installing packages", Rule: "use pnpm"}, testNow)
		return err
	})
	return id
}

// correct adds one correction of claim in workspace key ws, observed at testNow in the new
// capture session name, and returns its candidate.
func (f *fixture) correct(t *testing.T, claim, ws, name string) store.Candidate {
	t.Helper()
	return f.correctAt(t, claim, ws, name, testNow)
}

// correctAt is correct with the observation at at.
func (f *fixture) correctAt(t *testing.T, claim, ws, name string, at time.Time) store.Candidate {
	t.Helper()
	ctx := context.Background()
	s := openStore(t, f.store)
	defer closeStore(t, s)
	var c store.Candidate
	write(t, s, func(tx *sql.Tx) error {
		key := store.SessionKey{HerdrSession: "default", Harness: "claude", Kind: store.KeyCapture, ID: name}
		id, err := store.OpenSession(ctx, tx, key, "w14:p1", ws, testNow)
		if err != nil {
			return err
		}
		seg, err := store.StartSegment(ctx, tx, id, "w14:p1", ws, "visible", testNow)
		if err != nil {
			return err
		}
		o, err := store.AddObservation(ctx, tx, store.Observation{CaptureIdentity: name, Source: store.SourceHerdr,
			SegmentID: seg, Event: "idle", Excerpt: "text", Status: store.ObservationComplete,
			Sender: store.SenderUnconfirmed, Counts: true, At: at})
		if err != nil {
			return err
		}
		cid, err := store.Attach(ctx, tx, store.Candidate{ClaimID: claim, Scope: store.ScopeWorkspace, WorkspaceKey: ws,
			Kind: store.CandidateCorrection}, o, store.DefaultGates(), signal.Supports)
		if err != nil {
			return err
		}
		c, err = store.CandidateByID(ctx, tx, cid)
		return err
	})
	return c
}

// propose drafts an item of the claim of candidate c, and a proposal of c to approve it, as E13
// does. target is the target of the proposal. It returns the proposal id, or the error of
// store.ProposeCandidate.
func (f *fixture) propose(t *testing.T, c store.Candidate, target string) (int64, error) {
	t.Helper()
	ctx := context.Background()
	s := openStore(t, f.store)
	defer closeStore(t, s)
	var pid int64
	err := s.Write(ctx, func(tx *sql.Tx) error {
		key := c.WorkspaceKey
		item, err := store.InsertItem(ctx, tx, store.Item{ClaimID: c.ClaimID, CurrentRevision: 1, Kind: store.KindPreference,
			Scope: c.Scope, WorkspaceKey: &key, State: store.StateDraft, Created: testNow})
		if err != nil {
			return err
		}
		base := int64(1)
		pid, err = store.ProposeCandidate(ctx, tx, c.ID, store.Proposal{ItemID: &item, BaseRevision: &base,
			Text: "use pnpm", Scope: c.Scope, Target: target, Diff: "+ use pnpm", Status: store.ProposalPending, Created: testNow})
		if err != nil {
			return err
		}
		return store.InsertItemRevision(ctx, tx, store.ItemRevision{ItemID: item, Revision: 1, Text: "use pnpm",
			Scope: c.Scope, WorkspaceKey: &key, State: store.StateDraft, Author: store.AuthorBrain,
			Origin: store.OriginRemember, ProposalID: &pid, Created: testNow})
	})
	return pid, err
}

// AC: reject in "shop" does not hide the claim in "blog" (§7.2). New evidence after the
// rejection shows in --rejected and creates no proposal.
func TestReviewRejectClaim(t *testing.T) {
	f := newFixture(t)
	claim := f.claim(t)
	f.correct(t, claim, "ws_a14", "a1")
	shop := f.correct(t, claim, "ws_a14", "a2")
	f.correct(t, claim, "ws_a15", "b1")
	blog := f.correct(t, claim, "ws_a15", "b2")
	pShop, err := f.propose(t, shop, "workspace w14 (shop)")
	if err != nil {
		t.Fatal(err)
	}
	pBlog, err := f.propose(t, blog, "workspace w15 (blog)")
	if err != nil {
		t.Fatal(err)
	}

	out := f.mustReview(t, fmt.Sprint(pShop), "--reject", "--reason", "we use npm in shop")
	if want := fmt.Sprintf("proposal %d rejected: claim %s stays rejected in workspace w14 (shop) (rejection 1, brain review --rejected)\n",
		pShop, claim); out != want {
		t.Errorf("--reject printed %q, want %q", out, want)
	}
	if out, err := f.reviewBlog(t); err != nil || !strings.Contains(out, fmt.Sprintf("%d  workspace w15 (blog)  use pnpm", pBlog)) {
		t.Errorf("blog list = %q, %v; want its proposal %d", out, err, pBlog)
	}
	if out, err := f.reviewBlog(t, "--rejected"); err != nil || out != "no rejections\n" {
		t.Errorf("blog --rejected = %q, %v; want none", out, err)
	}
	if _, err := f.propose(t, blog, "workspace w15 (blog)"); err != nil {
		t.Errorf("a new proposal in blog: %v, want one", err)
	}

	// New evidence in shop attaches, but gives no proposal.
	f.correctAt(t, claim, "ws_a14", "a3", testNow.Add(time.Hour))
	before := len(f.rows(t).proposals)
	if _, err := f.propose(t, shop, "workspace w14 (shop)"); !errors.Is(err, store.ErrRejected) {
		t.Errorf("a new proposal in shop: %v, want %v", err, store.ErrRejected)
	}
	if after := len(f.rows(t).proposals); after != before {
		t.Errorf("proposals %d, want %d: the new evidence created a proposal", after, before)
	}
	out = f.mustReview(t, "--rejected")
	want := "rejection 1  workspace w14 (shop)  rejected " + testNow.Format("2006-01-02 15:04") +
		"  1 new evidence since (3 now, 2 then)\n" +
		"  claim " + claim + ": installing packages: use pnpm\n" +
		"  reason: we use npm in shop\n" +
		"\nreopen one: brain review --reopen <rejection>; count its new evidence from now: brain review --reset <rejection>\n"
	if out != want {
		t.Errorf("--rejected printed\n%s\nwant\n%s", out, want)
	}
}

// Reset counts the new evidence from now, and reopen makes the claim eligible again. Only a
// caller who sees the scope of a rejection ends it.
func TestReviewReopenRejection(t *testing.T) {
	f := newFixture(t)
	claim := f.claim(t)
	f.correct(t, claim, "ws_a14", "a1")
	c := f.correct(t, claim, "ws_a14", "a2")
	pid, err := f.propose(t, c, "workspace w14 (shop)")
	if err != nil {
		t.Fatal(err)
	}
	f.mustReview(t, fmt.Sprint(pid), "--reject")
	f.correctAt(t, claim, "ws_a14", "a3", testNow.Add(time.Hour))

	for _, args := range [][]string{{"--reopen", "1"}, {"--reset", "1"}, {"--rejected"}} {
		out, err := f.reviewBlog(t, args...)
		if args[0] == "--rejected" {
			if err != nil || out != "no rejections\n" {
				t.Errorf("blog %q = %q, %v; want no rejections", args, out, err)
			}
			continue
		}
		if !errors.Is(err, errNotFound) || out != "" {
			t.Errorf("blog %q = %q, %v; want not found", args, out, err)
		}
	}
	if _, err := f.review(t, "--reopen", "9"); !errors.Is(err, errNotFound) {
		t.Errorf("--reopen 9 = %v, want not found", err)
	}

	var buf bytes.Buffer
	e := f.env(f.sock, "w14:p1", "w14", &buf)
	e.Now = func() time.Time { return testNow.Add(2 * time.Hour) }
	if err := Review(context.Background(), e, []string{"--reset", "1"}); err != nil || !strings.Contains(buf.String(), "rejection 1 reset") {
		t.Errorf("--reset = %q, %v", buf.String(), err)
	}
	if out := f.mustReview(t, "--rejected"); !strings.Contains(out, "0 new evidence since (3 now, 3 then)") ||
		!strings.Contains(out, "reason: (none)") {
		t.Errorf("--rejected after reset = %q, want 0 new and no reason", out)
	}
	if _, err := f.propose(t, c, "workspace w14 (shop)"); !errors.Is(err, store.ErrRejected) {
		t.Errorf("a proposal after reset: %v, want %v", err, store.ErrRejected)
	}

	out := f.mustReview(t, "--reopen", "1")
	if want := fmt.Sprintf("rejection 1 reopened: claim %s can get proposals in workspace w14 (shop) again\n", claim); out != want {
		t.Errorf("--reopen printed %q, want %q", out, want)
	}
	if out := f.mustReview(t, "--rejected"); out != "no rejections\n" {
		t.Errorf("--rejected after reopen = %q", out)
	}
	if _, err := f.propose(t, c, "workspace w14 (shop)"); err != nil {
		t.Errorf("a proposal after reopen: %v, want one", err)
	}
	for _, args := range [][]string{{"--reopen", "1"}, {"--reset", "1"}} {
		if _, err := f.review(t, args...); !errors.Is(err, errDecided) {
			t.Errorf("%q of a reopened rejection = %v, want a conflict", args, err)
		}
	}
}

// A proposal of a candidate below its gate waits in --all only. You approve it from there with
// the E4 approve (§7.2).
func TestReviewApproveBelowGate(t *testing.T) {
	f := newFixture(t)
	c := f.correct(t, f.claim(t), "ws_a14", "a1")
	pid, err := f.propose(t, c, "workspace w14 (shop)")
	if err != nil {
		t.Fatal(err)
	}
	if out := f.mustReview(t); strings.Contains(out, "use pnpm") {
		t.Errorf("list = %q, want no proposal below the gate", out)
	}
	line := fmt.Sprintf("  proposal %d: use pnpm (brain review %d --approve)\n", pid, pid)
	if out := f.mustReview(t, "--all"); !strings.Contains(out, "below gate (1 of 2 occurrences)") || !strings.Contains(out, line) {
		t.Errorf("--all = %q, want the candidate below the gate and %q", out, line)
	}
	if out, err := f.reviewBlog(t, "--all"); err != nil || strings.Contains(out, "use pnpm") {
		t.Errorf("blog --all = %q, %v; want no text of shop", out, err)
	}
	if _, err := f.reviewBlog(t, fmt.Sprint(pid), "--approve"); !errors.Is(err, errNotFound) {
		t.Errorf("approve from blog = %v, want not found", err)
	}
	if out := f.mustReview(t, fmt.Sprint(pid), "--approve"); !strings.Contains(out, "approved") {
		t.Errorf("--approve printed %q", out)
	}
	r := f.rows(t)
	p := r.proposal(t, pid)
	if it, _ := r.item(t, *p.ItemID); p.Status != store.ProposalApproved || it.State != store.StateActive {
		t.Errorf("proposal = %+v, item = %+v; want approved and active", p, it)
	}
	if b := f.brief(t); !strings.Contains(b, "use pnpm") {
		t.Errorf("brief = %q, want the approved item", b)
	}
}

// A proposal from brain remember has no candidate: its reject writes no rejection.
func TestReviewRejectNoCandidate(t *testing.T) {
	f := newFixture(t)
	pid := f.remember(t, "a")
	if out := f.mustReview(t, fmt.Sprint(pid), "--reject", "--reason", "no"); out != fmt.Sprintf("proposal %d rejected\n", pid) {
		t.Errorf("--reject printed %q", out)
	}
	if out := f.mustReview(t, "--rejected"); out != "no rejections\n" {
		t.Errorf("--rejected = %q, want none", out)
	}
}

// A reject of one proposal rejects the other pending proposals of its candidate: none of them
// can make the rejected claim active.
func TestReviewRejectOtherProposals(t *testing.T) {
	f := newFixture(t)
	c := f.correct(t, f.claim(t), "ws_a14", "a1")
	first, err := f.propose(t, c, "workspace w14 (shop)")
	if err != nil {
		t.Fatal(err)
	}
	second, err := f.propose(t, c, "workspace w14 (shop)")
	if err != nil {
		t.Fatal(err)
	}
	if out := f.mustReview(t, fmt.Sprint(first), "--reject"); !strings.Contains(out, fmt.Sprintf("proposal %d of the same claim rejected", second)) {
		t.Errorf("--reject printed %q", out)
	}
	if p := f.rows(t).proposal(t, second); p.Status != store.ProposalRejected {
		t.Errorf("the other proposal = %+v, want rejected", p)
	}
	if _, err := f.review(t, fmt.Sprint(second), "--approve"); err == nil || !strings.Contains(err.Error(), "conflict: it is rejected") {
		t.Errorf("approve of the other proposal = %v, want a conflict", err)
	}
}
