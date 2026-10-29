package main

import (
	"context"
	"fmt"
	"time"

	"github.com/aryanVijaywargia/herdr-brain/internal/cli"
	"github.com/aryanVijaywargia/herdr-brain/internal/herdr"
	"github.com/aryanVijaywargia/herdr-brain/internal/launch"
	"github.com/aryanVijaywargia/herdr-brain/internal/mcp"
)

// checkDeadline is the deadline of the launch check of one MCP request: its herdr call.
const checkDeadline = 5 * time.Second

// mcpCommand is `brain mcp --launch <launch folder>` (E24-S6): the MCP gateway of one launch of
// `brain claude` or `brain codex`, on stdin and stdout. The CLI starts it from the launch flags.
func mcpCommand(ctx context.Context, e cli.Env, args []string) error {
	if len(args) != 2 || args[0] != "--launch" || args[1] == "" {
		return fmt.Errorf("want --launch <folder>, got %q", args)
	}
	return mcp.Serve(ctx, e.Stdin, e.Stdout, version, cli.NewGateway(e, launchScope(e.Bindings, args[1], e.Environ)))
}

// launchScope is the identity of the gateway of the launch folder dir: launch.Check on each
// request, so a launch that ended, or a pane that herdr does not confirm, gets no scope. The
// herdr session and socket come from the binding of the launch record, the workspace and the
// pane from the record. environ is only a hint: a HERDR_PANE_ID of another pane refuses.
func launchScope(bindingsFile, dir string, environ []string) cli.Identify {
	return func(ctx context.Context) (cli.Scope, error) {
		ctx, cancel := context.WithTimeout(ctx, checkDeadline)
		defer cancel()
		l, err := launch.Check(ctx, bindingsFile, dir, environ)
		if err != nil {
			return cli.Scope{}, err
		}
		return cli.Scope{Server: herdr.Target{Session: l.Binding.Session, Socket: l.Binding.Socket},
			WorkspaceID: l.WorkspaceID, PaneID: l.PaneID}, nil
	}
}
