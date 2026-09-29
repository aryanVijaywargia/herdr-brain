package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/aryanVijaywargia/herdr-brain/internal/adapter"
	"github.com/aryanVijaywargia/herdr-brain/internal/bindings"
	"github.com/aryanVijaywargia/herdr-brain/internal/brief"
	"github.com/aryanVijaywargia/herdr-brain/internal/output"
	"github.com/aryanVijaywargia/herdr-brain/internal/paths"
	"github.com/aryanVijaywargia/herdr-brain/internal/store"
	"github.com/aryanVijaywargia/herdr-brain/internal/workspace"
)

// Status is the run status of one reply (HLD §12.1). It is apart from the answer label.
type Status string

// The run statuses.
const (
	Delivered    Status = "delivered"     // an emitted receipt with the expected brief hash
	NotDelivered Status = "not delivered" // no receipt, or one that is not the expected brief
	Incomplete   Status = "reply incomplete"
	Skipped      Status = "skipped" // never a pass
)

// altScreen are the CLIs that draw in the alternate screen: the runner reads their visible
// screen after idle (E11-S1). Codex draws inline.
var altScreen = map[string]bool{"claude": true}

// outDir is the out folder of a store, as the hook reads it (§7.5 step 3).
const outDir = "out"

// briefHash returns the hash of the brief that the hook of a pane in workspace wsID prints
// with the test store of b: the runner reads it after the setup steps and before the probes,
// so a change that a probe makes does not move it. It is "" when the store has no brief.
func briefHash(b bindings.Binding, wsID string) (string, error) {
	// The same key and read as the hook: a workspace part only for a mapped workspace.
	m, err := workspace.For(b).Lookup(wsID)
	if err != nil {
		return "", err
	}
	var key string
	if m.State == workspace.Mapped {
		key = m.Entry.Key
	}
	br, err := output.Read(filepath.Join(b.Store, outDir), key)
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, output.ErrInconsistent) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(brief.Text(br.Global, br.Workspace)))
	return hex.EncodeToString(sum[:]), nil
}

// receipt reads the newest receipt of pane of harness in the test store of b. ok is false
// when there is none.
func (r *Runner) receipt(ctx context.Context, b bindings.Binding, pane, harness string) (d store.Delivery, ok bool, err error) {
	if _, err := os.Stat(filepath.Join(b.Store, store.FileName)); errors.Is(err, fs.ErrNotExist) {
		return d, false, nil // no Brain call and no hook wrote to the store
	}
	s, err := store.Open(ctx, b.Store, r.Timeouts.Call)
	if err != nil {
		return d, false, err
	}
	defer s.Close() //nolint:errcheck // read-only use: the read error is the one to report
	err = s.Write(ctx, func(tx *sql.Tx) error {
		d, ok, err = store.LatestPaneReceipt(ctx, tx, b.Session, harness, pane)
		return err
	})
	return d, ok, err
}

// cliVersion runs `<harness> --version` and returns its first version word. The hook does not
// record the CLI version in its receipt, so the runner reads it itself.
func cliVersion(ctx context.Context, environ []string, harness string) (string, error) {
	cmd := exec.CommandContext(ctx, harness, "--version")
	cmd.Env = environ
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("%s --version: %w", harness, err)
	}
	for _, w := range strings.Fields(string(out)) {
		if versionWord.MatchString(w) {
			return w, nil
		}
	}
	return "", fmt.Errorf("%s --version: no version in %q", harness, out)
}

// versionWord is a version word of `<cli> --version`, as in internal/harness.
var versionWord = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)

// versions reads the CLI version of each harness of cases that the run does not skip. A CLI
// whose version the runner cannot read keeps an empty version, with the version of its
// receipt, if any, as the fallback.
func (r *Runner) versions(ctx context.Context, cases []Case, skip map[string]string) map[string]string {
	vs := map[string]string{}
	for _, c := range cases {
		for _, p := range c.Probes {
			if _, ok := vs[p.Harness]; ok || skip[p.Harness] != "" {
				continue
			}
			v, _ := call(ctx, r.Timeouts.Call, func(ctx context.Context) (string, error) {
				return r.Version(ctx, p.Harness)
			})
			vs[p.Harness] = v
		}
	}
	return vs
}

// deliveryStatus is delivered when the store has an emitted receipt of the pane with the
// expected brief hash, and not delivered with the reason otherwise.
func deliveryStatus(d store.Delivery, ok bool, want string) (Status, string) {
	switch {
	case !ok:
		return NotDelivered, "no receipt of the pane"
	case d.Result != store.ResultEmitted:
		return NotDelivered, fmt.Sprintf("the receipt is %s", d.Result)
	case want == "":
		return NotDelivered, "the store has no brief"
	case d.BriefHash == nil || *d.BriefHash != want:
		return NotDelivered, "the receipt has another brief hash"
	}
	return Delivered, ""
}

// results makes the result of each probe of cases, in order. outs holds the outcomes of the
// cases that ran, in the same order, the last one maybe partial; a probe that did not run is
// skipped. versions holds the CLI version of each harness.
func results(cases []Case, outs []Outcome, versions map[string]string) []store.EvalResult {
	var rs []store.EvalResult
	for i, c := range cases {
		var o Outcome
		if i < len(outs) {
			o = outs[i]
		}
		for j, p := range c.Probes {
			if j >= len(o.Replies) {
				rs = append(rs, base(c, p.Harness, o, Reply{Status: Skipped, Why: "the probe did not run"}, versions))
				continue
			}
			rep := o.Replies[j]
			res := base(c, rep.Harness, o, rep, versions)
			lb, rule := labelReply(c, o.Markers, rep)
			res.Label, res.Rule = string(lb), rule
			rs = append(rs, res)
		}
	}
	flagControlPass(rs)
	return rs
}

// base fills the fields of one result that do not depend on the label.
func base(c Case, harness string, o Outcome, rep Reply, versions map[string]string) store.EvalResult {
	res := store.EvalResult{
		Case: c.ID, Stage: c.Stage, Source: c.Source, Control: c.Control, Harness: harness,
		Status: string(rep.Status), Why: rep.Why, Reply: rep.Text, Markers: o.Markers,
		ExpectRevision: c.ExpectRevision, Session: o.Session, Store: o.Store, CLIVersion: versions[harness],
	}
	if p, err := adapter.Load(harness); err == nil {
		res.Profile, res.ProfileSHA256 = harness, p.SHA256
	}
	if d := rep.Receipt; d != nil {
		if res.CLIVersion == "" {
			res.CLIVersion = deref(d.CLIVersion)
		}
		res.BriefHash = deref(d.BriefHash)
		if d.OutputGeneration != nil {
			res.Generation = *d.OutputGeneration
		}
		res.ItemRevisions = d.ItemRevisions
	}
	return res
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// labelReply labels one reply. A skipped reply gets no label and a reply that the runner
// could not see whole is UNSURE. A reply without its brief gets no label, as it cannot show
// what Brain did: only a control, which runs with no seed, and an exclusion case, whose brief
// must leave the fact out, get one then. An ad-hoc case has no check and gets no label.
func labelReply(c Case, markers map[string]string, rep Reply) (Label, string) {
	switch {
	case rep.Status == Skipped:
		return "", ""
	case rep.Status == Incomplete:
		return Unsure, "reply incomplete"
	case rep.Status == NotDelivered && !c.Control && c.Stage != "exclusion":
		return "", ""
	case len(c.Check.Expect) == 0 && len(c.Check.Stale) == 0 && !c.Check.Decline:
		return "", ""
	}
	return label(rep.Text, Check{
		Expect: fillAll(c.Check.Expect, markers), Stale: fillAll(c.Check.Stale, markers), Decline: c.Check.Decline,
	})
}

// flagControlPass flags each meaning result whose control passed with the same CLI (§12.1): a
// fact that the CLI finds without Brain does not show that Brain delivered it. The result
// stays. A restraint case must pass in its control, so it gets no flag; when its labeled
// control does not pass, the result says so.
func flagControlPass(rs []store.EvalResult) {
	ctl := map[[2]string]string{}
	for _, r := range rs {
		if r.Control {
			ctl[[2]string{r.Case, r.Harness}] = r.Label
		}
	}
	for i, r := range rs {
		lb := ctl[[2]string{r.Case, r.Harness}]
		switch {
		case r.Control:
		case r.Stage != "restraint":
			rs[i].ControlPass = lb == string(Pass)
		case lb != "" && lb != string(Pass):
			rs[i].Why = strings.TrimPrefix(rs[i].Why+"; ", "; ") + fmt.Sprintf("its control is %s, and a restraint case must pass there", lb)
		}
	}
}

// writeResults writes the results of a run to the results file of the test store.
func writeResults(storeDir string, started, finished time.Time, rs []store.EvalResult) error {
	return store.WriteEvalRun(paths.EvalResults(storeDir), store.EvalRun{Started: started, Finished: finished, Results: rs})
}
