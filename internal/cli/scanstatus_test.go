package cli

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/aryanVijaywargia/herdr-brain/internal/herdr"
	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

// scanRow writes the scan row of the store at dir in status, with counts. A row past pending
// has two scanned sessions with one repeated prompt, of 100 and 200 tokens. It returns the
// session ids.
func scanRow(t *testing.T, dir string, status store.ScanStatus, counts map[string]store.ScanCounts) []int64 {
	t.Helper()
	ctx := context.Background()
	s := openStore(t, dir)
	defer closeStore(t, s)
	var sessions []int64
	write(t, s, func(tx *sql.Tx) error {
		if err := store.RequestScan(ctx, tx, testNow); err != nil || status == store.ScanPending {
			return err
		}
		if _, _, err := store.ClaimScan(ctx, tx, "default", testNow); err != nil {
			return err
		}
		if err := store.SetScanCounts(ctx, tx, 1, counts); err != nil {
			return err
		}
		var turns []store.ScanTurn
		var obs []store.FindingObs
		for i, id := range []string{"c1", "c2"} {
			at := testNow.Add(time.Duration(i) * time.Minute)
			session, seg, err := store.ScanSession(ctx, tx, store.SessionKey{HerdrSession: "default", Harness: "claude",
				Kind: store.KeyAgent, ID: id}, "", at, at)
			if err != nil {
				return err
			}
			o := store.Observation{CaptureIdentity: "claude:" + id, Source: store.SourceTranscript, SegmentID: seg,
				Event: "prompt", Excerpt: "use pnpm, not npm", Status: store.ObservationComplete,
				Sender: store.SenderUnconfirmed, Counts: true, At: at}
			if o.ID, err = store.AddObservation(ctx, tx, o); err != nil {
				return err
			}
			sessions = append(sessions, session)
			turns = append(turns, store.ScanTurn{ID: o.ID, Tokens: int64(100 * (i + 1)), Known: true})
			obs = append(obs, store.FindingObs{Observation: o.ID, Identity: o.CaptureIdentity, Turn: o.ID})
		}
		if err := store.AddScanTurns(ctx, tx, 1, turns); err != nil {
			return err
		}
		if err := store.SetScanFindings(ctx, tx, 1, []store.Finding{{ID: 1, Kind: store.FindingPrompt, Obs: obs}}); err != nil {
			return err
		}
		return store.SetScanStatus(ctx, tx, 1, status, testNow)
	})
	return sessions
}

// wantLines fails unless out holds lines, in order, as whole lines.
func wantLines(t *testing.T, out string, lines ...string) {
	t.Helper()
	rest := out
	for _, l := range lines {
		i := strings.Index(rest, "\n"+l+"\n")
		if i < 0 {
			t.Errorf("brain status:\n%s\nwant the line, after the ones before it:\n%s", out, l)
			return
		}
		rest = rest[i+len(l)+1:]
	}
}

// AC 7: brain status shows why the scan waits, its progress line, the sessions that it
// skipped, then the report from the observations that still exist.
func TestStatusHistoryScan(t *testing.T) {
	t.Run("none", func(t *testing.T) {
		f := newStatusFixture(t)
		if out := f.mustStatus(t); strings.Contains(out, "History scan") {
			t.Errorf("brain status with no scan row:\n%s\nwant no history scan line", out)
		}
	})
	t.Run("pending", func(t *testing.T) {
		f := newStatusFixture(t)
		scanRow(t, f.store, store.ScanPending, nil)
		out := f.mustStatus(t)
		wantLines(t, out, string(WaitBrained), "History scan: you said yes on "+localDate(testNow)+
			"; brained reads your past sessions at its next tick.")
		if strings.Contains(out, "History scan, ") {
			t.Errorf("brain status:\n%s\nwant no report before the read", out)
		}
	})
	t.Run("learning off", func(t *testing.T) {
		f := newStatusFixture(t)
		scanRow(t, f.store, store.ScanPending, nil)
		s := openStore(t, f.store)
		write(t, s, func(tx *sql.Tx) error {
			return store.SetLearning(context.Background(), tx, store.LearningSetting{Scope: store.LearningStore, Changed: testNow})
		})
		closeStore(t, s)
		wantLines(t, f.mustStatus(t), string(WaitLearning))
	})
	t.Run("reading", func(t *testing.T) {
		f := newStatusFixture(t)
		scanRow(t, f.store, store.ScanReading, map[string]store.ScanCounts{
			"claude": {Found: 3, Read: 2},
			"codex":  {Found: 2, Skipped: map[store.SkipReason]int{store.SkipUnknownFormat: 2, store.SkipCompressed: 4}},
		})
		wantLines(t, f.mustStatus(t),
			"History scan: read 2 of 5 sessions.",
			"History scan skipped: Codex 4 compressed, 2 unknown format.",
			"History scan: no Codex session file parsed (2 files of unknown format), so the scan read no Codex session",
			"History scan, read so far: 2 sessions read (2 Claude, 0 Codex), 300 tokens recorded. 1 repeat, in turns that used 300 tokens.",
			`- "use pnpm, not npm": a skill or a tool; 2 sessions; 300 tokens recorded`)
	})
	// A forget takes its observations out of the numbers, and a finding below its gate
	// leaves the report.
	t.Run("extracting", func(t *testing.T) {
		f := newStatusFixture(t)
		sessions := scanRow(t, f.store, store.ScanExtracting, map[string]store.ScanCounts{"claude": {Found: 2, Read: 2}})
		wantLines(t, f.mustStatus(t),
			// The scan work of the global pool waits under its own name, not an empty key.
			"  waiting in the global pool: 1 batch, the oldest 3h old; no extractor CLI is chosen; run brain setup",
			string(WaitExtractor),
			"History scan: read 2 sessions; its proposals wait in the extractor queue.",
			"History scan, read done: 2 sessions read (2 Claude), 300 tokens recorded. 1 repeat, in turns that used 300 tokens.",
			`- "use pnpm, not npm": a skill or a tool; 2 sessions; 300 tokens recorded`)
		s := openStore(t, f.store)
		write(t, s, func(tx *sql.Tx) error { return store.ForgetSession(context.Background(), tx, sessions[0]) })
		closeStore(t, s)
		out := f.mustStatus(t)
		wantLines(t, out, "History scan, read done: 2 sessions read (2 Claude), 300 tokens recorded. 0 repeats.")
		if strings.Contains(out, "use pnpm") {
			t.Errorf("brain status after the forget:\n%s\nwant no finding below its gate", out)
		}
	})
}

// AC 9: brain doctor reports a CLI with no session file that parsed. It is a note: the
// scan ran once, and the other CLI and herdr capture still work.
func TestDoctorHistoryScanUnparsed(t *testing.T) {
	f := newDoctorFixture(t)
	receipt(t, f.store, "default", "claude", "w14:p1", "s-1", store.ResultEmitted, "")
	f.herdr.panes = []herdr.Pane{agent("w14:p1", "claude", "s-1")}
	scanRow(t, f.store, store.ScanDone, map[string]store.ScanCounts{
		"claude": {Found: 2, Read: 2},
		"codex":  {Found: 3, Skipped: map[store.SkipReason]int{store.SkipUnknownFormat: 3}},
	})
	out, err := f.doctor(t, f.sock, "")
	want := "note: History scan: no Codex session file parsed (3 files of unknown format), so the scan read no Codex " +
		"session; Brain may need an update for this format: brain update\nall checks passed\n"
	if err != nil || out != want {
		t.Errorf("Doctor() = %v\n%s\nwant\n%s", err, out, want)
	}
}
