package herdr

import (
	"reflect"
	"strings"
	"testing"
)

// E17-S11: agent.get gives the state of the event join and the event key (E17-S1 test).
func TestSocketAgent(t *testing.T) {
	path := serveOne(t, "agent.get", strings.TrimSpace(fixture(t, "agent-get.json")))
	got, err := Socket{Path: path}.Agent(testCtx(t), "w1:p1")
	if err != nil {
		t.Fatal(err)
	}
	want := Agent{PaneID: "w1:p1", WorkspaceID: "w1", Agent: "codex", Status: StatusIdle,
		Session: "01a0ec3e-5e95-7253-aa8d-e7d232139183", Cwd: "/private/tmp/e11s1b/agent", TerminalID: "term_65c9acb584b091",
		Seq: 8}
	if got != want {
		t.Errorf("Agent(w1:p1) = %+v, want %+v", got, want)
	}
}

func TestSocketAgentShape(t *testing.T) {
	path := serveOne(t, "agent.get",
		`{"id":"brain","result":{"type":"agent_info","agent":{"pane_id":"w1:p1","workspace_id":"w1","agent_status":"idle","state_change_seq":3}}}`)
	_, err := Socket{Path: path}.Agent(testCtx(t), "w1:p1")
	wantShape(t, err, "agent.terminal_id")
}

func TestAgents(t *testing.T) {
	c, dir := fakeClient(t, map[string]reply{"agent list": {Stdout: fixture(t, "agent-list.json")}})
	got, err := c.Agents(deadline(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Session == "" || got[1].Agent != "opencode" || got[1].Session != "" ||
		got[1].Status != StatusWorking || got[1].Cwd != "/private/tmp/e16s2/opencode" || got[1].PaneID != "w2:p1" {
		t.Errorf("Agents() = %+v, want the Codex agent with a session and the OpenCode agent without one", got)
	}
	if cs := calls(t, dir); len(cs) != 1 || !reflect.DeepEqual(cs[0].Args, []string{"agent", "list"}) {
		t.Errorf("herdr calls = %+v, want agent list", cs)
	}
}

// The pane label of an automation run is a display token; "" clears it.
func TestSetPaneToken(t *testing.T) {
	ok := `{"id":"cli:pane:report-metadata","result":{"type":"ok"}}`
	c, dir := fakeClient(t, map[string]reply{
		"pane report-metadata w1:p1 --source brain --token brain-automation=lint failed": {Stdout: ok},
		"pane report-metadata w1:p1 --source brain --clear-token brain-automation":       {Stdout: ok},
	})
	if err := c.SetPaneToken(deadline(t), "w1:p1", "brain", "brain-automation", "lint failed"); err != nil {
		t.Errorf("SetPaneToken(lint failed) = %v", err)
	}
	if err := c.SetPaneToken(deadline(t), "w1:p1", "brain", "brain-automation", ""); err != nil {
		t.Errorf("SetPaneToken(\"\") = %v", err)
	}
	if cs := calls(t, dir); len(cs) != 2 {
		t.Errorf("herdr calls = %+v, want 2", cs)
	}
	if err := c.SetPaneToken(deadline(t), "p1", "brain", "brain-automation", ""); err == nil {
		t.Error("SetPaneToken(p1) = nil, want a malformed id error")
	}
}
