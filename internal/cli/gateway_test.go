package cli

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aryanVijaywargia/herdr-brain/internal/herdr"
	"github.com/aryanVijaywargia/herdr-brain/internal/mcp"
	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

// gatewayNames are the four gateway tools, in list order.
var gatewayNames = []string{"find_tool", "run_tool", "recall", "remember"}

// mcpClient is the client end of an in-memory pipe to mcp.Serve with a gateway.
type mcpClient struct {
	w    *io.PipeWriter
	r    *bufio.Reader
	next int
	done chan error
}

// serveGateway serves the gateway of e with identify on an in-memory pipe, until the test ends.
func serveGateway(t *testing.T, e Env, identify Identify) *mcpClient {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	c := &mcpClient{w: inW, r: bufio.NewReader(outR), done: make(chan error, 1)}
	go func() {
		err := mcp.Serve(context.Background(), inR, outW, "test", NewGateway(e, identify))
		c.done <- errors.Join(err, outW.Close())
	}()
	t.Cleanup(func() {
		if err := inW.Close(); err != nil {
			t.Error(err)
		}
		if err := <-c.done; err != nil {
			t.Errorf("Serve() = %v", err)
		}
	})
	return c
}

// ask sends one request and returns its result; a protocol error fails the test.
func (c *mcpClient) ask(t *testing.T, method string, params any) json.RawMessage {
	t.Helper()
	res, code := c.rpc(t, method, params)
	if code != 0 {
		t.Fatalf("%s = error %d, want a result", method, code)
	}
	return res
}

// rpc sends one request and returns its result, or the code of its protocol error.
func (c *mcpClient) rpc(t *testing.T, method string, params any) (json.RawMessage, int) {
	t.Helper()
	c.next++
	b, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": c.next, "method": method, "params": params})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.w.Write(append(b, '\n')); err != nil {
		t.Fatal(err)
	}
	line, err := c.r.ReadBytes('\n')
	if err != nil {
		t.Fatal(err)
	}
	var a struct {
		ID     int             `json:"id"`
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(line, &a); err != nil {
		t.Fatalf("answer %s: %v", line, err)
	}
	switch {
	case a.ID != c.next:
		t.Fatalf("%s = %s, want the answer of request %d", method, line, c.next)
	case a.Error != nil:
		return nil, a.Error.Code
	}
	return a.Result, 0
}

// list returns the names of the tools that tools/list gives.
func (c *mcpClient) list(t *testing.T) []string {
	t.Helper()
	var r struct {
		Tools []struct {
			Name        string          `json:"name"`
			InputSchema json.RawMessage `json:"inputSchema"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(c.ask(t, "tools/list", map[string]any{}), &r); err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for _, tl := range r.Tools {
		if !json.Valid(tl.InputSchema) {
			t.Errorf("tool %s: input schema %s is not JSON", tl.Name, tl.InputSchema)
		}
		names = append(names, tl.Name)
	}
	return names
}

// call calls tool name with args and returns its text and whether it is an error.
func (c *mcpClient) call(t *testing.T, name string, args any) (string, bool) {
	t.Helper()
	var r struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(c.ask(t, "tools/call", map[string]any{"name": name, "arguments": args}), &r); err != nil {
		t.Fatal(err)
	}
	if len(r.Content) != 1 || r.Content[0].Type != "text" {
		t.Fatalf("tools/call %s = %+v, want one text", name, r)
	}
	return r.Content[0].Text, r.IsError
}

// paneScope is the identity of pane id of workspace ws in the fixture's herdr session.
func (f *fixture) paneScope(id, ws string) Identify {
	return func(context.Context) (Scope, error) {
		return Scope{Server: herdr.Target{Session: "default", Socket: f.sock}, WorkspaceID: ws, PaneID: id}, nil
	}
}

// gatewayEnv is the command context of the gateway server. Its environment holds the herdr
// variables of pane w15:p1, as a shared CLI process can: the gateway must not use them.
func (f *runFixture) gatewayEnv() Env {
	e := f.env(f.sock, "w15:p1", "w15", &bytes.Buffer{})
	e.Environ = append(e.Environ, "HOME="+f.dir)
	return e
}

// gatewayCalls reads the gateway_calls rows.
func (f *fixture) gatewayCalls(t *testing.T) []store.GatewayCall {
	t.Helper()
	s := openStore(t, f.store)
	defer closeStore(t, s)
	var cs []store.GatewayCall
	write(t, s, func(tx *sql.Tx) (err error) {
		cs, err = store.GatewayCalls(context.Background(), tx)
		return err
	})
	return cs
}

// wantGatewayCalls checks the method and the result of each gateway_calls row, and the time.
func wantGatewayCalls(t *testing.T, got []store.GatewayCall, want ...string) {
	t.Helper()
	var g []string
	for _, c := range got {
		if !c.At.Equal(testNow) {
			t.Errorf("gateway call %d at %v, want %v", c.ID, c.At, testNow)
		}
		g = append(g, c.Method+" "+string(c.Result))
	}
	if !slices.Equal(g, want) {
		t.Errorf("gateway_calls = %q, want %q", g, want)
	}
}

// readOnlyTool is runTool with the reviewed read-only restriction.
func readOnlyTool(name, dir string) string {
	return strings.Replace(runTool(name, dir, "10", writeWord), "read_only = false", "read_only = true", 1)
}

// AC S63, S61 (gateway part): with no valid identity, or with an identity of a closed pane or
// of a pane that moved to another workspace, tools/list is empty, and each call is refused: no
// tool_calls row, no draft, no tool run. Each refused call of a gateway tool writes a refused
// gateway_calls row in the store of its herdr server; with no identity, or a server with no
// store, there is no store for a row. The herdr variables of a live pane in the environment of
// the server change nothing.
func TestGatewayRefusesWithNoValidIdentity(t *testing.T) {
	noLaunch := func(*runFixture) Identify {
		return func(context.Context) (Scope, error) { return Scope{}, errors.New("no launch file") }
	}
	refused := []string{"find_tool refused", "run_tool refused", "recall refused", "remember refused"}
	for _, tc := range []struct {
		name     string
		identify func(f *runFixture) Identify
		want     string
		rows     []string // the gateway_calls rows
	}{
		{"no launch", noLaunch, "no launch file", nil},
		{"closed pane", func(f *runFixture) Identify { return f.paneScope("w14:p9", "w14") }, "pane not live", refused},
		{"moved pane", func(f *runFixture) Identify { return f.paneScope("w14:p2", "w14") }, "not a member", refused},
		{"other socket", func(f *runFixture) Identify {
			return func(context.Context) (Scope, error) {
				return Scope{Server: herdr.Target{Session: "default", Socket: f.sock + ".other"}, WorkspaceID: "w14",
					PaneID: "w14:p1"}, nil
			}
		}, "unbound session", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRunFixture(t)
			f.approvedTool(t, readOnlyTool("write-word", f.dir))
			before := f.rows(t)
			c := serveGateway(t, f.gatewayEnv(), tc.identify(f))
			if got := c.list(t); len(got) != 0 {
				t.Errorf("tools/list = %v, want no tools", got)
			}
			for _, call := range []struct {
				name string
				args any
			}{
				{"find_tool", map[string]any{}},
				{"run_tool", map[string]any{"tool": "write-word", "inputs": map[string]any{"word": "hello"}}},
				{"recall", map[string]any{"query": "live"}},
				{"remember", map[string]any{"text": "use pnpm"}},
				{"no_such_tool", map[string]any{}},
			} {
				text, isErr := c.call(t, call.name, call.args)
				if !isErr || !strings.HasPrefix(text, "refused: ") || !strings.Contains(text, tc.want) {
					t.Errorf("tools/call %s = %q, %v, want a refusal with %q", call.name, text, isErr, tc.want)
				}
			}
			after := f.rows(t)
			if len(after.items) != len(before.items) || len(after.proposals) != len(before.proposals) {
				t.Errorf("the calls wrote %d items and %d proposals, want none", len(after.items)-len(before.items),
					len(after.proposals)-len(before.proposals))
			}
			if got := f.ran(t); got != "" {
				t.Errorf("the tool ran with %q, want no run", got)
			}
			if cs := f.calls(t); len(cs) != 0 {
				t.Errorf("tool_calls = %+v, want none", cs)
			}
			wantGatewayCalls(t, f.gatewayCalls(t), tc.rows...)
		})
	}
}

// movingEnv is the gateway context of f in which pane w14:p1 moves to workspace w15 after the
// two checks of Open, as a call can wait between its checks and its use. With onClock, it
// moves at the first clock read after them; else at the next herdr check.
func (f *runFixture) movingEnv(onClock bool) Env {
	e := f.gatewayEnv()
	pane, checks := e.Pane, 0
	move := func() { f.panes["w14:p1"] = herdr.Pane{ID: "w14:p1", WorkspaceID: "w15"} }
	e.Pane = func(ctx context.Context, t herdr.Target, id string) (herdr.Pane, error) {
		if checks++; checks == 3 && !onClock {
			move()
		}
		return pane(ctx, t, id)
	}
	e.Now = func() time.Time {
		if checks >= 2 && onClock {
			move()
		}
		return testNow
	}
	return e
}

// A pane that moves to another workspace after the checks of Open runs no tool of its old
// workspace and drafts nothing there: the check just before the use refuses the call, and the
// gateway_calls row says refused. A recall that its own check under the store lock refuses is
// refused too.
func TestGatewayPaneMovesBeforeUse(t *testing.T) {
	for _, tc := range []struct {
		name    string
		args    map[string]any
		onClock bool
	}{
		{"run_tool", map[string]any{"tool": "read-word", "inputs": map[string]any{"word": "hello"}}, true},
		{"remember", map[string]any{"text": "a fact of the old workspace"}, true},
		{"recall", map[string]any{"query": "live"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRunFixture(t)
			f.approvedTool(t, readOnlyTool("read-word", f.dir))
			before := f.rows(t)
			c := serveGateway(t, f.movingEnv(tc.onClock), f.paneScope("w14:p1", "w14"))
			text, isErr := c.call(t, tc.name, tc.args)
			if !isErr || !strings.Contains(text, "pane w14:p1 moved to workspace w15") {
				t.Errorf("tools/call %s = %q, %v, want the refusal of the moved pane", tc.name, text, isErr)
			}
			if got := f.ran(t); got != "" {
				t.Errorf("tools/call %s ran the tool with %q, want no run", tc.name, got)
			}
			if after := f.rows(t); len(after.items) != len(before.items) || len(after.proposals) != len(before.proposals) {
				t.Errorf("tools/call %s wrote %d items, want none", tc.name, len(after.items)-len(before.items))
			}
			wantGatewayCalls(t, f.gatewayCalls(t), tc.name+" refused")
		})
	}
}

// A launched session lists the four tools. The scope comes from the identity, not from the
// herdr variables of the server's environment: a call works with none of them, and with those
// of another pane.
func TestGatewayListsTools(t *testing.T) {
	f := newRunFixture(t)
	for _, environ := range [][]string{nil, f.gatewayEnv().Environ} {
		e := f.gatewayEnv()
		e.Environ = environ
		c := serveGateway(t, e, f.paneScope("w14:p1", "w14"))
		if got := c.list(t); !slices.Equal(got, gatewayNames) {
			t.Errorf("tools/list = %v, want %v", got, gatewayNames)
		}
		if _, code := c.rpc(t, "tools/call", map[string]any{"name": "no_such_tool"}); code != -32602 {
			t.Errorf("tools/call no_such_tool = code %d, want -32602", code)
		}
	}
}

// find_tool lists the approved tools of the pane's workspace and the global ones: never a
// draft, never a tool of another workspace. It says which tools run_tool can run. It writes a
// gateway_calls row and no tool_calls row.
func TestGatewayFindTool(t *testing.T) {
	f := newRunFixture(t)
	f.approvedTool(t, readOnlyTool("read-word", f.dir))
	f.approvedTool(t, runTool("write-word", f.dir, "10", writeWord))
	f.toolDraft(t, readOnlyTool("draft-word", f.dir))
	c := serveGateway(t, f.gatewayEnv(), f.paneScope("w14:p1", "w14"))

	text, isErr := c.call(t, "find_tool", map[string]any{})
	if isErr {
		t.Fatalf("find_tool = %q, want the tools", text)
	}
	for _, want := range []string{
		"read-word (item ", ", workspace w14 (shop)): Write one word\n  brain run read-word --word <value>\n",
		"  --word: string, pattern ^[a-z]{1,16}$\n  run_tool: yes (reviewed read-only)\n",
		"write-word (item ", "  run_tool: no; run it with brain run in your shell\n",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("find_tool = %q, want %q", text, want)
		}
	}
	if strings.Contains(text, "draft-word") {
		t.Errorf("find_tool = %q, want no draft tool", text)
	}
	if text, _ := c.call(t, "find_tool", map[string]any{"query": "nothing"}); text != "no approved tool matches \"nothing\"\n" {
		t.Errorf("find_tool(nothing) = %q, want no match", text)
	}
	if text, _ := c.call(t, "find_tool", map[string]any{"query": "draft"}); strings.Contains(text, "draft-word") {
		t.Errorf("find_tool(draft) = %q, want no draft tool", text)
	}
	if text, isErr := c.call(t, "find_tool", map[string]any{"name": "x"}); !isErr || !strings.Contains(text, "unknown field") {
		t.Errorf("find_tool(name) = %q, %v, want a refusal of the unknown field", text, isErr)
	}
	// A pane of another workspace sees none of these tools.
	other := serveGateway(t, f.gatewayEnv(), f.paneScope("w15:p1", "w15"))
	if text, _ := other.call(t, "find_tool", nil); text != "no approved tool matches \"\"\n" {
		t.Errorf("find_tool from w15 = %q, want no tools", text)
	}
	if cs := f.calls(t); len(cs) != 0 {
		t.Errorf("tool_calls = %+v, want none", cs)
	}
	wantGatewayCalls(t, f.gatewayCalls(t), "find_tool ok", "find_tool ok", "find_tool ok", "find_tool refused", "find_tool ok")
}

// run_tool runs an approved tool with the reviewed read-only restriction, with the checks of
// brain run, and its output is the answer. It refuses any other tool and says to run it with
// brain run in the shell. Each call of a known tool writes its tool_calls row, and each call a
// gateway_calls row.
func TestGatewayRunTool(t *testing.T) {
	f := newRunFixture(t)
	ro := f.approvedTool(t, readOnlyTool("read-word", f.dir))
	rw := f.approvedTool(t, runTool("write-word", f.dir, "10", writeWord))
	draft := readOnlyTool("draft-word", f.dir)
	f.toolDraft(t, draft)
	c := serveGateway(t, f.gatewayEnv(), f.paneScope("w14:p1", "w14"))

	text, isErr := c.call(t, "run_tool", map[string]any{"tool": "read-word", "inputs": map[string]any{"word": "hello"}})
	if isErr || text != "word hello\n" || f.ran(t) != "hello" {
		t.Errorf("run_tool(read-word) = %q, %v, want the tool output", text, isErr)
	}
	for _, tc := range []struct {
		name string
		args map[string]any
		want string
	}{
		{"no restriction", map[string]any{"tool": "write-word", "inputs": map[string]any{"word": "hello"}},
			"no reviewed read-only restriction: refused; nothing ran; run it with brain run write-word in your shell"},
		{"bad input", map[string]any{"tool": "read-word", "inputs": map[string]any{"word": "Hello!"}}, "does not match the pattern"},
		{"number input", map[string]any{"tool": "read-word", "inputs": map[string]any{"word": 7}}, "does not match the pattern"},
		{"object input", map[string]any{"tool": "read-word", "inputs": map[string]any{"word": map[string]any{}}}, "want a string or an integer"},
		{"missing input", map[string]any{"tool": "read-word"}, "input word is missing"},
		{"draft", map[string]any{"tool": "draft-word", "inputs": map[string]any{"word": "hello"}}, "no tool draft-word"},
		{"no name", map[string]any{"inputs": map[string]any{"word": "hello"}}, "want a tool name"},
	} {
		text, isErr := c.call(t, "run_tool", tc.args)
		if !isErr || !strings.Contains(text, tc.want) {
			t.Errorf("run_tool %s = %q, %v, want an error with %q", tc.name, text, isErr, tc.want)
		}
		if got := f.ran(t); got != "" {
			t.Errorf("run_tool %s ran the tool with %q, want no run", tc.name, got)
		}
	}
	var got []string
	for _, cl := range f.calls(t) {
		got = append(got, fmt.Sprintf("%d %s %s", cl.ToolID, cl.Origin, cl.Result))
	}
	want := []string{fmt.Sprintf("%d agent ran", ro), fmt.Sprintf("%d agent refused", rw)}
	for range 4 {
		want = append(want, fmt.Sprintf("%d agent refused", ro))
	}
	if !slices.Equal(got, want) {
		t.Errorf("tool_calls = %q, want %q", got, want)
	}
	wantGatewayCalls(t, f.gatewayCalls(t), "run_tool ok", "run_tool refused", "run_tool refused", "run_tool refused",
		"run_tool refused", "run_tool refused", "run_tool refused", "run_tool refused")
}

// A tool that ran and failed is a failed call: the answer is its output and the failure.
func TestGatewayRunToolFails(t *testing.T) {
	f := newRunFixture(t)
	text := strings.Replace(readOnlyTool("fail-word", f.dir), writeWord, "echo \"no $1\" >&2\nexit 3\n", 1)
	f.approvedTool(t, text)
	c := serveGateway(t, f.gatewayEnv(), f.paneScope("w14:p1", "w14"))
	got, isErr := c.call(t, "run_tool", map[string]any{"tool": "fail-word", "inputs": map[string]any{"word": "hello"}})
	if !isErr || !strings.HasPrefix(got, "no hello\n") || !strings.Contains(got, "exit code 3") {
		t.Errorf("run_tool(fail-word) = %q, %v, want the output and exit code 3", got, isErr)
	}
	if cs := f.calls(t); len(cs) != 1 || cs[0].Result != store.CallRan || cs[0].ExitCode == nil || *cs[0].ExitCode != 3 {
		t.Errorf("tool_calls = %+v, want one ran row with exit code 3", cs)
	}
	wantGatewayCalls(t, f.gatewayCalls(t), "run_tool failed")
}

// recall returns the approved items in scope that match the words of the query, as brain
// recall does. It writes a gateway_calls row and no tool_calls row.
func TestGatewayRecall(t *testing.T) {
	f := newRunFixture(t)
	c := serveGateway(t, f.gatewayEnv(), f.paneScope("w14:p1", "w14"))
	if text, isErr := c.call(t, "recall", map[string]any{"query": "LIVE items"}); isErr ||
		!strings.HasPrefix(text, "Brain items for \"live items\", best match first:\n") || !strings.Contains(text, liveText) {
		t.Errorf("recall(LIVE items) = %q, %v, want the live item", text, isErr)
	}
	if text, isErr := c.call(t, "recall", map[string]any{"query": "zebra"}); isErr || text != "no Brain item matches \"zebra\"\n" {
		t.Errorf("recall(zebra) = %q, %v, want no match", text, isErr)
	}
	if text, isErr := c.call(t, "recall", map[string]any{"query": " ,; "}); !isErr || !strings.Contains(text, "want words") {
		t.Errorf("recall(no words) = %q, %v, want a refusal", text, isErr)
	}
	if cs := f.calls(t); len(cs) != 0 {
		t.Errorf("tool_calls = %+v, want none", cs)
	}
	wantGatewayCalls(t, f.gatewayCalls(t), "recall ok", "recall ok", "recall refused")
}

// remember makes a draft for review, as brain remember does, in the workspace of the scope or
// in the global part. The sender is the assistant, so it never drafts a preference. A text with
// a secret, a bad kind or a global handoff is refused and drafts nothing.
func TestGatewayRemember(t *testing.T) {
	f := newRunFixture(t)
	c := serveGateway(t, f.gatewayEnv(), f.paneScope("w14:p1", "w14"))
	key := "ws_a14"
	for _, tc := range []struct {
		draft string
		args  map[string]any
		kind  store.Kind
		key   *string
	}{
		{"use pnpm, not npm", map[string]any{}, store.KindFact, &key},
		{"we chose sqlite", map[string]any{"kind": "decision", "global": true}, store.KindDecision, nil},
		{"next: fix the lint", map[string]any{"kind": "handoff"}, store.KindHandoff, &key},
	} {
		tc.args["text"] = tc.draft
		before := f.rows(t)
		text, isErr := c.call(t, "remember", tc.args)
		after := f.rows(t)
		if isErr {
			t.Fatalf("remember(%v) = %q, want a draft", tc.args, text)
		}
		checkDraft(t, []string{tc.draft}, tc.draft, before, after, tc.kind, tc.key)
		if want := fmt.Sprintf("proposal %d: ", after.proposals[len(after.proposals)-1].ID); !strings.HasPrefix(text, want) ||
			!strings.Contains(text, "waiting for your review") {
			t.Errorf("remember(%v) = %q, want %q... waiting for your review", tc.args, text, want)
		}
	}
	before := f.rows(t)
	for _, tc := range []struct {
		args map[string]any
		want string
	}{
		{map[string]any{"text": "I prefer tabs", "kind": "preference"}, "cannot draft your preference"},
		{map[string]any{"text": "a1B2", "kind": "lesson"}, "unknown kind"},
		{map[string]any{"text": "next: deploy", "kind": "handoff", "global": true}, "drop --global"},
		{map[string]any{"text": "  "}, "the text is empty"},
		{map[string]any{"text": "token ghp_a1B2c3D4e5F6g7H8i9J0k1L2m3N4o5P6q7R8s9"}, "github-token"},
		{map[string]any{"text": "x", "scope": "global"}, "unknown field"},
	} {
		text, isErr := c.call(t, "remember", tc.args)
		if !isErr || !strings.HasPrefix(text, "refused: ") || !strings.Contains(text, tc.want) || strings.Contains(text, "a1B2") {
			t.Errorf("remember(%v) = %q, %v, want a refusal with %q and no text of the call", tc.args, text, isErr, tc.want)
		}
	}
	if after := f.rows(t); len(after.items) != len(before.items) || len(after.proposals) != len(before.proposals) {
		t.Errorf("the refused calls wrote %d items and %d proposals, want none", len(after.items)-len(before.items),
			len(after.proposals)-len(before.proposals))
	}
	if cs := f.calls(t); len(cs) != 0 {
		t.Errorf("tool_calls = %+v, want none", cs)
	}
	wantGatewayCalls(t, f.gatewayCalls(t), "remember ok", "remember ok", "remember ok", "remember refused",
		"remember refused", "remember refused", "remember refused", "remember refused", "remember refused")
}

// Two sessions in two workspaces: each call goes to the workspace of its own scope.
func TestGatewayScopeOfEachSession(t *testing.T) {
	f := newRunFixture(t)
	a := serveGateway(t, f.gatewayEnv(), f.paneScope("w14:p1", "w14"))
	b := serveGateway(t, f.gatewayEnv(), f.paneScope("w15:p1", "w15"))
	for _, c := range []struct {
		client *mcpClient
		key    string
	}{{a, "ws_a14"}, {b, "ws_a15"}} {
		before := f.rows(t)
		if text, isErr := c.client.call(t, "remember", map[string]any{"text": "fact of " + c.key}); isErr {
			t.Fatalf("remember = %q, want a draft", text)
		}
		checkDraft(t, []string{c.key}, "fact of "+c.key, before, f.rows(t), store.KindFact, &c.key)
	}
}
