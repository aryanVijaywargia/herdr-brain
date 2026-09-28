// Package hook is `brain hook` (HLD §8.3, §5.2). At the start of an agent session in a live
// pane of a bound herdr session, it prints the brief of the current output generation in the
// output form of the harness profile, and records a receipt. It prefers a missed delivery to a
// wrong one, and it never fails a session start: on any error it prints nothing.
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
	"strings"
	"sync"
	"time"

	"github.com/aryanVijaywargia/herdr-brain/internal/adapter"
	"github.com/aryanVijaywargia/herdr-brain/internal/brief"
	"github.com/aryanVijaywargia/herdr-brain/internal/herdr"
	"github.com/aryanVijaywargia/herdr-brain/internal/output"
	"github.com/aryanVijaywargia/herdr-brain/internal/paths"
	"github.com/aryanVijaywargia/herdr-brain/internal/resolver"
	"github.com/aryanVijaywargia/herdr-brain/internal/store"
	"github.com/aryanVijaywargia/herdr-brain/internal/workspace"
)

// The time limits of §8.3. The cap covers the whole hook from its start: the stdin read, the
// lookup, the file reads, the receipt and the print. The lookup ends by lookupDeadline, plus
// the 50 ms that the herdr client waits for herdr's pipes; a receipt waits at most busyTimeout
// for the store lock. That is 250 ms, so at least 50 ms of the cap is left for the file reads
// and the print, which took under 1 ms in M0 (E7-S4 FINDINGS). The agent check of earlier
// records (§8.3 limit) adds 1 + N short write transactions before the print, one read and one
// for each of the N pending records of the pane. Each waits for the lock too, so under lock
// contention that margin can be smaller; each wait still ends at the cap.
const (
	// hookCap is the total cap of one hook run (§8.3 "Time limits"). [NEW] 300 ms, no source.
	hookCap = 300 * time.Millisecond
	// lookupDeadline is the herdr lookup deadline, counted from the hook start (§8.3 "Time
	// limits", owner decision 2026-09-28).
	lookupDeadline = 150 * time.Millisecond
	// busyTimeout is how long a receipt write waits for the lock of another Brain process. [NEW]
	busyTimeout = 50 * time.Millisecond
	// outDir is the out folder of a store (§7.5 step 3). [NEW] its place in the store folder.
	outDir = "out"
)

// The reasons of the withheld receipts that the hook writes itself (§8.5 "Receipts"). The
// resolver gives the reasons of a failed live check.
const (
	reasonIdentity     = "identity"
	reasonInconsistent = "inconsistent"
	reasonForget       = "forget pending"
)

// errCapped: the hook cap stopped the hook. It printed nothing and wrote no receipt (S50). The
// agent checks of earlier records can have committed before the cap.
var errCapped = errors.New("hook cap reached")

// Main runs the hook for the frozen command `brain hook --profile <name> --event start` with
// the process environment environ. The caller prints nothing on the error and exits 0.
func Main(args, environ []string, stdin io.Reader, stdout io.Writer) error {
	start := time.Now()
	name, err := profileArg(args)
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
	h := Hook{Profile: p, Environ: environ, Bindings: file, Pane: resolver.HerdrPane(environ), Start: start, Now: time.Now}
	return h.Run(context.Background(), stdin, stdout)
}

// profileArg reads the profile name from the frozen arguments; any other form is an error.
func profileArg(args []string) (string, error) {
	if len(args) != 4 || args[0] != "--profile" || args[1] == "" || args[2] != "--event" || args[3] != "start" {
		return "", fmt.Errorf("want --profile <name> --event start, got %q", args)
	}
	return args[1], nil
}

// Hook is one run of `brain hook`.
type Hook struct {
	Profile  *adapter.Profile
	Environ  []string
	Bindings string // the bindings file
	Pane     resolver.PaneFunc
	Start    time.Time // the hook start; the lookup deadline counts from it
	Now      func() time.Time

	ended func() // tests only: called when the capped run ends, after the cap too
}

// Run reads the hook input from stdin and prints the brief to stdout, or nothing. A result
// that withholds the brief is not an error. On an error Run has printed nothing, unless the
// receipt write after the print failed: then the delivery is unknown (§8.3 step 7).
//
// Run returns by hookCap after Start, also when stdin, herdr or stdout stall. The caller
// exits then. A run that the cap stops prints nothing after the cap and writes no receipt,
// so status shows the session as unknown (S50).
func (h Hook) Run(ctx context.Context, stdin io.Reader, stdout io.Writer) error {
	// Outside herdr the hook reads no file and asks no herdr (S61).
	if lookup(h.Environ, "HERDR_ENV") != "1" {
		return nil
	}
	ctx, cancel := context.WithDeadline(ctx, h.Start.Add(hookCap))
	defer cancel()
	out := &gate{w: stdout}
	done := make(chan error, 1)
	go func() {
		err := h.run(ctx, stdin, out)
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

// run is Run without the cap.
func (h Hook) run(ctx context.Context, stdin io.Reader, stdout io.Writer) (err error) {
	raw, err := io.ReadAll(stdin)
	if err != nil {
		return fmt.Errorf("read hook input: %w", err)
	}
	in, err := h.Profile.Parse(raw)
	if errors.Is(err, adapter.ErrNotStart) {
		return nil
	}
	if err != nil {
		return err
	}
	open := func(ctx context.Context, dir string) (resolver.Store, error) {
		s, err := store.Open(ctx, dir, busyTimeout)
		if err != nil {
			return nil, err
		}
		return &receipts{s: s, profile: h.Profile.Kind, agentSession: in.SessionID, now: h.Now}, nil
	}
	// The resolver checks the earlier records of the pane and records this agent session; the
	// receipt of this run holds the record (§8.3 limit).
	res, err := resolver.New(h.Bindings, open, h.pane).Resolve(ctx, h.Environ, in.SessionID)
	if err != nil {
		return err
	}
	switch r := res.(type) {
	case *resolver.NoStore:
		// No store is open (S64), and a lookup timeout writes nothing.
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
// it, and writes the receipt.
func (h Hook) deliver(ctx context.Context, rc *receipts, r *resolver.Resolved, stdout io.Writer) error {
	m, err := workspace.For(r.Binding).Lookup(r.WorkspaceID)
	if err != nil {
		return err
	}
	d := store.Delivery{HerdrSession: &r.Binding.Session, PaneID: r.PaneID, Result: store.ResultEmitted}
	var key string
	switch m.State {
	case workspace.Mapped:
		key = m.Entry.Key
		d.WorkspaceKey = &key
	case workspace.Unmapped:
		// A new workspace gets the global part only, never the part of another workspace (S48).
		d.Result = store.ResultUnmapped
	case workspace.Withheld:
		// The global part still prints; only the workspace part is withheld.
		d = withheld(d, reasonIdentity)
		d.WorkspaceKey = &m.Entry.Key
	}
	// The forget check, the read, the print and the receipt are one write transaction. A forget
	// marks the outputs in a write transaction, so it commits before the check or after the
	// receipt, never between them (§6.7).
	return rc.s.Write(ctx, func(tx *sql.Tx) error {
		pending, err := store.ForgetPending(ctx, tx)
		if err != nil {
			return err
		}
		if pending {
			return rc.insert(ctx, tx, withheld(d, reasonForget))
		}
		b, err := output.Read(filepath.Join(r.Binding.Store, outDir), key)
		if errors.Is(err, output.ErrInconsistent) {
			return rc.insert(ctx, tx, withheld(d, reasonInconsistent))
		}
		if err != nil {
			return err
		}
		// The brief is made from the same bytes that passed the manifest check (S65). The
		// receipt names the (item, revision) pairs of that manifest, not the store rows now.
		text := brief.Text(b.Global, b.Workspace)
		if err := h.Profile.Write(stdout, text); err != nil {
			return err
		}
		sum := sha256.Sum256([]byte(text))
		briefHash := hex.EncodeToString(sum[:])
		d.BriefHash, d.OutputGeneration, d.ItemRevisions = &briefHash, &b.Generation, b.Revisions
		return rc.insert(ctx, tx, d)
	})
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
	now          func() time.Time
	// recorded: the resolver recorded the agent session, so the receipt of this run holds the
	// (pane, agent session id) record, with its check still pending.
	recorded bool
}

// WriteWithheld writes the receipt of a pane that failed the live check (S49, S63).
func (rc *receipts) WriteWithheld(ctx context.Context, w resolver.Withheld) error {
	return rc.write(ctx, withheld(store.Delivery{HerdrSession: &w.Session, PaneID: w.PaneID}, string(w.Reason)))
}

// RecordAgentSession writes nothing now: the receipt of this run, which has the same herdr
// session, pane and agent session, holds the record. At hook time herdr does not show the new
// agent yet, so the next brain call for the pane checks it (§17 item 29).
func (rc *receipts) RecordAgentSession(context.Context, string, string, string) error {
	rc.recorded = true
	return nil
}

func (rc *receipts) UncheckedAgentSessions(ctx context.Context, session, paneID string) ([]string, error) {
	var ids []string
	err := rc.s.Write(ctx, func(tx *sql.Tx) (err error) {
		ids, err = store.AgentSessions(ctx, tx, session, paneID, string(resolver.AgentPending))
		return err
	})
	return ids, err
}

func (rc *receipts) SetAgentCheck(ctx context.Context, session, paneID, agentSession string, c resolver.AgentCheck) error {
	return rc.s.Write(ctx, func(tx *sql.Tx) error {
		return store.SetAgentCheck(ctx, tx, session, paneID, agentSession, string(resolver.AgentPending), string(c))
	})
}

func (rc *receipts) Close() error { return rc.s.Close() }

func (rc *receipts) write(ctx context.Context, d store.Delivery) error {
	return rc.s.Write(ctx, func(tx *sql.Tx) error { return rc.insert(ctx, tx, d) })
}

func (rc *receipts) insert(ctx context.Context, tx *sql.Tx, d store.Delivery) error {
	d.Profile, d.AgentSessionID, d.At = rc.profile, &rc.agentSession, rc.now()
	if rc.recorded {
		pending := string(resolver.AgentPending)
		d.AgentCheck = &pending
	}
	_, err := store.InsertDelivery(ctx, tx, d)
	return err
}

// gate passes the print to w until the cap closes it. The print is one Write (one JSON
// encode). A print that passed the gate before the cap may stall in w; the exit ends it.
type gate struct {
	mu     sync.Mutex
	closed bool
	w      io.Writer
}

func (g *gate) Write(p []byte) (int, error) {
	g.mu.Lock()
	closed := g.closed
	g.mu.Unlock()
	if closed {
		return 0, errCapped
	}
	return g.w.Write(p)
}

func (g *gate) close() {
	g.mu.Lock()
	g.closed = true
	g.mu.Unlock()
}
