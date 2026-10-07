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

// outDir is the out folder of a store, as the hook reads it (§7.5 step 3).
const outDir = "out"

// expected is the brief that the hook of a pane in a case workspace prints with the test store,
// before the hook adds the line of the last session (E15-S4). The runner reads it after the
// setup steps and before the probes, so a change that a probe makes does not move it.
type expected struct {
	found             bool   // false when the store has no brief
	key               string // the workspace key of the brief; "" when the workspace is not mapped
	global, workspace string
}

// expectedBrief reads the brief that the hook of a pane in workspace wsID prints with the test
// store of b.
func expectedBrief(b bindings.Binding, wsID string) (expected, error) {
	// The same key and read as the hook: a workspace part only for a mapped workspace.
	m, err := workspace.For(b).Lookup(wsID)
	if err != nil {
		return expected{}, err
	}
	var e expected
	if m.State == workspace.Mapped {
		e.key = m.Entry.Key
	}
	br, err := output.Read(filepath.Join(b.Store, outDir), e.key)
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, output.ErrInconsistent) {
		return expected{}, nil
	}
	if err != nil {
		return expected{}, err
	}
	e.found, e.global, e.workspace = true, br.Global, br.Workspace
	return e, nil
}

// want returns the hash of the brief that the hook printed with receipt d: brief exp, and for a
// mapped workspace the line of the last session in it before the agent session of d (E15-S4).
// An earlier session in the case workspace adds that line: a setup agent, an earlier probe, or a
// CLI whose folder-trust dialog the runner answered and that ran its start hook before its close
// (#296). The other CLIs of the case are idle or gone when the runner reads it, so the store
// holds the last session that the hook read. It is "" when the store has no brief.
func (r *Runner) want(ctx context.Context, b bindings.Binding, exp expected, d store.Delivery) (string, error) {
	if !exp.found {
		return "", nil
	}
	ws := exp.workspace
	if exp.key != "" {
		last, ok, err := r.lastSession(ctx, b, exp.key, deref(d.AgentSessionID))
		if err != nil {
			return "", err
		}
		if ok {
			// The hook and the runner run on one machine, so the hook clock has the zone of
			// the runner clock.
			ws = brief.LastSession(ws, last.Profile, last.At.In(r.Now().Location()), *last.TranscriptPath)
		}
	}
	sum := sha256.Sum256([]byte(brief.Text(exp.global, ws)))
	return hex.EncodeToString(sum[:]), nil
}

// lastSession reads the last session in workspace key of the test store of b before agent
// session agentSession, as the hook reads it.
func (r *Runner) lastSession(ctx context.Context, b bindings.Binding, key, agentSession string) (d store.Delivery, ok bool, err error) {
	s, err := store.Open(ctx, b.Store, r.Timeouts.Call)
	if err != nil {
		return d, false, err
	}
	defer s.Close() //nolint:errcheck // read-only use: the read error is the one to report
	err = s.Write(ctx, func(tx *sql.Tx) error {
		d, ok, err = store.LastSession(ctx, tx, key, agentSession)
		return err
	})
	return d, ok, err
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

// probeCLI is what the results of a run record of the CLI of one harness.
type probeCLI struct {
	Version       string // "" when the runner did not read it
	ProfileSHA256 string // the hash of the harness profile
}

// clis reads the profile of each harness of cases, and the CLI version of each that the run
// does not skip. A CLI whose version the runner cannot read keeps an empty version, with the
// version of its receipt, if any, as the fallback.
func (r *Runner) clis(ctx context.Context, cases []Case, skip map[string]string) (map[string]probeCLI, error) {
	cs := map[string]probeCLI{}
	for _, c := range cases {
		for _, p := range c.Probes {
			if _, ok := cs[p.Harness]; ok {
				continue
			}
			prof, err := adapter.LoadFS(r.Profiles, p.Harness)
			if err != nil {
				return nil, err
			}
			cli := probeCLI{ProfileSHA256: prof.SHA256}
			if skip[p.Harness] == "" {
				cli.Version, _ = call(ctx, r.Timeouts.Call, func(ctx context.Context) (string, error) {
					return r.Version(ctx, p.Harness)
				})
			}
			cs[p.Harness] = cli
		}
	}
	return cs, nil
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

// results makes the result of each probe of cases, in order; a skill choice probe gets one for
// each fixture skill. outs holds the outcomes of the cases that ran, in the same order, the
// last one maybe partial; a probe that did not run is skipped. clis holds the CLI version and
// the profile hash of each harness.
func results(cases []Case, outs []Outcome, clis map[string]probeCLI) []store.EvalResult {
	var rs []store.EvalResult
	seeded := map[string]map[string]string{} // a skill choice case id to the markers of its run with skills
	for i, c := range cases {
		var o Outcome
		if i < len(outs) {
			o = outs[i]
		}
		if c.Stage == skillStage && !c.Control {
			seeded[c.ID] = o.Markers
		}
		for j, p := range c.Probes {
			rep := Reply{Harness: p.Harness, Status: Skipped, Why: "the probe did not run"}
			if j < len(o.Replies) {
				rep = o.Replies[j]
			}
			switch {
			case c.Stage == skillStage:
				rs = append(rs, skillResults(c, o, seeded[c.ID], rep, clis)...)
				continue
			case j >= len(o.Replies):
				rs = append(rs, base(c, p.Harness, o, rep, clis))
				continue
			}
			res := base(c, rep.Harness, o, rep, clis)
			lb, rule := labelReply(c, o.Markers, rep)
			// A pass of a full-brief case whose brief is not full size, or of a forget case
			// whose store still holds the forgotten text, does not show what the stage tests.
			switch {
			case lb != Pass:
			case o.BriefCheck != "":
				lb, rule = Unsure, o.BriefCheck
			case o.Leak != "":
				lb, rule = Stale, o.Leak
			}
			res.Label, res.Rule = string(lb), rule
			if rep.Record != "" {
				res.Why = strings.TrimPrefix(res.Why+"; ", "; ") + "record: " + rep.Record
			}
			rs = append(rs, res)
		}
	}
	flagControlPass(rs)
	return rs
}

// base fills the fields of one result that do not depend on the label.
func base(c Case, harness string, o Outcome, rep Reply, clis map[string]probeCLI) store.EvalResult {
	res := store.EvalResult{
		Case: c.ID, Stage: c.Stage, Source: c.Source, Control: c.Control, Harness: harness,
		Status: string(rep.Status), Why: rep.Why, Reply: rep.Text, Markers: o.Markers,
		ExpectRevision: c.ExpectRevision, Session: o.Session, Store: o.Store, CLIVersion: clis[harness].Version,
	}
	if cli, ok := clis[harness]; ok {
		res.Profile, res.ProfileSHA256 = harness, cli.ProfileSHA256
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
// must leave the fact out, get one then. A hook case is labeled by what the hook printed. An
// ad-hoc case has no check and gets no label.
func labelReply(c Case, markers map[string]string, rep Reply) (Label, string) {
	switch {
	case rep.Status == Skipped:
		return "", ""
	case rep.Status == Incomplete:
		return Unsure, "reply incomplete"
	case c.Hook != "":
		return hookLabel(c, markers, rep.Text)
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
// control does not pass, the result says so. A skill choice result gets no flag: its control
// has its own verdict.
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
		case r.Control || r.Stage == skillStage:
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
