package main

import (
	"bufio"
	"errors"
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
func setup(in *bufio.Reader, stdout io.Writer) error {
	bin, err := paths.BinaryPath()
	if err != nil {
		return fmt.Errorf("binary path: %w", err)
	}
	link, err := paths.LinkPath()
	if err != nil {
		return fmt.Errorf("link path: %w", err)
	}
	if err := offerLink(bin, link, in, stdout); err != nil {
		return err
	}
	fmt.Fprintln(stdout, "Bindings and agent hooks are not built yet (E9).")
	return nil
}

// offerLink shows the binary path and asks before it makes link -> bin.
// It never replaces a file that it did not make.
func offerLink(bin, link string, in *bufio.Reader, stdout io.Writer) error {
	fmt.Fprintf(stdout, "Brain binary: %s\n", bin)
	target, err := filepath.EvalSymlinks(link)
	switch {
	case err == nil && target == bin:
		fmt.Fprintf(stdout, "%s already points to it.\n", link)
		return nil
	case err == nil:
		fmt.Fprintf(stdout, "%s exists and points elsewhere. Brain did not change it.\nRun Brain with: %s\n", link, bin)
		return nil
	case !errors.Is(err, os.ErrNotExist):
		return fmt.Errorf("check link %s: %w", link, err)
	}
	if _, err := os.Lstat(link); err == nil {
		fmt.Fprintf(stdout, "%s exists but is broken. Brain did not change it.\nRun Brain with: %s\n", link, bin)
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("check link %s: %w", link, err)
	}

	fmt.Fprintf(stdout, "Make the symlink %s -> %s? [y/N] ", link, bin)
	// EOF with no answer means "no".
	answer, err := in.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("read answer: %w", err)
	}
	if a := strings.ToLower(strings.TrimSpace(answer)); a != "y" && a != "yes" {
		fmt.Fprintf(stdout, "\nNo link made. Run Brain with: %s\n", bin)
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		return fmt.Errorf("create link dir: %w", err)
	}
	if err := os.Symlink(bin, link); err != nil {
		return fmt.Errorf("create link: %w", err)
	}
	fmt.Fprintf(stdout, "\nLinked %s.\n", link)
	if !onPath(filepath.Dir(link)) {
		fmt.Fprintf(stdout, "%s is not on your PATH. Add it yourself, or run Brain with: %s\n", filepath.Dir(link), bin)
	}
	return nil
}

func onPath(dir string) bool {
	for _, d := range filepath.SplitList(os.Getenv("PATH")) {
		if filepath.Clean(d) == dir {
			return true
		}
	}
	return false
}
