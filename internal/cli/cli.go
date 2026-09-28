// Package cli is the command layer of the `brain` commands that work on a store: remember,
// and later review, undo, forget and pin (E4). Each command opens its store the same way: the
// resolver checks the calling pane (§8.3), the workspace map syncs with herdr (E2-S4), and
// output repair builds any output that is behind the store (§7.5 step 6).
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"time"

	"github.com/aryanVijaywargia/herdr-brain/internal/bindings"
	"github.com/aryanVijaywargia/herdr-brain/internal/herdr"
	"github.com/aryanVijaywargia/herdr-brain/internal/output"
	"github.com/aryanVijaywargia/herdr-brain/internal/paths"
	"github.com/aryanVijaywargia/herdr-brain/internal/resolver"
	"github.com/aryanVijaywargia/herdr-brain/internal/store"
	"github.com/aryanVijaywargia/herdr-brain/internal/workspace"
)

const (
	// busyTimeout is how long a command waits for the store lock of another Brain process.
	// [NEW] 5 s, as `brain event`: a person waits for a command, not an agent start.
	busyTimeout = 5 * time.Second
	// herdrDeadline is the deadline of each herdr call of a command. [NEW]
	herdrDeadline = 5 * time.Second
	// outDir is the out folder of a store, as in the hook (§7.5 step 3).
	outDir = "out"
)

// errNoReceipts: a command never writes a hook receipt or an agent session record.
var errNoReceipts = errors.New("a brain command writes no hook receipts")

// Env is the process context of one command.
type Env struct {
	Environ  []string
	Bindings string // the bindings file
	Pane     resolver.PaneFunc
	// Sync refreshes the workspace map of s from herdr; tests replace it.
	Sync   func(ctx context.Context, s workspace.Session, st *store.Store) error
	Now    func() time.Time
	Stdout io.Writer
	Stderr io.Writer // for warnings; the caller prints the error of a command
}

// NewEnv returns the Env of a command that runs with the process environment environ.
func NewEnv(environ []string, stdout, stderr io.Writer) (Env, error) {
	file, err := paths.BindingsFile()
	if err != nil {
		return Env{}, err
	}
	return Env{
		Environ:  environ,
		Bindings: file,
		Pane:     resolver.HerdrPane(environ),
		Sync: func(ctx context.Context, s workspace.Session, st *store.Store) error {
			return s.Sync(ctx, environ, st, time.Now())
		},
		Now:    time.Now,
		Stdout: stdout,
		Stderr: stderr,
	}, nil
}

// Call is a command call from a live pane of a bound herdr session, with its store open. The
// caller closes it.
type Call struct {
	Store       *store.Store
	Binding     bindings.Binding
	PaneID      string
	WorkspaceID string // herdr id of the calling pane's workspace
	Workspace   workspace.Session
}

// Close closes the store.
func (c *Call) Close() error { return c.Store.Close() }

// Open resolves the store of the calling pane, syncs the workspace map and repairs the output.
// A call that gets no store, or whose pane herdr does not confirm, opens nothing and writes
// nothing (S64): Open returns an error. A failed repair is only a warning: the command's own
// write does not need it, and the next call retries it.
func (e Env) Open(ctx context.Context) (*Call, error) {
	open := func(ctx context.Context, dir string) (resolver.Store, error) {
		s, err := store.Open(ctx, dir, busyTimeout)
		if err != nil {
			return nil, err
		}
		return commandStore{s}, nil
	}
	rctx, cancel := context.WithTimeout(ctx, herdrDeadline)
	res, err := resolver.New(e.Bindings, open, e.Pane).Resolve(rctx, e.Environ, "")
	cancel()
	if err != nil {
		return nil, err
	}
	var r *resolver.Resolved
	switch v := res.(type) {
	case *resolver.NoStore:
		return nil, noStoreError(v)
	case *resolver.Denied:
		return nil, fmt.Errorf("herdr does not confirm pane %s in workspace %s (%s); nothing was written",
			v.PaneID, v.WorkspaceID, v.Reason)
	case *resolver.Resolved:
		r = v
	default:
		panic(fmt.Sprintf("cli: unknown resolver result %T", res))
	}
	cs, ok := r.Store.(commandStore)
	if !ok {
		panic(fmt.Sprintf("cli: resolver store is %T, want the command's own", r.Store))
	}
	c := &Call{Store: cs.Store, Binding: r.Binding, PaneID: r.PaneID, WorkspaceID: r.WorkspaceID,
		Workspace: workspace.For(r.Binding)}
	sctx, cancel := context.WithTimeout(ctx, herdrDeadline)
	err = e.Sync(sctx, c.Workspace, c.Store)
	cancel()
	if err != nil {
		return nil, errors.Join(fmt.Errorf("sync workspace map: %w", err), c.Close())
	}
	if err := output.Repair(ctx, c.Store, filepath.Join(r.Binding.Store, outDir), e.Now); err != nil {
		if _, werr := fmt.Fprintf(e.Stderr, "brain: warning: output repair: %v (the next brain call retries)\n", err); werr != nil {
			return nil, errors.Join(werr, c.Close())
		}
	}
	// The pane can move to another workspace during the sync or the repair, so check it again
	// before the command writes (§5.1).
	if err := e.recheck(ctx, c); err != nil {
		return nil, errors.Join(err, c.Close())
	}
	return c, nil
}

// recheck asks herdr again for the calling pane, and fails when it is gone or no longer in
// the workspace that the resolver confirmed.
func (e Env) recheck(ctx context.Context, c *Call) error {
	ctx, cancel := context.WithTimeout(ctx, herdrDeadline)
	defer cancel()
	p, err := e.Pane(ctx, herdr.Target{Session: c.Binding.Session, Socket: c.Binding.Socket}, c.PaneID)
	if err != nil {
		return fmt.Errorf("check pane %s again: %w; nothing was written", c.PaneID, err)
	}
	if p.ID != c.PaneID || p.WorkspaceID != c.WorkspaceID {
		return fmt.Errorf("pane %s moved to workspace %s; nothing was written", c.PaneID, p.WorkspaceID)
	}
	return nil
}

func noStoreError(n *resolver.NoStore) error {
	if n.Err != nil {
		return fmt.Errorf("no Brain store for this pane (%s: %w); nothing was written", n.Reason, n.Err)
	}
	return fmt.Errorf("no Brain store for this pane (%s); nothing was written", n.Reason)
}

// commandStore is the store of a command as the resolver sees it. The resolver writes through
// it only on a hook call: a command passes no agent session and writes no receipt.
type commandStore struct{ *store.Store }

func (commandStore) WriteWithheld(context.Context, resolver.Withheld) error { return errNoReceipts }

func (commandStore) RecordAgentSession(context.Context, string, string, string) error {
	return errNoReceipts
}

// UncheckedAgentSessions returns none: the store records no agent sessions until E7-S3.
func (commandStore) UncheckedAgentSessions(context.Context, string, string) ([]string, error) {
	return nil, nil
}

func (commandStore) SetAgentCheck(context.Context, string, string, string, resolver.AgentCheck) error {
	return errNoReceipts
}

// WorkspaceKey returns the Brain key of the calling pane's workspace, and its label. A
// workspace that is not mapped yet, or whose saved root lies outside its project folder, has
// no key: an item written there could reach the wrong project.
func (c *Call) WorkspaceKey() (key, label string, err error) {
	m, err := c.Workspace.Lookup(c.WorkspaceID)
	if err != nil {
		return "", "", err
	}
	switch m.State {
	case workspace.Mapped:
		return m.Entry.Key, m.Entry.Label, nil
	case workspace.Unmapped:
		return "", "", fmt.Errorf("workspace %s is not mapped yet (herdr saves a new workspace after about 5 s); try again, or use --global", c.WorkspaceID)
	default:
		return "", "", fmt.Errorf("workspace %s: %w", c.WorkspaceID, m.Err)
	}
}
