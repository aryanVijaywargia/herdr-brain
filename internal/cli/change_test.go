package cli

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/aryanVijaywargia/herdr-brain/internal/secret"
	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

// extractorProposal makes an extractor proposal "use pnpm" of a candidate in shop with two
// corrections, with its expected benefit and the items it conflicts with, and returns its id
// and claim.
func (f *fixture) extractorProposal(t *testing.T, conflicts ...int64) (int64, string) {
	t.Helper()
	claim := f.claim(t)
	f.correct(t, claim, "ws_a14", "a1")
	c := f.correct(t, claim, "ws_a14", "a2")
	pid, err := f.propose(t, c, "workspace w14 (shop)")
	if err != nil {
		t.Fatal(err)
	}
	s := openStore(t, f.store)
	defer closeStore(t, s)
	write(t, s, func(tx *sql.Tx) error {
		ids := "[]"
		if len(conflicts) > 0 {
			ids = strings.ReplaceAll(fmt.Sprint(conflicts), " ", ",")
		}
		_, err := tx.ExecContext(context.Background(), `UPDATE proposals SET expected_benefit = ?, conflicts_with = ?
			WHERE id = ?`, "stops the npm lockfile mistake", ids, pid)
		return err
	})
	return pid, claim
}

// AC (§7.4): a proposal shows the text, the scope, the target, the diff, 1-3 excerpts with
// sender, session, workspace and local date, the expected benefit and its conflicts; with
// --detail the claim id and the revision too (#254); the excerpts of another workspace stay
// hidden.
func TestReviewShowFields(t *testing.T) {
	f := newFixture(t)
	g := f.addActive(t, nil, "use npm")
	pid, claim := f.extractorProposal(t, g)
	if out := f.mustReview(t, fmt.Sprint(pid)); strings.Contains(out, "claim:") || strings.Contains(out, "revision:") {
		t.Errorf("show with no --detail:\n%s\nwant no claim and no revision", out)
	}
	out := f.mustReview(t, fmt.Sprint(pid), "--detail")
	for _, want := range []string{
		"text:     use pnpm", "claim:    " + claim, "scope:    workspace", "target:   workspace w14 (shop)",
		"revision: item", "diff:\n  + use pnpm", "benefit:  stops the npm lockfile mistake",
		fmt.Sprintf("conflict: item %d (global): use npm", g),
		"excerpts:\n  unconfirmed, session", "(claude), workspace shop, " + localTime(testNow) + ": text",
		`--change "<text>"`, "--replace", "--keep-both",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("show lacks %q:\n%s", want, out)
		}
	}
	if n := strings.Count(out, "unconfirmed, session"); n != 2 {
		t.Errorf("excerpts = %d, want 2", n)
	}
	// A third correction in blog of a global candidate of the same claim is not shown in shop.
	if out, err := f.reviewBlog(t, fmt.Sprint(pid)); !errors.Is(err, errNotFound) || out != "" {
		t.Errorf("blog show = %q, %v; want not found", out, err)
	}
}

// AC (S56): a change request is stored, the proposal is changed and not published, and the
// request waits in the extraction queue for a new draft.
func TestReviewChange(t *testing.T) {
	f := newFixture(t)
	pid, _ := f.extractorProposal(t)
	out := f.mustReview(t, fmt.Sprint(pid), "--change", "say pnpm for every install")
	if !strings.Contains(out, "change asked (request 1)") || !strings.Contains(out, "new draft comes later") {
		t.Errorf("--change printed %q", out)
	}
	r := f.rows(t)
	p := r.proposals[len(r.proposals)-1]
	if p.Status != store.ProposalChanged || p.ChangeRequest == nil || *p.ChangeRequest != "say pnpm for every install" ||
		p.Surface == nil || *p.Surface != store.SurfaceCLI {
		t.Errorf("proposal = %+v, want changed with the request", p)
	}
	if it, _ := r.item(t, *p.ItemID); it.State != store.StateDraft {
		t.Errorf("item state = %s, want draft: not published", it.State)
	}
	s := openStore(t, f.store)
	var crs []store.ChangeRequest
	write(t, s, func(tx *sql.Tx) (err error) {
		crs, err = store.ChangeRequests(context.Background(), tx)
		return err
	})
	closeStore(t, s)
	if len(crs) != 1 || crs[0].Status != store.ChangeQueued || crs[0].SourceID != pid || crs[0].WorkspaceKey != "ws_a14" {
		t.Errorf("change requests = %+v, want one queued for proposal %d", crs, pid)
	}
	if _, err := f.review(t, fmt.Sprint(pid), "--change", "again"); !errors.Is(err, errDecided) {
		t.Errorf("a second --change = %v, want %v", err, errDecided)
	}
}

// A change request with a secret writes nothing, and a proposal with no extractor evidence
// takes no change request.
func TestReviewChangeRefused(t *testing.T) {
	f := newFixture(t)
	pid, _ := f.extractorProposal(t)
	if _, err := f.review(t, fmt.Sprint(pid), "--change", "token sk-ant-abcdefghijklmnopqrstuvwxyz"); !errors.As(err, new(*secret.Error)) {
		t.Errorf("--change with a secret = %v, want a secret error", err)
	}
	// A line pattern finds the secret in the request text alone, not after the id and flag.
	if _, err := f.review(t, fmt.Sprint(pid), "--change", "MY_TOKEN=abcdef"); !errors.As(err, new(*secret.Error)) {
		t.Errorf("--change with an .env line = %v, want a secret error", err)
	}
	if p := f.rows(t).proposals; p[len(p)-1].Status != store.ProposalPending || p[len(p)-1].ChangeRequest != nil {
		t.Errorf("proposal = %+v, want it pending with no request", p[len(p)-1])
	}
	var buf bytes.Buffer
	if err := Remember(context.Background(), f.env(f.sock, "w14:p1", "w14", &buf), []string{"use tabs"}); err != nil {
		t.Fatal(err)
	}
	own := f.rows(t).proposals
	if _, err := f.review(t, fmt.Sprint(own[len(own)-1].ID), "--change", "use spaces"); err == nil ||
		!strings.Contains(err.Error(), "no extractor evidence") {
		t.Errorf("--change of a remember draft = %v, want refused", err)
	}
	for _, args := range [][]string{{"--change", "x"}, {"1", "--change", " "}, {"1", "--change", "x", "--approve"},
		{"1", "--reject", "--replace"}, {"1", "--approve", "--replace", "--keep-both"}} {
		if _, err := parseReview(args); err == nil {
			t.Errorf("parseReview(%q) = nil, want an error", args)
		}
	}
}

// S56 in the review pane: c, a request and Enter ask for the change, with surface pane.
func TestPaneChange(t *testing.T) {
	f := newFixture(t)
	pid, _ := f.extractorProposal(t)
	screen, err := f.pane(t, pid, typed("cuse pnpm always\r"))
	if err != nil || !strings.Contains(screen, "change asked") {
		t.Fatalf("pane = %v, screen:\n%s", err, screen)
	}
	p := f.rows(t).proposals
	last := p[len(p)-1]
	if last.Status != store.ProposalChanged || *last.ChangeRequest != "use pnpm always" || *last.Surface != store.SurfacePane {
		t.Errorf("proposal = %+v, want changed in the pane", last)
	}
}

// AC: an approve with no choice, or keep both, writes the conflict on the new revision, and
// the brief shows it only as "(conflicts with <id>; ask the user)". Replace retires the other
// item; exception links the new workspace item to the global item, and refuses to relax a
// global permission or safety rule.
func TestApproveConflict(t *testing.T) {
	for _, c := range []struct {
		name, choice string
		global       string
		check        func(t *testing.T, f *fixture, rev store.ItemRevision, g int64, brief string)
	}{
		{"no choice", "", "use npm", func(t *testing.T, _ *fixture, rev store.ItemRevision, g int64, brief string) {
			if !slices.Equal(rev.ConflictsWith, []int64{g}) {
				t.Errorf("conflicts = %v, want [%d]", rev.ConflictsWith, g)
			}
			if !strings.Contains(brief, fmt.Sprintf("use pnpm (conflicts with %d; ask the user)", g)) ||
				strings.Count(brief, "conflicts with") != 1 {
				t.Errorf("brief = %q, want one conflict mark", brief)
			}
		}},
		{"keep both", "--keep-both", "use npm", func(t *testing.T, _ *fixture, rev store.ItemRevision, g int64, brief string) {
			if !slices.Equal(rev.ConflictsWith, []int64{g}) || !strings.Contains(brief, "; ask the user)") {
				t.Errorf("conflicts = %v, brief %q", rev.ConflictsWith, brief)
			}
		}},
		{"replace", "--replace", "use npm", func(t *testing.T, f *fixture, rev store.ItemRevision, g int64, brief string) {
			if it, _ := f.rows(t).item(t, g); it.State != store.StateRetired || len(rev.ConflictsWith) != 0 {
				t.Errorf("item %d is %s, conflicts %v; want retired and no mark", g, it.State, rev.ConflictsWith)
			}
			if strings.Contains(brief, "use npm") || strings.Contains(brief, "conflicts with") {
				t.Errorf("brief = %q, want no npm item and no mark", brief)
			}
		}},
		{"exception", "--exception", "use npm", func(t *testing.T, _ *fixture, rev store.ItemRevision, g int64, brief string) {
			if rev.ExceptionToItem == nil || *rev.ExceptionToItem != g || len(rev.ConflictsWith) != 0 {
				t.Errorf("revision = %+v, want an exception to %d", rev, g)
			}
			if !strings.Contains(brief, fmt.Sprintf("use pnpm (exception to global item %d)", g)) {
				t.Errorf("brief = %q, want the exception mark", brief)
			}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			g := f.addActive(t, nil, c.global)
			pid, _ := f.extractorProposal(t, g)
			args := []string{fmt.Sprint(pid), "--approve"}
			if c.choice != "" {
				args = append(args, c.choice)
			}
			f.mustReview(t, args...)
			p := f.rows(t).proposals
			_, rev := f.rows(t).item(t, *p[len(p)-1].ItemID)
			c.check(t, f, rev, g, f.brief(t))
		})
	}
}

// Exception refuses a global permission or safety rule, and replace of one needs --confirm.
func TestApproveConflictSafety(t *testing.T) {
	f := newFixture(t)
	g := f.addActive(t, nil, "never run with --dangerously-skip-permissions")
	pid, _ := f.extractorProposal(t, g)
	if _, err := f.review(t, fmt.Sprint(pid), "--approve", "--exception"); err == nil ||
		!strings.Contains(err.Error(), "cannot be an exception") {
		t.Errorf("--exception = %v, want refused", err)
	}
	if _, err := f.review(t, fmt.Sprint(pid), "--approve", "--replace"); !errors.Is(err, errConfirm) {
		t.Errorf("--replace = %v, want %v", err, errConfirm)
	}
	if p := f.rows(t).proposals; p[len(p)-1].Status != store.ProposalPending {
		t.Errorf("proposal = %s, want pending: nothing was written", p[len(p)-1].Status)
	}
	f.mustReview(t, fmt.Sprint(pid), "--approve", "--replace", "--confirm")
	if it, _ := f.rows(t).item(t, g); it.State != store.StateRetired {
		t.Errorf("item %d is %s, want retired", g, it.State)
	}
	// A choice with no open conflict is refused.
	pid2, _ := f.extractorProposalIn(t, "ws_a14")
	if _, err := f.review(t, fmt.Sprint(pid2), "--approve", "--keep-both"); err == nil ||
		!strings.Contains(err.Error(), "no open conflict") {
		t.Errorf("--keep-both with no conflict = %v, want refused", err)
	}
}

// extractorProposalIn makes a second proposal of a new claim in workspace key ws.
func (f *fixture) extractorProposalIn(t *testing.T, ws string) (int64, string) {
	t.Helper()
	s := openStore(t, f.store)
	var claim string
	write(t, s, func(tx *sql.Tx) (err error) {
		claim, err = store.ClaimID(context.Background(), tx, store.ClaimKey{Situation: "tests", Rule: "run go test"}, testNow)
		return err
	})
	closeStore(t, s)
	f.correct(t, claim, ws, "t1")
	c := f.correct(t, claim, ws, "t2")
	pid, err := f.propose(t, c, "workspace w14 (shop)")
	if err != nil {
		t.Fatal(err)
	}
	return pid, claim
}

// AC (§6.7): brain forget of the item of a proposal forgets its claim too (the claim key text
// goes, and a tombstone holds the claim id). It removes the change request text and discards
// the queue entry, so no draft comes of it.
func TestForgetRemovesChangeRequest(t *testing.T) {
	f := newFixture(t)
	pid, claim := f.extractorProposal(t)
	f.mustReview(t, fmt.Sprint(pid), "--change", "say pnpm for every install")
	r := f.rows(t)
	item := *r.proposals[len(r.proposals)-1].ItemID
	f.mustForget(t, clock(), fmt.Sprint(item))

	s := openStore(t, f.store)
	defer closeStore(t, s)
	var crs []store.ChangeRequest
	var ts []store.Tombstone
	var c store.Claim
	write(t, s, func(tx *sql.Tx) (err error) {
		ctx := context.Background()
		if crs, err = store.ChangeRequests(ctx, tx); err != nil {
			return err
		}
		if ts, err = store.TombstonesByClaim(ctx, tx, claim); err != nil {
			return err
		}
		c, err = store.ClaimByID(ctx, tx, claim)
		return err
	})
	if len(crs) != 1 || crs[0].Status != store.ChangeDiscarded {
		t.Errorf("change requests = %+v, want one discarded", crs)
	}
	if len(ts) != 1 || c.Key != (store.ClaimKey{}) {
		t.Errorf("tombstones %d, claim key %+v; want the claim forgotten", len(ts), c.Key)
	}
	if p := f.rows(t).proposals; p[len(p)-1].ChangeRequest != nil {
		t.Errorf("change request text = %q, want none", *p[len(p)-1].ChangeRequest)
	}
}
