package cli

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

// around adds stored text to the capture segment of the observation with capture identity name:
// one observation for each text of before, in order, just before it, and of after just after
// it. A text "<gap>" adds a gap.
func (f *fixture) around(t *testing.T, name string, before, after []string) {
	t.Helper()
	f.storeWrite(t, func(ctx context.Context, tx *sql.Tx) error {
		var seg int64
		var at time.Time
		obs, err := store.Observations(ctx, tx)
		if err != nil {
			return err
		}
		for _, o := range obs {
			if o.CaptureIdentity == name {
				seg, at = o.SegmentID, o.At
			}
		}
		add := func(text string, at time.Time, n int) error {
			o := store.Observation{CaptureIdentity: fmt.Sprintf("%s-%s-%d", name, at.Format("150405"), n),
				Source: store.SourceHerdr, SegmentID: seg, Event: "idle", Excerpt: text, Status: store.ObservationComplete,
				Sender: store.SenderUnconfirmed, At: at}
			if text == "<gap>" {
				o.Excerpt, o.Status = "", store.ObservationGap
			}
			_, err := store.AddObservation(ctx, tx, o)
			return err
		}
		for i, text := range before {
			if err := add(text, at.Add(time.Duration(i-len(before))*time.Second), i); err != nil {
				return err
			}
		}
		for i, text := range after {
			if err := add(text, at.Add(time.Duration(i+1)*time.Second), i); err != nil {
				return err
			}
		}
		return nil
	})
}

// AC 1 (E20-S4): key v shows the stored text around each source excerpt, as brain review <id>
// --evidence does; v again goes back to the proposal. The keys stay the last line. Golden screen.
func TestPaneEvidenceGolden(t *testing.T) {
	inZone(t)
	f := newFixture(t)
	pid, _ := f.extractorProposal(t)
	f.around(t, "a1", []string{"$ npm install", "added 3 packages"}, []string{"<gap>", "ok,\nusing pnpm now"})
	screen := f.paneScreen(t, pid, "vvq", 0, 0)
	if !strings.HasSuffix(screen, "\n"+itemKeys) {
		t.Errorf("screen =\n%s\nwant it to end with the keys %q", screen, itemKeys)
	}
	f.paneGolden(t, "evidence", screen)
	out := f.mustReview(t, fmt.Sprint(pid), "--evidence")
	if !strings.Contains(screen, out) {
		t.Errorf("pane screen =\n%s\nwant the view of --evidence:\n%s", screen, out)
	}
}

// AC 1 (E20-S4): the evidence view is cut to the pane height as the proposal view is (#254),
// and its cut line names brain review <id> --evidence.
func TestPaneEvidenceCut(t *testing.T) {
	f := newFixture(t)
	pid, _ := f.extractorProposal(t)
	f.around(t, "a1", lines("before", 20), lines("after", 20))
	screen := f.paneScreen(t, pid, "vq", 16, 80)
	last := strings.TrimPrefix(screen[strings.LastIndex(screen, clearScreen):], clearScreen)
	if n := screenRows(last, 80); n > 16 {
		t.Errorf("screen takes %d rows, want at most 16:\n%s", n, last)
	}
	if !strings.HasSuffix(last, "\n"+itemKeys) {
		t.Errorf("screen =\n%s\nwant it to end with the keys", last)
	}
	if !strings.Contains(last, fmt.Sprintf("more lines: brain review %d --evidence shows them all\n", pid)) {
		t.Errorf("screen =\n%s\nwant the cut line of --evidence", last)
	}
}

func lines(prefix string, n int) []string {
	var ls []string
	for i := range n {
		ls = append(ls, fmt.Sprintf("%s %d", prefix, i+1))
	}
	return ls
}

// AC 1 (E20-S4): at most 20 lines before and 20 lines after an excerpt, the nearest ones. The
// stored text of one observation can hold many lines: the bound counts lines.
func TestEvidenceLineBound(t *testing.T) {
	f := newFixture(t)
	pid, _ := f.extractorProposal(t)
	f.around(t, "a1", append(lines("old", 25), strings.Join(lines("before", 5), "\n")),
		append(lines("after", 15), strings.Join(lines("late", 10), "\n")))
	out := f.mustReview(t, fmt.Sprint(pid), "--evidence")
	// a1 is the older of the two excerpts: excerpt 2.
	view := out[strings.Index(out, "excerpt 2:"):]
	var marks []string
	for _, l := range strings.Split(strings.TrimSpace(view), "\n")[1:] {
		marks = append(marks, l[:4])
	}
	if got := strings.Join(marks, ""); got != strings.Repeat("  | ", 20)+"  > "+strings.Repeat("  | ", 20) {
		t.Errorf("evidence view =\n%s\nwant 20 lines before and 20 after the excerpt", view)
	}
	for _, want := range []string{"  | old 11\n", "  | before 5\n  > text\n  | after 1\n", "  | late 5\n"} {
		if !strings.Contains(view, want) {
			t.Errorf("evidence view =\n%s\nwant %q", view, want)
		}
	}
	for _, not := range []string{"old 10\n", "late 6\n"} {
		if strings.Contains(view, not) {
			t.Errorf("evidence view =\n%s\nwant no %q: it is past the bound", view, not)
		}
	}
}

// AC 3 (E20-S4): the stored text goes through the secret filter. AC 2: the view holds only
// the text of the stored observations of the excerpt's segment.
func TestEvidenceSecret(t *testing.T) {
	f := newFixture(t)
	pid, _ := f.extractorProposal(t)
	const key = "sk-ant-api03-AbCdEfGhIjKlMnOpQrStUv"
	f.around(t, "a1", []string{"export API_TOKEN=hunter2hunter2", "PASSWORD=x9y8z7"}, []string{"the key is " + key})
	out := f.mustReview(t, fmt.Sprint(pid), "--evidence")
	for _, s := range []string{"hunter2", "x9y8z7", key} {
		if strings.Contains(out, s) {
			t.Errorf("evidence view =\n%s\nholds the secret %q", out, s)
		}
	}
	if n := strings.Count(out, "[secret]"); n != 3 {
		t.Errorf("evidence view =\n%s\nwant 3 [secret] marks, got %d", out, n)
	}
	// The excerpt of a2 has its own segment: the text around a1 does not join it.
	if a2 := out[strings.Index(out, "excerpt 1:"):strings.Index(out, "excerpt 2:")]; strings.Contains(a2, "  | ") {
		t.Errorf("evidence view of a2 =\n%s\nwant no text of the segment of a1", a2)
	}
}

// AC 4 (E20-S4): when brain forget --session took the stored text, the view says so in one
// line. A proposal with no captured source says that it has none.
func TestEvidenceGone(t *testing.T) {
	f := newFixture(t)
	pid, _ := f.extractorProposal(t)
	f.storeWrite(t, func(ctx context.Context, tx *sql.Tx) error {
		ss, err := store.Sessions(ctx, tx)
		if err != nil {
			return err
		}
		for _, s := range ss {
			if err := store.ForgetSession(ctx, tx, s.ID); err != nil {
				return err
			}
		}
		return nil
	})
	out := f.mustReview(t, fmt.Sprint(pid), "--evidence")
	if want := fmt.Sprintf("proposal %d (pending): the stored text around each source excerpt, up to 20 lines before and after\n",
		pid) + evidenceGone; out != want {
		t.Errorf("evidence view = %q, want %q", out, want)
	}
	rid := f.remember(t, "use pnpm")
	if out := f.mustReview(t, fmt.Sprint(rid), "--evidence"); !strings.HasSuffix(out, "\n"+evidenceNone) {
		t.Errorf("evidence view of a remember = %q, want %q", out, evidenceNone)
	}
}

// AC 1 (E20-S4): --evidence takes one existing proposal id alone.
func TestReviewEvidenceArgs(t *testing.T) {
	f := newFixture(t)
	pid, _ := f.extractorProposal(t)
	if _, err := f.review(t, "999", "--evidence"); !errors.Is(err, errNotFound) {
		t.Errorf("review 999 --evidence = %v, want not found", err)
	}
	for _, args := range [][]string{{"--evidence"}, {fmt.Sprint(pid), "--evidence", "--approve"},
		{fmt.Sprint(pid), "--evidence", "--detail"}, {"--all", "--evidence"}} {
		if _, err := f.review(t, args...); err == nil || !strings.Contains(err.Error(), "goes with a proposal id alone") {
			t.Errorf("review %q = %v, want the --evidence error", args, err)
		}
	}
}

// AC 1 (E20-S4): v keeps the keys of the proposal's kind: a dispute keeps its keys in the
// evidence view.
func TestPaneEvidenceDisputeKeys(t *testing.T) {
	f := newFixture(t)
	_, git := f.gitRepo(t, "config/load.go")
	f.approved(t, loaderText)
	git("mv", "config/load.go", "config/loader.go")
	f.stale(t)
	screen := f.paneScreen(t, f.pendingDispute(t).ID, "vq", 0, 0)
	last := screen[strings.LastIndex(screen, clearScreen):]
	if !strings.Contains(last, evidenceNone) || !strings.HasSuffix(last, "\n"+disputeKeys) {
		t.Errorf("screen =\n%s\nwant the evidence view with the dispute keys", last)
	}
}
