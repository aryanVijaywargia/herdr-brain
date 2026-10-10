package cli

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/aryanVijaywargia/herdr-brain/internal/herdr"
	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

// AC #371: brain review does not approve or reject from a pane where herdr shows a live agent:
// each such call prints one line that points to the review pane and a shell pane, and writes
// nothing. The agent's brain remember and its --item --edit still make drafts, which wait. A
// shell pane and the review pane, also the review pane of a popup whose caller is the focused
// agent pane, still decide.
func TestAgentPaneDecides(t *testing.T) {
	f := newFixture(t)
	item := f.approved(t, "use pnpm") // in the shell pane w14:p1
	p := f.panes["w14:p1"]
	p.Agent = ptr("claude")
	f.panes["w14:p1"] = p

	lesson := f.remember(t, "run make lint first")
	f.mustReview(t, "--item", fmt.Sprint(item), "--edit", "use pnpm 9")
	edit := f.rows(t).proposals[len(f.rows(t).proposals)-1].ID
	for _, args := range [][]string{
		{fmt.Sprint(lesson), "--approve"},
		{fmt.Sprint(lesson), "--approve", "--edit", "run make lint"},
		{fmt.Sprint(edit), "--reject"},
		{fmt.Sprint(edit), "--keep"},
		{fmt.Sprint(edit), "--retire"},
		{"--merge", "1"},
		{"--apart", "1"},
		{"--reopen", "1"},
		{"--reset", "1"},
		{"--restore", fmt.Sprint(item)},
	} {
		out, err := f.review(t, args...)
		want := "pane w14:p1 runs an agent (claude), so brain review does not approve or reject here: decide in the review pane or in a shell pane; nothing was written"
		if err == nil || err.Error() != want || out != "" {
			t.Errorf("review %q = %q, %v; want the error %q", args, out, err, want)
		}
	}
	// --all only lists the candidates; no flag approves or rejects many proposals at once.
	if _, err := f.review(t, "--all"); err != nil {
		t.Errorf("review --all = %v, want the list", err)
	}
	if _, err := f.review(t, "--all", "--approve"); err == nil || !strings.Contains(err.Error(), "give one action") {
		t.Errorf("review --all --approve = %v, want a usage error", err)
	}
	r := f.rows(t)
	for _, id := range []int64{lesson, edit} {
		if s := r.proposal(t, id).Status; s != store.ProposalPending {
			t.Errorf("proposal %d is %s after the agent calls, want pending", id, s)
		}
	}

	// The review pane of the lesson, opened with the agent pane as its caller (as a popup is).
	if out, err := f.pane(t, lesson, typed("a")); err != nil {
		t.Fatalf("review pane approve = %v\n%s", err, out)
	}
	// The user's shell pane in the same workspace.
	f.panes["w14:p3"] = herdr.Pane{ID: "w14:p3", WorkspaceID: "w14", TabID: "w14:t1"}
	var out bytes.Buffer
	if err := Review(context.Background(), f.env(f.sock, "w14:p3", "w14", &out), []string{fmt.Sprint(edit), "--reject"}); err != nil {
		t.Fatalf("review from the shell pane = %v", err)
	}
	r = f.rows(t)
	if a, b := r.proposal(t, lesson).Status, r.proposal(t, edit).Status; a != store.ProposalApproved || b != store.ProposalRejected {
		t.Errorf("proposals are %s and %s, want approved in the review pane and rejected in the shell pane", a, b)
	}
}
