package main

import (
	"bufio"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/aryanVijaywargia/herdr-brain/internal/adapter"
	"github.com/aryanVijaywargia/herdr-brain/internal/install"
	"github.com/aryanVijaywargia/herdr-brain/internal/launch"
)

// globalHooks are the global Brain hook entries of one CLI with launch data (E24-S8): an
// install of an older Brain added them, and `brain <kind>` now gives Brain to one session.
type globalHooks struct {
	p *adapter.Profile
	// targets are the hook files of the entries in the install record: the files that the
	// removal changes, also when the CLI now reads another file.
	targets []string
	edited  bool // a target holds a Brain entry of the CLI that you edited
}

// oldGlobalHooks returns the global Brain hook entries of each CLI of ps with launch data that
// the install record holds and that are still in their hook file, changed or not.
func oldGlobalHooks(inst *install.Installer, ps []*adapter.Profile) ([]globalHooks, error) {
	rec, err := install.Read(inst.Record)
	if err != nil {
		return nil, err
	}
	var out []globalHooks
	for _, p := range ps {
		if !slices.Contains(launch.Kinds(), p.Kind) {
			continue
		}
		st, err := inst.WouldUninstall(p)
		if err != nil {
			return nil, err
		}
		if st != install.Removed && st != install.Edited {
			continue
		}
		var targets []string
		for _, h := range rec.Hooks(p) {
			if e, _ := rec.Entry(p.Kind, h); !slices.Contains(targets, e.Target) {
				targets = append(targets, e.Target)
			}
		}
		out = append(out, globalHooks{p: p, targets: targets, edited: st == install.Edited})
	}
	return out, nil
}

// askRemove shows the global Brain hooks of h and asks whether Brain may remove them. Only "y"
// or "yes" is yes, so a run with no answer, such as one with no terminal, removes nothing.
func askRemove(h globalHooks, in *bufio.Reader, stdout io.Writer) (bool, error) {
	kind := h.p.Kind
	for _, t := range h.targets {
		fmt.Fprintf(stdout, "- %s holds the global Brain hooks for %s. They run in each %s session, also outside herdr.\n",
			t, kind, kind)
	}
	fmt.Fprintf(stdout, "- brain %s, or the herdr action \"Brain: new %s pane\", gives Brain to one session with no global hook.\n",
		kind, title(kind))
	if h.edited {
		fmt.Fprintf(stdout, "- %s holds a Brain hook entry for %s that you edited. Brain keeps it.\n", strings.Join(h.targets, ", "), kind)
	}
	return ask(in, stdout, fmt.Sprintf("Remove the global Brain hooks for %s? A plain %s session then gets pane-screen "+
		"capture only. [y/N] ", kind, kind))
}

// removeGlobalHooks removes each unchanged Brain entry of h through the install record, one
// entry at a time: UninstallHook checks the hash of the entry again under the install lock.
// An entry that you edited stays, and Brain says so. An entry that Brain cannot find, as after
// an edit of its fixed arguments, stays in the record, and Brain says so too.
func removeGlobalHooks(inst *install.Installer, h globalHooks, stdout io.Writer) error {
	kind := h.p.Kind
	rec, err := install.Read(inst.Record)
	if err != nil {
		return err
	}
	var removed []string
	for _, hk := range rec.Hooks(h.p) {
		e, _ := rec.Entry(kind, hk)
		st, err := inst.WouldUninstallHook(h.p, hk)
		if err != nil {
			return err
		}
		if st == install.NotFound {
			fmt.Fprintf(stdout, "Brain could not find its %s entry for %s in %s. If you edited it, remove it by hand. "+
				"Brain keeps it in its install record.\n", hk.CLIEvent, kind, e.Target)
			continue
		}
		if st, err = inst.UninstallHook(h.p, hk); err != nil {
			return err
		}
		switch st {
		case install.Removed:
			if !slices.Contains(removed, e.Target) {
				removed = append(removed, e.Target)
			}
		case install.Edited:
			fmt.Fprintf(stdout, "%s holds the Brain %s entry for %s that you edited. Brain keeps it: remove it by hand.\n",
				e.Target, hk.CLIEvent, kind)
		case install.NotFound:
			// The entry went while Brain removed the others.
			fmt.Fprintf(stdout, "Brain could not find its %s entry for %s in %s. If you edited it, remove it by hand.\n",
				hk.CLIEvent, kind, e.Target)
		}
	}
	if len(removed) > 0 {
		fmt.Fprintf(stdout, "Removed the global Brain hooks for %s from %s.\n", kind, strings.Join(removed, ", "))
	}
	return nil
}

// askGlobalHooks asks, for each CLI of ps with launch data whose global Brain hooks are in
// place, whether Brain may remove them, as the next step of st. It adds the answer to p: a yes
// adds the removal, which runs only after the yes to the whole plan; a no changes nothing, and
// the next setup asks again.
func askGlobalHooks(inst *install.Installer, ps []*adapter.Profile, st *stepper, p *plan, in *bufio.Reader, stdout io.Writer) error {
	old, err := oldGlobalHooks(inst, ps)
	if err != nil || len(old) == 0 {
		return err
	}
	st.next("global hooks")
	for _, h := range old {
		yes, err := askRemove(h, in, stdout)
		if err != nil {
			return err
		}
		if !yes {
			p.say("- Global hooks: keep the Brain hooks for %s in %s. The next brain setup asks again.\n", h.p.Kind,
				strings.Join(h.targets, ", "))
			continue
		}
		p.say("- Global hooks: remove the unchanged Brain hook entries for %s from %s.\n", h.p.Kind,
			strings.Join(h.targets, ", "))
		p.add(func() error { return removeGlobalHooks(inst, h, stdout) })
	}
	return nil
}

// updateGlobalHooks is the global hook step of brain update (E24-S8): for each CLI of ps with
// launch data whose global Brain hooks are in place, it asks whether Brain may remove them, and
// removes them on yes. A no changes nothing.
func updateGlobalHooks(inst *install.Installer, ps []*adapter.Profile, in *bufio.Reader, stdout io.Writer) error {
	old, err := oldGlobalHooks(inst, ps)
	if err != nil {
		return err
	}
	for _, h := range old {
		yes, err := askRemove(h, in, stdout)
		if err != nil {
			return err
		}
		if !yes {
			fmt.Fprintf(stdout, "brain update keeps the global Brain hooks for %s. To remove them later, run brain update again.\n", h.p.Kind)
			continue
		}
		if err := removeGlobalHooks(inst, h, stdout); err != nil {
			return err
		}
	}
	return nil
}

// title is kind with an upper case first letter, as in the herdr action names: Claude, Codex.
func title(kind string) string {
	return strings.ToUpper(kind[:1]) + kind[1:]
}
