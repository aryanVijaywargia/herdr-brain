// Command brain is the one Brain binary: CLI, herdr plugin entry points and the agent hook.
package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"

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
  setup     [--session <name> [--test <folder>]]: bind a herdr session, add the agent hooks
  remember  "<text>" [--global | --workspace <w>] [--kind <k>]: draft an item for your review
  review    [<id> [--approve | --reject | --later]]: the queue of proposals (brain review --help)
  pin       [--off] <item>: pin an active item, so it never ages
  undo      <item>: undo the latest revision of an item
  forget    <item>: remove the text of an item from the store and the output
  repair    [--confirm]: repair the output and list its conflicts; --confirm deletes them
  pane      review --proposal <id>: the review pane of one proposal (herdr runs it)
  event     workspace-closed: herdr's workspace.closed event (D4); marks the workspace closed
  uninstall [--apply] [--data]: list what uninstall removes; --apply removes it, --data the stores too
  hook      --profile <name> --event start: the agent start hook (prints the brief or nothing)
`

// commands are the store commands of internal/cli, by name.
var commands = map[string]func(context.Context, cli.Env, []string) error{
	"remember":  cli.Remember,
	"review":    cli.Review,
	"pin":       cli.Pin,
	"undo":      cli.Undo,
	"forget":    cli.Forget,
	"repair":    cli.Repair,
	"pane":      cli.Pane,
	"uninstall": cli.Uninstall,
	"status":    cli.Status,
	"doctor":    cli.Doctor,
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
	case "status", "doctor", "remember", "review", "pin", "undo", "forget", "repair", "pane", "uninstall":
		err = command(commands[args[0]], args[1:], in, stdout, stderr)
	case "event":
		err = event.Main(context.Background(), args[1:], os.Environ())
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
