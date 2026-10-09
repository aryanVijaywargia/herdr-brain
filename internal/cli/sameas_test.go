package cli

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/aryanVijaywargia/herdr-brain/internal/extract"
	"github.com/aryanVijaywargia/herdr-brain/internal/signal"
	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

// sandboxRule is the rule of a correction in other words that holds a permission word (#358).
const sandboxRule = "never run npm install outside the sandbox"

// sameAsLink stores a same_as link that waits for review in ws_a14 (#358): the claim
// "installing packages: use pnpm" with one correction, and a correction in other words with a
// permission word, its draft and its link to the first claim, with one correction. It returns
// the candidate of the link and the candidate of the first claim.
func (f *fixture) sameAsLink(t *testing.T) (link, other store.Candidate) {
	t.Helper()
	first := f.claim(t)
	other = f.correct(t, first, "ws_a14", "b1")
	ctx := context.Background()
	s := openStore(t, f.store)
	var claim string
	write(t, s, func(tx *sql.Tx) (err error) {
		if claim, err = store.ClaimID(ctx, tx, store.ClaimKey{Situation: "installing packages", Rule: sandboxRule}, testNow); err != nil {
			return err
		}
		draft, err := json.Marshal(extract.Claim{Kind: "correction", Situation: "installing packages", Rule: sandboxRule,
			Scope: store.ScopeWorkspace, TouchesPermission: true, ExpectedBenefit: "no npm lockfile"})
		if err != nil {
			return err
		}
		if err := store.SetDraft(ctx, tx, claim, string(draft), testNow); err != nil {
			return err
		}
		_, err = store.SameAs(ctx, tx, claim, first, store.Match{Permissions: true})
		return err
	})
	closeStore(t, s)
	return f.correct(t, claim, "ws_a14", "a1"), other
}

// AC #358: brain review lists the link that waits, with the text of both claims, the reason and
// the two actions; brain status counts it. Another workspace does not see it. A merge counts the
// evidence together: the candidate reaches its gate, and its proposal waits for your review,
// with the permission warning: approve needs --confirm. A merge approves nothing.
func TestReviewSameAsLink(t *testing.T) {
	f := newStatusFixture(t)
	link, other := f.sameAsLink(t)
	out := f.mustReview(t)
	for _, want := range []string{
		fmt.Sprintf("link %d  workspace w14 (shop)  a permission or safety rule: Brain merges it only with you\n", link.ID),
		fmt.Sprintf(": When installing packages: %s (candidate %d, 1 occurrences)\n", sandboxRule, link.ID),
		fmt.Sprintf(": When installing packages: use pnpm (candidate %d, 1 occurrences)\n", other.ID),
		fmt.Sprintf("  merge: brain review --merge %d | keep apart: brain review --apart %d\n", link.ID, link.ID),
	} {
		if !strings.Contains(out, want) {
			t.Errorf("brain review:\n%s\nwant %q", out, want)
		}
	}
	if status := f.mustStatus(t); !strings.Contains(status, "\nlinks to review: 1\n") {
		t.Errorf("brain status:\n%s\nwant 1 link to review", status)
	}
	if out, err := f.reviewBlog(t); err != nil || strings.Contains(out, "link ") {
		t.Errorf("blog list = %q, %v; want no link of shop", out, err)
	}
	if _, err := f.reviewBlog(t, "--merge", fmt.Sprint(link.ID)); !errors.Is(err, errNotFound) {
		t.Errorf("--merge from blog = %v, want not found", err)
	}

	out = f.mustReview(t, "--merge", fmt.Sprint(link.ID))
	r := f.rows(t)
	if len(r.proposals) != 1 {
		t.Fatalf("proposals = %+v, want the one of the merged candidate", r.proposals)
	}
	p := r.proposals[0]
	want := fmt.Sprintf("link of candidate %d merged: the two claims mean the same, and their evidence counts together\n"+
		"candidate %d: 2 occurrences, gate reached; proposal %d waits for your review (brain review %d)\n",
		link.ID, other.ID, p.ID, p.ID)
	if out != want {
		t.Errorf("--merge printed %q, want %q", out, want)
	}
	if p.Status != store.ProposalPending || !p.PermissionWarning || p.Text != "When installing packages: "+sandboxRule {
		t.Errorf("proposal = %+v, want the pending text of the newest telling with the permission warning", p)
	}
	if status := f.mustStatus(t); !strings.Contains(status, "\nlinks to review: 0\n") {
		t.Errorf("brain status:\n%s\nwant no link to review", status)
	}
	if _, err := f.review(t, fmt.Sprint(p.ID), "--approve"); !errors.Is(err, errConfirm) {
		t.Errorf("--approve = %v, want the confirm of a permission rule", err)
	}
	if _, err := f.review(t, "--merge", fmt.Sprint(link.ID)); err == nil {
		t.Errorf("a second --merge: no error")
	}
}

// AC #358: keep apart ends the link; the list and status show it no more, and both claims keep
// their evidence.
func TestReviewKeepApart(t *testing.T) {
	f := newStatusFixture(t)
	link, other := f.sameAsLink(t)
	out := f.mustReview(t, "--apart", fmt.Sprint(link.ID))
	if !strings.HasPrefix(out, fmt.Sprintf("link of candidate %d kept apart: it does not come back for review with claim cl_", link.ID)) {
		t.Errorf("--apart printed %q", out)
	}
	if out := f.mustReview(t); strings.Contains(out, "link ") {
		t.Errorf("brain review:\n%s\nwant no link", out)
	}
	if status := f.mustStatus(t); !strings.Contains(status, "\nlinks to review: 0\n") {
		t.Errorf("brain status:\n%s\nwant no link to review", status)
	}
	r := f.rows(t)
	if len(r.proposals) != 0 {
		t.Errorf("proposals = %+v, want none", r.proposals)
	}
	out = f.mustReview(t, "--all")
	for _, c := range []store.Candidate{link, other} {
		if !strings.Contains(out, fmt.Sprintf("candidate %d  workspace w14 (shop)  correction  1 occurrences", c.ID)) {
			t.Errorf("brain review --all:\n%s\nwant candidate %d with its evidence", out, c.ID)
		}
	}
}

// The link actions take one candidate and no proposal id.
func TestReviewLinkFlags(t *testing.T) {
	f := newFixture(t)
	for _, args := range [][]string{{"--merge", "3", "--apart", "4"}, {"5", "--merge", "3"}, {"5", "--apart", "3"}} {
		if _, err := f.review(t, args...); err == nil {
			t.Errorf("brain review %q: no error", args)
		}
	}
}

// AC #358: the review pane says how many links wait; m lists them, m and y merge the link at
// the cursor, and q goes back to the proposal, which stays pending.
func TestPaneLinks(t *testing.T) {
	f := newFixture(t)
	link, _ := f.sameAsLink(t)
	id := f.remember(t, "use yarn")
	out, err := f.pane(t, id, typed("mmyqq"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"\nlinks to review: 1 (m shows them)\n",
		"same_as links that wait for review (global and this workspace)\n",
		fmt.Sprintf("> link %d  workspace w14 (shop)  a permission or safety rule", link.ID),
		fmt.Sprintf("merge link %d (the two claims mean the same)? press y to confirm", link.ID),
		fmt.Sprintf("link of candidate %d merged", link.ID),
		linksKeys,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("pane:\n%s\nwant %q", out, want)
		}
	}
	screens := strings.Split(out, clearScreen)
	if last := screens[len(screens)-1]; strings.Contains(last, "links to review") || !strings.Contains(last, "use yarn") {
		t.Errorf("last screen:\n%s\nwant the proposal with no link to review", last)
	}
	r := f.rows(t)
	for _, p := range r.proposals {
		if p.ID == id && p.Status != store.ProposalPending {
			t.Errorf("proposal %d = %s, want pending", id, p.Status)
		}
	}
}

// changeAsked makes a pending proposal of two corrections in ws_a14, asks a change of it as
// brain review --change does, and sets the request as the extractor left it: status after
// calls model calls, with reason. With no try left, the extractor gives the proposal back to
// review (store.ReturnChanged). It returns the proposal and the request.
func (f *fixture) changeAsked(t *testing.T, status store.ChangeStatus, calls int) (int64, int64) {
	t.Helper()
	claim := f.claim(t)
	f.correct(t, claim, "ws_a14", "a1")
	pid, err := f.propose(t, f.correct(t, claim, "ws_a14", "a2"), "workspace w14 (shop)")
	if err != nil {
		t.Fatal(err)
	}
	f.mustReview(t, fmt.Sprint(pid), "--change", "split it")
	ctx := context.Background()
	s := openStore(t, f.store)
	defer closeStore(t, s)
	var cr store.ChangeRequest
	write(t, s, func(tx *sql.Tx) (err error) {
		if _, err := tx.ExecContext(ctx, `UPDATE change_requests SET status = ?, model_calls = ?, reason = ?`,
			status, calls, "2 claims for one change request: invalid extractor response"); err != nil {
			return err
		}
		if cr, _, err = store.ProposalChangeOf(ctx, tx, pid); err != nil || !extract.ChangeSpent(cr) {
			return err
		}
		return store.ReturnChanged(ctx, tx, pid)
	})
	return pid, cr.ID
}

// AC #359: a proposal whose change request has no try left is back in brain review: its view
// says so, and you decide it, or ask for a change again with new tries. brain status lists the
// request with its next step.
func TestReviewSpentChangeRequest(t *testing.T) {
	f := newStatusFixture(t)
	pid, cr := f.changeAsked(t, store.ChangeInvalid, 3)
	if out := f.mustReview(t); !strings.Contains(out, fmt.Sprintf("%d  workspace w14 (shop)  use pnpm", pid)) {
		t.Errorf("brain review:\n%s\nwant proposal %d back in the list", out, pid)
	}
	want := fmt.Sprintf("change:   split it\n          request %d invalid with no try left after 3 calls: 2 claims for one change "+
		"request: invalid extractor response; decide the proposal, or ask for a change again\n", cr)
	if out := f.mustReview(t, fmt.Sprint(pid)); !strings.Contains(out, want) {
		t.Errorf("brain review %d:\n%s\nwant %q", pid, out, want)
	}
	want = fmt.Sprintf("  failed change requests: 1\n    request %d of proposal %d, workspace ws_a14: invalid, 2 claims for one change "+
		"request: invalid extractor response, 3h ago; no try left: proposal %d is back in brain review\n", cr, pid, pid)
	if status := f.mustStatus(t); !strings.Contains(status, want) {
		t.Errorf("brain status:\n%s\nwant %q", status, want)
	}
	f.mustReview(t, fmt.Sprint(pid), "--change", "split it in two")
	r := f.rows(t)
	if p := r.proposals[0]; p.Status != store.ProposalChanged || *p.ChangeRequest != "split it in two" {
		t.Errorf("proposal = %+v, want changed with the new request", p)
	}
	if status := f.mustStatus(t); strings.Contains(status, "failed change requests") {
		t.Errorf("brain status:\n%s\nwant no failed change request: the new request starts with new tries", status)
	}
}

// AC #359: brain status lists a failed change request that waits for its retry too; the
// proposal stays changed.
func TestStatusChangeRequestRetry(t *testing.T) {
	f := newStatusFixture(t)
	pid, _ := f.changeAsked(t, store.ChangeFailed, 3)
	if status := f.mustStatus(t); !strings.Contains(status, fmt.Sprintf("    request 1 of proposal %d, workspace ws_a14: failed, 2 claims for "+
		"one change request: invalid extractor response, 3h ago; it waits for a retry\n", pid)) {
		t.Errorf("brain status:\n%s\nwant the request that waits for its retry", status)
	}
	if out := f.mustReview(t); strings.Contains(out, fmt.Sprintf("%d  workspace", pid)) {
		t.Errorf("brain review:\n%s\nwant no changed proposal in the list", out)
	}
}

// AC #359: the next step of each failed change request in brain status.
func TestChangeNext(t *testing.T) {
	draft := int64(9)
	for _, tc := range []struct {
		r    store.ChangeRequest
		want string
	}{
		{store.ChangeRequest{Source: store.ChangeProposal, SourceID: 4, Status: store.ChangeFailed, ModelCalls: 3},
			"it waits for a retry"},
		{store.ChangeRequest{Source: store.ChangeProposal, SourceID: 4, Status: store.ChangeFailed, ModelCalls: 4},
			"no try left: proposal 4 is back in brain review"},
		{store.ChangeRequest{Source: store.ChangeItem, SourceID: 2, Revision: 3, Status: store.ChangeInvalid, ModelCalls: 3,
			NewProposalID: &draft}, "no try left: proposal 9 is back in brain review"},
		{store.ChangeRequest{Source: store.ChangeItem, SourceID: 2, Revision: 3, Status: store.ChangeInvalid, ModelCalls: 3},
			`no try left: edit the item with brain review --item 2 --edit "<text>"`},
	} {
		if got := changeNext(tc.r); got != tc.want {
			t.Errorf("changeNext(%+v) = %q, want %q", tc.r, got, tc.want)
		}
	}
}

// Sol review of #360 (go-quality rule 14): a link that names another claim after the pane
// showed it, as after a new telling, is not merged at the y: the pane says so, and nothing
// changes.
func TestPaneLinkChanged(t *testing.T) {
	f := newFixture(t)
	link, _ := f.sameAsLink(t)
	id := f.remember(t, "use yarn")
	k := typed("mmyqq")
	k.before = map[int]func(){2: func() {
		s := openStore(t, f.store)
		defer closeStore(t, s)
		write(t, s, func(tx *sql.Tx) error {
			ctx := context.Background()
			other, err := store.ClaimID(ctx, tx, store.ClaimKey{Situation: "installing packages", Rule: "use bun"}, testNow)
			if err != nil {
				return err
			}
			_, err = store.SameAs(ctx, tx, link.ClaimID, other, store.Match{})
			return err
		})
	}}
	out, err := f.pane(t, id, k)
	if err != nil {
		t.Fatal(err)
	}
	if want := fmt.Sprintf("the link of candidate %d changed while you looked at it: look again", link.ID); !strings.Contains(out, want) {
		t.Errorf("pane:\n%s\nwant %q", out, want)
	}
	if r := f.rows(t); len(r.proposals) != 1 {
		t.Errorf("proposals = %+v, want only the remember draft: no merge", r.proposals)
	}
}

// Sol review of #360 (AC #358 and 2): with no proposal, as in the run 4 shape, the popup of the
// action brain.links lists the link of the focused pane's workspace; m and y merge it, and q
// closes the popup.
func TestLinksPopup(t *testing.T) {
	f := newFixture(t)
	link, other := f.sameAsLink(t)
	var out bytes.Buffer
	e := f.env(f.sock, "", "", &out)
	e.Environ = f.popupEnviron("w14:p1", "w14")
	popup := popupOf([]string{"links"})
	if popup == nil || !strings.Contains(paneUsage, "brain pane links") {
		t.Fatalf("popupOf(links) = nil or usage %q, want the links popup", paneUsage)
	}
	if err := popup(context.Background(), e, bufio.NewReader(strings.NewReader("myq")), func() (int, int) { return 0, 0 }); err != nil {
		t.Fatalf("pane links = %v\n%s", err, out.String())
	}
	for _, want := range []string{
		fmt.Sprintf("> link %d  workspace w14 (shop)", link.ID),
		fmt.Sprintf("candidate %d: 2 occurrences, gate reached; proposal", other.ID),
		"\n" + linksPopupKeys + "\n",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("popup:\n%s\nwant %q", out.String(), want)
		}
	}
	if r := f.rows(t); len(r.proposals) != 1 || r.proposals[0].Status != store.ProposalPending {
		t.Errorf("proposals = %+v, want the pending proposal of the merge", r.proposals)
	}
}

// stuckLink stores a same_as link that waits for review in ws_a14 and that no merge can move
// (#361): a claim of the agent's own command (one tool record, a command chain candidate) linked
// to the claim "installing packages: use pnpm" with one correction. It returns the candidate of
// the link.
func (f *fixture) stuckLink(t *testing.T) int64 {
	t.Helper()
	first := f.claim(t)
	f.correct(t, first, "ws_a14", "b1")
	ctx := context.Background()
	s := openStore(t, f.store)
	var link int64
	write(t, s, func(tx *sql.Tx) error {
		claim, err := store.ClaimID(ctx, tx, store.ClaimKey{Situation: "installing packages", Rule: "run pnpm install"}, testNow)
		if err != nil {
			return err
		}
		key := store.SessionKey{HerdrSession: "default", Harness: "claude", Kind: store.KeyCapture, ID: "c1"}
		id, err := store.OpenSession(ctx, tx, key, "w14:p1", "ws_a14", testNow)
		if err != nil {
			return err
		}
		seg, err := store.StartSegment(ctx, tx, id, "w14:p1", "ws_a14", "visible", testNow)
		if err != nil {
			return err
		}
		o, err := store.AddObservation(ctx, tx, store.Observation{CaptureIdentity: "c1", Source: store.SourceHook,
			SegmentID: seg, Event: "tool", Excerpt: "pnpm install", Status: store.ObservationComplete,
			Sender: store.SenderTool, Counts: true, At: testNow})
		if err != nil {
			return err
		}
		if link, err = store.Attach(ctx, tx, store.Candidate{ClaimID: claim, Scope: store.ScopeWorkspace, WorkspaceKey: "ws_a14",
			Kind: store.CandidateCommandChain}, o, store.DefaultGates(), signal.Supports); err != nil {
			return err
		}
		_, err = store.SameAs(ctx, tx, claim, first, store.Match{})
		return err
	})
	closeStore(t, s)
	return link
}

// AC #361: a link of a claim of the agent's own commands to a claim of your text cannot move
// evidence. brain review shows why and offers keep apart only, and --merge refuses it and
// writes nothing: the review never says that evidence counts together when none moved.
func TestReviewStuckLink(t *testing.T) {
	f := newStatusFixture(t)
	link := f.stuckLink(t)
	out := f.mustReview(t)
	for _, want := range []string{
		fmt.Sprintf("  no merge: the evidence of candidate %d is the agent's own commands and tool output, which cannot count for a correction\n", link),
		fmt.Sprintf("  keep apart: brain review --apart %d\n", link),
	} {
		if !strings.Contains(out, want) {
			t.Errorf("brain review:\n%s\nwant %q", out, want)
		}
	}
	if strings.Contains(out, fmt.Sprintf("--merge %d", link)) {
		t.Errorf("brain review:\n%s\nwant no merge action for link %d", out, link)
	}
	if _, err := f.review(t, "--merge", fmt.Sprint(link)); err == nil || !strings.Contains(err.Error(), "a merge moves no evidence") ||
		!strings.Contains(err.Error(), "nothing was written") {
		t.Errorf("--merge = %v, want it refused with the reason", err)
	}
	if status := f.mustStatus(t); !strings.Contains(status, "\nlinks to review: 1\n") {
		t.Errorf("brain status:\n%s\nwant the link still to review", status)
	}
}

// Sol review of #370 (AC #361): the links popup and the review pane offer no merge for a link
// that no merge can move: the keys line has no m, and m says why with no confirm.
func TestLinksPopupStuck(t *testing.T) {
	f := newFixture(t)
	link := f.stuckLink(t)
	var out bytes.Buffer
	e := f.env(f.sock, "", "", &out)
	e.Environ = f.popupEnviron("w14:p1", "w14")
	if err := popupOf([]string{"links"})(context.Background(), e, bufio.NewReader(strings.NewReader("mq")),
		func() (int, int) { return 0, 0 }); err != nil {
		t.Fatalf("pane links = %v\n%s", err, out.String())
	}
	for _, want := range []string{
		fmt.Sprintf("> link %d  workspace w14 (shop)", link),
		"\nj down  k up  n keep apart  q close\n",
		fmt.Sprintf("no merge: the evidence of candidate %d is the agent's own commands and tool output", link),
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("popup:\n%s\nwant %q", out.String(), want)
		}
	}
	for _, not := range []string{linksPopupKeys, "press y to confirm"} {
		if strings.Contains(out.String(), not) {
			t.Errorf("popup:\n%s\nwant no %q", out.String(), not)
		}
	}
}
