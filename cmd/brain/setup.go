package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/aryanVijaywargia/herdr-brain/internal/paths"
)

// setup is the first command a new user runs, from the herdr `setup` action or by its path.
// This is the E1-S2 stub: it shows the binary path and offers the ~/.local/bin/brain link.
// Bindings and hook entries come with E9. It never edits a shell config file.
func setup(stdin io.Reader, stdout, stderr io.Writer) int {
	bin, err := paths.BinaryPath()
	if err != nil {
		fmt.Fprintf(stderr, "brain setup: binary path: %v\n", err)
		return 1
	}
	link, err := paths.LinkPath()
	if err != nil {
		fmt.Fprintf(stderr, "brain setup: link path: %v\n", err)
		return 1
	}
	in := bufio.NewReader(stdin)
	code := offerLink(bin, link, in, stdout, stderr)
	fmt.Fprintln(stdout, "Bindings and agent hooks are not built yet (E9).")
	// A popup pane closes when its command exits, so wait until the user has read the output.
	if os.Getenv("HERDR_PLUGIN_ENTRYPOINT_ID") != "" {
		fmt.Fprint(stdout, "Press Enter to close.")
		in.ReadString('\n')
	}
	return code
}

// offerLink shows the binary path and asks before it makes link -> bin.
// It never replaces a file that it did not make.
func offerLink(bin, link string, in *bufio.Reader, stdout, stderr io.Writer) int {
	fmt.Fprintf(stdout, "Brain binary: %s\n", bin)
	if target, err := filepath.EvalSymlinks(link); err == nil {
		if target == bin {
			fmt.Fprintf(stdout, "%s already points to it.\n", link)
			return 0
		}
		fmt.Fprintf(stdout, "%s exists and points elsewhere. Brain did not change it.\nRun Brain with: %s\n", link, bin)
		return 0
	} else if _, lerr := os.Lstat(link); lerr == nil {
		fmt.Fprintf(stdout, "%s exists but is broken. Brain did not change it.\nRun Brain with: %s\n", link, bin)
		return 0
	}

	fmt.Fprintf(stdout, "Make the symlink %s -> %s? [y/N] ", link, bin)
	answer, _ := in.ReadString('\n')
	if a := strings.ToLower(strings.TrimSpace(answer)); a != "y" && a != "yes" {
		fmt.Fprintf(stdout, "\nNo link made. Run Brain with: %s\n", bin)
		return 0
	}
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		fmt.Fprintf(stderr, "brain setup: %v\n", err)
		return 1
	}
	if err := os.Symlink(bin, link); err != nil {
		fmt.Fprintf(stderr, "brain setup: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "\nLinked %s.\n", link)
	if !onPath(filepath.Dir(link)) {
		fmt.Fprintf(stdout, "%s is not on your PATH. Add it yourself, or run Brain with: %s\n", filepath.Dir(link), bin)
	}
	return 0
}

func onPath(dir string) bool {
	for _, d := range filepath.SplitList(os.Getenv("PATH")) {
		if filepath.Clean(d) == dir {
			return true
		}
	}
	return false
}
