package main

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aryanVijaywargia/herdr-brain/internal/bindings"
	"github.com/aryanVijaywargia/herdr-brain/internal/launch"
	"github.com/aryanVijaywargia/herdr-brain/internal/paths"
	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

// mcpHome is a launch home whose herdr session s1 has an API socket that confirms each pane
// wN:pM in workspace wN, and a herdr CLI that does the same and lists the workspaces w1 and
// w2. The workspace map gives w1 the key ws_a1 and w2 the key ws_a2.
type mcpHome struct {
	*launchHome
	b bindings.Binding
}

func newMCPHome(t *testing.T) *mcpHome {
	t.Helper()
	h := &mcpHome{launchHome: newLaunchHome(t)}
	dir, err := os.MkdirTemp("", "bm") // short: a socket path has a limit of about 104 bytes
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "s")
	serveHerdr(t, sock)
	h.b = bindings.Binding{Session: "s1", Socket: sock, Store: h.store, Kind: bindings.KindMain, Created: time.Now()}
	if err := bindings.Write(filepath.Join(h.home, ".local", "share", "brain", "bindings.json"), []bindings.Binding{h.b}); err != nil {
		t.Fatal(err)
	}
	pane := `{"id":"cli:pane:get","result":{"type":"pane_info","pane":{"pane_id":"'"$3"'","tab_id":"'"$ws"':t1","workspace_id":"'"$ws"'"}}}`
	list := `{"id":"cli:workspace:list","result":{"type":"workspace_list","workspaces":[` +
		`{"workspace_id":"w1","label":"one","number":1},{"workspace_id":"w2","label":"two","number":2}]}}`
	script := "#!/bin/sh\ncase \"$1 $2\" in\n\"pane get\") ws=${3%%:*}; echo '" + pane + "' ;;\n" +
		"\"workspace list\") echo '" + list + "' ;;\n*) exit 1 ;;\nesac\n"
	if err := os.WriteFile(filepath.Join(h.home, "herdr", "herdr"), []byte(script), 0o700); err != nil { //nolint:gosec // the fake herdr must be executable
		t.Fatal(err)
	}
	type entry struct {
		Key         string `json:"key"`
		Session     string `json:"session"`
		WorkspaceID string `json:"workspace_id"`
		Label       string `json:"label"`
		Project     string `json:"project"`
	}
	writeJSONFile(t, paths.WorkspaceMap(h.store), map[string]any{"version": 1,
		"entries": []entry{{"ws_a1", "s1", "w1", "one", h.home}, {"ws_a2", "s1", "w2", "two", h.home}}})
	// herdr's session.json in its session folder, the folder of the socket: the saved roots.
	writeJSONFile(t, filepath.Join(dir, "session.json"), map[string]any{"version": 3,
		"workspaces": []map[string]string{{"id": "w1", "identity_cwd": h.home}, {"id": "w2", "identity_cwd": h.home}}})
	h.write(t, func(ctx context.Context, tx *sql.Tx) error {
		for _, w := range []string{"w1", "w2"} {
			if err := store.InsertWorkspace(ctx, tx, store.Workspace{Key: "ws_a" + w[1:], HerdrSession: "s1", HerdrWorkspaceID: w,
				Status: store.WorkspaceLive, LastSeen: time.Now()}); err != nil {
				return err
			}
		}
		return nil
	})
	h.env = with(h.env, "HERDR_SOCKET_PATH="+sock)
	return h
}

func writeJSONFile(t *testing.T, path string, v any) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// serveHerdr serves a herdr API socket at sock until the test ends. It answers each pane.get
// with the pane in the workspace of its id prefix.
func serveHerdr(t *testing.T, sock string) {
	t.Helper()
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			answerPane(conn)
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		wg.Wait()
	})
}

func answerPane(conn net.Conn) {
	defer conn.Close() //nolint:errcheck // test
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		return
	}
	var req struct {
		Params struct {
			PaneID string `json:"pane_id"`
		} `json:"params"`
	}
	if err := json.Unmarshal(line, &req); err != nil {
		return
	}
	id := req.Params.PaneID
	ws, _, _ := strings.Cut(id, ":")
	_, _ = fmt.Fprintf(conn, `{"id":"brain","result":{"type":"pane_info","pane":{"pane_id":%q,"workspace_id":%q,"tab_id":"%s:t1","agent":null}}}`+"\n",
		id, ws, ws)
}

// write runs fn in a write transaction of the store of h.
func (h *mcpHome) write(t *testing.T, fn func(context.Context, *sql.Tx) error) {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, h.store, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close() //nolint:errcheck // test
	if err := st.Write(ctx, func(tx *sql.Tx) error { return fn(ctx, tx) }); err != nil {
		t.Fatal(err)
	}
}

// launch makes the launch record of the CLI of kind in pane id, and returns its folder.
func (h *mcpHome) launch(t *testing.T, kind, id string) string {
	t.Helper()
	ws, _, _ := strings.Cut(id, ":")
	dir, err := launch.Create(context.Background(), h.b, ws, id, kind, "", os.Getpid(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// codexEnv is the env that Codex gives the Brain server: its default names and env_vars.
func (h *mcpHome) codexEnv() []string {
	return []string{"HOME=" + h.home, "PATH=" + h.fakes, "BRAIN_TEST_MAIN=1", "HERDR_BIN_PATH=" + filepath.Join(h.home, "herdr", "herdr")}
}

// mcpServer is a running `brain mcp --launch <folder>`: the test binary as brain.
type mcpServer struct {
	in   io.WriteCloser
	out  *bufio.Reader
	next int
}

// serve starts `brain mcp --launch dir` with env. The test ends it: it closes stdin, and the
// server must then exit 0.
func (h *mcpHome) serve(t *testing.T, env []string, dir string) *mcpServer {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	cmd := exec.CommandContext(ctx, self, "mcp", "--launch", dir)
	cmd.Env, cmd.Dir = env, h.home
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		defer cancel()
		_ = in.Close() // the wait below reports a server that did not stop
		if err := cmd.Wait(); err != nil {
			t.Errorf("brain mcp: %v\nstderr %s", err, stderr.String())
		}
	})
	return &mcpServer{in: in, out: bufio.NewReader(out)}
}

// ask sends one request and returns its result.
func (s *mcpServer) ask(t *testing.T, method string, params any) json.RawMessage {
	t.Helper()
	s.next++
	b, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": s.next, "method": method, "params": params})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.in.Write(append(b, '\n')); err != nil {
		t.Fatal(err)
	}
	line, err := s.out.ReadBytes('\n')
	if err != nil {
		t.Fatalf("%s: %v", method, err)
	}
	var a struct {
		ID     int             `json:"id"`
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(line, &a); err != nil || a.ID != s.next || a.Result == nil {
		t.Fatalf("%s = %s (%v), want the result of request %d", method, line, err, s.next)
	}
	return a.Result
}

// start sends initialize and its notification.
func (s *mcpServer) start(t *testing.T) {
	t.Helper()
	var r struct {
		ServerInfo struct {
			Name string `json:"name"`
		} `json:"serverInfo"`
	}
	res := s.ask(t, "initialize", map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{},
		"clientInfo": map[string]any{"name": "test", "version": "1"}})
	if err := json.Unmarshal(res, &r); err != nil || r.ServerInfo.Name == "" {
		t.Fatalf("initialize = %s, want the server info", res)
	}
	if _, err := s.in.Write([]byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}` + "\n")); err != nil {
		t.Fatal(err)
	}
}

// tools returns the names of tools/list.
func (s *mcpServer) tools(t *testing.T) []string {
	t.Helper()
	var r struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(s.ask(t, "tools/list", map[string]any{}), &r); err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for _, tl := range r.Tools {
		names = append(names, tl.Name)
	}
	return names
}

// call calls tool name with args, and returns its text and whether it failed.
func (s *mcpServer) call(t *testing.T, name string, args any) (string, bool) {
	t.Helper()
	var r struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(s.ask(t, "tools/call", map[string]any{"name": name, "arguments": args}), &r); err != nil || len(r.Content) != 1 {
		t.Fatalf("tools/call %s = %+v (%v), want one text", name, r, err)
	}
	return r.Content[0].Text, r.IsError
}

// gatewayTools are the four tools of the gateway, in list order.
var gatewayTools = []string{"find_tool", "run_tool", "recall", "remember"}

// AC #446: the server of a launched session (`brain mcp --launch <folder>`, here with the env
// of a Claude server) answers initialize and lists the four tools. After the launch record
// ends, tools/list is empty and each call is refused.
func TestMCPLaunch(t *testing.T) {
	h := newMCPHome(t)
	s := h.serve(t, h.env, h.launch(t, "claude", "w1:p1"))
	s.start(t)
	if got := s.tools(t); !slices.Equal(got, gatewayTools) {
		t.Fatalf("tools/list = %q, want %q", got, gatewayTools)
	}
	all := func(store.Launch) bool { return true }
	if err := launch.End(context.Background(), h.store, h.b.Socket, all, store.EndAgentExit, time.Now()); err != nil {
		t.Fatal(err)
	}
	if got := s.tools(t); len(got) != 0 {
		t.Errorf("tools/list after the end = %q, want none", got)
	}
	for _, name := range gatewayTools {
		args := map[string]any{"query": "x"}
		switch name {
		case "run_tool":
			args = map[string]any{"tool": "x"}
		case "remember":
			args = map[string]any{"text": "a fact"}
		}
		if text, isErr := s.call(t, name, args); !isErr || !strings.HasPrefix(text, "refused: no valid Brain launch") {
			t.Errorf("tools/call %s after the end = %q, error %t; want a refusal", name, text, isErr)
		}
	}
}

// AC #446: a bad form of the command is an error, and no server starts.
func TestMCPLaunchUsage(t *testing.T) {
	h := newMCPHome(t)
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"mcp"}, {"mcp", "--launch"}, {"mcp", "--launch", ""}, {"mcp", "--x", "y"}} {
		cmd := exec.Command(self, args...)
		cmd.Env, cmd.Dir = h.env, h.home
		out, err := cmd.CombinedOutput()
		if err == nil || !strings.Contains(string(out), "want --launch <folder>") {
			t.Errorf("brain %q = %v, %s; want a usage error", args, err, out)
		}
	}
}

// AC #446: two Codex panes in two workspaces, each with its server and the env that Codex
// gives a server: each call goes to the workspace of its own launch.
func TestMCPLaunchWorkspaces(t *testing.T) {
	h := newMCPHome(t)
	panes := []struct{ pane, key string }{{"w1:p1", "ws_a1"}, {"w2:p1", "ws_a2"}}
	servers := make([]*mcpServer, len(panes))
	for i, tt := range panes {
		servers[i] = h.serve(t, h.codexEnv(), h.launch(t, "codex", tt.pane))
		servers[i].start(t)
	}
	for i, tt := range panes {
		s := servers[i]
		if got := s.tools(t); !slices.Equal(got, gatewayTools) {
			t.Fatalf("pane %s: tools/list = %q, want %q", tt.pane, got, gatewayTools)
		}
		text := "a fact of " + tt.pane
		if out, isErr := s.call(t, "remember", map[string]any{"text": text}); isErr {
			t.Fatalf("pane %s: remember = %q, want a draft", tt.pane, out)
		}
		var keys []string
		h.write(t, func(ctx context.Context, tx *sql.Tx) error {
			for id := int64(1); ; id++ {
				it, err := store.ItemByID(ctx, tx, id)
				if errors.Is(err, sql.ErrNoRows) {
					return nil
				}
				if err != nil {
					return err
				}
				if it.WorkspaceKey != nil {
					keys = append(keys, *it.WorkspaceKey)
				}
			}
		})
		if len(keys) == 0 || keys[len(keys)-1] != tt.key {
			t.Errorf("pane %s: the workspace keys of the drafts = %q, want the last in %s", tt.pane, keys, tt.key)
		}
	}
}
