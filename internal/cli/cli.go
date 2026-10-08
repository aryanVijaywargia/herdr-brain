// Package cli is the command layer of the `brain` commands that work on a store: remember,
// and later review, undo, forget and pin (E4). Each command opens its store the same way: the
// resolver checks the calling pane (§8.3), the workspace map syncs with herdr (E2-S4), and
// output repair builds any output that is behind the store (§7.5 step 6).
package cli

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path/filepath"
	"syscall"
	"time"

	"github.com/aryanVijaywargia/herdr-brain/internal/bindings"
	"github.com/aryanVijaywargia/herdr-brain/internal/harness"
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

// Usage returns the usage text of command name, for `brain <name> --help` (#318), or "" for a
// command with no usage text in this package.
func Usage(name string) string {
	switch name {
	case "status":
		return statusUsage
	case "doctor":
		return doctorUsage
	case "remember":
		return rememberUsage
	case "recall":
		return recallUsage
	case "review":
		return reviewUsage
	case "pin":
		return pinUsage
	case "archive":
		return archiveUsage
	case "promote":
		return promoteUsage
	case "undo":
		return undoUsage
	case "workspace":
		return workspaceUsage
	case "forget":
		return forgetUsage
	case "repair":
		return repairUsage
	case "pane":
		return paneUsage
	case "uninstall":
		return uninstallUsage
	case "learn":
		return learnUsage
	case "extract":
		return extractUsage
	case "run":
		return runUsage
	case "tools":
		return toolsUsage
	case "automation":
		return automationUsage
	}
	return ""
}

// Env is the process context of one command.
type Env struct {
	Environ  []string
	Bindings string // the bindings file
	Pane     resolver.PaneFunc
	// Sync refreshes the workspace map of s from herdr; tests replace it.
	Sync func(ctx context.Context, s workspace.Session, st *store.Store) error
	// Herdr returns the herdr calls of the review panes in session t; tests replace it.
	Herdr func(t herdr.Target) (PaneHerdr, error)
	// StatusHerdr returns the herdr calls of brain status in session t; tests replace it.
	StatusHerdr func(t herdr.Target) (StatusHerdr, error)
	// RunHerdr returns the herdr calls of an automation run in session t; tests replace it.
	RunHerdr func(t herdr.Target) (RunHerdr, error)
	// Kill stops the process pid of an automation run (brain automation cancel); tests replace it.
	Kill func(pid int) error
	// Command returns the command line of process pid, or "" when there is no such process
	// (brain automation cancel checks it before Kill); tests replace it.
	Command func(ctx context.Context, pid int) (string, error)
	// Config is the Brain config file (paths.ConfigFile).
	Config string
	// Data is the data home; Adapters are its files that the adapter states come from.
	Data     string
	Adapters harness.Files
	// Profiles holds the harness profiles (adapter.LoadAll); nil is the embedded set.
	Profiles fs.FS
	Now      func() time.Time
	Stdin    io.Reader // the answers to a question; nil answers "no"
	Stdout   io.Writer
	Stderr   io.Writer // for warnings; the caller prints the error of a command
}

// NewEnv returns the Env of a command that runs with the process environment environ.
func NewEnv(environ []string, stdout, stderr io.Writer) (Env, error) {
	file, err := paths.BindingsFile()
	if err != nil {
		return Env{}, err
	}
	data, err := paths.DataHome()
	if err != nil {
		return Env{}, err
	}
	config, err := paths.ConfigFile()
	if err != nil {
		return Env{}, err
	}
	adapters, err := adapterFiles(data)
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
		Herdr: func(t herdr.Target) (PaneHerdr, error) {
			c, err := herdr.New(environ, t)
			if err != nil {
				return nil, err
			}
			return c, nil
		},
		StatusHerdr: func(t herdr.Target) (StatusHerdr, error) {
			c, err := herdr.New(environ, t)
			if err != nil {
				return nil, err
			}
			return c, nil
		},
		RunHerdr: func(t herdr.Target) (RunHerdr, error) {
			c, err := herdr.New(environ, t)
			if err != nil {
				return nil, err
			}
			return c, nil
		},
		Kill:     func(pid int) error { return syscall.Kill(pid, syscall.SIGTERM) },
		Command:  processCommand,
		Config:   config,
		Data:     data,
		Adapters: adapters,
		Now:      time.Now,
		Stdout:   stdout,
		Stderr:   stderr,
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
	// AgentSession is the agent session that herdr showed in the calling pane at the last
	// check, or "" when herdr showed none.
	AgentSession string
}

// Close closes the store.
func (c *Call) Close() error { return c.Store.Close() }

// Open resolves the store of the calling pane, syncs the workspace map and repairs the output.
// A call that gets no store, or whose pane herdr does not confirm, opens nothing and writes
// nothing (S64): Open returns an error. A failed repair is only a warning: the command's own
// write does not need it, and the next call retries it.
func (e Env) Open(ctx context.Context) (*Call, error) {
	return e.open(ctx, openRepair)
}

// openMode is what open does besides the resolve.
type openMode int

const (
	openRepair   openMode = iota // sync the workspace map and repair the output
	openNoRepair                 // sync only: brain repair repairs the output itself, and shows each conflict once
	// openRead writes nothing (the recall popup, E20-S3): no sync, so the workspace map is the
	// one that the hook reads (§8.3), no repair, and no agent check.
	openRead
)

// open is Open, with mode.
func (e Env) open(ctx context.Context, mode openMode) (*Call, error) {
	rctx, cancel := context.WithTimeout(ctx, herdrDeadline)
	res, err := resolver.New(e.Bindings, opener(mode), e.Pane).Resolve(rctx, e.Environ, "")
	cancel()
	if err != nil {
		return nil, err
	}
	var r *resolver.Resolved
	switch v := res.(type) {
	case *resolver.NoStore:
		return nil, &noStoreError{v}
	case *resolver.Denied:
		return nil, fmt.Errorf("herdr does not confirm pane %s in workspace %s (%s); nothing was written",
			v.PaneID, v.WorkspaceID, v.Reason)
	case *resolver.Resolved:
		r = v
	default:
		panic(fmt.Sprintf("cli: unknown resolver result %T", res))
	}
	c := &Call{Store: storeOf(r.Store), Binding: r.Binding, PaneID: r.PaneID, WorkspaceID: r.WorkspaceID,
		Workspace: workspace.For(r.Binding)}
	if mode == openRead {
		return c, nil
	}
	sctx, cancel := context.WithTimeout(ctx, herdrDeadline)
	err = e.Sync(sctx, c.Workspace, c.Store)
	cancel()
	if err != nil {
		return nil, errors.Join(fmt.Errorf("sync workspace map: %w", err), c.Close())
	}
	if mode == openRepair {
		if err := output.Repair(ctx, c.Store, filepath.Join(r.Binding.Store, outDir), e.Now); err != nil {
			if _, werr := fmt.Fprintf(e.Stderr, "brain: warning: output repair: %v (the next brain call retries)\n", err); werr != nil {
				return nil, errors.Join(werr, c.Close())
			}
		}
	}
	// The pane can move to another workspace during the sync or the repair, so check it again
	// before the command writes (§5.1).
	if err := e.recheck(ctx, c); err != nil {
		return nil, errors.Join(err, c.Close())
	}
	return c, nil
}

// opener opens the store of a command with mode: a read opens it as a readStore.
func opener(mode openMode) resolver.Opener {
	return func(ctx context.Context, dir string) (resolver.Store, error) {
		s, err := store.Open(ctx, dir, busyTimeout)
		if err != nil {
			return nil, err
		}
		if mode == openRead {
			return readStore{commandStore{s}}, nil
		}
		return commandStore{s}, nil
	}
}

// storeOf is the store that opener opened.
func storeOf(s resolver.Store) *store.Store {
	switch s := s.(type) {
	case commandStore:
		return s.Store
	case readStore:
		return s.Store
	}
	panic(fmt.Sprintf("cli: resolver store is %T, want the command's own", s))
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
	if p.AgentSession != nil {
		c.AgentSession = p.AgentSession.Value
	}
	return nil
}

// noStoreError is the error of a call that gets no store. brain status explains an unbound
// session instead of failing (§8.3 "Human diagnostics").
type noStoreError struct{ *resolver.NoStore }

func (n *noStoreError) Error() string {
	if n.Err != nil {
		return fmt.Sprintf("no Brain store for this pane (%s: %v); nothing was written", n.Reason, n.Err)
	}
	return fmt.Sprintf("no Brain store for this pane (%s); nothing was written", n.Reason)
}

func (n *noStoreError) Unwrap() error { return n.Err }

// commandStore is the store of a command as the resolver sees it. A command passes no agent
// session and writes no receipt; it only checks the earlier records of the calling pane
// against the agent that herdr shows now (§8.3 limit, §17 item 29).
type commandStore struct{ *store.Store }

func (commandStore) WriteWithheld(context.Context, resolver.Withheld) error { return errNoReceipts }

func (commandStore) RecordAgentSession(context.Context, string, string, string) error {
	return errNoReceipts
}

func (cs commandStore) UncheckedAgentSessions(ctx context.Context, session, paneID string) ([]string, error) {
	return cs.AgentSessions(ctx, session, paneID, string(resolver.AgentPending))
}

// readStore is the store of a call that writes nothing: it leaves the agent checks of the
// pane for the next call.
type readStore struct{ commandStore }

func (readStore) SetAgentCheck(context.Context, string, string, string, resolver.AgentCheck) error {
	return nil
}

func (cs commandStore) SetAgentCheck(ctx context.Context, session, paneID, agentSession string, c resolver.AgentCheck) error {
	return cs.Write(ctx, func(tx *sql.Tx) error {
		return store.SetAgentCheck(ctx, tx, session, paneID, agentSession, string(resolver.AgentPending), string(c))
	})
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
