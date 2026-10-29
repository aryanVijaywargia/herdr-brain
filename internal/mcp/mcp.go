// Package mcp is a stdio server of the Model Context Protocol: JSON-RPC 2.0, one message on
// each line. It answers initialize, ping, tools/list and tools/call, and an error to any other
// request. It knows no tool: a Handler gives the tools and runs their calls.
package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"sync"
)

// serverName is the name that the server gives in its initialize result.
const serverName = "brain"

// versions are the protocol versions that the server speaks, the newest first. Claude Code
// asks for 2025-11-25 and Codex for 2025-06-18; the tools part is the same in each. The server
// does not speak 2025-03-26: that version needs JSON-RPC batches, and the server takes none.
var versions = []string{"2025-11-25", "2025-06-18", "2024-11-05"}

// The JSON-RPC error codes.
const (
	codeParse          = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
	codeInternal       = -32603
)

// ErrUnknownTool is the error of Handler.Call for a name that is not a tool of the server.
var ErrUnknownTool = errors.New("unknown tool")

// Tool is one tool of the server, as tools/list gives it.
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

// Result is the result of a tool call: one text, and whether the call failed. A failed call is
// a result, not a protocol error, so the agent reads why.
type Result struct {
	Text    string
	IsError bool
}

// Handler gives the tools of the server and runs their calls.
type Handler interface {
	// Tools returns the tools that the client may call now; none is an empty list.
	Tools(ctx context.Context) []Tool
	// Call runs tool name with the JSON object args (nil when the client gave none). The error
	// is ErrUnknownTool, or a failure of the server itself.
	Call(ctx context.Context, name string, args json.RawMessage) (Result, error)
}

// request is one JSON-RPC message from the client. A message with no id is a notification,
// which gets no answer.
type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// null is the id of an answer to a message whose id cannot be read.
var null = json.RawMessage("null")

// Serve reads the messages of one client from in and writes the answers to out, one message
// on each line, until in ends. Each tool call runs in a goroutine of its own, and one call runs
// at a time, so a ping gets its answer while a tool runs. Serve waits for each call before it
// ends. When ctx ends, a call that waits for its turn ends with an error, and the handler gets
// ctx. The error is a failure to read in or to write out.
func Serve(ctx context.Context, in io.Reader, out io.Writer, version string, h Handler) error {
	s := &server{version: version, h: h, enc: json.NewEncoder(out), turn: make(chan struct{}, 1)}
	s.enc.SetEscapeHTML(false)
	var calls sync.WaitGroup
	r := bufio.NewReader(in)
	var readErr error
	for readErr == nil && s.failed() == nil {
		var line []byte
		line, readErr = r.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			s.handle(ctx, line, &calls)
		}
	}
	calls.Wait()
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		readErr = fmt.Errorf("read message: %w", readErr)
	} else {
		readErr = nil
	}
	return errors.Join(readErr, s.failed())
}

type server struct {
	version string
	h       Handler
	turn    chan struct{} // holds the one tool call that runs

	mu       sync.Mutex // guards enc and writeErr
	enc      *json.Encoder
	writeErr error
}

// write writes one answer. After a failed write it writes nothing more.
func (s *server) write(r response) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.writeErr != nil {
		return
	}
	if err := s.enc.Encode(r); err != nil {
		s.writeErr = fmt.Errorf("write answer: %w", err)
	}
}

// failed returns the error of a failed write, or nil.
func (s *server) failed() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.writeErr
}

// handle answers one message: a tool call in a goroutine that calls owns, any other request
// at once. A notification gets no answer.
func (s *server) handle(ctx context.Context, line []byte, calls *sync.WaitGroup) {
	req, bad := parse(line)
	switch {
	case bad != nil:
		s.write(*bad)
	case req.ID == nil:
		// A notification, such as notifications/initialized: there is nothing to answer.
	case req.Method == "tools/call":
		calls.Add(1)
		go func() {
			defer calls.Done()
			s.write(s.call(ctx, req))
		}()
	default:
		s.write(s.answer(ctx, req))
	}
}

// parse reads one message, and checks it before anything else: one object, "jsonrpc": "2.0",
// a method, and an id that is a string or an integer when it has one. bad is the error
// answer of a message that fails.
func parse(line []byte) (_ request, bad *response) {
	fail := func(id json.RawMessage, code int, msg string) (request, *response) {
		r := failure(id, code, msg)
		return request{}, &r
	}
	if !json.Valid(line) {
		return fail(null, codeParse, "parse error")
	}
	var req request
	// A batch (a JSON array) is not part of the protocol versions that the server speaks.
	if err := json.Unmarshal(line, &req); err != nil {
		return fail(null, codeInvalidRequest, "invalid request: want one JSON object")
	}
	id := null
	if req.ID != nil && validID(req.ID) {
		id = req.ID
	}
	switch {
	case req.JSONRPC != "2.0" || req.Method == "":
		return fail(id, codeInvalidRequest, `invalid request: want "jsonrpc": "2.0" and a method`)
	case req.ID != nil && !validID(req.ID):
		return fail(null, codeInvalidRequest, "invalid request: the id must be a string or an integer")
	}
	return req, nil
}

// answer answers a request that is not a tool call.
func (s *server) answer(ctx context.Context, req request) response {
	switch req.Method {
	case "initialize":
		return s.initialize(req)
	case "ping":
		return success(req.ID, struct{}{})
	case "tools/list":
		tools := s.h.Tools(ctx)
		if tools == nil {
			tools = []Tool{}
		}
		return success(req.ID, map[string]any{"tools": tools})
	}
	// Claude Code sends server/discover before initialize, and goes on after this error.
	return failure(req.ID, codeMethodNotFound, "method not found: "+req.Method)
}

// validID reports whether id is a string or an integer, as the protocol wants.
func validID(id json.RawMessage) bool {
	d := json.NewDecoder(bytes.NewReader(id))
	d.UseNumber()
	var v any
	if err := d.Decode(&v); err != nil {
		return false
	}
	switch v := v.(type) {
	case string:
		return true
	case json.Number:
		_, err := strconv.ParseInt(v.String(), 10, 64)
		return err == nil
	}
	return false
}

// initialize answers with the version that the client asks for when the server speaks it,
// else with the newest one: the client then decides whether it can go on.
func (s *server) initialize(req request) response {
	var p struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if err := unmarshalParams(req.Params, &p); err != nil {
		return failure(req.ID, codeInvalidParams, "invalid params: "+err.Error())
	}
	v := versions[0]
	if slices.Contains(versions, p.ProtocolVersion) {
		v = p.ProtocolVersion
	}
	return success(req.ID, map[string]any{
		"protocolVersion": v,
		"capabilities":    map[string]any{"tools": map[string]any{}},
		"serverInfo":      map[string]string{"name": serverName, "version": s.version},
	})
}

// call runs one tool call when its turn comes.
func (s *server) call(ctx context.Context, req request) response {
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := unmarshalParams(req.Params, &p); err != nil {
		return failure(req.ID, codeInvalidParams, "invalid params: "+err.Error())
	}
	if p.Name == "" {
		return failure(req.ID, codeInvalidParams, "invalid params: the tool name is missing")
	}
	if bytes.Equal(bytes.TrimSpace(p.Arguments), null) {
		p.Arguments = nil
	}
	select {
	case s.turn <- struct{}{}:
	case <-ctx.Done():
		return failure(req.ID, codeInternal, fmt.Sprintf("the server stopped: %v", ctx.Err()))
	}
	res, err := s.h.Call(ctx, p.Name, p.Arguments)
	<-s.turn
	switch {
	case errors.Is(err, ErrUnknownTool):
		return failure(req.ID, codeInvalidParams, fmt.Sprintf("%v: %s", err, p.Name))
	case err != nil:
		return failure(req.ID, codeInternal, err.Error())
	}
	return success(req.ID, map[string]any{
		"content": []map[string]string{{"type": "text", "text": res.Text}},
		"isError": res.IsError,
	})
}

// unmarshalParams reads the params object of a request into v; no params is an empty object.
func unmarshalParams(params json.RawMessage, v any) error {
	if params == nil || bytes.Equal(params, null) {
		return nil
	}
	return json.Unmarshal(params, v)
}

func success(id json.RawMessage, result any) response {
	return response{JSONRPC: "2.0", ID: id, Result: result}
}

func failure(id json.RawMessage, code int, msg string) response {
	return response{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: msg}}
}
