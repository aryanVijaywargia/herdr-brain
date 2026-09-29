package herdr

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Event lines as herdr 0.9.1 sent them in the M0 probe (spike/m0/e11probe).
func TestParseEvent(t *testing.T) {
	tests := []struct {
		name string
		line string
		want Event
	}{
		{"created", `{"event":"pane_created","data":{"type":"pane_created","pane":{"pane_id":"w1:p2","workspace_id":"w1","tab_id":"w1:t1","agent_status":"unknown"}}}`,
			Event{Kind: PaneCreated, PaneID: "w1:p2", WorkspaceID: "w1"}},
		{"detected", `{"data":{"agent":"claude","pane_id":"w1:p1","type":"pane_agent_detected","workspace_id":"w1"},"event":"pane_agent_detected"}`,
			Event{Kind: PaneAgentDetected, PaneID: "w1:p1", WorkspaceID: "w1", Agent: "claude"}},
		{"released", `{"data":{"agent":"claude","final_status":"idle","pane_id":"w1:p1","released":true,"workspace_id":"w1"},"event":"pane_agent_detected"}`,
			Event{Kind: PaneAgentDetected, PaneID: "w1:p1", WorkspaceID: "w1", Agent: "claude", Released: true}},
		{"status", `{"data":{"agent":"claude","agent_status":"idle","pane_id":"w1:p1","workspace_id":"w1"},"event":"pane.agent_status_changed"}`,
			Event{Kind: PaneStatusChanged, PaneID: "w1:p1", WorkspaceID: "w1", Agent: "claude", Status: "idle"}},
		{"exited", `{"event":"pane_exited","data":{"pane_id":"w1:p1","workspace_id":"w1"}}`,
			Event{Kind: PaneExited, PaneID: "w1:p1", WorkspaceID: "w1"}},
		{"closed", `{"event":"pane_closed","data":{"pane_id":"w1:p1","workspace_id":"w1"}}`,
			Event{Kind: PaneClosed, PaneID: "w1:p1", WorkspaceID: "w1"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseEvent([]byte(tt.line))
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Errorf("parseEvent = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// An event that Brain does not know is a ShapeError, never a zero event.
func TestParseEventShape(t *testing.T) {
	for _, line := range []string{
		`not json`,
		`{"event":"pane_moved","data":{"pane_id":"w1:p1","workspace_id":"w1"}}`,
		`{"event":"pane.agent_status_changed","data":{"pane_id":"w1:p1","workspace_id":"w1"}}`,
		`{"event":"pane_closed","data":{"pane_id":"w2:p1","workspace_id":"w1"}}`,
		`{"event":"pane_closed"}`,
	} {
		var se *ShapeError
		if _, err := parseEvent([]byte(line)); !errors.As(err, &se) {
			t.Errorf("parseEvent(%s) error = %v, want a ShapeError", line, err)
		}
	}
}

// serveOne answers one connection on a new socket: it checks the request method, writes
// replies and then closes. It returns the socket path.
func serveOne(t *testing.T, method string, replies ...string) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "hs") // short: a socket path has a limit of about 104 bytes
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "s")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	t.Cleanup(func() { _ = ln.Close(); <-done })
	go func() {
		defer close(done)
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close() //nolint:errcheck // test server
		line, err := bufio.NewReader(conn).ReadBytes('\n')
		if err != nil {
			t.Error(err)
			return
		}
		var req struct {
			Method string `json:"method"`
		}
		if err := json.Unmarshal(line, &req); err != nil || req.Method != method {
			t.Errorf("request = %s, want method %s", line, method)
		}
		for _, r := range replies {
			if _, err := io.WriteString(conn, r+"\n"); err != nil {
				t.Error(err)
			}
		}
	}()
	return path
}

func testCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// A stream gives the events after subscription_started, then io.EOF when herdr closes it.
func TestSubscribe(t *testing.T) {
	path := serveOne(t, "events.subscribe",
		`{"id":"brain","result":{"type":"subscription_started"}}`,
		`{"event":"pane_closed","data":{"pane_id":"w1:p1","workspace_id":"w1"}}`)
	st, err := Socket{Path: path}.Subscribe(testCtx(t), []Subscription{{Type: PaneClosed}})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close() //nolint:errcheck // test
	ev, err := st.Next()
	if err != nil || ev.Kind != PaneClosed {
		t.Errorf("Next = %+v, %v, want pane.closed", ev, err)
	}
	if _, err := st.Next(); !errors.Is(err, io.EOF) {
		t.Errorf("Next at the end = %v, want io.EOF", err)
	}
}

// herdr refuses a status subscription for a pane that is gone.
func TestSubscribeRefused(t *testing.T) {
	path := serveOne(t, "events.subscribe", `{"id":"brain","error":{"code":"pane_not_found","message":"pane w1:p9 not found"}}`)
	_, err := Socket{Path: path}.Subscribe(testCtx(t), []Subscription{{Type: PaneStatusChanged, PaneID: "w1:p9"}})
	if !errors.Is(err, ErrPaneNotFound) {
		t.Errorf("Subscribe = %v, want ErrPaneNotFound", err)
	}
}

func TestPlugins(t *testing.T) {
	path := serveOne(t, "plugin.list",
		`{"id":"brain","result":{"type":"plugin_list","plugins":[{"plugin_id":"brain","enabled":false,"name":"brain"}]}}`)
	got, err := Socket{Path: path}.Plugins(testCtx(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != (Plugin{ID: "brain", Enabled: false}) {
		t.Errorf("Plugins = %+v, want brain, disabled", got)
	}
}

func TestSocketPanes(t *testing.T) {
	path := serveOne(t, "pane.list",
		`{"id":"brain","result":{"type":"pane_list","panes":[{"pane_id":"w1:p1","workspace_id":"w1","tab_id":"w1:t1","agent":"claude"}]}}`)
	got, err := Socket{Path: path}.Panes(testCtx(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "w1:p1" || got[0].Agent == nil || *got[0].Agent != "claude" {
		t.Errorf("Panes = %+v, want w1:p1 with claude", got)
	}
}
