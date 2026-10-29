package cli

import (
	"bytes"
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/aryanVijaywargia/herdr-brain/internal/bindings"
	"github.com/aryanVijaywargia/herdr-brain/internal/herdr"
	"github.com/aryanVijaywargia/herdr-brain/internal/mcp"
	"github.com/aryanVijaywargia/herdr-brain/internal/secret"
	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

// Scope is the herdr pane that a gateway session serves: the herdr server, the workspace and
// the pane.
type Scope struct {
	Server      herdr.Target // the herdr session and its API socket
	WorkspaceID string
	PaneID      string
}

// Identify returns the scope of the gateway session, or an error that refuses the call. The
// gateway asks it at each tools/list and each tools/call. It never takes the scope from the
// environment of the server: a shared CLI process can have the environment of another pane.
// A tools/list can ask it while a tool call runs, so it must be safe for concurrent use.
type Identify func(ctx context.Context) (Scope, error)

// Gateway is the MCP gateway (#172, E24-S6): the tools find_tool, run_tool, recall and remember
// for the pane of one session. Each call asks Identify for the scope, and then herdr checks the
// pane live in its workspace, as for each brain command. With no valid scope, the gateway lists
// no tool and refuses each call.
type Gateway struct {
	env      Env
	identify Identify
}

// NewGateway returns the gateway of the command context e. The scope of each call comes from
// identify, never from e.Environ. The answers go to the client through the MCP server, so the
// gateway writes nothing to e.Stdout; warnings go to e.Stderr, the log of the client.
func NewGateway(e Env, identify Identify) *Gateway {
	return &Gateway{env: e, identify: identify}
}

// errRefusedCall means that a call broke a rule of its gateway tool, so nothing ran and nothing
// was drafted.
var errRefusedCall = errors.New("refused")

// errAgentPreference refuses a preference draft from the agent: the sender of a gateway draft is
// the assistant, and only you can state your preference.
var errAgentPreference = errors.New("an agent cannot draft your preference: ask the user to run brain remember --kind preference")

// gatewayKinds are the kinds that the remember tool can draft.
var gatewayKinds = []store.Kind{store.KindFact, store.KindDecision, store.KindHandoff}

// gatewayTool is one gateway tool: its MCP definition and its call on an open command call. The
// call writes its answer to e.Stdout.
type gatewayTool struct {
	def  mcp.Tool
	call func(ctx context.Context, e Env, c *Call, args json.RawMessage) error
}

// gatewayTools are the four gateway tools.
func gatewayTools() []gatewayTool {
	return []gatewayTool{
		{mcp.Tool{Name: "find_tool", Description: "Find the approved Brain tools of this workspace and the global ones, " +
			"with their inputs. With a query, only the tools whose name or description match its words.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string",` +
				`"description":"words to match; empty lists all approved tools"}},"additionalProperties":false}`)},
			findToolCall},
		{mcp.Tool{Name: "run_tool", Description: "Run an approved Brain tool that has the reviewed read-only restriction, " +
			"with its inputs. Run any other tool with brain run in your shell.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"tool":{"type":"string"},` +
				`"inputs":{"type":"object","additionalProperties":{"type":["string","integer"]}}},` +
				`"required":["tool"],"additionalProperties":false}`)},
			runToolCall},
		{mcp.Tool{Name: "recall", Description: "Find the approved Brain items, skills and tools of this workspace " +
			"and the global ones whose text matches the words of the query, best match first.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"}},` +
				`"required":["query"],"additionalProperties":false}`)},
			recallCall},
		{mcp.Tool{Name: "remember", Description: "Draft a Brain item for this workspace, or a global one. " +
			"The draft waits for the user's review; it is not live until the user approves it.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"},` +
				`"kind":{"type":"string","enum":["fact","decision","handoff"]},"global":{"type":"boolean"}},` +
				`"required":["text"],"additionalProperties":false}`)},
			rememberCall},
	}
}

// Tools lists the four tools when the scope is valid and herdr confirms its pane, else none.
// The reason for none goes to the log of the client.
func (g *Gateway) Tools(ctx context.Context) []mcp.Tool {
	if err := g.check(ctx); err != nil {
		// The list is the answer; a failed write to the log changes nothing in it.
		_, _ = fmt.Fprintf(g.env.Stderr, "brain mcp: no tools: %v\n", err)
		return nil
	}
	var defs []mcp.Tool
	for _, t := range gatewayTools() {
		defs = append(defs, t.def)
	}
	return defs
}

// check checks the scope, and the pane of it live with herdr. It writes nothing.
func (g *Gateway) check(ctx context.Context) error {
	e, _, err := g.scoped(ctx, io.Discard)
	if err != nil {
		return err
	}
	c, err := e.open(ctx, openRead)
	if err != nil {
		return err
	}
	return c.Close()
}

// Call runs gateway tool name. A call with no valid scope is refused and writes nothing. A call
// whose pane herdr does not confirm is refused, and its refused row goes to the store of the
// herdr server of the scope, when one is bound. Any other call writes one gateway_calls row.
func (g *Gateway) Call(ctx context.Context, name string, args json.RawMessage) (mcp.Result, error) {
	var out bytes.Buffer
	e, s, err := g.scoped(ctx, &out)
	if err != nil {
		return refusal(err), nil
	}
	tools := gatewayTools()
	i := slices.IndexFunc(tools, func(t gatewayTool) bool { return t.def.Name == name })
	c, err := e.Open(ctx)
	switch {
	case err != nil && i < 0:
		return refusal(err), nil
	case err != nil:
		return refusal(errors.Join(err, g.logRefusal(ctx, s, name, e.Now()))), nil
	case i < 0:
		return mcp.Result{}, errors.Join(mcp.ErrUnknownTool, c.Close())
	}
	err = tools[i].call(ctx, e, c, args)
	result := gatewayResult(err)
	// A call that ran is logged also when the client stopped waiting.
	wctx := context.WithoutCancel(ctx)
	logErr := c.Store.Write(wctx, func(tx *sql.Tx) error {
		_, err := store.AddGatewayCall(wctx, tx, store.GatewayCall{Method: name, Result: result, At: e.Now()})
		return err
	})
	if err := errors.Join(err, logErr, c.Close()); err != nil {
		return mcp.Result{Text: withError(out.String(), err), IsError: true}, nil
	}
	return mcp.Result{Text: out.String()}, nil
}

// logRefusal writes the refused row of a call of method whose pane check failed. The row goes
// to the store that the bindings file binds to the socket of scope s; with no such binding,
// there is no store and no row. It writes only the method, the result and the time, and reads
// nothing of the store, so the refused call gets no access to it.
func (g *Gateway) logRefusal(ctx context.Context, s Scope, method string, at time.Time) error {
	bs, _, err := bindings.Read(g.env.Bindings)
	if err != nil {
		return fmt.Errorf("log the refusal: %w", err)
	}
	b, ok, err := bindings.BySocket(bs, s.Server.Socket)
	if err != nil || !ok {
		return err
	}
	ctx = context.WithoutCancel(ctx)
	st, err := store.Open(ctx, b.Store, busyTimeout)
	if err != nil {
		return fmt.Errorf("log the refusal: %w", err)
	}
	err = st.Write(ctx, func(tx *sql.Tx) error {
		_, err := store.AddGatewayCall(ctx, tx, store.GatewayCall{Method: method, Result: store.GatewayRefused, At: at})
		return err
	})
	return errors.Join(err, st.Close())
}

// scoped returns the command context of a call of the scope that Identify gives, and the
// scope: the herdr variables of the scope come last in the environment, so they win over any
// inherited ones in the resolver and in a tool run. The answer of the call goes to out.
func (g *Gateway) scoped(ctx context.Context, out io.Writer) (Env, Scope, error) {
	s, err := g.identify(ctx)
	if err != nil {
		return Env{}, Scope{}, fmt.Errorf("no valid Brain launch for this session: %w", err)
	}
	e := g.env
	e.Environ = append(e.Environ[:len(e.Environ):len(e.Environ)], "HERDR_ENV=1",
		"HERDR_SOCKET_PATH="+s.Server.Socket, "HERDR_SESSION="+s.Server.Session,
		"HERDR_PANE_ID="+s.PaneID, "HERDR_WORKSPACE_ID="+s.WorkspaceID)
	e.Stdout = out
	return e, s, nil
}

// refusal is the result of a call that the checks of the session refused.
func refusal(err error) mcp.Result {
	return mcp.Result{Text: fmt.Sprintf("refused: %v", err), IsError: true}
}

// gatewayResult is the gateway_calls result of a call that ended with err.
func gatewayResult(err error) store.GatewayResult {
	switch {
	case err == nil:
		return store.GatewayOK
	case errors.Is(err, errRefusedCall), errors.Is(err, ErrRefused), errors.As(err, new(*scopeError)):
		return store.GatewayRefused
	}
	return store.GatewayFailed
}

// withError is the text of a call that ended with err: its output, then the error.
func withError(out string, err error) string {
	if out != "" && !strings.HasSuffix(out, "\n") {
		out += "\n"
	}
	return out + err.Error()
}

// refuseCall is the error of a call that broke a rule of its tool.
func refuseCall(err error) error { return fmt.Errorf("%w: %w", errRefusedCall, err) }

// decodeArgs reads the arguments of a call into v. A field that the tool does not have is
// refused. No arguments are an empty object.
func decodeArgs(args json.RawMessage, v any) error {
	if args == nil {
		return nil
	}
	d := json.NewDecoder(bytes.NewReader(args))
	d.DisallowUnknownFields()
	d.UseNumber()
	if err := d.Decode(v); err != nil {
		return refuseCall(fmt.Errorf("arguments: %w", err))
	}
	return nil
}

// findToolCall lists the approved tools that the pane can run, as brain tools does, and says for
// each whether run_tool can run it.
func findToolCall(ctx context.Context, e Env, c *Call, args json.RawMessage) error {
	var a struct {
		Query string `json:"query"`
	}
	if err := decodeArgs(args, &a); err != nil {
		return err
	}
	cl := newCaller(e, c)
	ts, err := cl.approvedTools(ctx, recallWords(a.Query))
	if err != nil {
		return err
	}
	if len(ts) == 0 {
		_, err = fmt.Fprintf(e.Stdout, "no approved tool matches %q\n", a.Query)
		return err
	}
	var b strings.Builder
	for _, t := range ts {
		writeTool(&b, t.Tool, t.contract, cl.target(t.Scope))
		if t.contract.ReadOnly {
			b.WriteString("  run_tool: yes (reviewed read-only)\n")
		} else {
			b.WriteString("  run_tool: no; run it with brain run in your shell\n")
		}
	}
	_, err = fmt.Fprint(e.Stdout, b.String())
	return err
}

// runToolCall runs a tool with the checks of brain run, and only a tool with the reviewed
// read-only restriction. Its output is the answer. A call of a known tool writes its tool_calls
// row, as brain run does.
func runToolCall(ctx context.Context, e Env, c *Call, args json.RawMessage) error {
	var a struct {
		Tool   string         `json:"tool"`
		Inputs map[string]any `json:"inputs"`
	}
	if err := decodeArgs(args, &a); err != nil {
		return err
	}
	if a.Tool == "" {
		return refuseCall(errors.New("want a tool name"))
	}
	inputs, inputErr := toolInputs(a.Inputs)
	_, err := runOn(ctx, e, c, ToolRun{PaneID: c.PaneID, WorkspaceID: c.WorkspaceID, Origin: store.OriginAgent,
		Tool: a.Tool, ReadOnly: true, Inputs: inputs, Stdout: e.Stdout, Stderr: e.Stdout}, inputErr)
	if errors.Is(err, errNotReadOnly) {
		return fmt.Errorf("%w; run it with brain run %s in your shell", err, a.Tool)
	}
	return err
}

// toolInputs are the input values of a run_tool call as brain run takes them: a string, or the
// digits of a number. Any other value is a bad input.
func toolInputs(in map[string]any) (map[string]string, error) {
	out := make(map[string]string, len(in))
	for _, k := range slices.Sorted(maps.Keys(in)) {
		switch v := in[k].(type) {
		case string:
			out[k] = v
		case json.Number:
			out[k] = v.String()
		default:
			return nil, fmt.Errorf("input %s: want a string or an integer", k)
		}
	}
	return out, nil
}

// recallCall runs brain recall for the words of the query.
func recallCall(ctx context.Context, e Env, c *Call, args json.RawMessage) error {
	var a struct {
		Query string `json:"query"`
	}
	if err := decodeArgs(args, &a); err != nil {
		return err
	}
	words := recallWords(a.Query)
	if len(words) == 0 {
		return refuseCall(errors.New("want words to match in the query"))
	}
	return recallOn(ctx, e, c, words)
}

// rememberCall drafts an item as brain remember does. The sender of the draft is the assistant,
// so it is never a preference, and it goes live only when you approve it in review.
func rememberCall(ctx context.Context, e Env, c *Call, args json.RawMessage) error {
	var a struct {
		Text   string `json:"text"`
		Kind   string `json:"kind"`
		Global bool   `json:"global"`
	}
	if err := decodeArgs(args, &a); err != nil {
		return err
	}
	// The filter runs before any other check, as in brain remember (E4-S1).
	if err := secret.Check(a.Text); err != nil {
		return refuseCall(err)
	}
	r := remember{text: a.Text, global: a.Global, kind: store.Kind(cmp.Or(a.Kind, string(store.KindFact)))}
	switch {
	case r.kind == store.KindPreference:
		return refuseCall(errAgentPreference)
	case !slices.Contains(gatewayKinds, r.kind):
		return refuseCall(fmt.Errorf("unknown kind %q: want fact, decision or handoff", a.Kind))
	}
	if err := r.check(); err != nil {
		return refuseCall(err)
	}
	return rememberOn(ctx, e, c, r)
}
