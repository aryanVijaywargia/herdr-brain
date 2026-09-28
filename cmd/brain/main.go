// Command brain is the one Brain binary: CLI, herdr plugin entry points and the agent hook.
package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"

	"github.com/aryanVijaywargia/herdr-brain/internal/event"
	"github.com/aryanVijaywargia/herdr-brain/internal/hook"
	"github.com/aryanVijaywargia/herdr-brain/internal/paths"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

const usage = `usage: brain <command>

commands:
  version   print the Brain version
  status    show Brain status
  setup     show the binary path and offer a ~/.local/bin/brain link
  event     workspace-closed: herdr's workspace.closed event (D4); marks the workspace closed
  hook      --profile <name> --event start: the agent start hook (prints the brief or nothing)
`

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
	case "status":
		err = status(stdout)
	case "setup":
		err = setup(in, stdout)
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

func status(stdout io.Writer) error {
	bin, err := paths.BinaryPath()
	if err != nil {
		return fmt.Errorf("binary path: %w", err)
	}
	data, err := paths.DataHome()
	if err != nil {
		return fmt.Errorf("data home: %w", err)
	}
	fmt.Fprintf(stdout, "brain %s: not set up yet\nbinary: %s\ndata: %s\n", version, bin, data)
	return nil
}
