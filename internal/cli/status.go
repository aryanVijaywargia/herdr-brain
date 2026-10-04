package cli

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/aryanVijaywargia/herdr-brain/internal/adapter"
	"github.com/aryanVijaywargia/herdr-brain/internal/bindings"
	"github.com/aryanVijaywargia/herdr-brain/internal/brief"
	"github.com/aryanVijaywargia/herdr-brain/internal/extract"
	"github.com/aryanVijaywargia/herdr-brain/internal/harness"
	"github.com/aryanVijaywargia/herdr-brain/internal/herdr"
	"github.com/aryanVijaywargia/herdr-brain/internal/output"
	"github.com/aryanVijaywargia/herdr-brain/internal/paths"
	"github.com/aryanVijaywargia/herdr-brain/internal/resolver"
	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

const statusUsage = `usage: brain status`

// retestDir is the folder of the retest markers in the data home, as in the hook (S14).
const retestDir = "retest"

// StatusHerdr is the part of a herdr client that brain status uses. *herdr.Client is one.
type StatusHerdr interface {
	Panes(ctx context.Context) ([]herdr.Pane, error)
	CheckServer(ctx context.Context) (version string, untested bool, err error)
}

// adapterFiles are the files of the data home data that harness.Check reads.
func adapterFiles(data string) (harness.Files, error) {
	inst, err := paths.InstallFile()
	if err != nil {
		return harness.Files{}, err
	}
	conf, err := paths.ConformanceFile()
	if err != nil {
		return harness.Files{}, err
	}
	return harness.Files{Install: inst, Conformance: conf, Retest: filepath.Join(data, retestDir)}, nil
}

// Status runs `brain status` (E9-S2, §8.3 "Human diagnostics"): one screen of counts and short
// lines, never item text. It opens the store as every store command does, so it runs the
// repair and the deferred session-id check. In a session with no binding it explains, prints
// the setup command, and reads and writes nothing (S84).
func Status(ctx context.Context, e Env, args []string) (err error) {
	if len(args) != 0 {
		return fmt.Errorf("unexpected argument %q\n%s", args[0], statusUsage)
	}
	w := &lineWriter{w: e.Stdout}
	w.printf("data: %s\n", e.Data)
	c, err := e.Open(ctx)
	if explained, err := explainNoStore(w, e, err, "status"); explained || err != nil {
		return errors.Join(err, w.err)
	}
	defer func() { err = errors.Join(err, c.Close()) }()
	s := statusCall{caller: newCaller(e, c), w: w}
	s.run(ctx)
	return w.err
}

// explainNoStore explains a call from an unbound session or from outside herdr, when err
// says so, and prints the setup command (S84). It reads and writes nothing. cmd is the
// command name. It returns err when it is another error.
func explainNoStore(w *lineWriter, e Env, err error, cmd string) (explained bool, _ error) {
	var ns *noStoreError
	switch {
	case errors.As(err, &ns) && ns.Reason == resolver.Unbound:
		name := cmp.Or(lookupEnv(e.Environ, "HERDR_SESSION"), "default")
		w.printf("herdr session %q has no Brain binding, so Brain gives its agents nothing.\n", name)
		w.printf("To bind it, run: brain setup --session %s\n", name)
		return true, nil
	case errors.As(err, &ns) && ns.Reason == resolver.NotInHerdr:
		w.printf("not in a herdr pane: run brain %s in a pane of a herdr session\n", cmd)
		return true, nil
	}
	return false, err
}

// herdrState is what one command learns from herdr: the server version check and the one
// pane list of the session.
type herdrState struct {
	version  string
	untested bool
	// versionErr is why the version check failed: herdr is too old or does not answer.
	versionErr error
	panes      []herdr.Pane
	// panesErr is why the pane list failed; then the delivery coverage is unavailable.
	panesErr error
}

// readHerdr checks the server version of the binding's herdr session and lists its panes once.
func readHerdr(ctx context.Context, e Env, b bindings.Binding) herdrState {
	var st herdrState
	h, err := e.StatusHerdr(herdr.Target{Session: b.Session, Socket: b.Socket})
	if err != nil {
		st.versionErr, st.panesErr = err, err
		return st
	}
	hctx, cancel := context.WithTimeout(ctx, herdrDeadline)
	st.version, st.untested, st.versionErr = h.CheckServer(hctx)
	cancel()
	hctx, cancel = context.WithTimeout(ctx, herdrDeadline)
	st.panes, st.panesErr = h.Panes(hctx)
	cancel()
	return st
}

// statusCall is one brain status call.
type statusCall struct {
	caller
	w *lineWriter
	herdrState
	kinds []string // the profile kinds, in the order that status shows them
}

func (s *statusCall) run(ctx context.Context) {
	b := s.Binding
	s.w.printf("binding: herdr session %q, %s store %s\n", b.Session, b.Kind, b.Store)
	s.herdr(ctx)
	if m, err := s.Workspace.Lookup(s.WorkspaceID); err != nil {
		s.w.printf("workspace %s: %v\n", s.WorkspaceID, err)
	} else {
		s.w.printf("%s\n", m.Status(s.WorkspaceID))
	}
	ps, err := adapter.LoadAll(s.Profiles)
	if err != nil {
		s.w.fail(err)
		return
	}
	for _, p := range ps {
		s.kinds = append(s.kinds, p.Kind)
	}
	s.clis(ctx, ps)
	s.fail(s.Store.Write(ctx, func(tx *sql.Tx) error {
		if err := s.delivery(ctx, tx); err != nil {
			return err
		}
		if err := s.queue(ctx, tx); err != nil {
			return err
		}
		if err := s.extraction(ctx, tx); err != nil {
			return err
		}
		if err := s.archive(ctx, tx); err != nil {
			return err
		}
		pending, err := store.ForgetPending(ctx, tx)
		if err != nil {
			return err
		}
		s.output(pending)
		line, err := learningLine(ctx, tx, s.Binding)
		if err != nil {
			return err
		}
		s.w.printf("\n%s\n", line)
		return s.measurement(ctx, tx)
	}))
	s.tools(ctx)
	s.relinks()
	s.w.printf("\n%s\n", evalLine(s.Bindings, s.Now()))
}

// tools shows the count of active tools, and each one that may not run, with the reason: a
// contract or a tool file that changed after your approval stops the tool (§10.1, E17-S4).
func (s *statusCall) tools(ctx context.Context) {
	var ts []store.Tool
	s.fail(s.Store.Write(ctx, func(tx *sql.Tx) error {
		revs, err := store.ActiveTools(ctx, tx)
		if err != nil {
			return err
		}
		for _, r := range revs {
			t, err := store.ToolByID(ctx, tx, r.ItemID)
			if err != nil {
				return err
			}
			ts = append(ts, t)
		}
		return nil
	}))
	var stopped []string
	for _, t := range ts {
		if _, err := output.MayRun(ctx, s.Store, filepath.Join(s.Binding.Store, outDir), t.ID); err != nil {
			stopped = append(stopped, fmt.Sprintf("  stopped: tool %s (item %d): %v", t.Name, t.ID, err))
		}
	}
	if len(stopped) == 0 {
		s.w.printf("\ntools: %d active, none stopped\n", len(ts))
		return
	}
	s.w.printf("\ntools: %d active, %d stopped; brain run refuses them\n", len(ts), len(stopped))
	for _, l := range stopped {
		s.w.printf("%s\n", l)
	}
}

// evalSession is the herdr session of the eval runner, whose test store holds the results of
// the last eval run (HLD §12.1).
const evalSession = "brain-eval"

// evalLabels are the answer labels in the order that the eval line counts them.
var evalLabels = []string{"PASS", "STALE", "INVENTED", "MISS", "UNSURE"}

// evalLine is the one line of the last eval run (E10-S2): its age and the count of each label,
// of the skipped probes and of the control passes. It only reads.
func evalLine(bindingsFile string, now time.Time) string {
	kept, _, err := bindings.Read(bindingsFile)
	if err != nil {
		return fmt.Sprintf("evals: %v", err)
	}
	i := slices.IndexFunc(kept, func(b bindings.Binding) bool { return b.Session == evalSession && b.Kind == bindings.KindTest })
	if i < 0 {
		return "evals: no run yet"
	}
	run, err := store.ReadEvalRun(paths.EvalResults(kept[i].Store))
	if errors.Is(err, fs.ErrNotExist) {
		return "evals: no run yet"
	}
	if err != nil {
		return fmt.Sprintf("evals: %v", err)
	}
	counts := map[string]int{}
	for _, r := range run.Results {
		switch {
		case r.Label != "":
			counts[r.Label]++
		case r.Status == "skipped":
			counts["skipped"]++
		default:
			counts["no label"]++
		}
		if r.ControlPass {
			counts["control_pass"]++
		}
	}
	var parts []string
	for _, k := range slices.Concat(evalLabels, []string{"no label", "skipped", "control_pass"}) {
		if counts[k] > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", counts[k], k))
		}
	}
	return fmt.Sprintf("evals: last run %s ago, %d results: %s", age(now.Sub(run.Finished)), len(run.Results), strings.Join(parts, ", "))
}

func (s *statusCall) fail(err error) {
	if err != nil {
		s.w.fail(err)
	}
}

// herdr prints the herdr version with the S54 warning, and lists the panes of the session once.
func (s *statusCall) herdr(ctx context.Context) {
	s.herdrState = readHerdr(ctx, s.Env, s.Binding)
	switch {
	case s.versionErr != nil:
		s.w.printf("herdr: %v\n", s.versionErr)
	case s.untested:
		s.w.printf("herdr %s: not tested with Brain yet; Brain keeps working\n", s.version)
	default:
		s.w.printf("herdr %s\n", s.version)
	}
}

// fixedPanes gives harness.Check the one pane list of the call.
type fixedPanes []herdr.Pane

func (f fixedPanes) Panes(context.Context) ([]herdr.Pane, error) { return f, nil }

// clis prints the adapter state of each profile, and "no start-time delivery" for each live
// agent kind with no profile (D3).
func (s *statusCall) clis(ctx context.Context, ps []*adapter.Profile) {
	s.w.printf("\nagent CLIs:\n")
	sts, err := harness.Check(ctx, fixedPanes(s.panes), s.Adapters, ps)
	if err != nil {
		s.w.printf("  unavailable: %v\n", err)
		return
	}
	for _, st := range sts {
		version := st.CLIVersion
		if st.VersionErr != nil {
			version = "(version unknown)"
		}
		word := string(st.State())
		switch st.State() {
		case harness.Configured:
			word = "configured, unverified"
		case harness.Detected:
			word = "detected, no Brain hook"
		case harness.None:
			// With no pane list, herdr may run an agent of the kind (#136).
			if s.panesErr != nil {
				word = "unknown"
			}
		}
		s.w.printf("  %s %s: %s\n", st.Kind, version, word)
	}
	others := map[string]int{}
	for _, p := range s.panes {
		if p.Agent != nil && !slices.Contains(s.kinds, *p.Agent) {
			others[*p.Agent]++
		}
	}
	for _, kind := range slices.Sorted(maps.Keys(others)) {
		s.w.printf("  %s: no start-time delivery; live panes: %d\n", kind, others[kind])
	}
	if s.panesErr != nil {
		s.w.printf("  (herdr pane list failed, so live agents are not counted)\n")
	}
}

// delivery prints the delivery gaps of each profile: each live agent joins its newest receipt
// by binding, harness and agent session id. Then the withheld history, apart.
func (s *statusCall) delivery(ctx context.Context, tx *sql.Tx) error {
	s.w.printf("\ndelivery to live agents:\n")
	if s.panesErr != nil {
		s.w.printf("  coverage unavailable: %v\n", s.panesErr)
	} else {
		for _, kind := range s.kinds {
			if err := s.coverage(ctx, tx, kind); err != nil {
				return err
			}
		}
	}
	s.w.printf("  sessions that ended before any Brain call are not measured in M1\n")
	s.w.printf("\nwithheld history:\n")
	for _, kind := range s.kinds {
		cs, err := store.WithheldCounts(ctx, tx, s.Binding.Session, kind)
		if err != nil {
			return err
		}
		if len(cs) == 0 {
			s.w.printf("  %s: none\n", kind)
			continue
		}
		total, parts := 0, make([]string, len(cs))
		for i, c := range cs {
			total += c.Count
			parts[i] = fmt.Sprintf("%s %d", cmp.Or(c.Reason, "no reason"), c.Count)
		}
		s.w.printf("  %s: %d (%s)\n", kind, total, strings.Join(parts, ", "))
	}
	return s.observedDelivery(ctx, tx)
}

// observedDelivery prints the delivery gaps of each profile in the sessions that capture saw
// (§12 "Delivery gaps"), with the sessions observed and the capture gaps beside them.
func (s *statusCall) observedDelivery(ctx context.Context, tx *sql.Tx) error {
	s.w.printf("\ndelivery to observed sessions:\n")
	for _, kind := range s.kinds {
		d, err := store.DeliveryGaps(ctx, tx, s.Binding.Session, kind)
		if err != nil {
			return err
		}
		if d.Sessions == 0 {
			s.w.printf("  %s: no session observed\n", kind)
			continue
		}
		s.w.printf("  %s: sessions observed %d, capture gaps %d; no receipt %d, withheld %d, no agent session id and no tied receipt %d;"+
			" outcomes: delivery gap %d, delivery unknown %d\n", kind, d.Sessions, d.Gaps, d.NoReceipt, d.Withheld, d.NoID,
			d.Gap, d.Unknown)
	}
	return nil
}

// coverage prints the counts and the gap lines of the live agents of one kind.
func (s *statusCall) coverage(ctx context.Context, tx *sql.Tx, kind string) error {
	counts := map[string]int{}
	var gaps []string
	n := 0
	for _, p := range s.panes {
		if p.Agent == nil || *p.Agent != kind {
			continue
		}
		n++
		gap, err := gap(ctx, tx, s.Binding.Session, kind, p)
		if err != nil {
			return err
		}
		word, _, _ := strings.Cut(gap, ":")
		counts[word]++
		if word != string(store.ResultEmitted) {
			gaps = append(gaps, fmt.Sprintf("    %s %s", p.ID, gap))
		}
	}
	if n == 0 {
		s.w.printf("  %s: no live agent\n", kind)
		return nil
	}
	var parts []string
	for _, word := range []string{string(store.ResultEmitted), string(store.ResultUnmapped), string(store.ResultWithheld), "unknown"} {
		if counts[word] > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", counts[word], word))
		}
	}
	s.w.printf("  %s: %d live: %s\n", kind, n, strings.Join(parts, ", "))
	for _, g := range gaps {
		s.w.printf("%s\n", g)
	}
	return nil
}

// gap is the delivery word of live pane p: the result of the newest receipt of its agent
// session, or "unknown" with the reason. The receipt of an older agent in the same pane has
// another agent session id, so it never counts.
func gap(ctx context.Context, tx *sql.Tx, session, kind string, p herdr.Pane) (string, error) {
	if p.AgentSession == nil || p.AgentSession.Value == "" {
		return "unknown: herdr shows no agent session id", nil
	}
	d, ok, err := store.LatestReceipt(ctx, tx, session, kind, p.AgentSession.Value)
	if err != nil || !ok {
		return "unknown: no receipt for its agent session", err
	}
	if d.Result == store.ResultWithheld && d.WithheldReason != nil {
		return fmt.Sprintf("%s: %s", d.Result, *d.WithheldReason), nil
	}
	return string(d.Result), nil
}

// queue prints the count and the oldest age of the pending proposals that the caller sees in
// the default brain review list: a proposal of a candidate below its gate is not counted.
func (s *statusCall) queue(ctx context.Context, tx *sql.Tx) error {
	ids, err := store.PendingProposalIDs(ctx, tx)
	if err != nil {
		return err
	}
	gates := store.DefaultGates()
	n := 0
	var oldest time.Time
	for _, id := range ids {
		p, _, _, err := s.proposal(ctx, tx, id)
		if errors.Is(err, errNotFound) {
			continue
		}
		if err != nil {
			return err
		}
		below, err := belowGate(ctx, tx, gates, p)
		if err != nil {
			return err
		}
		if below {
			continue
		}
		if n == 0 || p.Created.Before(oldest) {
			oldest = p.Created
		}
		n++
	}
	if n == 0 {
		s.w.printf("\nreview queue: empty\n")
		return nil
	}
	s.w.printf("\nreview queue: %d pending, the oldest %s old\n", n, age(s.Now().Sub(oldest)))
	return nil
}

// archiveShown is how many of the oldest archive candidates brain status names. [NEW] 3.
const archiveShown = 3

// archive prints the count of the archive candidates that the caller sees and the ids and idle
// times of the oldest ones, then one count line for each closed workspace with live items
// (§9.2, §9.4). It shows no item text and asks nothing (D7). The items of a closed workspace
// are in no pane's view: you act on them after a link.
func (s *statusCall) archive(ctx context.Context, tx *sql.Tx) error {
	all, err := store.ArchiveCandidates(ctx, tx, s.Now().Add(-store.ArchiveAge))
	if err != nil {
		return err
	}
	var oldest []string
	n := 0
	byKey := map[string]int{}
	for _, c := range all {
		if c.Closed {
			byKey[*c.WorkspaceKey]++
			continue
		}
		if !s.sees(c.WorkspaceKey) {
			continue
		}
		if n < archiveShown {
			oldest = append(oldest, fmt.Sprintf("item %d (idle %s)", c.ItemID, age(s.Now().Sub(c.Since))))
		}
		n++
	}
	if n == 0 {
		s.w.printf("\narchive: no candidates\n")
	} else {
		s.w.printf("\narchive: %d candidates with no relevant session for 90 days, the oldest %s; list them: brain archive\n",
			n, strings.Join(oldest, ", "))
	}
	closed, err := store.ClosedWorkspaces(ctx, tx)
	if err != nil {
		return err
	}
	for _, w := range closed {
		s.w.printf("  closed workspace %s (%s): %d items, %d archive candidates; act on them after a relink\n",
			w.Key, w.Label, w.Items, byKey[w.Key])
	}
	return nil
}

// extraction prints the extractor and its switches, the use of the daily caps, the work of
// this herdr session that waits with its oldest age and why by workspace key (§7.3, S15), and
// the batches that failed or gave an invalid response (S70). Their watermark stays, so their
// evidence waits for the next pass.
func (s *statusCall) extraction(ctx context.Context, tx *sql.Tx) error {
	if err := s.extractor(ctx, tx); err != nil {
		return err
	}
	p := extract.Pass{HerdrSession: s.Binding.Session, DefaultOn: learningDefault(s.Binding), Now: s.Now}
	d, err := p.Day(ctx, tx)
	if err != nil {
		return err
	}
	s.w.printf("  last 24h: %d of %d extractor calls, %d of %d new proposals\n", d.Calls, d.Caps.Calls,
		d.Proposals, d.Caps.Proposals)
	ws, err := p.Waiting(ctx, tx)
	if err != nil {
		return err
	}
	if len(ws) == 0 {
		s.w.printf("  waiting: none\n")
	}
	for _, w := range ws {
		s.w.printf("  waiting in workspace %s: %d batches, the oldest %s old; %s\n", w.WorkspaceKey, w.Batches,
			age(s.Now().Sub(w.Oldest)), cmp.Or(w.Why, "the next pass takes them"))
	}
	bs, err := store.FailedBatches(ctx, tx)
	if err != nil {
		return err
	}
	if len(bs) == 0 {
		s.w.printf("  failed batches: none\n")
		return nil
	}
	s.w.printf("  failed batches: %d; their evidence waits for the next pass\n", len(bs))
	for _, b := range bs {
		s.w.printf("    batch %d, session %d, workspace %s: %s, %s, %s ago\n", b.ID, b.SessionID, b.WorkspaceKey,
			b.Status, b.Reason, age(s.Now().Sub(b.At)))
	}
	return nil
}

// extractor prints the extraction line: the chosen CLI and the switches. Extraction off is
// apart from learning off: capture and the rules keep running (D9).
func (s *statusCall) extractor(ctx context.Context, tx *sql.Tx) error {
	x, err := store.Extractor(ctx, tx)
	if err != nil {
		return err
	}
	offs, err := store.ExtractionOffKeys(ctx, tx)
	if err != nil {
		return err
	}
	switch {
	case x.CLI == "":
		s.w.printf("extraction: no extractor CLI is chosen, so Brain makes no extractor call; run brain setup\n")
	case !x.On:
		s.w.printf("extraction: off for the whole store; capture and rules keep running\n")
	case len(offs) > 0:
		s.w.printf("extraction: on; off for workspace %s; capture and rules keep running\n", strings.Join(offs, ", "))
	default:
		s.w.printf("extraction: on\n")
	}
	if x.CLI != "" {
		s.w.printf("  extractor: %s, %s\n", x.CLI, cmp.Or(x.Model, "its default model"))
	}
	return nil
}

// measurementWeeks is the count of weeks that brain status shows, the current one first. [NEW]
const measurementWeeks = 4

// maxExamples is the count of example observations for each item revision. [NEW]
const maxExamples = 3

// measurement prints the M2 counts (§12): raw counts with no score (R6). Each line of counts
// sits under the sessions observed and the capture gaps of its span. Brain shows no week
// percentage and no target. The backlog, the switches, the delivery gaps and the block sizes
// are on their own lines above.
func (s *statusCall) measurement(ctx context.Context, tx *sql.Tx) error {
	ws, err := store.Weeks(ctx, tx, weekStarts(s.Now(), measurementWeeks))
	if err != nil {
		return err
	}
	s.w.printf("\nmeasurement (raw counts, no score; a week starts on Monday 00:00 UTC):\n")
	for _, w := range ws {
		s.week(w)
	}
	c, err := store.Observed(ctx, tx)
	if err != nil {
		return err
	}
	cs, err := store.RevisionOutcomes(ctx, tx)
	if err != nil {
		return err
	}
	s.w.printf("  all weeks: sessions observed %d, capture gaps %d\n", c.Sessions, c.Gaps)
	for _, kind := range []store.OutcomeKind{store.OutcomeComeback, store.OutcomeUncertain} {
		var lines []string
		for _, o := range cs {
			if o.Kind == kind {
				lines = append(lines, fmt.Sprintf("item %d revision %d: %d (%s)", o.ItemID, o.Revision,
					len(o.Observations), examples(o.Observations)))
			}
		}
		word := "comebacks"
		if kind == store.OutcomeUncertain {
			word = "uncertain matches"
		}
		if len(lines) == 0 {
			s.w.printf("    %s: none\n", word)
			continue
		}
		s.w.printf("    %s:\n", word)
		for _, l := range lines {
			s.w.printf("      %s\n", l)
		}
	}
	return nil
}

// week prints the counts of one week: the repeats, the review effort and results, and the
// live suggestions. A week with no count at all is one line.
func (s *statusCall) week(w store.Week) {
	decided := 0
	for _, n := range w.Decided {
		decided += n
	}
	head := fmt.Sprintf("  week of %s: sessions observed %d, capture gaps %d", w.Start.Format(time.DateOnly), w.Sessions, w.Gaps)
	if w.Sessions+w.Gaps+w.Corrections+w.Proposals+decided+w.Shown == 0 {
		s.w.printf("%s; nothing counted\n", head)
		return
	}
	s.w.printf("%s\n", head)
	s.w.printf("    correction candidates %d\n", w.Corrections)
	effort := "no timed review"
	if n := len(w.ReviewSeconds); n > 0 {
		effort = fmt.Sprintf("median review %ds of %d timed", median(w.ReviewSeconds), n)
	}
	s.w.printf("    proposals made %d; decided %d: approved %d, edited %d, rejected %d, changed %d; %s\n", w.Proposals,
		decided, w.Decided[store.ReviewApproved], w.Decided[store.ReviewEdited], w.Decided[store.ReviewRejected],
		w.Decided[store.ReviewChanged], effort)
	s.w.printf("    live suggestions shown %d: approved %d, rejected %d, changed %d, later %d\n", w.Shown,
		w.Suggested[store.SuggestionApproved], w.Suggested[store.SuggestionRejected],
		w.Suggested[store.SuggestionChanged], w.Suggested[store.SuggestionLater])
}

// weekStarts returns the starts of the n weeks up to now, the newest first: Monday 00:00 UTC.
// [NEW] the week boundary.
func weekStarts(now time.Time, n int) []time.Time {
	now = now.UTC()
	day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	monday := day.AddDate(0, 0, -(int(day.Weekday())+6)%7)
	starts := make([]time.Time, n)
	for i := range n {
		starts[i] = monday.AddDate(0, 0, -7*i)
	}
	return starts
}

// median is the median of the sorted xs: the mean of the two middle values for an even count,
// rounded down.
func median(xs []int64) int64 {
	m := len(xs) / 2
	if len(xs)%2 == 1 {
		return xs[m]
	}
	return (xs[m-1] + xs[m]) / 2
}

// examples lists the first maxExamples observation ids, the links to the examples.
func examples(ids []int64) string {
	parts := make([]string, 0, maxExamples)
	for _, id := range ids[:min(len(ids), maxExamples)] {
		parts = append(parts, fmt.Sprint(id))
	}
	word := "observations "
	if len(ids) == 1 {
		word = "observation "
	}
	more := ""
	if len(ids) > maxExamples {
		more = ", …"
	}
	return word + strings.Join(parts, ", ") + more
}

// age is a short age. [NEW] the units and their limits.
func age(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "under 1m"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d/time.Minute))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d/time.Hour))
	}
	return fmt.Sprintf("%dd", int(d/(24*time.Hour)))
}

// output prints the brief size of each part against its budget (§8.1), the output conflicts
// (S66) and a pending forget (S77).
func (s *statusCall) output(forgetPending bool) {
	out := filepath.Join(s.Binding.Store, outDir)
	b, err := output.Read(out, s.key)
	switch {
	// A missing part of a generation is damage, not a brief that was never built (#136).
	case errors.Is(err, output.ErrInconsistent):
		s.w.printf("\nbrief: %v\n", err)
	case errors.Is(err, fs.ErrNotExist):
		s.w.printf("\nbrief: not built yet\n")
	case err != nil:
		s.w.printf("\nbrief: %v\n", err)
	default:
		size, limit := brief.GlobalSize(b.Global)
		s.w.printf("\nbrief (output generation %d):\n  global: %d of %d bytes\n", b.Generation, size, limit)
		if s.key != "" {
			size, limit := brief.WorkspaceSize(b.Workspace)
			s.w.printf("  workspace %s: %d of %d bytes\n", s.key, size, limit)
		}
	}
	cs, err := output.Conflicts(out)
	switch {
	case err != nil:
		s.w.printf("output conflicts: %v\n", err)
	case len(cs) == 0:
		s.w.printf("output conflicts: none\n")
	default:
		s.w.printf("output conflicts: %d (brain repair --confirm deletes them)\n", len(cs))
		for _, c := range cs {
			s.w.printf("  %v\n", &c)
		}
	}
	if forgetPending {
		s.w.printf("forget pending: an output generation still holds forgotten text; brain repair removes it\n")
	} else {
		s.w.printf("forget pending: none\n")
	}
}

// relinks lists the old workspace keys whose items wait for a relink (S83), and the command
// that links one.
func (s *statusCall) relinks() {
	rs, err := s.Workspace.Relinks()
	if err != nil {
		s.w.printf("\nrelink: %v\n", err)
		return
	}
	if len(rs) == 0 {
		s.w.printf("\nrelink: none\n")
		return
	}
	s.w.printf("\nrelink: old workspaces with the same root: %d\n", len(rs))
	for _, r := range rs {
		why := "same root as"
		switch {
		case r.Old.ReplacedBy != "":
			why = "replaced by"
		case r.Old.Closed:
			why = "closed; same root as"
		}
		s.w.printf("  %s (%s %s): %s %s (%s %s)\n", r.Old.Key, r.Old.WorkspaceID, r.Old.Label, why,
			r.New.Key, r.New.WorkspaceID, r.New.Label)
	}
	// [NEW] the hint: Brain never links by itself (S20); you link on request.
	s.w.printf("  link on request, from a pane in the new workspace: brain workspace link <old> <new>\n")
}

// lookupEnv returns the value of key in environ; the last entry wins, as in os/exec.
func lookupEnv(environ []string, key string) string {
	var v string
	for _, kv := range environ {
		if k, val, ok := strings.Cut(kv, "="); ok && k == key {
			v = val
		}
	}
	return v
}

// lineWriter keeps the first write error, so the screen code need not check each line.
type lineWriter struct {
	w   io.Writer
	err error
}

func (l *lineWriter) printf(format string, args ...any) {
	if l.err == nil {
		_, l.err = fmt.Fprintf(l.w, format, args...)
	}
}

func (l *lineWriter) fail(err error) {
	if l.err == nil {
		l.err = err
	}
}
