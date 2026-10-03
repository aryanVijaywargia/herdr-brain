package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/aryanVijaywargia/herdr-brain/internal/paths"
	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

// The verdicts of the score of a run (E10-S3). A control gets none: it is the no-memory side of
// its case (§12.1).
const (
	verdictPass    = "pass"
	verdictFail    = "fail"
	verdictUnsure  = "unsure"  // you resolve it to pass or fail at the milestone exit
	verdictSkipped = "skipped" // never a pass
	verdictRecord  = "record"  // a usefulness case: a record for your review, not a gate
)

// verdicts returns the verdict of each result of rs, in order: "" for a control and for an
// ad-hoc case. A result with no label did not get its brief, so it fails. A restraint case
// must pass in its control too: it fails when its control does not pass, also when its control
// was skipped, and it is unsure while its control is UNSURE (§12.1). A skill choice control
// gets its own verdict: no marker may appear in it (§12.1).
func verdicts(rs []store.EvalResult) []string {
	ctl := map[[2]string]string{}
	for _, r := range rs {
		if r.Control {
			ctl[[2]string{r.Case, r.Harness}] = r.Label
		}
	}
	out := make([]string, len(rs))
	for i, r := range rs {
		switch {
		case r.Stage == "" || r.Control && r.Stage != skillStage:
		case r.Status == string(Skipped):
			out[i] = verdictSkipped
		case r.Stage == "usefulness":
			out[i] = verdictRecord
		case r.Label == string(Unsure):
			out[i] = verdictUnsure
		case r.Label != string(Pass):
			out[i] = verdictFail
		case r.Stage != "restraint":
			out[i] = verdictPass
		default:
			switch ctl[[2]string{r.Case, r.Harness}] {
			case string(Pass):
				out[i] = verdictPass
			case string(Unsure):
				out[i] = verdictUnsure
			default:
				out[i] = verdictFail
			}
		}
	}
	return out
}

// score is the one-line score of a run: the count of each verdict.
func score(vs []string) string {
	n := map[string]int{}
	total := 0
	for _, v := range vs {
		if v != "" {
			n[v]++
			total++
		}
	}
	return fmt.Sprintf("score: %d pass, %d fail, %d unsure, %d skipped, %d record, of %d probes",
		n[verdictPass], n[verdictFail], n[verdictUnsure], n[verdictSkipped], n[verdictRecord], total)
}

// writeReport writes the short report of a run to the test store folder and prints its score
// to w: the score, one line for each probe, the UNSURE results that you resolve at the
// milestone exit, the usefulness records (§12.1, E10-S3), and the skill choice verdict of each
// CLI (E16-S5). A run of ad-hoc cases only has no report.
func writeReport(storeDir string, cases []Case, rs []store.EvalResult, w io.Writer) error {
	if !hasStage(cases) {
		return nil
	}
	vs := verdicts(rs)
	var b strings.Builder
	fmt.Fprintf(&b, "# brain-eval report\n\n%s\n\n", score(vs))
	b.WriteString("| Case | Stage | CLI | Status | Label | Verdict |\n|---|---|---|---|---|---|\n")
	for i, r := range rs {
		id := r.Case
		if r.Control {
			id += " (control)"
		}
		fmt.Fprintf(&b, "| %s | %s | %s %s | %s | %s | %s |\n", id, r.Stage, r.Harness, r.CLIVersion, r.Status, r.Label, vs[i])
	}
	section := func(title, verdict string, line func(store.EvalResult) string) {
		fmt.Fprintf(&b, "\n## %s\n\n", title)
		n := 0
		for i, r := range rs {
			if vs[i] == verdict {
				b.WriteString("- " + r.Case + " " + r.Harness + ": " + line(r) + "\n")
				n++
			}
		}
		if n == 0 {
			b.WriteString("None.\n")
		}
	}
	section("UNSURE: resolve each to pass or fail", verdictUnsure, func(r store.EvalResult) string {
		return fmt.Sprintf("%s; reply %q", r.Rule+r.Why, clip(r.Reply))
	})
	section("Usefulness records", verdictRecord, func(r store.EvalResult) string {
		return fmt.Sprintf("%s; reply %q", r.Why, clip(r.Reply))
	})
	skills, lines := skillReport(rs, vs)
	b.WriteString(skills)
	path := paths.EvalReport(storeDir)
	f, err := os.CreateTemp(filepath.Dir(path), "eval-report.*.tmp")
	if err != nil {
		return err
	}
	if _, err := f.WriteString(b.String()); err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(f.Name())
		return err
	}
	if err := os.Rename(f.Name(), path); err != nil {
		_ = os.Remove(f.Name())
		return err
	}
	fmt.Fprintf(w, "%s\n%sreport: %s\n", score(vs), lines, path)
	return nil
}

func hasStage(cases []Case) bool {
	for _, c := range cases {
		if c.Stage != "" {
			return true
		}
	}
	return false
}

// clip shortens a reply for one report line. [NEW] 300 bytes; the results file keeps it all.
func clip(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 300 {
		return s[:300] + "..."
	}
	return s
}
