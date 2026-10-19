package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"

	"github.com/aryanVijaywargia/herdr-brain/internal/adapter"
	"github.com/aryanVijaywargia/herdr-brain/internal/install"
	"github.com/aryanVijaywargia/herdr-brain/internal/paths"
)

const uninstallUsage = `usage: brain uninstall [--apply] [--data]`

// Uninstall runs `brain uninstall` with args after "uninstall" (E8-S5). It is a dry run
// by default: it lists what would change. With --apply it removes each Brain hook entry whose
// hash is unchanged, then the install record, the ~/.local/bin/brain link when it points to
// this binary, and the bindings file. --data also removes the data home with the stores, and
// the plugin's herdr config and state folders. Uninstall needs no binding: every path comes
// from HOME, so a test session and a real session find the same install record.
func Uninstall(_ context.Context, e Env, args []string) error {
	flags := flag.NewFlagSet("uninstall", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	apply := flags.Bool("apply", false, "")
	data := flags.Bool("data", false, "")
	if err := flags.Parse(args); err != nil {
		return fmt.Errorf("%w\n%s", err, uninstallUsage)
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected argument %q\n%s", flags.Arg(0), uninstallUsage)
	}
	home, err := paths.Home()
	if err != nil {
		return err
	}
	record, err := paths.InstallFile()
	if err != nil {
		return err
	}
	u := &uninstaller{out: e.Stdout, apply: *apply}
	edited, err := u.entries(&install.Installer{Home: home, Record: record}, e.Profiles)
	if err != nil {
		return err
	}
	if err := u.record(record, edited); err != nil {
		return err
	}
	link, err := ownLink()
	if err != nil {
		return err
	}
	remove := []string{link, e.Bindings}
	dataHome, err := paths.DataHome()
	if err != nil {
		return err
	}
	if *data {
		config, state, err := paths.PluginDirs()
		if err != nil {
			return err
		}
		remove = append(remove, dataHome, config, state)
	}
	for _, path := range remove {
		if err := u.remove(path); err != nil {
			return err
		}
	}
	return u.summary(dataHome, *data)
}

// uninstaller prints each change of one `brain uninstall`, and makes it only with apply.
type uninstaller struct {
	out   io.Writer
	apply bool
	lines int // the changes printed so far
}

// entries removes the Brain hook entry of each profile in the install record whose hash is
// unchanged; profiles holds the profiles (nil is the embedded set). An entry that you edited
// stays, and Brain reports it; edited says whether one did.
func (u *uninstaller) entries(in *install.Installer, profiles fs.FS) (edited bool, err error) {
	rec, err := install.Read(in.Record)
	if err != nil {
		return false, err
	}
	for _, kind := range slices.Sorted(maps.Keys(rec.Entries)) {
		p, err := adapter.LoadFS(profiles, kind)
		if err != nil {
			return false, err
		}
		do := in.WouldUninstall
		if u.apply {
			do = in.Uninstall
		}
		st, err := do(p)
		if err != nil {
			return false, err
		}
		target := rec.Entries[kind].Target
		switch st {
		case install.Removed:
			err = u.printf("%s the Brain hook entry for %s in %s\n", u.verb(), kind, target)
		case install.Edited:
			edited = true
			err = u.printf("keep the Brain hook entry for %s in %s: you edited it, so remove it by hand\n", kind, target)
		case install.NotFound:
			err = u.printf("no Brain hook entry for %s in %s any more\n", kind, target)
		}
		if err != nil {
			return false, err
		}
	}
	return edited, nil
}

// record removes the install record when it holds no entry any more. It keeps the record of an
// edited entry, and one that an install wrote since the entries went (install.RemoveRecord).
func (u *uninstaller) record(path string, edited bool) error {
	if _, err := os.Lstat(path); errors.Is(err, fs.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	removed := !edited
	if u.apply {
		var err error
		if removed, err = install.RemoveRecord(path); err != nil {
			return err
		}
	}
	if !removed {
		return u.printf("keep %s: it still records a Brain hook entry\n", path)
	}
	return u.printf("%s %s\n", u.verb(), path)
}

// remove removes the file or folder at path when it exists. An empty path is nothing.
func (u *uninstaller) remove(path string) error {
	if path == "" {
		return nil
	}
	if _, err := os.Lstat(path); errors.Is(err, fs.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	if u.apply {
		if err := os.RemoveAll(path); err != nil {
			return err
		}
	}
	return u.printf("%s %s\n", u.verb(), path)
}

func (u *uninstaller) summary(dataHome string, data bool) error {
	if u.lines == 0 {
		return u.printf("Brain has nothing to remove.\n")
	}
	if !data {
		if err := u.printf("The stores stay in %s. Add --data to remove them.\n", dataHome); err != nil {
			return err
		}
	}
	if !u.apply {
		cmd := "brain uninstall --apply"
		if data {
			cmd += " --data"
		}
		return u.printf("Dry run: nothing changed. Run %s to do it.\n", cmd)
	}
	return u.printf("Next: herdr plugin uninstall brain\n")
}

func (u *uninstaller) verb() string {
	if u.apply {
		return "removed"
	}
	return "would remove"
}

func (u *uninstaller) printf(format string, a ...any) error {
	u.lines++
	_, err := fmt.Fprintf(u.out, format, a...)
	return err
}

// ownLink returns ~/.local/bin/brain when it points to this binary (E1-S2), and "" when it
// points elsewhere, is broken or does not exist.
func ownLink() (string, error) {
	link, err := paths.LinkPath()
	if err != nil {
		return "", err
	}
	bin, err := paths.BinaryPath()
	if err != nil {
		return "", err
	}
	target, err := filepath.EvalSymlinks(link)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "", nil
	case err != nil:
		return "", fmt.Errorf("check link %s: %w", link, err)
	case target != bin:
		return "", nil
	}
	return link, nil
}
