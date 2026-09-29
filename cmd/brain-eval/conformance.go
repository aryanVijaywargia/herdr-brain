package main

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/aryanVijaywargia/herdr-brain/internal/adapter"
	"github.com/aryanVijaywargia/herdr-brain/internal/harness"
	"github.com/aryanVijaywargia/herdr-brain/internal/install"
	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

// The conformance results other than harness.Pass (§8.5, E10-S3).
const (
	resultPending      = "pending" // the run has not completed; an interrupted run stays so
	resultFail         = "fail"
	resultInconclusive = "inconclusive"
)

// fullBriefMin is the smallest brief that counts as full size for the Codex inline check: near
// the whole brief budget of 9,500 (§8.1). [NEW] 9,000.
const fullBriefMin = 9000

// runID is the id of a run that started at t. [NEW] form.
func runID(t time.Time) string { return t.UTC().Format("20060102T150405.000Z") }

// startConformance writes a pending record for each harness whose delivery stage the run
// asks (§8.5 conformance test), before any case runs, and returns those records. A harness
// that the run skips, or whose delivery stage it does not run, keeps its record.
func (r *Runner) startConformance(cases []Case, skip map[string]string, versions map[string]string, started time.Time) ([]harness.Result, error) {
	var hs []string
	for _, c := range cases {
		if c.Stage != "delivery" || c.Control {
			continue
		}
		for _, p := range c.Probes {
			if skip[p.Harness] == "" && !slices.Contains(hs, p.Harness) {
				hs = append(hs, p.Harness)
			}
		}
	}
	if len(hs) == 0 {
		return nil, nil
	}
	rec, err := install.Read(r.Install)
	if err != nil {
		return nil, err
	}
	var out []harness.Result
	for _, h := range hs {
		p, err := adapter.Load(h)
		if err != nil {
			return nil, err
		}
		res := harness.Result{Harness: h, CLIVersion: versions[h], ProfileSHA256: p.SHA256,
			EntrySHA256: rec.Entries[h].SHA256, RunID: runID(started), Result: resultPending, At: started}
		if err := harness.WriteConformance(r.Conformance, res); err != nil {
			return nil, err
		}
		out = append(out, res)
	}
	return out, nil
}

// endConformance writes the result of each pending record after a run that was not
// interrupted. A run that stopped at a failed case gives no pass: its result is inconclusive,
// or fail when a delivery probe failed.
func (r *Runner) endConformance(pending []harness.Result, cases []Case, rs []store.EvalResult, failed bool, finished time.Time) error {
	for _, res := range pending {
		res.Result, res.At = conformance(cases, rs, res.Harness), finished
		if failed && res.Result == harness.Pass {
			res.Result = resultInconclusive
		}
		fmt.Fprintf(r.Stdout, "conformance %s %s: %s\n", res.Harness, res.CLIVersion, res.Result)
		if err := harness.WriteConformance(r.Conformance, res); err != nil {
			return err
		}
	}
	return nil
}

// conformance is the conformance result of harness h from the delivery results of a completed
// run: pass when each delivery probe passed; fail when one failed; else inconclusive, for
// example an UNSURE or a skipped probe. A Codex pass also needs a pass of a full-size brief
// with the marker in its middle: it shows that Codex gave the model the whole brief inline,
// not a preview (E8-S2).
func conformance(cases []Case, rs []store.EvalResult, h string) string {
	full := map[string]bool{}
	for _, c := range cases {
		full[c.ID] = c.FullBrief
	}
	result, inline := harness.Pass, false
	vs := verdicts(rs)
	for i, res := range rs {
		if res.Stage != "delivery" || res.Control || res.Harness != h {
			continue
		}
		switch vs[i] {
		case verdictFail:
			return resultFail
		case verdictPass:
			inline = inline || full[res.Case]
		default:
			result = resultInconclusive
		}
	}
	if h == "codex" && result == harness.Pass && !inline {
		return resultInconclusive
	}
	return result
}

// briefCheck checks that text is a full-size brief with marker in its middle third. It
// returns why not, or "". [NEW] the middle third.
func briefCheck(text, marker string) string {
	i := strings.Index(text, marker)
	switch {
	case len(text) < fullBriefMin:
		return fmt.Sprintf("the brief is %d bytes, below the full size of %d", len(text), fullBriefMin)
	case i < 0:
		return "the brief does not hold the marker"
	case i < len(text)/3 || i > 2*len(text)/3:
		return fmt.Sprintf("the marker is at byte %d of %d, not in the middle third", i, len(text))
	}
	return ""
}
