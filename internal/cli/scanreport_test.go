package cli

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

var scanDone = time.Date(2026, 10, 13, 12, 5, 0, 0, time.UTC)

// scanFixture is a done scan report: 64 sessions read, 12.4 M tokens, and repeats of each kind.
func scanFixture(repeats int) store.ScanReport {
	r := store.ScanReport{
		Scan: store.Scan{ID: 1, Status: store.ScanDone, Requested: scanDone.Add(-time.Hour), Claimed: scanDone.Add(-time.Hour),
			Finished: scanDone, Counts: map[string]store.ScanCounts{
				"codex":  {Found: 30, Read: 23, Skipped: map[store.SkipReason]int{store.SkipFork: 7}},
				"claude": {Found: 41, Read: 41},
			}},
		All:       store.TurnTokens{Tokens: 12_400_000, Unknown: 3},
		InRepeats: store.TurnTokens{Tokens: 2_100_000},
	}
	kinds := []store.CandidateKind{store.CandidateCorrection, store.FindingPrompt, store.CandidateCommandChain,
		store.CandidateFailure, store.CandidateProcedure, store.CandidateLesson}
	for i := range repeats {
		r.Repeats = append(r.Repeats, store.Repeat{
			Finding:  store.Finding{ID: i + 1, Kind: kinds[i%len(kinds)]},
			Sessions: 2 + i,
			Turns:    store.TurnTokens{Tokens: int64(48_200 - i*1000), Unknown: i % 2 * 2},
			Excerpt:  fmt.Sprintf("use pnpm,\n  not npm %d", i+1),
		})
	}
	return r
}

// AC 5: the summary line, with the sessions of each CLI and the recorded tokens.
func TestScanSummary(t *testing.T) {
	when := localTime(scanDone)
	for name, c := range map[string]struct {
		r    store.ScanReport
		want string
	}{
		"done": {scanFixture(18), "History scan, done " + when + ": 64 sessions read (41 Claude, 23 Codex), " +
			"12.4 M tokens recorded; 3 turns with no usage. 18 repeats, in turns that used 2.1 M tokens."},
		"one of each": {store.ScanReport{
			Scan:      store.Scan{Status: store.ScanExtracting, Counts: map[string]store.ScanCounts{"claude": {Found: 1, Read: 1}}},
			All:       store.TurnTokens{Tokens: 900},
			InRepeats: store.TurnTokens{Tokens: 900},
			Repeats:   []store.Repeat{{}},
		}, "History scan, read done: 1 session read (1 Claude), 900 tokens recorded. 1 repeat, in turns that used 900 tokens."},
	} {
		if got := ScanSummary(c.r); got != c.want {
			t.Errorf("%s: ScanSummary = %q,\nwant %q", name, got, c.want)
		}
	}
}

// AC 5: at most 10 finding lines, each on one line with what it can become, its sessions, its
// tokens and its proposal id when one exists.
func TestScanFindings(t *testing.T) {
	got := ScanFindings(scanFixture(12), map[int]int64{2: 31})
	if len(got) != 10 {
		t.Fatalf("ScanFindings = %d lines, want 10:\n%s", len(got), strings.Join(got, "\n"))
	}
	want := []string{
		`- "use pnpm, not npm 1": an item in the brief; 2 sessions; 48,200 tokens recorded`,
		`- "use pnpm, not npm 2": a skill or a tool; 3 sessions; 47,200 tokens recorded; 2 turns with no usage; proposal 31`,
		`- "use pnpm, not npm 3": a tool; 4 sessions; 46,200 tokens recorded`,
		`- "use pnpm, not npm 4": a lesson; 5 sessions; 45,200 tokens recorded; 2 turns with no usage`,
		`- "use pnpm, not npm 5": a skill or a tool; 6 sessions; 44,200 tokens recorded`,
		`- "use pnpm, not npm 6": an item in the brief; 7 sessions; 43,200 tokens recorded; 2 turns with no usage`,
	}
	if !reflect.DeepEqual(got[:len(want)], want) {
		t.Errorf("ScanFindings =\n%s\nwant first\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	long := scanFixture(1)
	long.Repeats[0].Excerpt = strings.Repeat("a", 200)
	if got := ScanFindings(long, nil)[0]; !strings.Contains(got, `"`+strings.Repeat("a", 77)+`..."`) {
		t.Errorf("long excerpt line = %q, want it cut to 80 characters", got)
	}
}

// AC 5: the tokens form shows the turns with no usage apart, never as 0 tokens.
func TestTokensRecorded(t *testing.T) {
	for _, c := range []struct {
		in   store.TurnTokens
		want string
	}{
		{store.TurnTokens{Tokens: 48_200, Unknown: 2}, "48,200 tokens recorded; 2 turns with no usage"},
		{store.TurnTokens{Tokens: 48_200}, "48,200 tokens recorded"},
		{store.TurnTokens{Unknown: 1}, "0 tokens recorded; 1 turn with no usage"},
		{store.TurnTokens{Tokens: 1_234_567}, "1.2 M tokens recorded"},
		{store.TurnTokens{Tokens: 999}, "999 tokens recorded"},
	} {
		if got := TokensRecorded(c.in); got != c.want {
			t.Errorf("TokensRecorded(%+v) = %q, want %q", c.in, got, c.want)
		}
	}
}

// AC 5: the notification names the top repeat and its tokens and points to brain review, or
// says that the proposals wait for an extractor choice.
func TestScanNotification(t *testing.T) {
	r := scanFixture(3)
	for _, c := range []struct {
		r         store.ScanReport
		extractor bool
		want      string
	}{
		{r, true, `Brain read 64 past sessions. Top repeat: "use pnpm, not npm 1", 2 sessions, 48,200 tokens. See brain review.`},
		{r, false, `Brain read 64 past sessions. Top repeat: "use pnpm, not npm 1", 2 sessions, 48,200 tokens. ` +
			`Proposals wait for an extractor choice: brain setup --extractor`},
		{scanFixture(0), true, `Brain read 64 past sessions. No repeats found.`},
	} {
		if got := ScanNotification(c.r, c.extractor); got != c.want {
			t.Errorf("ScanNotification(extractor %v) = %q,\nwant %q", c.extractor, got, c.want)
		}
	}
}

// AC 3 and 5: the state line of each status, and the waiting reasons.
func TestScanState(t *testing.T) {
	s := scanFixture(0).Scan
	for status, want := range map[store.ScanStatus]string{
		store.ScanPending: "History scan: you said yes on " + localDate(s.Requested) +
			"; brained reads your past sessions at its next tick.",
		store.ScanReading:    "History scan: read 64 of 71 sessions.",
		store.ScanExtracting: "History scan: read 64 sessions; its proposals wait in the extractor queue.",
		store.ScanDone:       "History scan: done on " + localDate(scanDone) + ".",
	} {
		s.Status = status
		if got := ScanState(s); got != want {
			t.Errorf("ScanState(%s) = %q, want %q", status, got, want)
		}
	}
	for _, w := range []ScanWait{WaitBrained, WaitLearning, WaitExtractor, WaitCaps} {
		if !strings.HasPrefix(string(w), "History scan: ") {
			t.Errorf("waiting reason %q does not name the history scan", w)
		}
	}
}
