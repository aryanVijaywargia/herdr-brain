package cli

import (
	"bytes"
	"context"
	"database/sql"
	"reflect"
	"testing"

	"github.com/aryanVijaywargia/herdr-brain/internal/herdr"
	"github.com/aryanVijaywargia/herdr-brain/internal/resolver"
	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

// TestOpenChecksAgents: a command in a pane checks the (pane, agent session id) records of
// earlier hook runs there against the agent that herdr shows now.
func TestOpenChecksAgents(t *testing.T) {
	pending, unknown := string(resolver.AgentPending), string(resolver.AgentUnknown)
	tests := []struct {
		name  string
		agent *herdr.AgentSession // the agent session that herdr shows in w14:p1
		want  map[string]string   // agent session id → check
	}{
		{
			// A nested `claude -p` in the pane saw the parent's agent_session.
			name:  "nested run",
			agent: &herdr.AgentSession{Agent: "claude", Source: "herdr:claude", Kind: "id", Value: "sess-1"},
			want:  map[string]string{"sess-1": string(resolver.AgentMatch), "nested-1": string(resolver.NotThisPanesAgent), "other-pane": pending},
		},
		{
			name: "no agent_session",
			want: map[string]string{"sess-1": unknown, "nested-1": unknown, "other-pane": pending},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			s := openStore(t, f.store)
			write(t, s, func(tx *sql.Tx) error {
				for id, pane := range map[string]string{"sess-1": "w14:p1", "nested-1": "w14:p1", "other-pane": "w16:p1"} {
					_, err := store.InsertDelivery(context.Background(), tx, store.Delivery{Profile: "claude",
						HerdrSession: ptr("default"), PaneID: pane, AgentSessionID: ptr(id), ItemRevisions: []store.RevisionRef{},
						Result: store.ResultEmitted, At: testNow, AgentCheck: ptr(pending)})
					if err != nil {
						return err
					}
				}
				return nil
			})
			closeStore(t, s)
			p := f.panes["w14:p1"]
			p.AgentSession = tt.agent
			f.panes["w14:p1"] = p

			c, err := f.env(f.sock, "w14:p1", "w14", &bytes.Buffer{}).Open(context.Background())
			if err != nil {
				t.Fatalf("Open() = %v, want nil", err)
			}
			got := map[string]string{}
			write(t, c.Store, func(tx *sql.Tx) error {
				for id := 1; id <= 3; id++ {
					d, err := store.DeliveryByID(context.Background(), tx, int64(id))
					if err != nil {
						return err
					}
					got[*d.AgentSessionID] = *d.AgentCheck
				}
				return nil
			})
			if err := c.Close(); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("checks = %v, want %v", got, tt.want)
			}
		})
	}
}
