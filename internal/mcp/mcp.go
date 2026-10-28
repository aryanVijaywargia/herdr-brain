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
)

// serverName is the name that the server gives in its initialize result.
const serverName = "brain"

// versions are the protocol versions that the server speaks, the newest first. Claude Code
// asks for 2025-11-25 and Codex for 2025-06-18; the tools part is the same in each.
var versions = []string{"2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"}

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
// on each line, until in ends. It answers in order: a call runs to its end before the next
// message is read. The error is a failure to read in or to write out.
func Serve(ctx context.Context, in io.Reader, out io.Writer, version string, h Handler) error {
	enc := json.NewEncoder(out)
	enc.SetEscapeHTML(false)
	s := server{version: version, h: h}
	r := bufio.NewReader(in)
	for {
		line, readErr := r.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			if resp, ok := s.handle(ctx, line); ok {
				if err := enc.Encode(resp); err != nil {
					return fmt.Errorf("write answer: %w", err)
				}
			}
		}
		if errors.Is(readErr, io.EOF) {
			return nil
		}
		if readErr != nil {
			return fmt.Errorf("read message: %w", readErr)
		}
	}
}

type server struct {
	version string
	h       Handler
}

// handle answers one message. ok is false for a notification.
func (s server) handle(ctx context.Context, line []byte) (_ response, ok bool) {
	if !json.Valid(line) {
		return failure(null, codeParse, "parse error"), true
	}
	var req request
	// A batch (a JSON array) is not part of the protocol since 2025-06-18.
	if err := json.Unmarshal(line, &req); err != nil {
		return failure(null, codeInvalidRequest, "invalid request: want one JSON object"), true
	}
	if req.ID == nil {
		// A notification, such as notifications/initialized: there is nothing to answer.
		return response{}, false
	}
	if !validID(req.ID) {
		return failure(null, codeInvalidRequest, "invalid request: the id must be a string or a number"), true
	}
	if req.JSONRPC != "2.0" || req.Method == "" {
		return failure(req.ID, codeInvalidRequest, `invalid request: want "jsonrpc": "2.0" and a method`), true
	}
	switch req.Method {
	case "initialize":
		return s.initialize(req), true
	case "ping":
		return success(req.ID, struct{}{}), true
	case "tools/list":
		tools := s.h.Tools(ctx)
		if tools == nil {
			tools = []Tool{}
		}
		return success(req.ID, map[string]any{"tools": tools}), true
	case "tools/call":
		return s.call(ctx, req), true
	}
	// Claude Code sends server/discover before initialize, and goes on after this error.
	return failure(req.ID, codeMethodNotFound, "method not found: "+req.Method), true
}

// validID reports whether id is a string or a number, as the protocol wants.
func validID(id json.RawMessage) bool {
	var v any
	if err := json.Unmarshal(id, &v); err != nil {
		return false
	}
	switch v.(type) {
	case string, float64:
		return true
	}
	return false
}

// initialize answers with the version that the client asks for when the server speaks it,
// else with the newest one: the client then decides whether it can go on.
func (s server) initialize(req request) response {
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

func (s server) call(ctx context.Context, req request) response {
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
	res, err := s.h.Call(ctx, p.Name, p.Arguments)
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
