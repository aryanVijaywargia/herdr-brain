// Command brain is the one Brain binary: CLI, herdr plugin entry points and the agent hook.
package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"

	"github.com/aryanVijaywargia/herdr-brain/internal/brained"
	"github.com/aryanVijaywargia/herdr-brain/internal/cli"
	"github.com/aryanVijaywargia/herdr-brain/internal/event"
	"github.com/aryanVijaywargia/herdr-brain/internal/hook"
	"github.com/aryanVijaywargia/herdr-brain/internal/update"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

const usage = `usage: brain <command>   (brain <command> --help: the usage of one command)

commands:
  version   print the Brain version
  status    show Brain status
  doctor    [--fix]: check the Brain setup and print the fix of each failed check; --fix applies each
            offered repair with no question
  setup     [--session <name> [--test <folder>]] [--extractor]: bind a herdr session, add the agent hooks,
            choose the extractor CLI
  remember  "<text>" [--global | --workspace <w>] [--kind <k>]: draft an item for your review
  recall    <words>: print the approved items of this pane's brief that match the words, best match first
  review    [<id> [--approve | --reject | --later | --change "<text>"]]: the queue of proposals (brain review --help)
  pin       [--off] <item>: pin an active item, so it never ages
  archive   [--all | --keep]: list the items with no relevant session for 90 days; --all retires them, --keep keeps them
  promote   <item>: propose to move a workspace item to the global part of the brief
  undo      <item>: undo the latest revision of an item
  workspace link <old> <new> [--confirm]: move the items of an old workspace key to this workspace
  forget    <item> | --session <id>: remove the text of an item, or the capture evidence of a session
  repair    [--confirm]: repair the output and list its conflicts; --confirm deletes them
  pane      review --proposal <id> | recall: the review pane of one proposal, or the recall popup (herdr runs them)
  event     workspace-closed: herdr's workspace.closed event (D4); marks the workspace closed
  learn     on|off [--workspace <id> | --pane <id>]: record the learning switch; delivery stays on
  extract   on|off [--workspace <id>]: record the extraction switch; capture and rules keep running
  uninstall [--apply] [--data]: list what uninstall removes; --apply removes it, --data the stores too
  hook      --profile <name> --event start|prompt|tool|stop: the agent hooks (start prints the brief or nothing)
  brained   the herdr listener of one herdr session (herdr starts it; D7)
  start     start brained for this herdr session, and open setup when it has no binding (the start action)
  update    [--check | --records]: update the Brain plugin to the newest commit of its repo, add the missing
            hook record entries (one question), then run brain doctor --fix; --check only compares;
            --records only adds the hook record entries. The only Brain command that uses the network
  suggest   the live suggestion of one prompt (the hook or brained starts it)
  run       <tool> [--<input> <value>]...: run an approved tool in this workspace; exits with its exit code
  tools     list the approved tools that this workspace can run, with their inputs
  automation add <tool> --folder <dir> --cli <cli> | list | pause <item> | resume <item> | remove <item>:
            run an approved tool after each stop of the agent (brain automation --help)
`

// commands are the store commands of internal/cli, by name.
var commands = map[string]func(context.Context, cli.Env, []string) error{
	"remember":   cli.Remember,
	"recall":     cli.Recall,
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
	"tools":      cli.Tools,
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
	if helpFlag(args[0], args[1:]) {
		fmt.Fprint(stdout, commandUsage(args[0]))
		return 0
	}
	in := bufio.NewReader(stdin)
	var err error
	switch args[0] {
	case "version", "--version":
		fmt.Fprintf(stdout, "brain %s\n", version)
	case "setup":
		err = setup(context.Background(), osSetupDeps(os.Environ()), args[1:], in, stdout)
	case "status", "doctor", "remember", "recall", "review", "pin", "archive", "promote", "undo", "workspace", "forget", "repair", "pane", "uninstall", "learn", "extract",
		"suggest", "run", "tools", "automation":
		err = command(commands[args[0]], args[1:], in, stdout, stderr)
	case "update":
		err = updateMain(args[1:], stdin, stdout, stderr)
	case "event":
		err = event.Main(context.Background(), args[1:], os.Environ())
	case "start":
		err = start(context.Background(), osStartDeps(os.Environ()))
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
	if err != nil {
		fmt.Fprintf(stderr, "brain %s: %v\n", args[0], err)
	}
	// A popup pane closes when its command exits, so wait until the user has read the output.
	if args[0] == "setup" && os.Getenv("HERDR_PLUGIN_ENTRYPOINT_ID") != "" {
		fmt.Fprint(stdout, "Press Enter to close.")
		_, _ = in.ReadString('\n') // any error, such as EOF, also ends the wait
	}
	return exitCode(err)
}

// exitCode is the exit code of a command that returned err: 0 for none, and 1 for an error,
// except that brain run exits with the exit code of the tool (E17-S4).
func exitCode(err error) int {
	var x *cli.ExitError
	switch {
	case err == nil:
		return 0
	case errors.As(err, &x):
		return x.Code
	}
	return 1
}

// updateMain runs brain update with stdin, the input of the process. The new binary of an
// update asks its question on stdin itself, so it gets the file, not a reader: exec copies
// from a reader until EOF, so update would wait for one more line after the new binary exits.
func updateMain(args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	return update.Main(context.Background(), args, os.Environ(), stdin, stdout, stderr, func(r io.Reader, w io.Writer) error {
		return updateRecords(osSetupDeps(os.Environ()), bufio.NewReader(r), w)
	})
}

// helpFlag reports whether the arguments args of command name are only -h or --help, for a
// command that takes arguments (#318). The hook gets no help: it prints only the brief.
func helpFlag(name string, args []string) bool {
	_, store := commands[name]
	cmd := store || slices.Contains([]string{"setup", "update", "event", "start", "brained"}, name)
	return cmd && len(args) == 1 && (args[0] == "-h" || args[0] == "--help")
}

// commandUsage is the text of `brain <name> --help` (#318): the usage of the command, or the
// usage of brain for a command with none of its own.
func commandUsage(name string) string {
	switch name {
	case "setup":
		return setupUsage + "\n"
	case "update":
		return update.Usage + "\n"
	}
	if u := cli.Usage(name); u != "" {
		return u + "\n"
	}
	return usage
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
