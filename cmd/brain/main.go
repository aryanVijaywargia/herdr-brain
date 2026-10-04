// Command brain is the one Brain binary: CLI, herdr plugin entry points and the agent hook.
package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/aryanVijaywargia/herdr-brain/internal/brained"
	"github.com/aryanVijaywargia/herdr-brain/internal/cli"
	"github.com/aryanVijaywargia/herdr-brain/internal/event"
	"github.com/aryanVijaywargia/herdr-brain/internal/hook"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

const usage = `usage: brain <command>

commands:
  version   print the Brain version
  status    show Brain status
  doctor    check the Brain setup and print the fix of each failed check
  setup     [--session <name> [--test <folder>]] [--extractor]: bind a herdr session, add the agent hooks,
            choose the extractor CLI
  remember  "<text>" [--global | --workspace <w>] [--kind <k>]: draft an item for your review
  review    [<id> [--approve | --reject | --later | --change "<text>"]]: the queue of proposals (brain review --help)
  pin       [--off] <item>: pin an active item, so it never ages
  archive   [--all | --keep]: list the items with no relevant session for 90 days; --all retires them, --keep keeps them
  promote   <item>: propose to move a workspace item to the global part of the brief
  undo      <item>: undo the latest revision of an item
  workspace link <old> <new> [--confirm]: move the items of an old workspace key to this workspace
  forget    <item> | --session <id>: remove the text of an item, or the capture evidence of a session
  repair    [--confirm]: repair the output and list its conflicts; --confirm deletes them
  pane      review --proposal <id>: the review pane of one proposal (herdr runs it)
  event     workspace-closed: herdr's workspace.closed event (D4); marks the workspace closed
  learn     on|off [--workspace <id> | --pane <id>]: record the learning switch; delivery stays on
  extract   on|off [--workspace <id>]: record the extraction switch; capture and rules keep running
  uninstall [--apply] [--data]: list what uninstall removes; --apply removes it, --data the stores too
  hook      --profile <name> --event start|prompt|tool|stop: the agent hooks (start prints the brief or nothing)
  brained   the herdr listener of one herdr session (herdr starts it; D7)
  suggest   the live suggestion of one prompt (the hook or brained starts it)
  run       <tool> [--<input> <value>]...: run an approved tool in this workspace; exits with its exit code
  automation add <tool> --folder <dir> --cli <cli> | list | pause <item> | resume <item> | remove <item>:
            run an approved tool after each stop of the agent (brain automation --help)
`

// commands are the store commands of internal/cli, by name.
var commands = map[string]func(context.Context, cli.Env, []string) error{
	"remember":   cli.Remember,
	"review":     cli.Review,
	"pin":        cli.Pin,
	"archive":    cli.Archive,
	"promote":    cli.Promote,
	"undo":       cli.Undo,
	"workspace":  cli.Workspace,
	"forget":     cli.Forget,
	"repair":     cli.Repair,
	"pane":       cli.Pane,
	"uninstall":  cli.Uninstall,
	"status":     cli.Status,
	"doctor":     cli.Doctor,
	"learn":      cli.Learn,
	"extract":    cli.Extract,
	"suggest":    cli.Suggest,
	"run":        cli.Run,
	"automation": cli.Automation,
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	if args[0] == "hook" {
		// The hook never fails a session start: on any error it prints nothing and exits 0 (§8.3).
		_ = hook.Main(args[1:], os.Environ(), stdin, stdout)
		return 0
	}
	in := bufio.NewReader(stdin)
	var err error
	switch args[0] {
	case "version", "--version":
		fmt.Fprintf(stdout, "brain %s\n", version)
	case "setup":
		err = setup(context.Background(), osSetupDeps(os.Environ()), args[1:], in, stdout)
	case "status", "doctor", "remember", "review", "pin", "archive", "promote", "undo", "workspace", "forget", "repair", "pane", "uninstall", "learn", "extract",
		"suggest", "run", "automation":
		err = command(commands[args[0]], args[1:], in, stdout, stderr)
	case "event":
		err = event.Main(context.Background(), args[1:], os.Environ())
	case "brained":
		err = brained.Main(context.Background(), os.Environ(), stderr)
		if errors.Is(err, brained.ErrStop) {
			fmt.Fprintf(stderr, "brain brained: %v\n", err)
			return 0
		}
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usage)
	default:
		fmt.Fprintf(stderr, "brain: unknown command %q\n\n%s", args[0], usage)
		return 2
	}
	code := 0
	if err != nil {
		fmt.Fprintf(stderr, "brain %s: %v\n", args[0], err)
		code = 1
		// brain run exits with the exit code of the tool (E17-S4).
		var x *cli.ExitError
		if errors.As(err, &x) {
			code = x.Code
		}
	}
	// A popup pane closes when its command exits, so wait until the user has read the output.
	if args[0] == "setup" && os.Getenv("HERDR_PLUGIN_ENTRYPOINT_ID") != "" {
		fmt.Fprint(stdout, "Press Enter to close.")
		_, _ = in.ReadString('\n') // any error, such as EOF, also ends the wait
	}
	return code
}

// command runs a store command of internal/cli with the process environment.
func command(fn func(context.Context, cli.Env, []string) error, args []string, in io.Reader, stdout, stderr io.Writer) error {
	e, err := cli.NewEnv(os.Environ(), stdout, stderr)
	if err != nil {
		return err
	}
	e.Stdin = in
	return fn(context.Background(), e, args)
}
