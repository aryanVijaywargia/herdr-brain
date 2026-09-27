// Command brain is the one Brain binary: CLI, herdr plugin entry points and the agent hook.
package main

import (
	"fmt"
	"io"
	"os"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

const usage = `usage: brain <command>

commands:
  version   print the Brain version
  status    show Brain status
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	switch args[0] {
	case "version", "--version":
		fmt.Fprintf(stdout, "brain %s\n", version)
		return 0
	case "status":
		fmt.Fprintf(stdout, "brain %s: not set up yet\n", version)
		return 0
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usage)
		return 0
	default:
		fmt.Fprintf(stderr, "brain: unknown command %q\n\n%s", args[0], usage)
		return 2
	}
}
