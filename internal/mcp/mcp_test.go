package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"testing"
)

// fakeHandler gives tools and answers each call with call.
type fakeHandler struct {
	tools []Tool
	call  func(name string, args json.RawMessage) (Result, error)
}

func (f fakeHandler) Tools(context.Context) []Tool { return f.tools }

func (f fakeHandler) Call(_ context.Context, name string, args json.RawMessage) (Result, error) {
	return f.call(name, args)
}

// client is the client end of an in-memory pipe to a running Serve.
type client struct {
	w    *io.PipeWriter
	r    *bufio.Reader
	done chan error
}

// start runs Serve with h on an in-memory pipe. The test ends the client with stop.
func start(t *testing.T, h Handler) *client {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	c := &client{w: inW, r: bufio.NewReader(outR), done: make(chan error, 1)}
	go func() {
		err := Serve(context.Background(), inR, outW, "1.2.3", h)
		c.done <- errors.Join(err, outW.Close())
	}()
	return c
}

// send writes one line to the server.
func (c *client) send(t *testing.T, line string) {
	t.Helper()
	if _, err := io.WriteString(c.w, line+"\n"); err != nil {
		t.Fatalf("send %s: %v", line, err)
	}
}

// answer is a JSON-RPC answer as the test reads it.
type answer struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *rpcError       `json:"error"`
}

// recv reads the next answer line.
func (c *client) recv(t *testing.T) answer {
	t.Helper()
	line, err := c.r.ReadBytes('\n')
	if err != nil {
		t.Fatalf("read answer: %v", err)
	}
	var a answer
	if err := json.Unmarshal(line, &a); err != nil {
		t.Fatalf("answer %s: %v", line, err)
	}
	if a.JSONRPC != "2.0" {
		t.Errorf("answer %s: want jsonrpc 2.0", line)
	}
	return a
}

// ask sends line and reads its answer.
func (c *client) ask(t *testing.T, line string) answer {
	t.Helper()
	c.send(t, line)
	return c.recv(t)
}

// stop closes the input of the server and waits for Serve to end: the end of the input ends
// it with no error, and it writes nothing more.
func (c *client) stop(t *testing.T) {
	t.Helper()
	if err := c.w.Close(); err != nil {
		t.Fatal(err)
	}
	if rest, err := io.ReadAll(c.r); err != nil || len(rest) != 0 {
		t.Errorf("after the end of input: %q, %v, want nothing", rest, err)
	}
	if err := <-c.done; err != nil {
		t.Errorf("Serve() = %v, want nil at the end of input", err)
	}
}

func decode[T any](t *testing.T, raw json.RawMessage) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("result %s: %v", raw, err)
	}
	return v
}

// initialize answers with the version that the client asks for when the server speaks it, and
// with the newest version for any other. 2025-03-26 needs batches, so the server does not
// speak it.
func TestInitialize(t *testing.T) {
	c := start(t, fakeHandler{})
	defer c.stop(t)
	for _, tc := range []struct{ ask, want string }{
		{"2025-11-25", "2025-11-25"}, // Claude Code
		{"2025-06-18", "2025-06-18"}, // Codex
		{"2024-11-05", "2024-11-05"},
		{"2025-03-26", "2025-11-25"},
		{"2099-01-01", "2025-11-25"},
		{"", "2025-11-25"},
	} {
		a := c.ask(t, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"`+tc.ask+
			`","capabilities":{},"clientInfo":{"name":"test","version":"0"}}}`)
		if a.Error != nil {
			t.Fatalf("initialize(%s) = error %+v", tc.ask, a.Error)
		}
		r := decode[struct {
			ProtocolVersion string                     `json:"protocolVersion"`
			Capabilities    map[string]json.RawMessage `json:"capabilities"`
			ServerInfo      map[string]string          `json:"serverInfo"`
		}](t, a.Result)
		if r.ProtocolVersion != tc.want || r.Capabilities["tools"] == nil || r.ServerInfo["name"] != "brain" ||
			r.ServerInfo["version"] != "1.2.3" {
			t.Errorf("initialize(%s) = %s, want version %s, the tools capability and server brain 1.2.3", tc.ask, a.Result, tc.want)
		}
	}
}

// An unknown method gets a method-not-found error, not silence: Claude Code sends
// server/discover first and goes on after the error. A notification gets no answer, so the
// next answer is the one of the next request.
func TestUnknownMethodAndNotification(t *testing.T) {
	c := start(t, fakeHandler{})
	defer c.stop(t)
	a := c.ask(t, `{"jsonrpc":"2.0","id":0,"method":"server/discover"}`)
	if a.Error == nil || a.Error.Code != codeMethodNotFound || string(a.ID) != "0" {
		t.Errorf("server/discover = %+v, want error %d with id 0", a, codeMethodNotFound)
	}
	c.send(t, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	c.send(t, `{"jsonrpc":"2.0","method":"no/such/notification"}`)
	c.send(t, "")
	a = c.ask(t, `{"jsonrpc":"2.0","id":"p-1","method":"ping"}`)
	if a.Error != nil || string(a.ID) != `"p-1"` || string(a.Result) != "{}" {
		t.Errorf("ping = %+v, want an empty result with id \"p-1\"", a)
	}
}

// A message that is not a valid request gets an error with the code of its fault, and the
// server goes on.
func TestBadMessages(t *testing.T) {
	c := start(t, fakeHandler{})
	defer c.stop(t)
	for _, tc := range []struct {
		line, id string
		code     int
	}{
		{`{"jsonrpc":"2.0","id":1,"method":`, "null", codeParse},
		{`not json`, "null", codeParse},
		{`null`, "null", codeInvalidRequest},
		{`{}`, "null", codeInvalidRequest},
		{`{"jsonrpc":"1.0","method":"ping"}`, "null", codeInvalidRequest},
		{`{"jsonrpc":"2.0","id":1.5,"method":"ping"}`, "null", codeInvalidRequest},
		{`{"jsonrpc":"2.0","id":1e3,"method":"ping"}`, "null", codeInvalidRequest},
		{`{"jsonrpc":"2.0","id":true,"method":"ping"}`, "null", codeInvalidRequest},
		{`[{"jsonrpc":"2.0","id":1,"method":"ping"}]`, "null", codeInvalidRequest},
		{`{"jsonrpc":"2.0","id":null,"method":"ping"}`, "null", codeInvalidRequest},
		{`{"jsonrpc":"2.0","id":{"a":1},"method":"ping"}`, "null", codeInvalidRequest},
		{`{"id":2,"method":"ping"}`, "2", codeInvalidRequest},
		{`{"jsonrpc":"2.0","id":3}`, "3", codeInvalidRequest},
		{`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"arguments":{}}}`, "4", codeInvalidParams},
		{`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":[1]}`, "5", codeInvalidParams},
	} {
		a := c.ask(t, tc.line)
		if a.Error == nil || a.Error.Code != tc.code || string(a.ID) != tc.id {
			t.Errorf("%s = %+v, want error %d with id %s", tc.line, a, tc.code, tc.id)
		}
	}
	if a := c.ask(t, `{"jsonrpc":"2.0","id":6,"method":"ping"}`); a.Error != nil {
		t.Errorf("ping after the bad messages = %+v, want a result", a)
	}
}

// tools/list gives the tools of the handler, and an empty list, not null, when it has none.
func TestToolsList(t *testing.T) {
	for _, tc := range []struct {
		name  string
		tools []Tool
		want  string
	}{
		{"none", nil, `{"tools":[]}`},
		{"one", []Tool{{Name: "echo", Description: "Echo the text", InputSchema: json.RawMessage(`{"type":"object"}`)}},
			`{"tools":[{"name":"echo","description":"Echo the text","inputSchema":{"type":"object"}}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := start(t, fakeHandler{tools: tc.tools})
			defer c.stop(t)
			if a := c.ask(t, `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`); a.Error != nil || string(a.Result) != tc.want {
				t.Errorf("tools/list = %s, %+v, want %s", a.Result, a.Error, tc.want)
			}
		})
	}
}

// tools/call gives the name and the arguments to the handler. A result is text content with
// isError; an unknown tool is an invalid-params error, and any other handler error an internal
// error.
func TestToolsCall(t *testing.T) {
	var gotName, gotArgs string
	h := fakeHandler{call: func(name string, args json.RawMessage) (Result, error) {
		gotName, gotArgs = name, string(args)
		switch name {
		case "echo":
			return Result{Text: "said: <" + string(args) + ">\nend"}, nil
		case "refuse":
			return Result{Text: "refused: no launch", IsError: true}, nil
		case "broken":
			return Result{}, errors.New("store is gone")
		}
		return Result{}, ErrUnknownTool
	}}
	c := start(t, h)
	defer c.stop(t)
	type content struct {
		Content []struct{ Type, Text string } `json:"content"`
		IsError bool                          `json:"isError"`
	}
	for _, tc := range []struct {
		params, name, args string
		want               content
		code               int // 0: a result
	}{
		{`{"name":"echo","arguments":{"text":"hi"}}`, "echo", `{"text":"hi"}`,
			content{Content: []struct{ Type, Text string }{{"text", `said: <{"text":"hi"}>` + "\nend"}}}, 0},
		{`{"name":"echo"}`, "echo", "", content{Content: []struct{ Type, Text string }{{"text", "said: <>\nend"}}}, 0},
		{`{"name":"echo","arguments":null}`, "echo", "", content{Content: []struct{ Type, Text string }{{"text", "said: <>\nend"}}}, 0},
		{`{"name":"refuse","arguments":{}}`, "refuse", "{}",
			content{Content: []struct{ Type, Text string }{{"text", "refused: no launch"}}, IsError: true}, 0},
		{`{"name":"nope","arguments":{}}`, "nope", "{}", content{}, codeInvalidParams},
		{`{"name":"broken","arguments":{}}`, "broken", "{}", content{}, codeInternal},
	} {
		a := c.ask(t, `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":`+tc.params+`}`)
		if gotName != tc.name || gotArgs != tc.args {
			t.Errorf("tools/call %s: handler got %q %q, want %q %q", tc.params, gotName, gotArgs, tc.name, tc.args)
		}
		if tc.code != 0 {
			if a.Error == nil || a.Error.Code != tc.code {
				t.Errorf("tools/call %s = %+v, want error %d", tc.params, a, tc.code)
			}
			continue
		}
		if a.Error != nil {
			t.Fatalf("tools/call %s = error %+v", tc.params, a.Error)
		}
		got := decode[content](t, a.Result)
		if len(got.Content) != 1 || got.Content[0] != tc.want.Content[0] || got.IsError != tc.want.IsError {
			t.Errorf("tools/call %s = %s, want %+v", tc.params, a.Result, tc.want)
		}
	}
}

// The last message counts also with no newline after it, and the end of input ends Serve.
func TestLastLineWithNoNewline(t *testing.T) {
	c := start(t, fakeHandler{})
	if _, err := io.WriteString(c.w, `{"jsonrpc":"2.0","id":9,"method":"ping"}`); err != nil {
		t.Fatal(err)
	}
	if err := c.w.Close(); err != nil {
		t.Fatal(err)
	}
	if a := c.recv(t); a.Error != nil || string(a.ID) != "9" {
		t.Errorf("ping = %+v, want a result with id 9", a)
	}
	c.stop(t)
}

// A ping gets its answer while a tool call runs, and tool calls run one at a time: the second
// call starts only when the first ends. Each call answers, also after the end of input.
func TestPingWhileToolRuns(t *testing.T) {
	started := make(chan string, 2)
	release := map[string]chan struct{}{"a": make(chan struct{}), "b": make(chan struct{})}
	h := fakeHandler{call: func(name string, _ json.RawMessage) (Result, error) {
		started <- name
		<-release[name]
		return Result{Text: "done " + name}, nil
	}}
	c := start(t, h)
	c.send(t, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"a"}}`)
	if got := <-started; got != "a" {
		t.Fatalf("started %s, want a", got)
	}
	c.send(t, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"b"}}`)
	if a := c.ask(t, `{"jsonrpc":"2.0","id":3,"method":"ping"}`); string(a.ID) != "3" || a.Error != nil {
		t.Errorf("ping while a tool runs = %+v, want the ping result", a)
	}
	select {
	case got := <-started:
		t.Errorf("call %s started while call a runs, want one call at a time", got)
	default:
	}
	close(release["a"])
	if a := c.recv(t); string(a.ID) != "1" {
		t.Errorf("answer = %+v, want the answer of call 1", a)
	}
	if got := <-started; got != "b" {
		t.Fatalf("started %s, want b", got)
	}
	if err := c.w.Close(); err != nil {
		t.Fatal(err)
	}
	close(release["b"])
	if a := c.recv(t); string(a.ID) != "2" {
		t.Errorf("answer = %+v, want the answer of call 2", a)
	}
	c.stop(t)
}

// When ctx ends, a call that waits for its turn ends with an error.
func TestStoppedCallWaitsNoMore(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	h := fakeHandler{call: func(string, json.RawMessage) (Result, error) {
		close(started)
		<-release
		return Result{Text: "done"}, nil
	}}
	ctx, cancel := context.WithCancel(context.Background())
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	done := make(chan error, 1)
	go func() { done <- errors.Join(Serve(ctx, inR, outW, "test", h), outW.Close()) }()
	c := &client{w: inW, r: bufio.NewReader(outR), done: done}
	c.send(t, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"a"}}`)
	<-started
	c.send(t, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"b"}}`)
	// The ping answer shows that both calls were read.
	if a := c.ask(t, `{"jsonrpc":"2.0","id":3,"method":"ping"}`); string(a.ID) != "3" {
		t.Fatalf("ping = %+v", a)
	}
	cancel()
	if a := c.recv(t); string(a.ID) != "2" || a.Error == nil || a.Error.Code != codeInternal {
		t.Errorf("waiting call after the stop = %+v, want an internal error with id 2", a)
	}
	close(release)
	if a := c.recv(t); string(a.ID) != "1" || a.Error != nil {
		t.Errorf("running call = %+v, want its result", a)
	}
	c.stop(t)
}
