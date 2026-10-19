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

// step is one hook run in w14:p1 of herdr session default, while herdr shows agent there.
type step struct {
	agent string // the agent session that herdr shows at the run; "" for none
	id    string // the hook session_id
}

// TestAgentCheck: a hook run records (pane, agent session id), and the next run for the pane
// checks it against the agent that herdr shows then.
func TestAgentCheck(t *testing.T) {
	const (
		pending = resolver.AgentPending
		unknown = resolver.AgentUnknown
		match   = resolver.AgentMatch
		nested  = resolver.NotThisPanesAgent
	)
	tests := []struct {
		name  string
		steps []step
		want  []record // after the record of session work, which no step checks
	}{
		{
			// A nested `claude -p` in the pane sees the parent's agent_session.
			name:  "nested run",
			steps: []step{{"", "sess-1"}, {"sess-1", "nested-1"}, {"sess-1", "nested-2"}},
			want:  []record{{"default", "sess-1", match}, {"default", "nested-1", nested}, {"default", "nested-2", pending}},
		},
		{
			name:  "no agent_session",
			steps: []step{{"", "sess-1"}, {"", "nested-1"}},
			want:  []record{{"default", "sess-1", unknown}, {"default", "nested-1", pending}},
		},
		{
			// Agent A exits with no brain call, and agent B starts in the same pane. B's hook
			// checks A; a later call sees B and checks B only, not A against B.
			name:  "sequential agents",
			steps: []step{{"", "sess-a"}, {"", "sess-b"}, {"sess-b", "nested-1"}},
			want:  []record{{"default", "sess-a", unknown}, {"default", "sess-b", match}, {"default", "nested-1", pending}},
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
			for _, s := range tt.steps {
				p := f.herdr.panes[f.mainSock]["w14:p1"]
				p.AgentSession = nil
				if s.agent != "" {
					p.AgentSession = &herdr.AgentSession{Agent: "claude", Source: "herdr:claude", Kind: "id", Value: s.agent}
				}
				f.herdr.panes[f.mainSock]["w14:p1"] = p
				run(f.mainSock, "", s.id)
			}

			var got []record
			for _, d := range deliveries(t, f.mainStore) {
				got = append(got, record{*d.HerdrSession, *d.AgentSessionID, resolver.AgentCheck(*d.AgentCheck)})
			}
			want := append([]record{{"work", "sess-w", pending}}, tt.want...)
			if !reflect.DeepEqual(got, want) {
				t.Errorf("records = %v, want %v", got, want)
			}
		})
	}
}
