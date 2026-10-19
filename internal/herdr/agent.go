package herdr

import (
	"context"
	"fmt"
)

// The agent statuses that the automation runner reads (E17-S1 test).
const (
	StatusWorking = "working"
	StatusIdle    = "idle"
	StatusDone    = "done"
)

// Agent is the agent state of one pane, from agent.get or agent.list. herdr gives
// state_change_seq only here, not in pane.get or pane.list (E17-S1 test).
type Agent struct {
	PaneID      string
	WorkspaceID string
	Agent       string // the harness; "" when herdr names none
	Status      string // agent_status
	Session     string // the herdr agent session id; "" when herdr has none
	Cwd         string // "" when herdr does not know it
	TerminalID  string // changes at a herdr server restart (E17-S1 test)
	Seq         uint64 // state_change_seq: one counter for the whole herdr server
}

type rawAgent struct {
	PaneID       *string          `json:"pane_id"`
	WorkspaceID  *string          `json:"workspace_id"`
	Agent        *string          `json:"agent"`
	Status       *string          `json:"agent_status"`
	AgentSession *rawAgentSession `json:"agent_session"`
	Cwd          *string          `json:"cwd"`
	TerminalID   *string          `json:"terminal_id"`
	Seq          *uint64          `json:"state_change_seq"`
}

func (r *rawAgent) parse(c *check, field string) Agent {
	if r == nil {
		c.fail(field, "missing")
		return Agent{}
	}
	a := Agent{
		PaneID:      req(c, r.PaneID, field+".pane_id"),
		WorkspaceID: req(c, r.WorkspaceID, field+".workspace_id"),
		Status:      req(c, r.Status, field+".agent_status"),
		TerminalID:  req(c, r.TerminalID, field+".terminal_id"),
	}
	if r.Agent != nil {
		a.Agent = *r.Agent
	}
	if r.Cwd != nil {
		a.Cwd = *r.Cwd
	}
	if r.Seq != nil { // the schema default is 0
		a.Seq = *r.Seq
	}
	if s := r.AgentSession; s != nil {
		a.Session = req(c, s.Value, field+".agent_session.value")
	}
	c.id(workspaceIDRe, a.WorkspaceID, "", field+".workspace_id")
	c.id(paneIDRe, a.PaneID, a.WorkspaceID, field+".pane_id")
	return a
}

// Agent returns the agent state of pane id, from agent.get. Brain calls it at once for each
// status event, so the event joins the state that herdr has now.
func (s Socket) Agent(ctx context.Context, id string) (Agent, error) {
	if err := checkPaneID(id); err != nil {
		return Agent{}, err
	}
	var r struct {
		Agent *rawAgent `json:"agent"`
	}
	if err := s.call(ctx, "agent.get", map[string]any{"target": id}, "agent_info", &r); err != nil {
		return Agent{}, fmt.Errorf("herdr agent.get: %w", err)
	}
	var ck check
	a := r.Agent.parse(&ck, "agent")
	if ck.err != nil {
		return Agent{}, fmt.Errorf("herdr agent.get: %w", ck.err)
	}
	return a, nil
}

// Agents returns the agents of the target session, from `herdr agent list`.
func (c *Client) Agents(ctx context.Context) ([]Agent, error) {
	var r struct {
		Agents *[]*rawAgent `json:"agents"`
	}
	if err := c.result(ctx, "agent_list", &r, "agent", "list"); err != nil {
		return nil, fmt.Errorf("herdr agent list: %w", err)
	}
	var ck check
	raw := req(&ck, r.Agents, "agents")
	as := make([]Agent, len(raw))
	for i, ra := range raw {
		as[i] = ra.parse(&ck, fmt.Sprintf("agents[%d]", i))
	}
	if ck.err != nil {
		return nil, fmt.Errorf("herdr agent list: %w", ck.err)
	}
	return as, nil
}

// SetPaneToken sets the display token name of pane id to value for metadata source source, or
// clears it when value is "", with `herdr pane report-metadata`. The token is display only.
func (c *Client) SetPaneToken(ctx context.Context, id, source, name, value string) error {
	if err := checkPaneID(id); err != nil {
		return fmt.Errorf("herdr pane report-metadata: %w", err)
	}
	args := []string{"pane", "report-metadata", id, "--source", source, "--clear-token", name}
	if value != "" {
		args = []string{"pane", "report-metadata", id, "--source", source, "--token", name + "=" + value}
	}
	if err := c.result(ctx, "ok", &struct{}{}, args...); err != nil {
		return fmt.Errorf("herdr pane report-metadata: %w", err)
	}
	return nil
}
