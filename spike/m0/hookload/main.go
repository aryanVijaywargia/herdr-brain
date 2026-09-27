// hookload is the E7-S4 probe. It extends the M0 hook probe (spike/m0/hook) with the steps
// that M0 did not time: the manifest check of an assumed E6 output generation and the
// receipt write to a SQLite store. It also drives the hook many times and prints percentiles.
//
//	hookload gen <store>              write a fake generation and an empty receipt table
//	hookload hook                     run as the SessionStart hook (env below)
//	hookload drive -n N -out <file>   run the hook N times as child processes, log wall time
//	hookload stats <file.jsonl>...    print n, p50, p95 and max of each ms_ field
//
// Hook env: HOOKLOAD_STORE (store folder), HOOKLOAD_KEY (workspace key), HOOKLOAD_LOG (a
// JSONL file for the timing record), and the herdr pane env.
package main

import (
	"fmt"
	"io"
	"os"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		_, _ = fmt.Fprintln(stderr, "usage: hookload gen|hook|drive|stats") // terminal: no one to report to
		return 2
	}
	var err error
	switch args[0] {
	case "gen":
		if len(args) != 2 {
			_, _ = fmt.Fprintln(stderr, "usage: hookload gen <store>") // terminal: no one to report to
			return 2
		}
		err = gen(args[1], defaultKey)
	case "hook":
		// A hook prints nothing on an error and exits 0 (HLD §8.3 step 7). The error goes to
		// the timing log only.
		hook(stdin, stdout, os.Getenv)
		return 0
	case "drive":
		err = drive(args[1:], stderr)
	case "stats":
		err = stats(args[1:], stdout)
	default:
		err = fmt.Errorf("unknown command %q", args[0])
	}
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "hookload: %v\n", err) // terminal: no one to report to
		return 1
	}
	return 0
}
