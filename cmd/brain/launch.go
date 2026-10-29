package main

import (
	"context"
	"os"
	"syscall"

	"github.com/aryanVijaywargia/herdr-brain/internal/cli"
	"github.com/aryanVijaywargia/herdr-brain/internal/launch"
)

// launchCommand returns `brain <kind> [args]` (E24-S2) for the CLI of kind. It returns only on
// an error: else the CLI replaces the process (syscall.Exec).
func launchCommand(kind string) func(context.Context, cli.Env, []string) error {
	return func(ctx context.Context, e cli.Env, args []string) error {
		self, err := os.Executable()
		if err != nil {
			return err
		}
		return launch.Main(ctx, launch.Deps{Environ: e.Environ, Self: self, Bindings: e.Bindings, Pane: e.Pane,
			Now: e.Now, Stderr: e.Stderr, Exec: syscall.Exec}, kind, args)
	}
}
