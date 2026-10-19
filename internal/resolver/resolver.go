// Package resolver finds the one store of a Brain call from the process environment. Every
// entry point calls it, and there is no --store flag. A call gets one of three results:
// Resolved, Denied or NoStore.
package resolver

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/aryanVijaywargia/herdr-brain/internal/bindings"
	"github.com/aryanVijaywargia/herdr-brain/internal/herdr"
)

// NoStoreReason says why a call gets no store.
type NoStoreReason string

// The reasons for NoStore.
const (
	NotInHerdr NoStoreReason = "not in herdr"    // HERDR_ENV is not 1, or a herdr variable is missing
	Unbound    NoStoreReason = "unbound session" // no binding has the socket of HERDR_SOCKET_PATH
	// BindingDropped: the bindings file has a test binding for this session, but Read dropped it.
	BindingDropped NoStoreReason = "binding dropped"
	Mismatch       NoStoreReason = "binding mismatch" // HERDR_SESSION names another session than the binding
	HerdrTimeout   NoStoreReason = "herdr timeout"    // `pane get` did not answer before the deadline
	HerdrFailed    NoStoreReason = "herdr failed"     // `pane get` failed, or its reply has an unknown shape
)

// DenyReason says why the live check refused a pane.
type DenyReason string

// The reasons for Denied.
const (
	PaneGone DenyReason = "pane not live" // herdr has no pane HERDR_PANE_ID
	// NotMember: the pane is not HERDR_PANE_ID in HERDR_WORKSPACE_ID. herdr answers the old id
	// of a pane that moved to another workspace with its new id and workspace.
	NotMember DenyReason = "not a member"
)

// AgentCheck is the result of the check of a (pane, agent session id) record against the
// `agent_session` that herdr later shows for that pane.
type AgentCheck string

// The agent checks.
const (
	// AgentPending: recorded, not checked yet. The next call for the pane checks it.
	AgentPending      AgentCheck = "pending"
	AgentUnknown      AgentCheck = "unknown" // at the next call herdr showed no agent session
	AgentMatch        AgentCheck = "match"
	NotThisPanesAgent AgentCheck = "not this pane's agent" // for example a nested `claude -p`
)

// Withheld is the "withheld" receipt of a denied call.
type Withheld struct {
	Session      string // herdr session of the binding
	PaneID       string
	WorkspaceID  string
	AgentSession string // from the hook input; empty when unknown
	Reason       DenyReason
}

// Store is the part of a store that the resolver uses.
//
// All real herdr sessions share the main store, and two sessions can have the same pane id.
// So an agent record belongs to a herdr session and a pane id.
type Store interface {
	WriteWithheld(ctx context.Context, w Withheld) error
	RecordAgentSession(ctx context.Context, session, paneID, agentSession string) error
	// UncheckedAgentSessions returns the agent session ids recorded for paneID of session
	// whose check is still AgentPending.
	UncheckedAgentSessions(ctx context.Context, session, paneID string) ([]string, error)
	SetAgentCheck(ctx context.Context, session, paneID, agentSession string, c AgentCheck) error
	Close() error
}

// Opener opens the store in the store folder dir.
type Opener func(ctx context.Context, dir string) (Store, error)

// PaneFunc asks the herdr session t for the pane with id.
type PaneFunc func(ctx context.Context, t herdr.Target, id string) (herdr.Pane, error)

// HerdrPane returns a PaneFunc that runs `herdr pane get` with environ as the parent
// environment.
func HerdrPane(environ []string) PaneFunc {
	return func(ctx context.Context, t herdr.Target, id string) (herdr.Pane, error) {
		c, err := herdr.New(environ, t)
		if err != nil {
			return herdr.Pane{}, err
		}
		return c.Pane(ctx, id)
	}
}

// Result is Resolved, Denied or NoStore.
type Result interface{ result() }

// Resolved is a call from a live pane of a bound herdr session. The caller closes Store.
type Resolved struct {
	Binding     bindings.Binding // herdr session, its socket and its store folder
	PaneID      string
	WorkspaceID string
	Store       Store
}

// Denied is a call from a bound herdr session whose pane failed the live check. It gives no
// store: its one write is the "withheld" receipt.
type Denied struct {
	Binding      bindings.Binding
	PaneID       string
	WorkspaceID  string
	AgentSession string
	Reason       DenyReason
	open         Opener
}

// NoStore is a call that gets no store. Nothing opened a store, and the call writes nothing.
type NoStore struct {
	Reason NoStoreReason
	Err    error // details; nil when the reason says all
}

func (*Resolved) result() {}
func (*Denied) result()   {}
func (*NoStore) result()  {}

// Withheld writes the "withheld" receipt in the store of the binding, and nothing else.
func (d *Denied) Withheld(ctx context.Context) (err error) {
	st, err := d.open(ctx, d.Binding.Store)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, st.Close()) }()
	return st.WriteWithheld(ctx, Withheld{
		Session:      d.Binding.Session,
		PaneID:       d.PaneID,
		WorkspaceID:  d.WorkspaceID,
		AgentSession: d.AgentSession,
		Reason:       d.Reason,
	})
}

// Resolver resolves calls against one bindings file.
type Resolver struct {
	bindings string
	open     Opener
	pane     PaneFunc
}

// New returns a resolver that reads the bindings file at path, asks herdr through pane and
// opens stores with open.
func New(path string, open Opener, pane PaneFunc) *Resolver {
	return &Resolver{bindings: path, open: open, pane: pane}
}

// env is the part of the process environment that herdr puts in each pane.
type env struct {
	socket, session, paneID, workspaceID string
}

// parseEnv reads the herdr variables. A pane of the default session has no HERDR_SESSION,
// so "not set" means "default".
func parseEnv(environ []string) (env, *NoStore) {
	if lookup(environ, "HERDR_ENV") != "1" {
		return env{}, &NoStore{Reason: NotInHerdr}
	}
	e := env{
		socket:      lookup(environ, "HERDR_SOCKET_PATH"),
		session:     lookup(environ, "HERDR_SESSION"),
		paneID:      lookup(environ, "HERDR_PANE_ID"),
		workspaceID: lookup(environ, "HERDR_WORKSPACE_ID"),
	}
	if e.session == "" {
		e.session = "default"
	}
	switch {
	case !filepath.IsAbs(e.socket):
		return env{}, &NoStore{Reason: NotInHerdr, Err: fmt.Errorf("HERDR_SOCKET_PATH %q is not absolute", e.socket)}
	// Clean drops "link/.." as text, but the kernel follows the link first: a cleaned path
	// could select another binding than the socket the pane uses.
	case e.socket != filepath.Clean(e.socket):
		return env{}, &NoStore{Reason: NotInHerdr, Err: fmt.Errorf("HERDR_SOCKET_PATH %q is not a clean path", e.socket)}
	case e.paneID == "":
		return env{}, &NoStore{Reason: NotInHerdr, Err: errors.New("HERDR_PANE_ID is not set")}
	case e.workspaceID == "":
		return env{}, &NoStore{Reason: NotInHerdr, Err: errors.New("HERDR_WORKSPACE_ID is not set")}
	}
	return e, nil
}

// lookup returns the value of key in environ; the last entry wins, as in os/exec.
func lookup(environ []string, key string) string {
	var v string
	for _, kv := range environ {
		if k, val, ok := strings.Cut(kv, "="); ok && k == key {
			v = val
		}
	}
	return v
}

// Resolve finds the store for a call with the process environment environ. agentSession is
// the agent session id from the hook input, or empty. ctx carries the herdr lookup deadline.
// The error is for a broken bindings file or store; a caller on an agent-facing path prints
// nothing on it, as on NoStore.
func (r *Resolver) Resolve(ctx context.Context, environ []string, agentSession string) (Result, error) {
	e, ns := parseEnv(environ)
	if ns != nil {
		return ns, nil
	}
	bs, dropped, err := bindings.Read(r.bindings)
	if err != nil {
		return nil, err
	}
	// The socket selects the binding; HERDR_SESSION only checks it. A socket with no binding
	// never falls back to the default binding.
	b, ok, err := bindings.BySocket(bs, e.socket)
	if err != nil {
		return nil, err
	}
	if !ok {
		if d, ok := droppedFor(dropped, e); ok {
			return &NoStore{Reason: BindingDropped, Err: d.Err}, nil
		}
		return &NoStore{Reason: Unbound}, nil
	}
	if e.session != b.Session {
		return &NoStore{Reason: Mismatch, Err: fmt.Errorf("HERDR_SESSION is %q, the socket is bound to session %q", e.session, b.Session)}, nil
	}
	p, err := r.pane(ctx, herdr.Target{Session: b.Session, Socket: b.Socket}, e.paneID)
	switch {
	case errors.Is(err, herdr.ErrPaneNotFound):
		return r.denied(b, e, agentSession, PaneGone), nil
	case errors.Is(err, context.DeadlineExceeded):
		return &NoStore{Reason: HerdrTimeout, Err: err}, nil
	case err != nil:
		// A herdr failure is a result, not an error of Resolve: the call gets no store.
		return &NoStore{Reason: HerdrFailed, Err: err}, nil //nolint:nilerr // NoStore carries err
	case p.ID != e.paneID || p.WorkspaceID != e.workspaceID:
		return r.denied(b, e, agentSession, NotMember), nil
	}
	st, err := r.open(ctx, b.Store)
	if err != nil {
		return nil, err
	}
	if err := checkAgents(ctx, st, b.Session, p, agentSession); err != nil {
		return nil, errors.Join(err, st.Close())
	}
	return &Resolved{Binding: b, PaneID: p.ID, WorkspaceID: p.WorkspaceID, Store: st}, nil
}

// droppedFor returns the dropped binding of the session in e. A dropped binding may have a
// socket that does not resolve, so it matches by session name or by socket text.
func droppedFor(dropped []bindings.Dropped, e env) (bindings.Dropped, bool) {
	for _, d := range dropped {
		if d.Binding.Session == e.session || filepath.Clean(d.Binding.Socket) == filepath.Clean(e.socket) {
			return d, true
		}
	}
	return bindings.Dropped{}, false
}

func (r *Resolver) denied(b bindings.Binding, e env, agentSession string, reason DenyReason) *Denied {
	return &Denied{Binding: b, PaneID: e.paneID, WorkspaceID: e.workspaceID, AgentSession: agentSession, Reason: reason, open: r.open}
}

// checkAgents checks the pending (pane, agent session id) records of pane p of session
// against the agent session that herdr shows now, then records agentSession. At hook time
// herdr does not show the new agent yet, so a record is checked on the next call.
// Each record gets one check only: a later call could see another agent in the pane.
func checkAgents(ctx context.Context, st Store, session string, p herdr.Pane, agentSession string) error {
	ids, err := st.UncheckedAgentSessions(ctx, session, p.ID)
	if err != nil {
		return err
	}
	for _, id := range ids {
		c := AgentUnknown
		switch {
		case p.AgentSession == nil:
		case id == p.AgentSession.Value:
			c = AgentMatch
		default:
			c = NotThisPanesAgent
		}
		if err := st.SetAgentCheck(ctx, session, p.ID, id, c); err != nil {
			return err
		}
	}
	if agentSession == "" {
		return nil
	}
	return st.RecordAgentSession(ctx, session, p.ID, agentSession)
}
