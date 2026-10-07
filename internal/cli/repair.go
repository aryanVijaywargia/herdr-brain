package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"path/filepath"

	"github.com/aryanVijaywargia/herdr-brain/internal/output"
	"github.com/aryanVijaywargia/herdr-brain/internal/tool"
)

const repairUsage = `usage: brain repair [--confirm]`

// Repair runs `brain repair` with args after "repair" (E6-S4). It runs the output repair
// (§7.5 step 6) and lists the open output conflicts under the same lock, and keeps their
// folders. With --confirm it deletes each conflict folder and builds again (§7.5 step 5, D5).
func Repair(ctx context.Context, e Env, args []string) (err error) {
	fs := flag.NewFlagSet("repair", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	confirm := fs.Bool("confirm", false, "")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("%w\n%s", err, repairUsage)
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("want no arguments, got %q\n%s", fs.Args(), repairUsage)
	}
	c, err := e.open(ctx, openNoRepair)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, c.Close()) }()
	out := filepath.Join(c.Binding.Store, outDir)
	if *confirm {
		cs, err := output.Confirm(ctx, c.Store, out, e.Now)
		if err != nil {
			return err
		}
		if err := printConflicts(e.Stdout, cs, "deleted: %v\n", "the output is built again\n"); err != nil {
			return err
		}
		return printSkillFiles(e.Stdout, cs)
	}
	cs, err := output.RepairConflicts(ctx, c.Store, out, e.Now)
	if err != nil {
		return err
	}
	return printConflicts(e.Stdout, cs, "conflict: %v\n",
		"brain keeps these folders; brain repair --confirm deletes them and builds again\n")
}

// printConflicts prints each conflict of cs with format, and then end. With no conflict it
// says so.
func printConflicts(w io.Writer, cs []output.ConflictError, format, end string) error {
	if len(cs) == 0 {
		_, err := fmt.Fprintln(w, "no output conflict; the output is repaired")
		return err
	}
	for _, c := range cs {
		if _, err := fmt.Fprintf(w, format, &c); err != nil {
			return err
		}
	}
	_, err := fmt.Fprint(w, end)
	return err
}

// printSkillFiles names each skill or tool file of cs that you confirmed (§7.5 step 5). Repair
// wrote it again when the generation still lists its revision; else cleanup removes it with
// its item.
func printSkillFiles(w io.Writer, cs []output.ConflictError) error {
	for _, c := range cs {
		for _, f := range c.Files {
			// A skill or tool file has an absolute path; a file of a generation folder has a
			// relative one.
			if !filepath.IsAbs(f) {
				continue
			}
			kind := "skill"
			if b := filepath.Base(f); b == tool.TomlName || b == tool.ScriptName {
				kind = "tool"
			}
			if _, err := fmt.Fprintf(w, "%s file %s: written again, or removed when the %s is no longer active\n", kind, f, kind); err != nil {
				return err
			}
		}
	}
	return nil
}
