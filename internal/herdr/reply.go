package herdr

import (
	"fmt"
	"path/filepath"
	"regexp"
)

// ShapeError reports a herdr reply that Brain does not know: the JSON does not decode, or a
// required field is missing, null or malformed. Callers fail closed on it (HLD §4.2).
type ShapeError struct {
	Field  string // JSON path, for example "pane.workspace_id"; empty for the whole reply
	Reason string
}

// Error names the field and the problem.
func (e *ShapeError) Error() string {
	if e.Field == "" {
		return "unknown reply shape: " + e.Reason
	}
	return fmt.Sprintf("unknown reply shape: %s: %s", e.Field, e.Reason)
}

// Pane is a herdr pane. ID, WorkspaceID and TabID are required. Agent, AgentSession and Cwd
// are nil when herdr does not know them; the caller decides what that means.
type Pane struct {
	ID           string
	WorkspaceID  string
	TabID        string
	Agent        *string
	AgentSession *AgentSession
	Cwd          *string
}

// AgentSession is the agent session that herdr found in a pane.
type AgentSession struct {
	Agent  string
	Source string // for example "herdr:claude"
	Kind   string // "id" or "path"
	Value  string
}

// Workspace is a herdr workspace. herdr gives no folder for it (HLD §4.2).
type Workspace struct {
	ID     string
	Label  string
	Number uint
}

// Session is a herdr session from `herdr session list --json`.
type Session struct {
	Name    string
	Default bool
	Running bool
	Dir     string
	Socket  string
}

// PluginPane is a pane that a plugin opened.
type PluginPane struct {
	PluginID   string
	Entrypoint string
	Pane       Pane
}

// Ids as herdr 0.9.1 prints them: workspace w14, pane w14:p2F, tab w14:t1. A pane or tab id
// starts with its workspace id (HLD §4.2).
var (
	workspaceIDRe = regexp.MustCompile(`^w[0-9]+$`)
	paneIDRe      = regexp.MustCompile(`^(w[0-9]+):p[0-9A-Za-z]+$`)
	tabIDRe       = regexp.MustCompile(`^(w[0-9]+):t[0-9A-Za-z]+$`)
)

// check keeps the first problem that it finds in a reply.
type check struct{ err error }

func (c *check) fail(field, reason string) {
	if c.err == nil {
		c.err = &ShapeError{Field: field, Reason: reason}
	}
}

// id checks v against re. When re has a group, the group must equal the workspace id ws.
func (c *check) id(re *regexp.Regexp, v, ws, field string) {
	if c.err != nil {
		return
	}
	m := re.FindStringSubmatch(v)
	switch {
	case m == nil:
		c.fail(field, fmt.Sprintf("malformed id %q", v))
	case len(m) == 2 && m[1] != ws:
		c.fail(field, fmt.Sprintf("id %q is not in workspace %q", v, ws))
	}
}

// req returns *p, or records field as missing when p is nil (absent or null in the JSON).
func req[T any](c *check, p *T, field string) T {
	if p == nil {
		c.fail(field, "missing")
		var zero T
		return zero
	}
	return *p
}

type rawPane struct {
	PaneID       *string          `json:"pane_id"`
	WorkspaceID  *string          `json:"workspace_id"`
	TabID        *string          `json:"tab_id"`
	Agent        *string          `json:"agent"`
	AgentSession *rawAgentSession `json:"agent_session"`
	Cwd          *string          `json:"cwd"`
}

type rawAgentSession struct {
	Agent  *string `json:"agent"`
	Source *string `json:"source"`
	Kind   *string `json:"kind"`
	Value  *string `json:"value"`
}

func (r *rawPane) parse(field string) (Pane, error) {
	var c check
	if r == nil {
		c.fail(field, "missing")
		return Pane{}, c.err
	}
	p := Pane{
		ID:          req(&c, r.PaneID, field+".pane_id"),
		WorkspaceID: req(&c, r.WorkspaceID, field+".workspace_id"),
		TabID:       req(&c, r.TabID, field+".tab_id"),
		Agent:       r.Agent,
		Cwd:         r.Cwd,
	}
	c.id(workspaceIDRe, p.WorkspaceID, "", field+".workspace_id")
	c.id(paneIDRe, p.ID, p.WorkspaceID, field+".pane_id")
	c.id(tabIDRe, p.TabID, p.WorkspaceID, field+".tab_id")
	if s := r.AgentSession; s != nil {
		f := field + ".agent_session"
		p.AgentSession = &AgentSession{
			Agent:  req(&c, s.Agent, f+".agent"),
			Source: req(&c, s.Source, f+".source"),
			Kind:   req(&c, s.Kind, f+".kind"),
			Value:  req(&c, s.Value, f+".value"),
		}
	}
	if c.err != nil {
		return Pane{}, c.err
	}
	return p, nil
}

type rawWorkspace struct {
	WorkspaceID *string `json:"workspace_id"`
	Label       *string `json:"label"`
	Number      *uint   `json:"number"`
}

func (r rawWorkspace) parse(c *check, field string) Workspace {
	w := Workspace{
		ID:     req(c, r.WorkspaceID, field+".workspace_id"),
		Label:  req(c, r.Label, field+".label"),
		Number: req(c, r.Number, field+".number"),
	}
	c.id(workspaceIDRe, w.ID, "", field+".workspace_id")
	return w
}

type rawSession struct {
	Name       *string `json:"name"`
	Default    *bool   `json:"default"`
	Running    *bool   `json:"running"`
	SessionDir *string `json:"session_dir"`
	SocketPath *string `json:"socket_path"`
}

func (r rawSession) parse(c *check, field string) Session {
	s := Session{
		Name:    req(c, r.Name, field+".name"),
		Default: req(c, r.Default, field+".default"),
		Running: req(c, r.Running, field+".running"),
		Dir:     req(c, r.SessionDir, field+".session_dir"),
		Socket:  req(c, r.SocketPath, field+".socket_path"),
	}
	if c.err != nil {
		return s
	}
	if s.Name == "" {
		c.fail(field+".name", "empty")
	}
	if !filepath.IsAbs(s.Dir) {
		c.fail(field+".session_dir", fmt.Sprintf("%q is not absolute", s.Dir))
	}
	if !filepath.IsAbs(s.Socket) {
		c.fail(field+".socket_path", fmt.Sprintf("%q is not absolute", s.Socket))
	}
	return s
}
