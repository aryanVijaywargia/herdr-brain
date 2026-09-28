package hook

import (
	"bytes"
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/aryanVijaywargia/herdr-brain/internal/herdr"
	"github.com/aryanVijaywargia/herdr-brain/internal/resolver"
)

// record is the (herdr session, agent session id) record of a receipt with its check.
type record struct {
	session, agentSession string
	check                 resolver.AgentCheck
}

// TestAgentCheck: a hook run records (pane, agent session id), and the next run for the pane
// checks it against the agent that herdr shows then (§8.3 limit, §17 item 29).
func TestAgentCheck(t *testing.T) {
	const (
		unknown = resolver.AgentUnknown
		match   = resolver.AgentMatch
		nested  = resolver.NotThisPanesAgent
	)
	tests := []struct {
		name  string
		agent string // the agent session that herdr shows in w14:p1 after the first run; "" for none
		want  []record
	}{
		{
			// A nested `claude -p` in the pane sees the parent's agent_session.
			name:  "nested run",
			agent: "sess-1",
			want: []record{{"work", "sess-w", unknown}, {"default", "sess-1", match},
				{"default", "nested-1", nested}, {"default", "nested-2", unknown}},
		},
		{
			name: "no agent_session",
			want: []record{{"work", "sess-w", unknown}, {"default", "sess-1", unknown},
				{"default", "nested-1", unknown}, {"default", "nested-2", unknown}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			run := func(sock, session, id string) {
				t.Helper()
				in := fmt.Sprintf(`{"session_id":%q,"cwd":"/tmp/x"}`, id)
				var out bytes.Buffer
				if err := f.hook(claude(t), environ(sock, session, "w14:p1", "w14")).Run(context.Background(), strings.NewReader(in), &out); err != nil {
					t.Fatalf("Run(%s) = %v, want nil", id, err)
				}
			}
			// The same pane id in herdr session work shares the main store; its record is not
			// checked against the agent of default's w14:p1.
			run(f.workSock, "work", "sess-w")
			run(f.mainSock, "", "sess-1")
			if tt.agent != "" {
				p := f.herdr.panes[f.mainSock]["w14:p1"]
				p.AgentSession = &herdr.AgentSession{Agent: "claude", Source: "herdr:claude", Kind: "id", Value: tt.agent}
				f.herdr.panes[f.mainSock]["w14:p1"] = p
			}
			run(f.mainSock, "", "nested-1")
			run(f.mainSock, "", "nested-2")

			var got []record
			for _, d := range deliveries(t, f.mainStore) {
				got = append(got, record{*d.HerdrSession, *d.AgentSessionID, resolver.AgentCheck(*d.AgentCheck)})
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("records = %v, want %v", got, tt.want)
			}
		})
	}
}
