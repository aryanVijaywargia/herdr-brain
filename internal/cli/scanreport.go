package cli

import (
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

// The text of the history scan report (§6.8 "Efficiency report"). brain status, brain review
// and the brained notification show it; brain setup shows the state line.

// maxScanFindings is the most finding lines of the report. [NEW] 10 (§6.8).
const maxScanFindings = 10

// maxScanExcerpt is the most characters of a finding excerpt on its one line. [NEW] 80, as the
// failed command of the handoff lines (§8.3).
const maxScanExcerpt = 80

// ScanState is the one state line of the scan s, for the setup plan and brain status:
// "History scan: done on 2026-10-13." for a done scan.
func ScanState(s store.Scan) string {
	switch s.Status {
	case store.ScanPending:
		return fmt.Sprintf("History scan: you said yes on %s; brained reads your past sessions at its next tick.", localDate(s.Requested))
	case store.ScanReading:
		found, read := scanSessions(s)
		return fmt.Sprintf("History scan: read %d of %d sessions.", read, found)
	case store.ScanExtracting:
		_, read := scanSessions(s)
		return fmt.Sprintf("History scan: read %d sessions; its proposals wait in the extractor queue.", read)
	}
	return fmt.Sprintf("History scan: done on %s.", localDate(s.Finished))
}

// ScanWait is a reason why the scan or its proposals wait (§6.8). brain status says it.
type ScanWait string

// The waiting reasons.
const (
	WaitBrained   ScanWait = "History scan: waits for brained, which starts with a herdr session bound to this store."
	WaitLearning  ScanWait = "History scan: waits until learning is on: brain learn on"
	WaitExtractor ScanWait = "History scan: proposals wait for an extractor choice: brain setup --extractor"
	WaitCaps      ScanWait = "History scan: the rest of its proposals waits for room in the daily caps, the next day at the latest."
)

// ScanSummary is the summary line of the report r: "History scan, done 2026-10-13 14:05: 64
// sessions read (41 Claude, 23 Codex), 12.4 M tokens recorded. 18 repeats, in turns that used
// 2.1 M tokens."
func ScanSummary(r store.ScanReport) string {
	head := "History scan, read so far"
	switch r.Scan.Status {
	case store.ScanDone:
		head = "History scan, done " + localTime(r.Scan.Finished)
	case store.ScanExtracting:
		head = "History scan, read done"
	}
	_, read := scanSessions(r.Scan)
	repeats := plural(len(r.Repeats), "repeat")
	switch {
	case r.InRepeats.Known > 0:
		repeats += ", in turns that used " + tokensUsed(r.InRepeats)
	case r.InRepeats.Unknown > 0:
		repeats += "; " + tokensUsed(r.InRepeats)
	}
	return fmt.Sprintf("%s: %s read (%s), %s. %s.", head, plural(read, "session"), perCLI(r.Scan),
		TokensRecorded(r.All), repeats)
}

// ScanFindings are the finding lines of the report r, at most maxScanFindings, most tokens
// first. proposals maps a finding id to its proposal id; a finding with none shows no id.
func ScanFindings(r store.ScanReport, proposals map[int]int64) []string {
	var lines []string
	for _, rep := range r.Repeats[:min(len(r.Repeats), maxScanFindings)] {
		l := fmt.Sprintf("- %q: %s; %s; %s", scanExcerpt(rep.Excerpt), becomes(rep.Finding.Kind),
			plural(rep.Sessions, "session"), TokensRecorded(rep.Turns))
		if id, ok := proposals[rep.Finding.ID]; ok {
			l += "; proposal " + strconv.FormatInt(id, 10)
		}
		lines = append(lines, l)
	}
	return lines
}

// ScanNotification is the text of the one herdr notification at the end of the read. It
// names the top repeat and its tokens and points to brain review; with no extractor chosen, it
// says that the proposals wait for that choice.
func ScanNotification(r store.ScanReport, extractor bool) string {
	_, read := scanSessions(r.Scan)
	head := fmt.Sprintf("Brain read %d past sessions.", read)
	if len(r.Repeats) == 0 {
		return head + " No repeats found."
	}
	top := r.Repeats[0]
	next := "See brain review."
	if !extractor {
		next = "Proposals wait for an extractor choice: brain setup --extractor"
	}
	return fmt.Sprintf("%s Top repeat: %q, %s, %s. %s", head, scanExcerpt(top.Excerpt),
		plural(top.Sessions, "session"), tokensUsed(top.Turns), next)
}

// TokensRecorded is the tokens form of t: "48,200 tokens recorded; 2 turns with no usage".
// The turns with no usage show apart, never as 0 tokens, and with no turn that has a usage
// field it is "tokens: unknown" (S91).
func TokensRecorded(t store.TurnTokens) string {
	return tokens(t, " tokens recorded")
}

// tokensUsed is the tokens form of t in a sentence about use: "48,200 tokens; 2 turns with no
// usage", or "tokens: unknown" (S91).
func tokensUsed(t store.TurnTokens) string {
	return tokens(t, " tokens")
}

// tokens is the known tokens of t with the unit unit, and its turns with no usage apart; with
// no known turn it is "tokens: unknown".
func tokens(t store.TurnTokens, unit string) string {
	if t.Known == 0 {
		return "tokens: unknown"
	}
	s := tokenCount(t.Tokens) + unit
	if t.Unknown > 0 {
		s += "; " + plural(t.Unknown, "turn") + " with no usage"
	}
	return s
}

// becomes says what a finding of kind k can become (§6.8 "Efficiency report").
func becomes(k store.CandidateKind) string {
	switch k {
	case store.FindingPrompt, store.CandidateProcedure:
		return "a skill or a tool"
	case store.CandidateCommandChain:
		return "a tool"
	case store.CandidateFailure:
		return "a lesson"
	}
	return "an item in the brief"
}

// scanSessions returns the sessions that the scan found and read, over every CLI.
func scanSessions(s store.Scan) (found, read int) {
	for _, c := range s.Counts {
		found += c.Found
		read += c.Read
	}
	return found, read
}

// perCLI is the sessions read by each CLI: "41 Claude, 23 Codex".
func perCLI(s store.Scan) string {
	var parts []string
	for _, h := range slices.Sorted(maps.Keys(s.Counts)) {
		parts = append(parts, fmt.Sprintf("%d %s", s.Counts[h].Read, cliName(h)))
	}
	return strings.Join(parts, ", ")
}

// cliName is the name of harness h in the report.
func cliName(h string) string {
	switch h {
	case "claude":
		return "Claude"
	case "codex":
		return "Codex"
	}
	return h
}

// tokenCount is n with thousands commas, or in millions with one decimal from 1,000,000 on
// [NEW]: "48,200", "12.4 M".
func tokenCount(n int64) string {
	if n >= 1_000_000 {
		return fmt.Sprintf("%.1f M", float64(n)/1_000_000)
	}
	s := strconv.FormatInt(n, 10)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

// plural is n and noun, with an s when n is not 1.
func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return strconv.Itoa(n) + " " + noun + "s"
}

// scanExcerpt is excerpt on one line, cut to maxScanExcerpt characters with "...".
func scanExcerpt(excerpt string) string {
	r := []rune(oneLine(excerpt))
	if len(r) <= maxScanExcerpt {
		return string(r)
	}
	return string(r[:maxScanExcerpt-3]) + "..."
}

// localDate is the local date of t.
func localDate(t time.Time) string { return t.Local().Format(time.DateOnly) }
