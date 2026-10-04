package cli

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/aryanVijaywargia/herdr-brain/internal/output"
	"github.com/aryanVijaywargia/herdr-brain/internal/store"
	"github.com/aryanVijaywargia/herdr-brain/internal/tool"
)

const runUsage = `usage: brain run <tool> [--<input> <value>]...`

// ErrRefused means that a check refused a tool call, so nothing ran (§10.1).
var ErrRefused = errors.New("refused; nothing ran")

// ExitError is the error of a tool run whose last step did not exit 0. brain run exits with
// Code. [NEW] brain run exits with the tool's exit code.
type ExitError struct {
	Code int
	Err  error
}

func (x *ExitError) Error() string { return x.Err.Error() }

func (x *ExitError) Unwrap() error { return x.Err }

// ToolRun is one call of a tool: the pane that calls it, who calls, the tool and its inputs.
// brain run takes the pane from the agent's environment; the automation runner (E17-S11) gives
// the pane of the event. Both get the same checks in RunTool.
type ToolRun struct {
	PaneID      string
	WorkspaceID string
	Origin      store.CallOrigin
	Tool        string
	// ToolID and ContractHash pin the call of an automation to the tool and the contract of
	// its binding: another tool of the name, or another contract, is refused. 0 and "" pin
	// nothing.
	ToolID       int64
	ContractHash string
	Inputs       map[string]string
	Stdout       io.Writer
	Stderr       io.Writer
}

// ToolResult is the result of a tool run that passed the checks.
type ToolResult struct {
	CallID   int64 // the tool_calls row
	ExitCode int
	Duration time.Duration
}

// Run runs `brain run <tool> [--<input> <value>]` (E17-S4, §10.1) in the agent's shell. It
// writes the tool's output to its own and exits with the tool's exit code. brain run is not
// a sandbox: it checks the call and runs the reviewed command.
func Run(ctx context.Context, e Env, args []string) error {
	name, inputs, inputErr := parseRun(args)
	if name == "" {
		return inputErr
	}
	// Ctrl-C or a stop of the agent's shell stops the tool's process group too.
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	res, err := runChecked(ctx, e, ToolRun{PaneID: lookupEnv(e.Environ, "HERDR_PANE_ID"),
		WorkspaceID: lookupEnv(e.Environ, "HERDR_WORKSPACE_ID"), Origin: store.OriginAgent, Tool: name,
		Inputs: inputs, Stdout: e.Stdout, Stderr: e.Stderr}, inputErr)
	if err != nil && res.CallID != 0 {
		return &ExitError{Code: res.ExitCode, Err: err}
	}
	return err
}

// parseRun reads the tool name and the inputs: --<input> <value> or --<input>=<value>, each
// input once. [NEW] the flag forms. On a bad input argument it still returns the name, so the
// call of a known tool is logged as refused; the name is "" when there is none.
func parseRun(args []string) (string, map[string]string, error) {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return "", nil, fmt.Errorf("want a tool name first\n%s", runUsage)
	}
	name, inputs := args[0], map[string]string{}
	for rest := args[1:]; len(rest) > 0; {
		flag, ok := strings.CutPrefix(rest[0], "--")
		if !ok || flag == "" {
			return name, nil, fmt.Errorf("unexpected argument %q\n%s", rest[0], runUsage)
		}
		in, v, ok := strings.Cut(flag, "=")
		rest = rest[1:]
		if !ok {
			if len(rest) == 0 {
				return name, nil, fmt.Errorf("--%s has no value\n%s", in, runUsage)
			}
			v, rest = rest[0], rest[1:]
		}
		if _, dup := inputs[in]; dup {
			return name, nil, fmt.Errorf("--%s is given twice", in)
		}
		inputs[in] = v
	}
	return name, inputs, nil
}

// RunTool runs the tool r.Tool for pane r.PaneID of workspace r.WorkspaceID. The checks come in
// order: herdr confirms the pane in its workspace live, through the resolver (§8.3); the tool
// is a tool of that workspace or a global one; output.Runnable checks the approval, the
// contract hash and the tool files; each input matches its type and its pattern. Then it runs
// the commands with no shell in the cwd of the contract, within its timeout.
//
// A call from outside herdr, from an unbound session or from a pane that herdr does not
// confirm runs nothing and writes nothing. Any other call of a known tool writes one tool_calls
// row: refused when a check fails, else ran with the exit code. A ToolResult with a CallID
// comes with each row; the error is the refusal, or the failure of the run.
func RunTool(ctx context.Context, e Env, r ToolRun) (ToolResult, error) {
	return runChecked(ctx, e, r, nil)
}

// runChecked is RunTool. inputErr is a bad input argument of brain run: once the tool is found,
// the call is refused with it.
func runChecked(ctx context.Context, e Env, r ToolRun, inputErr error) (_ ToolResult, err error) {
	e.Environ = withPane(e.Environ, r.PaneID, r.WorkspaceID)
	c, err := e.Open(ctx)
	if err != nil {
		return ToolResult{}, fmt.Errorf("tool %s: %w", r.Tool, err)
	}
	defer func() { err = errors.Join(err, c.Close()) }()
	found, session, err := findTool(ctx, c, r.Tool)
	if err != nil {
		return ToolResult{}, err
	}
	call := store.ToolCall{ToolID: found.ID, SessionID: session, Origin: r.Origin, At: e.Now()}
	refuse := func(why error) (ToolResult, error) {
		call.Result = store.CallRefused
		if found.ApprovedHash != nil {
			call.ContractHash = *found.ApprovedHash
		}
		id, err := addCall(context.WithoutCancel(ctx), c.Store, call)
		if err != nil {
			return ToolResult{}, errors.Join(why, err)
		}
		return ToolResult{CallID: id, ExitCode: 1}, fmt.Errorf("tool %s: %w: %w", r.Tool, why, ErrRefused)
	}
	if inputErr != nil {
		return refuse(inputErr)
	}
	if r.ToolID != 0 && found.ID != r.ToolID {
		return refuse(fmt.Errorf("the tool of the name is item %d, not item %d of the binding", found.ID, r.ToolID))
	}
	// Any failed check of a known tool is a refusal, also a tool file that cannot be read.
	t, dir, err := output.Runnable(ctx, c.Store, filepath.Join(c.Binding.Store, outDir), found.ID)
	if err != nil {
		return refuse(err)
	}
	if r.ContractHash != "" && t.ContractHash != r.ContractHash {
		return refuse(fmt.Errorf("the contract hash is %s, not %s of the binding", t.ContractHash, r.ContractHash))
	}
	contract, err := tool.Parse(t.Contract)
	if err != nil {
		return refuse(err)
	}
	cmds, err := contract.Fill(r.Inputs)
	if err != nil {
		return refuse(err)
	}
	run := tool.Run{Commands: cmds, Dir: contract.Dir(lookupEnv(e.Environ, "HOME")), ScriptDir: dir,
		Env: e.Environ, Timeout: time.Duration(t.TimeoutS) * time.Second, Stdout: r.Stdout, Stderr: r.Stderr}
	call.ContractHash = t.ContractHash
	return execTool(ctx, c.Store, run, call, r.Tool)
}

// findTool finds tool name for call c (store.ToolByName), and the open capture session of
// the calling pane, which gives the session key of the call.
func findTool(ctx context.Context, c *Call, name string) (found store.Tool, session int64, err error) {
	key, label, keyErr := c.WorkspaceKey()
	err = c.Store.Write(ctx, func(tx *sql.Tx) (err error) {
		if found, err = store.ToolByName(ctx, tx, name, key); err != nil {
			return err
		}
		session, err = store.PaneSession(ctx, tx, c.Binding.Session, c.PaneID)
		return err
	})
	switch {
	case errors.Is(err, sql.ErrNoRows) && keyErr != nil:
		return store.Tool{}, 0, fmt.Errorf("no global tool %s, and no workspace tools here (%w); nothing ran", name, keyErr)
	case errors.Is(err, sql.ErrNoRows):
		return store.Tool{}, 0, fmt.Errorf("no tool %s in workspace %s (%s) or in the global tools; nothing ran", name,
			c.WorkspaceID, label)
	}
	return found, session, err
}

// execTool runs run outside any transaction, and logs it as call, a ran row with the exit
// code and the duration.
func execTool(ctx context.Context, s *store.Store, run tool.Run, call store.ToolCall, name string) (ToolResult, error) {
	start := time.Now()
	code, runErr := run.Exec(ctx)
	call.Result, call.ExitCode, call.Duration = store.CallRan, &code, time.Since(start)
	// A Ctrl-C ends ctx and stops the run, but the call ran: log it still.
	id, err := addCall(context.WithoutCancel(ctx), s, call)
	if err != nil {
		return ToolResult{}, errors.Join(runErr, err)
	}
	res := ToolResult{CallID: id, ExitCode: code, Duration: call.Duration}
	if runErr != nil {
		return res, fmt.Errorf("tool %s: %w (exit code %d)", name, runErr, code)
	}
	return res, nil
}

func addCall(ctx context.Context, s *store.Store, c store.ToolCall) (id int64, err error) {
	err = s.Write(ctx, func(tx *sql.Tx) error {
		id, err = store.AddToolCall(ctx, tx, c)
		return err
	})
	return id, err
}

// withPane returns environ with the pane and the workspace of a call; the resolver checks
// them with herdr.
func withPane(environ []string, paneID, workspaceID string) []string {
	return append(environ[:len(environ):len(environ)], "HERDR_PANE_ID="+paneID, "HERDR_WORKSPACE_ID="+workspaceID)
}
