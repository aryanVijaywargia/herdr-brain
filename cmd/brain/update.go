package main

import (
	"bufio"
	"fmt"
	"io"
	"time"

	"github.com/aryanVijaywargia/herdr-brain/internal/adapter"
	"github.com/aryanVijaywargia/herdr-brain/internal/install"
	"github.com/aryanVijaywargia/herdr-brain/internal/paths"
)

// updateRecords is the hook record step of brain update (#315). For each installed CLI whose
// Brain start entry is in place, it plans the hook record entries that the profile turns on and
// that are missing, as setup does, and asks once: yes adds them, and any other answer writes
// nothing. A CLI with no start entry needs brain setup, which adds both, so the step skips it.
func updateRecords(d setupDeps, in *bufio.Reader, stdout io.Writer) error {
	bin, err := paths.BinaryPath()
	if err != nil {
		return fmt.Errorf("binary path: %w", err)
	}
	inst, err := installer(bin)
	if err != nil {
		return err
	}
	ps, err := adapter.LoadAll(d.profiles)
	if err != nil {
		return err
	}
	var p plan
	for _, pr := range ps {
		if _, err := d.lookPath(pr.Kind); err != nil {
			continue
		}
		_, st, err := inst.WouldInstall(pr)
		if err != nil {
			return err
		}
		if st != install.Present {
			continue
		}
		records, err := planRecords(inst, pr, &p, updateRecordsHead)
		if err != nil {
			return err
		}
		if len(records) > 0 {
			p.add(func() error { return applyRecords(inst, pr, records, d.now(), stdout) })
		}
	}
	if len(p.steps) == 0 {
		fmt.Fprintln(stdout, "Hook records: no hook record entry to add.")
		return nil
	}
	fmt.Fprint(stdout, "Hook records: the profiles of this Brain turn on hook records, and these entries are missing.\n"+
		p.text.String())
	_, err = p.apply(in, stdout, "brain update wrote no hook record entry. To add them later, run brain update again.\n")
	return err
}

// applyRecords adds the hook record entries records of p when the Brain start entry of p is
// still in place: you can change the hook file while update waits for the answer.
func applyRecords(inst *install.Installer, p *adapter.Profile, records []adapter.Hook, now time.Time, stdout io.Writer) error {
	start, st, err := inst.WouldInstall(p)
	if err != nil {
		return err
	}
	if st != install.Present {
		fmt.Fprintf(stdout, "%s no longer holds the Brain hook entry for %s, so brain update adds no hook record entry for it. "+
			"Run brain setup.\n", start.Target, p.Kind)
		return nil
	}
	return applyHooks(inst, p, start.Target, false, records, now, stdout)
}
