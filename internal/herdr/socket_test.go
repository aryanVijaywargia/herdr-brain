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
	"reflect"
	"strings"
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

func TestSocketPane(t *testing.T) {
	reply, err := os.ReadFile("testdata/pane-agent.json")
	if err != nil {
		t.Fatal(err)
	}
	path := serveOne(t, "pane.get", strings.TrimSpace(string(reply)))
	got, err := Socket{Path: path}.Pane(testCtx(t), "w14:p2F")
	if err != nil {
		t.Fatal(err)
	}
	if got.WorkspaceID != "w14" || got.AgentSession == nil || got.AgentSession.Value != "6a345ec1-3050-4e61-b736-d0e9d2edab82" {
		t.Errorf("Pane = %+v, want w14 with its agent session", got)
	}
}

func TestSocketPaneGone(t *testing.T) {
	path := serveOne(t, "pane.get", `{"id":"brain","error":{"code":"pane_not_found","message":"pane w1:p9 not found"}}`)
	if _, err := (Socket{Path: path}).Pane(testCtx(t), "w1:p9"); !errors.Is(err, ErrPaneNotFound) {
		t.Errorf("Pane = %v, want ErrPaneNotFound", err)
	}
}

func TestSocketRead(t *testing.T) {
	path := serveOne(t, "pane.read",
		`{"id":"brain","result":{"type":"pane_read","read":{"pane_id":"w1:p2","workspace_id":"w1","tab_id":"w1:t1","source":"recent_unwrapped","format":"text","text":"a\nb\n","revision":7,"truncated":false}}}`)
	got, err := Socket{Path: path}.Read(testCtx(t), "w1:p2", ReadRecentUnwrapped, 120)
	if err != nil {
		t.Fatal(err)
	}
	want := PaneRead{PaneID: "w1:p2", WorkspaceID: "w1", Source: ReadRecentUnwrapped, Text: "a\nb\n", Revision: 7}
	if got != want {
		t.Errorf("Read = %+v, want %+v", got, want)
	}
}

func TestSocketReadShape(t *testing.T) {
	path := serveOne(t, "pane.read",
		`{"id":"brain","result":{"type":"pane_read","read":{"pane_id":"w1:p2","workspace_id":"w1","source":"visible","format":"text","revision":7,"truncated":false}}}`)
	var se *ShapeError
	if _, err := (Socket{Path: path}).Read(testCtx(t), "w1:p2", ReadVisible, 0); !errors.As(err, &se) || se.Field != "read.text" {
		t.Errorf("Read = %v, want a ShapeError on read.text", err)
	}
}

// The reply shape of herdr 0.9.1 pane.process_info; argv may be null.
func TestSocketProcesses(t *testing.T) {
	path := serveOne(t, "pane.process_info",
		`{"id":"brain","result":{"type":"pane_process_info","process_info":{"foreground_process_group_id":80590,"foreground_processes":[`+
			`{"argv":["codex","--version"],"argv0":"codex","cmdline":"codex --version","cwd":"/tmp","name":"codex","pid":80590},`+
			`{"argv":null,"name":"zsh","pid":80255}],"pane_id":"w1:p2","shell_pid":null,"tty":null}}}`)
	got, err := Socket{Path: path}.Processes(testCtx(t), "w1:p2")
	if err != nil {
		t.Fatal(err)
	}
	want := []Process{{Name: "codex", Argv: []string{"codex", "--version"}, PID: 80590}, {Name: "zsh", PID: 80255}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Processes = %+v, want %+v", got, want)
	}
}

func TestSocketProcessesShape(t *testing.T) {
	for name, reply := range map[string]string{
		"other pane": `{"id":"brain","result":{"type":"pane_process_info","process_info":{"foreground_processes":[],"pane_id":"w1:p3"}}}`,
		"no pid":     `{"id":"brain","result":{"type":"pane_process_info","process_info":{"foreground_processes":[{"name":"codex"}],"pane_id":"w1:p2"}}}`,
	} {
		path := serveOne(t, "pane.process_info", reply)
		var se *ShapeError
		if _, err := (Socket{Path: path}).Processes(testCtx(t), "w1:p2"); !errors.As(err, &se) {
			t.Errorf("%s: Processes = %v, want a ShapeError", name, err)
		}
	}
}
