// Command brain is the one Brain binary: CLI, herdr plugin entry points and the agent hook.
package main

import (
	"fmt"
	"io"
	"os"

	"github.com/aryanVijaywargia/herdr-brain/internal/paths"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

const usage = `usage: brain <command>

commands:
  version   print the Brain version
  status    show Brain status
  setup     show the binary path and offer a ~/.local/bin/brain link
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	switch args[0] {
	case "version", "--version":
		fmt.Fprintf(stdout, "brain %s\n", version)
		return 0
	case "status":
		return status(stdout, stderr)
	case "setup":
		return setup(stdin, stdout, stderr)
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usage)
		return 0
	default:
		fmt.Fprintf(stderr, "brain: unknown command %q\n\n%s", args[0], usage)
		return 2
	}
}

func status(stdout, stderr io.Writer) int {
	bin, err := paths.BinaryPath()
	if err != nil {
		fmt.Fprintf(stderr, "brain: binary path: %v\n", err)
		return 1
	}
	data, err := paths.DataHome()
	if err != nil {
		fmt.Fprintf(stderr, "brain: data home: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "brain %s: not set up yet\nbinary: %s\ndata: %s\n", version, bin, data)
	return 0
}
