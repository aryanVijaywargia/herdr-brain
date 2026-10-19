package cli

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aryanVijaywargia/herdr-brain/internal/extract"
	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

// scanPrompt is the repeat of the scan fixture.
const scanPrompt = "no, use pnpm instead of npm"

// scanFixture stores two sessions that the history scan imported in workspace ws_a14, each with
// scanPrompt as a turn of 24,100 tokens, and the scan row in its extracting state with one
// correction finding of them.
func (f *fixture) scanFixture(t *testing.T) {
	t.Helper()
	f.storeWrite(t, func(ctx context.Context, tx *sql.Tx) error {
		var obs []store.FindingObs
		var turns []store.ScanTurn
		for i := range 2 {
			key := store.SessionKey{HerdrSession: "default", Harness: "claude", Kind: store.KeyAgent, ID: fmt.Sprint("scan-", i)}
			at := testNow.Add(-time.Duration(48-i) * time.Hour)
			sid, err := store.OpenSession(ctx, tx, key, "", "ws_a14", at)
			if err != nil {
				return err
			}
			seg, err := store.StartSegment(ctx, tx, sid, "", "ws_a14", "recent_unwrapped", at)
			if err != nil {
				return err
			}
			id := fmt.Sprint("scan-", i, "-0")
			o, err := store.AddObservation(ctx, tx, store.Observation{CaptureIdentity: id, Source: "transcript",
				SegmentID: seg, Event: "prompt", Excerpt: scanPrompt, Status: store.ObservationComplete,
				Sender: store.SenderUnconfirmed, Counts: true, At: at})
			if err != nil {
				return err
			}
			if err := store.MoveWatermark(ctx, tx, store.Batch{SessionID: sid, WorkspaceKey: "ws_a14",
				LastObservation: o, LastCapture: id}); err != nil {
				return err
			}
			if err := store.EndSession(ctx, tx, key, at.Add(time.Minute)); err != nil {
				return err
			}
			obs = append(obs, store.FindingObs{Observation: o, Identity: id, Turn: o})
			turns = append(turns, store.ScanTurn{ID: o, Tokens: 24100, Known: true})
		}
		if err := store.RequestScan(ctx, tx, testNow); err != nil {
			return err
		}
		s, _, err := store.ClaimScan(ctx, tx, "default", testNow)
		if err != nil {
			return err
		}
		if err := store.SetScanCounts(ctx, tx, s.ID, map[string]store.ScanCounts{"claude": {Found: 2, Read: 2}}); err != nil {
			return err
		}
		if err := store.AddScanTurns(ctx, tx, s.ID, turns); err != nil {
			return err
		}
		if err := store.SetScanFindings(ctx, tx, s.ID, []store.Finding{{ID: 1, Kind: store.CandidateCorrection,
			Scope: "ws_a14", Obs: obs}}); err != nil {
			return err
		}
		return store.SetScanStatus(ctx, tx, s.ID, store.ScanExtracting, testNow)
	})
}

// extractPass runs one extract pass with cli at testNow.
func (f *fixture) extractPass(t *testing.T, cli extract.Runner) {
	t.Helper()
	s := openStore(t, f.store)
	defer closeStore(t, s)
	p := extract.Pass{Store: s, Runner: cli, HerdrSession: "default", DefaultOn: true, Now: func() time.Time { return testNow }}
	if err := p.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// AC: brain review shows the scan report at its top while a scan batch or a scan proposal
// waits, and each scan proposal shows the numbers of its finding in one line.
func TestReviewScanReport(t *testing.T) {
	f := newFixture(t)
	f.setClaude(t)
	f.scanFixture(t)
	finding := `- "` + scanPrompt + `": an item in the brief; 2 sessions; 48,200 tokens recorded`
	summary := "History scan, read done: 2 sessions read (2 Claude), 48,200 tokens recorded. 1 repeat, in turns that used 48,200 tokens."

	// The scan batch waits: the report shows, with no proposal id yet.
	if out, want := f.mustReview(t), summary+"\n"+finding+"\n\nno proposals wait for your review\n"; out != want {
		t.Errorf("brain review while the scan batch waits =\n%s\nwant\n%s", out, want)
	}

	cli := &claimCLI{}
	f.extractPass(t, cli)
	if cli.calls != 1 {
		t.Fatalf("calls = %d, want 1", cli.calls)
	}
	ps := f.rows(t).proposals
	pid := ps[len(ps)-1].ID
	out := f.mustReview(t)
	head := summary + "\n" + finding + "; proposal " + strconv.FormatInt(pid, 10) + "\n\n"
	if !strings.HasPrefix(out, head) || !strings.Contains(out, fmt.Sprintf("%d  workspace w14", pid)) {
		t.Errorf("brain review =\n%s\nwant first\n%s", out, head)
	}
	show := f.mustReview(t, strconv.FormatInt(pid, 10))
	if want := "From the history scan: 2 past sessions; those turns used 48,200 tokens.\n"; !strings.Contains(show, want) {
		t.Errorf("brain review %d =\n%s\nwant the line %q", pid, show, want)
	}

	// The next pass marks the scan done; the report stays while its proposal waits.
	f.extractPass(t, cli)
	if out := f.mustReview(t); !strings.HasPrefix(out, "History scan, done ") {
		t.Errorf("brain review with the scan done and its proposal waiting =\n%s\nwant the report first", out)
	}
	f.mustReview(t, strconv.FormatInt(pid, 10), "--reject", "--reason", "not now")
	if out := f.mustReview(t); strings.Contains(out, "History scan") {
		t.Errorf("brain review with no scan work waiting =\n%s\nwant no report", out)
	}
}

// A live proposal shows no scan line.
func TestReviewNoScanLine(t *testing.T) {
	f := newFixture(t)
	id := f.remember(t, "use pnpm here")
	if out := f.mustReview(t, strconv.FormatInt(id, 10)); strings.Contains(out, "history scan") {
		t.Errorf("brain review %d =\n%s\nwant no scan line", id, out)
	}
}
