// Package hook is `brain hook`. At the start of an agent session in a live pane of a bound
// herdr session, it prints the brief of the current output generation in the output form of
// the harness profile, and records a receipt. It prefers a missed delivery to a wrong one, and
// it never fails a session start: on any error it prints nothing. At a prompt, a shell command
// or a stop it stores one hook record, and prints nothing (E11-S5).
package hook

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/aryanVijaywargia/herdr-brain/internal/adapter"
	"github.com/aryanVijaywargia/herdr-brain/internal/brained"
	"github.com/aryanVijaywargia/herdr-brain/internal/brief"
	"github.com/aryanVijaywargia/herdr-brain/internal/harness"
	"github.com/aryanVijaywargia/herdr-brain/internal/herdr"
	"github.com/aryanVijaywargia/herdr-brain/internal/output"
	"github.com/aryanVijaywargia/herdr-brain/internal/paths"
	"github.com/aryanVijaywargia/herdr-brain/internal/resolver"
	"github.com/aryanVijaywargia/herdr-brain/internal/store"
	"github.com/aryanVijaywargia/herdr-brain/internal/workspace"
)

// The time limits of the hook. The cap covers the whole hook from its start: the stdin read,
// the lookup, the file reads, the receipt and the print. The lookup ends by lookupDeadline,
// plus the 50 ms that the herdr client waits for herdr's pipes; a receipt waits at most
// busyTimeout for the store lock. That is 250 ms, so at least 50 ms of the cap is left for the
// file reads and the print, which took under 1 ms in the E7-S4 test. The agent check of earlier
// records adds one read before the print, which takes no lock; its updates join the receipt
// transaction, so the hook still waits for the store lock once.
const (
	// hookCap is the total cap of one hook run.
	hookCap = 300 * time.Millisecond
	// lookupDeadline is the herdr lookup deadline, counted from the hook start.
	lookupDeadline = 150 * time.Millisecond
	// busyTimeout is how long a receipt write waits for the lock of another Brain process.
	busyTimeout = 50 * time.Millisecond
	// outDir is the out folder of a store.
	outDir = "out"
	// retestDir is the folder of the retest markers in the data home.
	retestDir = "retest"
)

// The reasons of the withheld receipts that the hook writes itself. The resolver gives the
// reasons of a failed live check.
const (
	reasonIdentity     = "identity"
	reasonInconsistent = "inconsistent"
	reasonForget       = "forget pending"
)

// errCapped: the hook cap stopped the hook. It printed nothing and wrote nothing.
var errCapped = errors.New("hook cap reached")

// Main runs the hook for the frozen command `brain hook --profile <name> --event <event>` with
// the process environment environ. A launched session's hooks add `--launch <launch folder>`:
// its prompt, tool and stop hooks (E24-S5) and its start hook (E24-S4). The caller prints
// nothing on the error and exits 0.
func Main(args, environ []string, stdin io.Reader, stdout io.Writer) error {
	start := time.Now()
	name, event, dir, err := profileArg(args)
	if err != nil {
		return err
	}
	p, err := adapter.Load(name)
	if err != nil {
		return err
	}
	file, err := paths.BindingsFile()
	if err != nil {
		return err
	}
	data, err := paths.DataHome()
	if err != nil {
		return err
	}
	h := Hook{Profile: p, Event: event, Launch: dir, Environ: environ, Bindings: file, Retest: filepath.Join(data, retestDir),
		Pane: resolver.HerdrPane(environ), Dispatch: brained.Start, Start: start, Now: time.Now}
	return h.Run(context.Background(), stdin, stdout)
}

// profileArg reads the profile name, the event and the launch folder ("" for none) from the
// frozen arguments; any other form is an error.
func profileArg(args []string) (name, event, dir string, err error) {
	if len(args) == 6 && args[4] == "--launch" && args[5] != "" {
		dir, args = args[5], args[:4]
	}
	if len(args) != 4 || args[0] != "--profile" || args[1] == "" || args[2] != "--event" ||
		!slices.Contains([]string{adapter.EventStart, adapter.EventPrompt, adapter.EventTool, adapter.EventStop}, args[3]) {
		return "", "", "", fmt.Errorf("want --profile <name> --event start|prompt|tool|stop [--launch <folder>], got %q", args)
	}
	return args[1], args[3], dir, nil
}

// Hook is one run of `brain hook`.
type Hook struct {
	Profile *adapter.Profile
	Event   string // the Brain event; "" is start
	// Launch is the launch folder of a launched session (E24-S5); "" for none. A hook with a
	// launch folder uses the pane of the launch record, not the environment.
	Launch   string
	Environ  []string
	Bindings string // the bindings file
	Retest   string // the folder of the retest markers (harness.MarkRetest)
	Pane     resolver.PaneFunc
	// Dispatch starts the detached process of a suggestion that a prompt record reserved (E14-S3).
	Dispatch func(ctx context.Context, environ []string, d brained.Dispatch) error
	Start    time.Time // the hook start; the lookup deadline counts from it
	Now      func() time.Time

	ended func() // tests only: called when the capped run ends, after the cap too
	noted func() // tests only: called when a hook record has noted the capture generation
}

// Run reads the hook input from stdin and prints the brief to stdout, or nothing. A result
// that withholds the brief is not an error. On an error Run has printed nothing, unless the
// receipt write after the print failed: then the delivery is unknown. For a profile with
// output_none, "nothing" is that output (E16-S11), also on an error.
//
// Run returns by hookCap after Start, also when stdin, herdr or stdout stall. The caller
// exits then. A run that the cap stops prints nothing after the cap and writes no receipt,
// so status shows the session as unknown.
func (h Hook) Run(ctx context.Context, stdin io.Reader, stdout io.Writer) error {
	ctx, cancel := context.WithDeadline(ctx, h.Start.Add(hookCap))
	defer cancel()
	out := &gate{w: stdout}
	done := make(chan error, 1)
	go func() {
		err := h.inHerdr(ctx, stdin, out)
		// The output for "nothing" is under the cap too.
		if !out.used() {
			err = errors.Join(err, h.Profile.WriteNone(out))
		}
		if h.ended != nil {
			h.ended()
		}
		done <- err
	}()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		// The canceled ctx rolls back an open receipt transaction.
		out.close()
		return fmt.Errorf("%w: %w", errCapped, ctx.Err())
	}
}

// inHerdr is run for a hook inside herdr. Outside herdr the hook reads no file and asks no
// herdr. A launched session is in herdr by its launch record: its environment is only a hint.
func (h Hook) inHerdr(ctx context.Context, stdin io.Reader, stdout io.Writer) error {
	if h.Launch == "" && lookup(h.Environ, "HERDR_ENV") != "1" {
		return nil
	}
	return h.run(ctx, stdin, stdout)
}

// run is Run without the cap.
func (h Hook) run(ctx context.Context, stdin io.Reader, stdout io.Writer) (err error) {
	if h.Event != "" && h.Event != adapter.EventStart {
		return h.record(ctx, stdin)
	}
	raw, err := io.ReadAll(stdin)
	if err != nil {
		return fmt.Errorf("read hook input: %w", err)
	}
	in, err := h.Profile.Parse(raw)
	if errors.Is(err, adapter.ErrNotStart) {
		return nil
	}
	// Input of an unknown shape sets the adapter state to retest. The marker is a file outside
	// every store, so the run still writes the store at most once.
	if errors.Is(err, adapter.ErrRetest) {
		return errors.Join(err, harness.MarkRetest(h.Retest, h.Profile.Kind, h.Profile.SHA256, h.Now()))
	}
	if err != nil {
		return err
	}
	if h.Launch != "" {
		return h.launched(ctx, in, stdout)
	}
	open := func(ctx context.Context, dir string) (resolver.Store, error) {
		s, err := store.Open(ctx, dir, busyTimeout)
		if err != nil {
			return nil, err
		}
		return &receipts{s: s, profile: h.Profile.Kind, agentSession: in.SessionID, transcript: in.TranscriptPath,
			now: h.Now}, nil
	}
	// The resolver checks the earlier records of the pane and records this agent session; the
	// receipt of this run holds the record.
	res, err := resolver.New(h.Bindings, open, h.pane).Resolve(ctx, h.Environ, in.SessionID)
	if err != nil {
		return err
	}
	switch r := res.(type) {
	case *resolver.NoStore:
		// No store is open, and a lookup timeout writes nothing.
		return nil
	case *resolver.Denied:
		return r.Withheld(ctx)
	case *resolver.Resolved:
		defer func() { err = errors.Join(err, r.Store.Close()) }()
		rc, ok := r.Store.(*receipts)
		if !ok {
			panic(fmt.Sprintf("hook: resolver store is %T, want the hook's own", r.Store))
		}
		return h.deliver(ctx, rc, r, stdout)
	default:
		panic(fmt.Sprintf("hook: unknown resolver result %T", res))
	}
}

// pane asks herdr for the pane with a deadline of lookupDeadline from the hook start.
func (h Hook) pane(ctx context.Context, t herdr.Target, id string) (herdr.Pane, error) {
	ctx, cancel := context.WithDeadline(ctx, h.Start.Add(lookupDeadline))
	defer cancel()
	return h.Pane(ctx, t, id)
}

// deliver prints the brief of the current generation for the live pane of r, or withholds
// it, and writes the receipt. A launched session's receipt names its launch, which must still
// run.
func (h Hook) deliver(ctx context.Context, rc *receipts, r *resolver.Resolved, stdout io.Writer) error {
	d, key, err := delivery(r)
	if err != nil {
		return err
	}
	// The forget check, the read, the print and the receipt are one write transaction. A forget
	// marks the outputs in a write transaction, so it commits before the check or after the
	// receipt, never between them.
	return rc.s.Write(ctx, func(tx *sql.Tx) error {
		if rc.launch != 0 {
			if err := launchRunning(ctx, tx, rc.launch); err != nil {
				return err
			}
		}
		d, text, err := h.compile(ctx, tx, r.Binding.Store, d, key, rc.agentSession)
		if err != nil {
			return err
		}
		if text != "" {
			if err := h.Profile.Write(stdout, text); err != nil {
				return err
			}
		}
		return rc.insert(ctx, tx, d)
	})
}

// delivery returns the receipt of the brief for the pane of r before the read of the brief,
// and the workspace key of its workspace part: "" for none.
func delivery(r *resolver.Resolved) (d store.Delivery, key string, err error) {
	m, err := workspace.For(r.Binding).Lookup(r.WorkspaceID)
	if err != nil {
		return d, "", err
	}
	d = store.Delivery{HerdrSession: &r.Binding.Session, PaneID: r.PaneID, Result: store.ResultEmitted}
	switch m.State {
	case workspace.Mapped:
		key = m.Entry.Key
		d.WorkspaceKey = &key
	case workspace.Unmapped:
		// A new workspace gets the global part only, never the part of another workspace.
		d.Result = store.ResultUnmapped
	case workspace.Withheld:
		// The global part still prints; only the workspace part is withheld.
		d = withheld(d, reasonIdentity)
		d.WorkspaceKey = &m.Entry.Key
	}
	return d, key, nil
}

// compile reads the brief of the current generation in the store folder dir for the agent
// session agentSession, in tx, and returns receipt d with its brief fields and the brief text.
// The text is "" when the receipt withholds the brief: a forget is pending, or the generation
// is not whole. key is the workspace key of delivery.
func (h Hook) compile(ctx context.Context, tx *sql.Tx, dir string, d store.Delivery, key, agentSession string) (store.Delivery, string, error) {
	pending, err := store.ForgetPending(ctx, tx)
	if err != nil {
		return d, "", err
	}
	if pending {
		return withheld(d, reasonForget), "", nil
	}
	b, err := output.Read(filepath.Join(dir, outDir), key)
	if errors.Is(err, output.ErrInconsistent) {
		return withheld(d, reasonInconsistent), "", nil
	}
	if err != nil {
		return d, "", err
	}
	ws, err := h.lastSession(ctx, tx, key, agentSession, b)
	if err != nil {
		return d, "", err
	}
	// The brief is made from the same bytes that passed the manifest check. The receipt
	// names the (item, revision) pairs of that manifest, not the store rows now.
	// The brief hash covers the last session line and its handoff lines too.
	text := brief.Text(b.Global, ws)
	sum := sha256.Sum256([]byte(text))
	briefHash := hex.EncodeToString(sum[:])
	d.BriefHash, d.OutputGeneration, d.ItemRevisions = &briefHash, &b.Generation, b.Revisions
	return d, text, nil
}

// lastSession adds the line of the last session of workspace key before agent session
// agentSession to the workspace part of b (E15-S4), and below it the handoff lines of that
// session that fit the brief budget (E20-S8). key "" is an unmapped or withheld workspace: it
// gets no workspace part, so no lines. The time is in the zone of the hook clock.
func (h Hook) lastSession(ctx context.Context, tx *sql.Tx, key, agentSession string, b output.Brief) (string, error) {
	if key == "" {
		return b.Workspace, nil
	}
	last, ok, err := store.LastSession(ctx, tx, key, agentSession)
	if err != nil || !ok {
		return b.Workspace, err
	}
	// The approved items are those of the manifest that passed the check, as the receipt names.
	ho, err := store.ReadHandoff(ctx, tx, key, last, b.Revisions)
	if err != nil {
		return "", err
	}
	failed := make([]brief.Failure, len(ho.Failed))
	for i, f := range ho.Failed {
		failed[i] = brief.Failure{Command: f.Argv, Exit: f.ExitStatus}
	}
	ws := brief.LastSession(b.Workspace, last.Profile, last.At.In(h.Now().Location()), *last.TranscriptPath)
	return brief.Handoff(b.Global, ws, ho.Approved, ho.Skills, failed), nil
}

func withheld(d store.Delivery, reason string) store.Delivery {
	d.Result, d.WithheldReason = store.ResultWithheld, &reason
	return d
}

// lookup returns the value of key in environ; the last entry wins, as in the resolver.
func lookup(environ []string, key string) string {
	var v string
	for _, kv := range environ {
		if k, val, ok := strings.Cut(kv, "="); ok && k == key {
			v = val
		}
	}
	return v
}

// receipts is the store of one hook run as the resolver sees it. It writes the receipts.
type receipts struct {
	s            *store.Store
	profile      string
	agentSession string
	transcript   string // the transcript path of the hook input; "" for none
	now          func() time.Time
	// recorded: the resolver recorded the agent session, so the receipt of this run holds the
	// (pane, agent session id) record, with its check still pending.
	recorded bool
	// checks are the agent checks of earlier records. They join the receipt transaction.
	checks []agentCheck
	launch int64 // the launch record of a launched session; 0 for none
}

// agentCheck is one check of an earlier (pane, agent session id) record.
type agentCheck struct {
	session, paneID, agentSession string
	check                         resolver.AgentCheck
}

// WriteWithheld writes the receipt of a pane that failed the live check.
func (rc *receipts) WriteWithheld(ctx context.Context, w resolver.Withheld) error {
	return rc.write(ctx, withheld(store.Delivery{HerdrSession: &w.Session, PaneID: w.PaneID}, string(w.Reason)))
}

// RecordAgentSession writes nothing now: the receipt of this run, which has the same herdr
// session, pane and agent session, holds the record. At hook time herdr does not show the new
// agent yet, so the next brain call for the pane checks it.
func (rc *receipts) RecordAgentSession(context.Context, string, string, string) error {
	rc.recorded = true
	return nil
}

func (rc *receipts) UncheckedAgentSessions(ctx context.Context, session, paneID string) ([]string, error) {
	return rc.s.AgentSessions(ctx, session, paneID, string(resolver.AgentPending))
}

// SetAgentCheck writes nothing now: the receipt transaction writes the check, so a hook run
// takes the store lock once. The write changes only a record that is still pending.
func (rc *receipts) SetAgentCheck(_ context.Context, session, paneID, agentSession string, c resolver.AgentCheck) error {
	rc.checks = append(rc.checks, agentCheck{session, paneID, agentSession, c})
	return nil
}

func (rc *receipts) Close() error { return rc.s.Close() }

func (rc *receipts) write(ctx context.Context, d store.Delivery) error {
	return rc.s.Write(ctx, func(tx *sql.Tx) error { return rc.insert(ctx, tx, d) })
}

func (rc *receipts) insert(ctx context.Context, tx *sql.Tx, d store.Delivery) error {
	for _, c := range rc.checks {
		if err := store.SetAgentCheck(ctx, tx, c.session, c.paneID, c.agentSession,
			string(resolver.AgentPending), string(c.check)); err != nil {
			return err
		}
	}
	d.Profile, d.AgentSessionID, d.At = rc.profile, &rc.agentSession, rc.now()
	if rc.transcript != "" {
		d.TranscriptPath = &rc.transcript
	}
	if rc.recorded {
		pending := string(resolver.AgentPending)
		d.AgentCheck = &pending
	}
	if rc.launch != 0 {
		d.LaunchID = &rc.launch
	}
	_, err := store.InsertDelivery(ctx, tx, d)
	return err
}

// gate passes the print to w until the cap closes it. The print is one Write (one JSON
// encode). A print that passed the gate before the cap may stall in w; the exit ends it.
type gate struct {
	mu     sync.Mutex
	closed bool
	wrote  bool // a print came to the gate, before the cap or after it
	w      io.Writer
}

func (g *gate) Write(p []byte) (int, error) {
	g.mu.Lock()
	closed := g.closed
	g.wrote = true
	g.mu.Unlock()
	if closed {
		return 0, errCapped
	}
	return g.w.Write(p)
}

// used reports whether the gate is closed or a print came to it: then the output for
// "nothing" does not go out.
func (g *gate) used() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.closed || g.wrote
}

func (g *gate) close() {
	g.mu.Lock()
	g.closed = true
	g.mu.Unlock()
}
