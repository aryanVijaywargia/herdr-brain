package main

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/aryanVijaywargia/herdr-brain/internal/bindings"
	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

// skillStage is the skill choice stage (HLD §12.1 "Skill choice", E16-S5). Its cases come from
// the [[skill]] and [[negative]] entries of a probe file, not from [[probe]].
const skillStage = "skill-choice"

// skillSetup is the setup step of a skill choice case: a Brain call in the shell pane maps the
// case's workspace and builds the output with the seeded skills, as every brain call does.
// [NEW] the step.
const skillSetup = "brain status > /dev/null"

// FixtureSkill is one fixture skill of a probe file. Its steps hold {{marker}}, which gets a
// fresh marker of its own in each case.
type FixtureSkill struct {
	Name        string
	Description string
	Example     string // the one example prompt
	Steps       string
}

// text is the skill text of s, in the E16-S3 form, with marker in its steps. [NEW]
// "required-tools: none": a fixture skill needs no tool.
func (s FixtureSkill) text(marker string) string {
	return "---\nname: " + s.Name + "\ndescription: " + s.Description + "\nrequired-tools: none\nexample: " + s.Example +
		"\n---\n\n" + strings.ReplaceAll(strings.TrimSpace(s.Steps), "{{marker}}", marker) + "\n"
}

// skillMarker is the key of the marker of skill name in the markers of a case.
func skillMarker(name string) string { return "skill:" + name }

// seedSkills adds each fixture skill of c to the empty test store of b as an active global
// skill, with its marker in its steps. No brain command adds a skill yet (the drafts come with
// E16-S4), so the runner writes them itself; the setup step then builds the output. A control
// gets no skill: its brief has no skill list (§12.1). [NEW] the global scope: the skill list is
// then in the brief also before the workspace map has the case's workspace.
func (r *Runner) seedSkills(ctx context.Context, b bindings.Binding, c Case, markers map[string]string) error {
	if c.Control || len(c.Skills) == 0 {
		return nil
	}
	s, err := store.Open(ctx, b.Store, r.Timeouts.Call)
	if err != nil {
		return err
	}
	err = s.Write(ctx, func(tx *sql.Tx) error {
		now := r.Now()
		for i, sk := range c.Skills {
			rev := store.ItemRevision{Text: sk.text(markers[skillMarker(sk.Name)]), Scope: store.ScopeGlobal,
				State: store.StateActive, Author: store.AuthorYou, Origin: store.OriginReview, Created: now}
			id, _, err := store.CreateItem(ctx, tx, store.Item{ClaimID: fmt.Sprintf("cl_eval%d", i+1), Kind: store.KindSkill, Created: now}, rev)
			if err != nil {
				return err
			}
			if err := store.CheckSkill(ctx, tx, id); err != nil {
				return err
			}
		}
		return nil
	})
	if cerr := s.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("seed fixture skills: %w", err)
	}
	return nil
}

// skillResults makes the results of one reply of skill choice case c: one for each fixture
// skill. Its case id is the prompt's case id and the skill name.
func skillResults(c Case, o Outcome, rep Reply, clis map[string]probeCLI) []store.EvalResult {
	rs := make([]store.EvalResult, 0, len(c.Skills))
	for _, sk := range c.Skills {
		res := base(c, rep.Harness, o, rep, clis)
		res.Case = c.ID + "/" + sk.Name
		lb, rule := skillLabel(c, sk.Name, o.Markers[skillMarker(sk.Name)], rep)
		res.Label, res.Rule = string(lb), rule
		rs = append(rs, res)
	}
	return rs
}

// skillLabel labels reply rep for skill name, whose marker is marker (§12.1 skill choice). The
// example prompt of the skill must use the marker: only the skill file holds it, so the
// marker shows that the agent read the file and followed its steps. Every other prompt, and
// every control, must not use it. A skipped reply gets no label, a reply that the runner could
// not see whole is UNSURE, and a reply without its brief gets no label, except in a control.
func skillLabel(c Case, name, marker string, rep Reply) (Label, string) {
	switch {
	case rep.Status == Skipped:
		return "", ""
	case rep.Status == Incomplete:
		return Unsure, "reply incomplete"
	case rep.Status == NotDelivered && !c.Control:
		return "", ""
	}
	found, negated := find(normalize(rep.Text), []string{marker})
	selected := !c.Control && c.SkillFor == name
	switch {
	case negated:
		return Unsure, fmt.Sprintf("the marker of skill %s in a sentence with a negation", name)
	case selected && found:
		return Pass, fmt.Sprintf("the example of skill %s: the reply uses its marker", name)
	case selected:
		return Miss, fmt.Sprintf("the example of skill %s: the reply does not use its marker", name)
	case found && c.Control:
		return Invented, fmt.Sprintf("the reply uses the marker of skill %s, but the control lists no skill", name)
	case found:
		return Invented, fmt.Sprintf("the reply uses the marker of skill %s, which this prompt must not select", name)
	}
	return Pass, fmt.Sprintf("no marker of skill %s", name)
}

// skillVerdicts returns the verdict of the skill choice stage of each CLI with a result of the
// stage, from vs, the verdicts of rs. [NEW] the pass rule: every result of the stage passes,
// its controls too. A skipped result is never a pass, so it fails the stage; an UNSURE result
// keeps the stage unsure until you resolve it.
func skillVerdicts(rs []store.EvalResult, vs []string) map[string]string {
	out := map[string]string{}
	for i, r := range rs {
		if r.Stage != skillStage {
			continue
		}
		switch v := vs[i]; {
		case v == verdictFail || v == verdictSkipped:
			out[r.Harness] = verdictFail
		case v == verdictUnsure && out[r.Harness] != verdictFail:
			out[r.Harness] = verdictUnsure
		case out[r.Harness] == "":
			out[r.Harness] = verdictPass
		}
	}
	return out
}
