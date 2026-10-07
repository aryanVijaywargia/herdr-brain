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

	"github.com/aryanVijaywargia/herdr-brain/internal/adapter"
	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

// probeFile is one file evals/probes/*.toml (HLD §12.1 "Probe format"). Its [[skill]] and
// [[negative]] entries make the skill choice cases (E16-S5), and its [[bug]] entries the
// regression cases (E20-S1).
type probeFile struct {
	Probe    []probeEntry    `toml:"probe"`
	Skill    []skillEntry    `toml:"skill"`
	Negative []negativeEntry `toml:"negative"`
	Bug      []bugEntry      `toml:"bug"`
}

// skillEntry is one [[skill]] of a probe file: a fixture skill, whose example prompt is a
// skill choice case of each of its harnesses. Its steps hold {{marker}}.
type skillEntry struct {
	Name        string   `toml:"name"`
	Description string   `toml:"description"`
	Example     string   `toml:"example"`
	Steps       string   `toml:"steps"`
	Harnesses   []string `toml:"harnesses"`
}

// negativeEntry is one [[negative]] of a probe file: a prompt that must select no fixture
// skill of the file.
type negativeEntry struct {
	ID        string   `toml:"id"`
	Ask       string   `toml:"ask"`
	Harnesses []string `toml:"harnesses"`
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
	// Hook makes an exclusion probe a direct `brain hook` call with test input in one of
	// hookModes, in place of a CLI and its ask (E10-S3).
	Hook string `toml:"hook"`
	// FullBrief makes a delivery probe check that its brief is full size, with {{marker}} in
	// its middle: the Codex inline check (E8-S2).
	FullBrief bool `toml:"full_brief"`
	// Forgotten are the answers that a forget probe removed: the store must not hold them.
	Forgotten []string `toml:"forgotten"`
}

// stages are the eval stages of §12.1.
var stages = []string{"delivery", "exclusion", "transfer", "update", "restraint", "forget", "usefulness", skillStage, regressionStage}

// hookModes are the herdr states in which an exclusion probe calls the hook (§12.1
// exclusion): no herdr, a closed pane, no herdr answer in time, a socket with no binding, and
// a workspace with no map entry, which gets only the global part.
var hookModes = []string{"no-herdr-env", "closed-pane", "no-herdr-answer", "unbound-session", "other-workspace"}

// probeID is the form of a probe id: it goes into a herdr workspace label. [NEW] form.
var probeID = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// slot is a marker placeholder: {{marker}}, {{marker2}} and so on.
var slot = regexp.MustCompile(`\{\{([^{}]*)\}\}`)

// markerName is the name inside a marker placeholder.
var markerName = regexp.MustCompile(`^marker[0-9]*$`)

// loadProbes reads the probe files of path, a probe file or a folder of *.toml files in name
// order, and returns the cases: each probe and then its control, the same case with no setup
// steps (§12.1 "Control"), and each regression case, which has no control. An error names the
// file and the probe.
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
	kinds := adapter.Kinds(nil)
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
		if len(pf.Probe) == 0 && len(pf.Skill) == 0 && len(pf.Bug) == 0 {
			return nil, fmt.Errorf("%s: no [[probe]], [[skill]] or [[bug]]", f)
		}
		var fc []Case
		for i, p := range pf.Probe {
			if err := p.check(kinds); err != nil {
				return nil, fmt.Errorf("%s: probe %d (%q): %w", f, i+1, p.ID, err)
			}
			fc = append(fc, p.toCase())
		}
		sc, err := skillCases(pf, kinds)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", f, err)
		}
		bc, err := bugCases(pf.Bug)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", f, err)
		}
		for _, c := range slices.Concat(fc, sc, bc) {
			if other, ok := seen[c.ID]; ok {
				return nil, fmt.Errorf("%s: id %q is also in %s", f, c.ID, other)
			}
			seen[c.ID] = f
			cases = append(cases, c)
			if c.Stage != regressionStage {
				ctl := c
				ctl.Control, ctl.Setup = true, nil
				cases = append(cases, ctl)
			}
		}
	}
	return cases, nil
}

// skillCases checks the [[skill]] and [[negative]] entries of pf and returns their skill
// choice cases: the example of each skill, then each negative prompt, each with all fixture
// skills of the file. The case of a skill's example has the id "skill-<name>".
func skillCases(pf probeFile, kinds []string) ([]Case, error) {
	if len(pf.Negative) > 0 && len(pf.Skill) == 0 {
		return nil, errors.New("[[negative]] needs a [[skill]] in the same file")
	}
	skills := make([]FixtureSkill, len(pf.Skill))
	for i, e := range pf.Skill {
		skills[i] = FixtureSkill{Name: e.Name, Description: e.Description, Example: e.Example, Steps: e.Steps}
		if err := e.check(kinds, pf.Skill[:i]); err != nil {
			return nil, fmt.Errorf("skill %d (%q): %w", i+1, e.Name, err)
		}
	}
	var cases []Case
	add := func(id, ask, skillFor string, harnesses []string) {
		c := Case{ID: id, Stage: skillStage, Source: "fixture skills", Setup: []string{skillSetup}, Skills: skills, SkillFor: skillFor}
		for _, h := range harnesses {
			c.Probes = append(c.Probes, Probe{Harness: h, Prompt: ask})
		}
		cases = append(cases, c)
	}
	for _, e := range pf.Skill {
		add("skill-"+e.Name, e.Example, e.Name, e.Harnesses)
	}
	for i, e := range pf.Negative {
		errs := []error{checkHarnesses(e.Harnesses, kinds), checkPlain("ask", e.Ask)}
		if !probeID.MatchString(e.ID) {
			errs = append(errs, errors.New("id: want lower-case letters, digits and dashes"))
		}
		if err := errors.Join(errs...); err != nil {
			return nil, fmt.Errorf("negative %d (%q): %w", i+1, e.ID, err)
		}
		add(e.ID, e.Ask, "", e.Harnesses)
	}
	return cases, nil
}

// check checks one fixture skill; before are the skills before it in its file. Its text, with
// a marker in its steps, must be a skill text that Brain accepts (E16-S3).
func (e skillEntry) check(kinds []string, before []skillEntry) error {
	errs := []error{checkHarnesses(e.Harnesses, kinds), checkPlain("description", e.Description), checkPlain("example", e.Example)}
	if slices.ContainsFunc(before, func(o skillEntry) bool { return o.Name == e.Name }) {
		errs = append(errs, errors.New("name: another skill of this file has it"))
	}
	if !strings.Contains(e.Steps, "{{marker}}") {
		errs = append(errs, errors.New("steps: want {{marker}}, so that a reply shows that the agent read the skill"))
	}
	for _, m := range slot.FindAllString(e.Steps, -1) {
		if m != "{{marker}}" {
			errs = append(errs, fmt.Errorf("placeholder %s: a skill's steps hold {{marker}} only", m))
		}
	}
	sk := FixtureSkill{Name: e.Name, Description: e.Description, Example: e.Example, Steps: e.Steps}
	if _, err := store.ParseSkill(sk.text("kest-1234")); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// checkPlain checks a one-line text field with no placeholder: a marker in a prompt or in the
// skill list would give the answer away.
func checkPlain(field, s string) error {
	switch {
	case strings.TrimSpace(s) == "":
		return fmt.Errorf("%s is empty", field)
	case strings.Contains(s, "\n"):
		return fmt.Errorf("%s: want one line", field)
	case slot.MatchString(s):
		return fmt.Errorf("%s: want no placeholder", field)
	}
	return nil
}

// check checks the fields of one probe; kinds are the profile kinds that a probe can ask.
func (p probeEntry) check(kinds []string) error {
	var errs []error
	if !probeID.MatchString(p.ID) {
		errs = append(errs, errors.New("id: want lower-case letters, digits and dashes"))
	}
	switch {
	case p.Stage == skillStage:
		errs = append(errs, errors.New("stage skill-choice: give its cases as [[skill]] and [[negative]]"))
	case p.Stage == regressionStage:
		errs = append(errs, errors.New("stage regression: give its cases as [[bug]]"))
	case !slices.Contains(stages, p.Stage):
		errs = append(errs, fmt.Errorf("stage %q: want one of %s", p.Stage, strings.Join(stages, ", ")))
	}
	switch {
	case p.Hook == "" && strings.TrimSpace(p.Ask) == "":
		errs = append(errs, errors.New("ask is empty"))
	case p.Hook != "" && p.Ask != "":
		errs = append(errs, errors.New("a hook probe has no ask: the runner calls the hook, not a CLI"))
	}
	if p.Hook != "" && (p.Stage != "exclusion" || !slices.Contains(hookModes, p.Hook)) {
		errs = append(errs, fmt.Errorf("hook %q: want an exclusion probe with one of %s", p.Hook, strings.Join(hookModes, ", ")))
	}
	if len(p.Expect) == 0 && !p.Decline && p.Hook == "" {
		errs = append(errs, errors.New("expect is empty; only a decline probe (decline = true) or a hook probe has no expected answer"))
	}
	if p.FullBrief && p.Stage != "delivery" {
		errs = append(errs, errors.New("full_brief: want a delivery probe"))
	}
	if len(p.Forgotten) > 0 && p.Stage != "forget" {
		errs = append(errs, errors.New("forgotten: want a forget probe"))
	}
	if p.ExpectRevision < 0 {
		errs = append(errs, fmt.Errorf("expect_revision %d: want 1 or more", p.ExpectRevision))
	}
	errs = append(errs, checkHarnesses(p.Harnesses, kinds))
	if slices.ContainsFunc(slices.Concat(p.Setup, p.Expect, p.Stale, p.Forgotten), func(s string) bool { return strings.TrimSpace(s) == "" }) {
		errs = append(errs, errors.New("an empty entry in setup, expect, stale or forgotten"))
	}
	for _, s := range slices.Concat(p.Setup, []string{p.Ask}, p.Expect, p.Stale, p.Forgotten) {
		for _, m := range slot.FindAllStringSubmatch(s, -1) {
			if !markerName.MatchString(m[1]) {
				errs = append(errs, fmt.Errorf("placeholder %s: want {{marker}} or {{marker<n>}}", m[0]))
			}
		}
	}
	return errors.Join(errs...)
}

// checkHarnesses checks the harnesses of one entry: one or more profile kinds of kinds, each
// once.
func checkHarnesses(harnesses, kinds []string) error {
	if len(harnesses) == 0 {
		return errors.New("harnesses is empty")
	}
	var errs []error
	for i, h := range harnesses {
		switch {
		case !slices.Contains(kinds, h):
			errs = append(errs, fmt.Errorf("harness %q: want %s", h, oneOf(kinds)))
		case slices.Contains(harnesses[:i], h):
			errs = append(errs, fmt.Errorf("harness %q is listed twice", h))
		}
	}
	return errors.Join(errs...)
}

// oneOf lists kinds for an error: "claude or codex".
func oneOf(kinds []string) string {
	if len(kinds) < 2 {
		return strings.Join(kinds, "")
	}
	return strings.Join(kinds[:len(kinds)-1], ", ") + " or " + kinds[len(kinds)-1]
}

func (p probeEntry) toCase() Case {
	c := Case{
		ID: p.ID, Stage: p.Stage, Source: p.Source, Setup: p.Setup, ExpectRevision: p.ExpectRevision,
		Check: Check{Expect: p.Expect, Stale: p.Stale, Decline: p.Decline},
		Hook:  p.Hook, FullBrief: p.FullBrief, Forgotten: p.Forgotten,
	}
	for _, h := range p.Harnesses {
		c.Probes = append(c.Probes, Probe{Harness: h, Prompt: p.Ask})
	}
	return c
}
