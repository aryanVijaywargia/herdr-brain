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
	"github.com/aryanVijaywargia/herdr-brain/internal/brief"
	"github.com/aryanVijaywargia/herdr-brain/internal/harness"
	"github.com/aryanVijaywargia/herdr-brain/internal/herdr"
	"github.com/aryanVijaywargia/herdr-brain/internal/output"
	"github.com/aryanVijaywargia/herdr-brain/internal/paths"
	"github.com/aryanVijaywargia/herdr-brain/internal/resolver"
	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

const statusUsage = `usage: brain status`

// profiles are the harnesses with a Brain profile, in the order that status shows them.
var profiles = []string{"claude", "codex"}

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
	var ns *noStoreError
	switch {
	case errors.As(err, &ns) && ns.Reason == resolver.Unbound:
		name := cmp.Or(lookupEnv(e.Environ, "HERDR_SESSION"), "default")
		w.printf("herdr session %q has no Brain binding, so Brain gives its agents nothing.\n", name)
		w.printf("To bind it, run: brain setup --session %s\n", name)
		return w.err
	case errors.As(err, &ns) && ns.Reason == resolver.NotInHerdr:
		w.printf("not in a herdr pane: run brain status in a pane of a herdr session\n")
		return w.err
	case err != nil:
		return err
	}
	defer func() { err = errors.Join(err, c.Close()) }()
	s := statusCall{caller: newCaller(e, c), w: w}
	s.run(ctx)
	return w.err
}

// statusCall is one brain status call.
type statusCall struct {
	caller
	w     *lineWriter
	panes []herdr.Pane
	// panesErr is why the one pane list failed; then the delivery coverage is unavailable.
	panesErr error
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
	ps, err := loadProfiles()
	if err != nil {
		s.w.fail(err)
		return
	}
	s.clis(ctx, ps)
	s.fail(s.Store.Write(ctx, func(tx *sql.Tx) error {
		if err := s.delivery(ctx, tx); err != nil {
			return err
		}
		if err := s.queue(ctx, tx); err != nil {
			return err
		}
		pending, err := store.ForgetPending(ctx, tx)
		if err != nil {
			return err
		}
		s.output(pending)
		return nil
	}))
	s.relinks()
}

func (s *statusCall) fail(err error) {
	if err != nil {
		s.w.fail(err)
	}
}

// herdr prints the herdr version with the S54 warning, and lists the panes of the session once.
func (s *statusCall) herdr(ctx context.Context) {
	h, err := s.StatusHerdr(herdr.Target{Session: s.Binding.Session, Socket: s.Binding.Socket})
	if err != nil {
		s.w.printf("herdr: %v\n", err)
		s.panesErr = err
		return
	}
	hctx, cancel := context.WithTimeout(ctx, herdrDeadline)
	v, untested, err := h.CheckServer(hctx)
	cancel()
	switch {
	case err != nil:
		s.w.printf("herdr: %v\n", err)
	case untested:
		s.w.printf("herdr %s: not tested with Brain yet; Brain keeps working\n", v)
	default:
		s.w.printf("herdr %s\n", v)
	}
	hctx, cancel = context.WithTimeout(ctx, herdrDeadline)
	s.panes, s.panesErr = h.Panes(hctx)
	cancel()
}

func loadProfiles() ([]*adapter.Profile, error) {
	ps := make([]*adapter.Profile, len(profiles))
	for i, name := range profiles {
		p, err := adapter.Load(name)
		if err != nil {
			return nil, err
		}
		ps[i] = p
	}
	return ps, nil
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
		}
		s.w.printf("  %s %s: %s\n", st.Kind, version, word)
	}
	others := map[string]int{}
	for _, p := range s.panes {
		if p.Agent != nil && !slices.Contains(profiles, *p.Agent) {
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
		for _, kind := range profiles {
			if err := s.coverage(ctx, tx, kind); err != nil {
				return err
			}
		}
	}
	s.w.printf("  sessions that ended before any Brain call are not measured in M1\n")
	s.w.printf("\nwithheld history:\n")
	for _, kind := range profiles {
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
		gap, err := s.gap(ctx, tx, kind, p)
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
func (s *statusCall) gap(ctx context.Context, tx *sql.Tx, kind string, p herdr.Pane) (string, error) {
	if p.AgentSession == nil || p.AgentSession.Value == "" {
		return "unknown: herdr shows no agent session id", nil
	}
	d, ok, err := store.LatestReceipt(ctx, tx, s.Binding.Session, kind, p.AgentSession.Value)
	if err != nil || !ok {
		return "unknown: no receipt for its agent session", err
	}
	if d.Result == store.ResultWithheld && d.WithheldReason != nil {
		return fmt.Sprintf("%s: %s", d.Result, *d.WithheldReason), nil
	}
	return string(d.Result), nil
}

// queue prints the count and the oldest age of the pending proposals that the caller sees.
func (s *statusCall) queue(ctx context.Context, tx *sql.Tx) error {
	ids, err := store.PendingProposalIDs(ctx, tx)
	if err != nil {
		return err
	}
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

// relinks lists the old workspace keys whose items wait for a relink (S83).
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
	s.w.printf("\nrelink: %d old workspaces with the same root\n", len(rs))
	for _, r := range rs {
		why := "replaced by"
		if r.Old.Closed {
			why = "closed; same root as"
		}
		s.w.printf("  %s (%s %s): %s %s (%s %s)\n", r.Old.Key, r.Old.WorkspaceID, r.Old.Label, why,
			r.New.Key, r.New.WorkspaceID, r.New.Label)
	}
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
