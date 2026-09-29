package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/BurntSushi/toml"
)

// probeFile is one file evals/probes/*.toml (HLD §12.1 "Probe format").
type probeFile struct {
	Probe []probeEntry `toml:"probe"`
}

// probeEntry is one [[probe]] of a probe file. Setup holds shell steps, as the runner runs
// them in the case's shell pane; "{{marker}}" and "{{marker2}}" and so on get a fresh marker in
// each case.
type probeEntry struct {
	ID             string   `toml:"id"`
	Stage          string   `toml:"stage"`
	Source         string   `toml:"source"`
	Setup          []string `toml:"setup"`
	ExpectRevision int64    `toml:"expect_revision"`
	Ask            string   `toml:"ask"`
	Expect         []string `toml:"expect"`
	Stale          []string `toml:"stale"`
	Decline        bool     `toml:"decline"`
	Harnesses      []string `toml:"harnesses"`
}

// stages are the eval stages of §12.1.
var stages = []string{"delivery", "exclusion", "transfer", "update", "restraint", "forget", "usefulness"}

// harnesses are the CLIs that a probe can ask.
var harnesses = []string{"claude", "codex"}

// probeID is the form of a probe id: it goes into a herdr workspace label. [NEW] form.
var probeID = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// slot is a marker placeholder: {{marker}}, {{marker2}} and so on.
var slot = regexp.MustCompile(`\{\{([^{}]*)\}\}`)

// markerName is the name inside a marker placeholder.
var markerName = regexp.MustCompile(`^marker[0-9]*$`)

// loadProbes reads the probe files of path, a probe file or a folder of *.toml files in name
// order, and returns the cases: each probe and then its control, the same case with no setup
// steps (§12.1 "Control"). An error names the file and the probe.
func loadProbes(path string) ([]Case, error) {
	files := []string{path}
	if fi, err := os.Stat(path); err != nil {
		return nil, err
	} else if fi.IsDir() {
		if files, err = filepath.Glob(filepath.Join(path, "*.toml")); err != nil {
			return nil, err
		}
		if len(files) == 0 {
			return nil, fmt.Errorf("%s: no probe file (*.toml)", path)
		}
	}
	var cases []Case
	seen := map[string]string{} // probe id to its file
	for _, f := range files {
		var pf probeFile
		md, err := toml.DecodeFile(f, &pf)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", f, err)
		}
		if un := md.Undecoded(); len(un) > 0 {
			return nil, fmt.Errorf("%s: unknown key %q", f, un[0].String())
		}
		if len(pf.Probe) == 0 {
			return nil, fmt.Errorf("%s: no [[probe]]", f)
		}
		for i, p := range pf.Probe {
			if err := p.check(); err != nil {
				return nil, fmt.Errorf("%s: probe %d (%q): %w", f, i+1, p.ID, err)
			}
			if other, ok := seen[p.ID]; ok {
				return nil, fmt.Errorf("%s: probe %d: id %q is also in %s", f, i+1, p.ID, other)
			}
			seen[p.ID] = f
			c := p.toCase()
			ctl := c
			ctl.Control, ctl.Setup = true, nil
			cases = append(cases, c, ctl)
		}
	}
	return cases, nil
}

// check checks the fields of one probe.
func (p probeEntry) check() error {
	var errs []error
	if !probeID.MatchString(p.ID) {
		errs = append(errs, errors.New("id: want lower-case letters, digits and dashes"))
	}
	if !slices.Contains(stages, p.Stage) {
		errs = append(errs, fmt.Errorf("stage %q: want one of %s", p.Stage, strings.Join(stages, ", ")))
	}
	if strings.TrimSpace(p.Ask) == "" {
		errs = append(errs, errors.New("ask is empty"))
	}
	if len(p.Expect) == 0 && !p.Decline {
		errs = append(errs, errors.New("expect is empty; only a decline probe (decline = true) has no expected answer"))
	}
	if p.ExpectRevision < 0 {
		errs = append(errs, fmt.Errorf("expect_revision %d: want 1 or more", p.ExpectRevision))
	}
	if len(p.Harnesses) == 0 {
		errs = append(errs, errors.New("harnesses is empty"))
	}
	for i, h := range p.Harnesses {
		switch {
		case !slices.Contains(harnesses, h):
			errs = append(errs, fmt.Errorf("harness %q: want claude or codex", h))
		case slices.Contains(p.Harnesses[:i], h):
			errs = append(errs, fmt.Errorf("harness %q is listed twice", h))
		}
	}
	if slices.ContainsFunc(slices.Concat(p.Setup, p.Expect, p.Stale), func(s string) bool { return strings.TrimSpace(s) == "" }) {
		errs = append(errs, errors.New("an empty entry in setup, expect or stale"))
	}
	for _, s := range slices.Concat(p.Setup, []string{p.Ask}, p.Expect, p.Stale) {
		for _, m := range slot.FindAllStringSubmatch(s, -1) {
			if !markerName.MatchString(m[1]) {
				errs = append(errs, fmt.Errorf("placeholder %s: want {{marker}} or {{marker<n>}}", m[0]))
			}
		}
	}
	return errors.Join(errs...)
}

func (p probeEntry) toCase() Case {
	c := Case{
		ID: p.ID, Stage: p.Stage, Source: p.Source, Setup: p.Setup, ExpectRevision: p.ExpectRevision,
		Check: Check{Expect: p.Expect, Stale: p.Stale, Decline: p.Decline},
	}
	for _, h := range p.Harnesses {
		c.Probes = append(c.Probes, Probe{Harness: h, Prompt: p.Ask})
	}
	return c
}
