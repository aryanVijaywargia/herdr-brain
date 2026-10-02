package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aryanVijaywargia/herdr-brain/internal/brief"
	"github.com/aryanVijaywargia/herdr-brain/internal/output"
	"github.com/aryanVijaywargia/herdr-brain/internal/paths"
	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

// skillFile is a probe file with two fixture skills and one negative prompt.
const skillFile = `
[[skill]]
name = "release-notes"
description = "Write the release notes for a new version tag."
example = "Write the release notes for v1.4.0 in your reply."
steps = """
1. Start the reply with the line ` + "`Skill marker: {{marker}}`" + `.
"""
harnesses = ["claude", "codex"]

[[skill]]
name = "sqlite-migration"
description = "Add a database migration for the SQLite cache of the CLI."
example = "Add a migration that adds a hits column to the SQLite cache table."
steps = """
1. Start the reply with the line ` + "`Skill marker: {{marker}}`" + `.
"""
harnesses = ["claude", "codex"]

[[negative]]
id = "skill-negative-sql"
ask = "In two sentences, explain what an index is."
harnesses = ["claude", "codex"]
`

// greedySkill is a fixture skill whose description takes the example prompt of release-notes.
const greedySkill = `
[[skill]]
name = "greedy-notes"
description = "Write the release notes for v1.4.0 in your reply, and any other text."
example = "Write a short project summary."
steps = """
1. Start the reply with the line ` + "`Skill marker: {{marker}}`" + `.
"""
harnesses = ["claude", "codex"]
`

// The example probe file has the skill choice stage for Claude and Codex: each skill's example
// and each negative prompt, all with every fixture skill, and each with its control.
func TestSkillProbes(t *testing.T) {
	cases, err := loadProbes("../../evals/probes")
	if err != nil {
		t.Fatal(err)
	}
	var positive, negative int
	for _, c := range cases {
		if c.Stage != skillStage {
			continue
		}
		if len(c.Skills) < 2 || len(c.Probes) != 2 || c.Probes[0].Harness != "claude" || c.Probes[1].Harness != "codex" {
			t.Errorf("case %s: %d skills, probes %+v; want all fixture skills, claude and codex", c.ID, len(c.Skills), c.Probes)
		}
		if c.Control {
			if c.Setup != nil {
				t.Errorf("control %s has setup %q", c.ID, c.Setup)
			}
			continue
		}
		if !slices.Equal(c.Setup, []string{skillSetup}) {
			t.Errorf("case %s: setup %q, want %q", c.ID, c.Setup, skillSetup)
		}
		if c.SkillFor == "" {
			negative++
			continue
		}
		positive++
		i := slices.IndexFunc(c.Skills, func(s FixtureSkill) bool { return s.Name == c.SkillFor })
		if i < 0 || c.ID != "skill-"+c.SkillFor || c.Probes[0].Prompt != c.Skills[i].Example {
			t.Errorf("case %s: want the example of skill %s", c.ID, c.SkillFor)
		}
	}
	if positive < 2 || negative < 1 {
		t.Errorf("%d skill examples and %d negative prompts, want 2 or more and 1 or more", positive, negative)
	}
}

// The loader names each fault of a [[skill]] or a [[negative]].
func TestLoadSkillErrors(t *testing.T) {
	tests := []struct {
		name, text string
		want       []string
	}{
		{"no marker", strings.Replace(skillFile, "{{marker}}", "kest-1234", 1), []string{`skill 1 ("release-notes")`, "steps: want {{marker}}"}},
		{"other placeholder", strings.Replace(skillFile, "{{marker}}", "{{marker}} {{marker2}}", 1), []string{"placeholder {{marker2}}"}},
		{"bad name", strings.Replace(skillFile, `"release-notes"`, `"Release Notes"`, 1), []string{"name \"Release Notes\""}},
		{"same name", strings.Replace(skillFile, `"sqlite-migration"`, `"release-notes"`, 1), []string{"another skill of this file"}},
		{"no description", strings.Replace(skillFile, `"Write the release notes for a new version tag."`, `""`, 1), []string{"description is empty"}},
		{"two-line example", strings.Replace(skillFile, `"Write the release notes for v1.4.0 in your reply."`, `"a\nb"`, 1), []string{"example: want one line"}},
		{"marker in a prompt", strings.Replace(skillFile, "explain what an index is.", "say {{marker}}.", 1), []string{`negative 1 ("skill-negative-sql")`, "ask: want no placeholder"}},
		{"bad harness", strings.Replace(skillFile, `["claude", "codex"]`, `["gemini"]`, 1), []string{`harness "gemini"`}},
		{"negative alone", "[[negative]]\nid = \"n\"\nask = \"hi\"\nharnesses = [\"claude\"]\n", []string{"no [[probe]] or [[skill]]"}},
		{"negative with a probe only", goodProbe + "[[negative]]\nid = \"n\"\nask = \"hi\"\nharnesses = [\"claude\"]\n", []string{"[[negative]] needs a [[skill]]"}},
		{"skill stage in a probe", strings.Replace(goodProbe, `"update"`, `"skill-choice"`, 1), []string{"give its cases as [[skill]]"}},
		{"duplicate id", skillFile + strings.Replace(goodProbe, "pm-update", "skill-negative-sql", 1), []string{`id "skill-negative-sql" is also in`}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "s.toml")
			write(t, path, tt.text)
			_, err := loadProbes(path)
			if err == nil {
				t.Fatal("loadProbes() = nil, want an error")
			}
			for _, w := range tt.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error %q, want %q", err, w)
				}
			}
		})
	}
}

// skillLabel: the example of a skill must use its marker, every other prompt and every
// control must not; a negation, or a reply that the runner cannot see whole, is UNSURE.
func TestSkillLabel(t *testing.T) {
	const m = "kest-1234"
	pos := Case{Stage: skillStage, SkillFor: "a"}
	neg := Case{Stage: skillStage, SkillFor: "b"}
	ctl := Case{Stage: skillStage, SkillFor: "a", Control: true}
	ok := func(text string) Reply { return Reply{Status: Delivered, Text: text} }
	tests := []struct {
		name string
		c    Case
		rep  Reply
		want Label
	}{
		{"example uses its marker", pos, ok("Skill marker: kest-1234\nDone."), Pass},
		{"example without its marker", pos, ok("Done."), Miss},
		{"example with a longer marker", pos, ok("Skill marker: kest-12345"), Miss},
		{"other prompt uses the marker", neg, ok("Skill marker: kest-1234"), Invented},
		{"other prompt without the marker", neg, ok("Done."), Pass},
		{"control uses the marker", ctl, Reply{Status: NotDelivered, Text: "kest-1234"}, Invented},
		{"control without the marker", ctl, Reply{Status: NotDelivered, Text: "Done."}, Pass},
		{"negation", pos, ok("I did not use kest-1234."), Unsure},
		{"reply incomplete", pos, Reply{Status: Incomplete}, Unsure},
		{"skipped", pos, Reply{Status: Skipped}, ""},
		{"not delivered", pos, Reply{Status: NotDelivered, Text: "kest-1234"}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got, rule := skillLabel(tt.c, "a", m, tt.rep); got != tt.want {
				t.Errorf("skillLabel() = %s (%s), want %s", got, rule, tt.want)
			}
		})
	}
}

// The stage verdict of a CLI: pass when every result passes, fail at any fail or skip, else
// unsure while an UNSURE result waits for you.
func TestSkillVerdicts(t *testing.T) {
	rs := []store.EvalResult{
		{Stage: skillStage, Harness: "claude"}, {Stage: skillStage, Harness: "claude"},
		{Stage: skillStage, Harness: "codex"}, {Stage: skillStage, Harness: "codex"},
		{Stage: skillStage, Harness: "pi"}, {Stage: skillStage, Harness: "pi"},
		{Stage: "delivery", Harness: "agy"},
	}
	vs := []string{verdictUnsure, verdictFail, verdictPass, verdictUnsure, verdictPass, verdictSkipped, verdictFail}
	got := skillVerdicts(rs, vs)
	want := map[string]string{"claude": verdictFail, "codex": verdictUnsure, "pi": verdictFail}
	if len(got) != len(want) {
		t.Errorf("skillVerdicts() = %v, want %v", got, want)
	}
	for h, v := range want {
		if got[h] != v {
			t.Errorf("skillVerdicts()[%s] = %q, want %q", h, got[h], v)
		}
	}
}

// skillMarkerLine is the marker line of a fixture skill's steps.
var skillMarkerLine = regexp.MustCompile("Skill marker: ([a-z]{4}-[0-9]{4})")

// skillEntryLine is one entry of the skill list of the brief.
var skillEntryLine = regexp.MustCompile(`(?m)^- ([a-z0-9-]+): (.*) File: (\S+)$`)

// skillBrain is a fake Brain and a fake CLI for the skill choice stage. A setup step builds the
// output from the test store, as a brain call does; the hook of an agent start writes a
// receipt of that brief. The agent reads the skill whose description shares the most words
// with the prompt, at least 3, and follows its first step. It records each brief that it saw,
// and the count of active skills in the test store then.
type skillBrain struct {
	t      *testing.T
	e      *env
	briefs []string
	skills []int
}

func (sb *skillBrain) install() {
	sb.e.fake.onRun = sb.build
	sb.e.fake.onStart = sb.receipt
	sb.e.fake.answer = sb.answer
}

func (sb *skillBrain) open() *store.Store {
	s, err := store.Open(context.Background(), sb.e.testStore, time.Second)
	if err != nil {
		sb.t.Fatal(err)
	}
	return s
}

// read returns the brief of the test store; "" when it has none.
func (sb *skillBrain) read() string {
	br, err := output.Read(filepath.Join(sb.e.testStore, outDir), "")
	if errors.Is(err, fs.ErrNotExist) {
		return ""
	}
	if err != nil {
		sb.t.Fatal(err)
	}
	return brief.Text(br.Global, br.Workspace)
}

func (sb *skillBrain) build() {
	s := sb.open()
	defer s.Close() //nolint:errcheck // test
	if err := output.Repair(context.Background(), s, filepath.Join(sb.e.testStore, outDir), time.Now); err != nil {
		sb.t.Fatal(err)
	}
}

func (sb *skillBrain) receipt(pane, kind string) {
	text := sb.read()
	if text == "" {
		return // an empty store: the hook writes no receipt
	}
	sum := sha256.Sum256([]byte(text))
	h, sess := hex.EncodeToString(sum[:]), session
	s := sb.open()
	defer s.Close() //nolint:errcheck // test
	err := s.Write(context.Background(), func(tx *sql.Tx) error {
		_, err := store.InsertDelivery(context.Background(), tx, store.Delivery{Profile: kind, HerdrSession: &sess, PaneID: pane,
			BriefHash: &h, Result: store.ResultEmitted, At: time.Now()})
		return err
	})
	if err != nil {
		sb.t.Fatal(err)
	}
}

func (sb *skillBrain) answer(_ *fakePane, prompt string) string {
	text := sb.read()
	sb.briefs = append(sb.briefs, text)
	s := sb.open()
	err := s.Write(context.Background(), func(tx *sql.Tx) error {
		rs, err := store.ActiveSkills(context.Background(), tx)
		sb.skills = append(sb.skills, len(rs))
		return err
	})
	if err := errors.Join(err, s.Close()); err != nil {
		sb.t.Fatal(err)
	}
	file := choose(text, prompt)
	if file == "" {
		return "Done, with no skill."
	}
	data, err := os.ReadFile(file)
	if err != nil {
		sb.t.Fatal(err)
	}
	return skillMarkerLine.FindString(string(data)) + "\nDone."
}

// choose returns the file of the skill of brief whose description shares the most words with
// prompt, at least 3; "" when none does.
func choose(brief, prompt string) string {
	words := strings.Fields(strings.ToLower(prompt))
	best, file := 2, ""
	for _, m := range skillEntryLine.FindAllStringSubmatch(brief, -1) {
		n := 0
		for _, w := range strings.Fields(strings.ToLower(m[2])) {
			if slices.Contains(words, w) {
				n++
			}
		}
		if n > best {
			best, file = n, m[3]
		}
	}
	return file
}

// skillRun loads probe text, runs it with the fake Brain and the fake CLIs, and returns the
// results, the report and the fake Brain. A fake brain on the PATH answers the setup step.
func skillRun(t *testing.T, text string, set func(e *env)) ([]store.EvalResult, string, *skillBrain) {
	t.Helper()
	bin := t.TempDir()
	write(t, filepath.Join(bin, "brain"), "#!/bin/sh\nexit 0\n")
	if err := os.Chmod(filepath.Join(bin, "brain"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	path := filepath.Join(t.TempDir(), "skills.toml")
	write(t, path, text)
	cases, err := loadProbes(path)
	if err != nil {
		t.Fatal(err)
	}
	e := newEnv(t)
	sb := &skillBrain{t: t, e: e}
	sb.install()
	if set != nil {
		set(e)
	}
	if _, err := e.r.Run(context.Background(), cases); err != nil {
		t.Fatal(err)
	}
	report, err := os.ReadFile(paths.EvalReport(e.testStore))
	if err != nil {
		t.Fatal(err)
	}
	return readResults(t, e), string(report), sb
}

// AC 1: the stage runs for Claude and Codex and writes one result per skill, prompt and CLI:
// the controls too. Each case starts from a reset store with its own fresh markers: every brief
// lists the two skills once, and a control's brief lists none.
func TestSkillChoiceStage(t *testing.T) {
	rs, report, sb := skillRun(t, skillFile, nil)
	var got []string
	for _, r := range rs {
		id := r.Case + " " + r.Harness
		if r.Control {
			id += " control"
		}
		got = append(got, id)
		if r.Stage != skillStage || r.Label != string(Pass) {
			t.Errorf("%s: stage %s, label %s (%s), status %s; want a skill choice pass", id, r.Stage, r.Label, r.Rule, r.Status)
		}
		if !r.Control && r.Status != string(Delivered) {
			t.Errorf("%s: status %s (%s), want delivered", id, r.Status, r.Why)
		}
	}
	var want []string
	for _, prompt := range []string{"skill-release-notes", "skill-sqlite-migration", "skill-negative-sql"} {
		for _, ctl := range []string{"", " control"} {
			for _, h := range []string{"claude", "codex"} {
				for _, sk := range []string{"release-notes", "sqlite-migration"} {
					want = append(want, prompt+"/"+sk+" "+h+ctl)
				}
			}
		}
	}
	if !slices.Equal(got, want) {
		t.Errorf("results:\n%q\nwant:\n%q", got, want)
	}
	// The markers of the skills differ in each case.
	seen := map[string]bool{}
	for _, r := range rs {
		m := r.Markers[skillMarker("release-notes")]
		if m == "" {
			t.Fatalf("%s: no marker of release-notes in %v", r.Case, r.Markers)
		}
		seen[m] = true
	}
	if len(seen) != 6 {
		t.Errorf("%d release-notes markers, want one for each of the 6 cases", len(seen))
	}
	if len(sb.briefs) != 12 {
		t.Fatalf("%d replies, want 12", len(sb.briefs))
	}
	for i, b := range sb.briefs {
		n := len(skillEntryLine.FindAllString(b, -1))
		control := i/2%2 == 1 // each case asks claude and then codex; each control follows its case
		if control && (b != "" || sb.skills[i] != 0) || !control && (n != 2 || sb.skills[i] != 2) {
			t.Errorf("reply %d (control %v): %d skills in the store, the brief lists %d: %q", i+1, control, sb.skills[i], n, b)
		}
	}
	for _, w := range []string{"## Skill choice", "- claude: pass", "- codex: pass"} {
		if !strings.Contains(report, w) {
			t.Errorf("report:\n%s\nwant %q", report, w)
		}
	}
}

// AC 2: a fixture skill whose description takes the example prompt of another skill fails the
// stage for both CLIs: that prompt misses its own skill and uses the other one's marker.
func TestSkillChoiceStealFails(t *testing.T) {
	rs, report, _ := skillRun(t, skillFile+greedySkill, nil)
	labels := map[string]string{}
	for _, r := range rs {
		if !r.Control {
			labels[r.Case+" "+r.Harness] = r.Label
		}
	}
	for _, h := range []string{"claude", "codex"} {
		for id, want := range map[string]Label{
			"skill-release-notes/release-notes": Miss, "skill-release-notes/greedy-notes": Invented,
			"skill-sqlite-migration/sqlite-migration": Pass, "skill-negative-sql/greedy-notes": Pass,
		} {
			if got := labels[id+" "+h]; got != string(want) {
				t.Errorf("%s %s: label %s, want %s", id, h, got, want)
			}
		}
	}
	for _, w := range []string{"- claude: fail", "- codex: fail"} {
		if !strings.Contains(report, w) {
			t.Errorf("report:\n%s\nwant %q", report, w)
		}
	}
}

// AC 3: a skill choice result that the rules cannot decide is UNSURE, and the report lists it
// for you to resolve to pass or fail, as in E10-S3; the stage stays unsure until then.
func TestSkillChoiceUnsure(t *testing.T) {
	rs, report, _ := skillRun(t, skillFile, func(e *env) {
		answer := e.fake.answer
		e.fake.answer = func(p *fakePane, prompt string) string {
			if strings.Contains(prompt, "release notes") && *p.pane.Agent == "codex" {
				// The marker line, then a sentence that denies the use of the marker.
				line := strings.TrimSuffix(answer(p, prompt), "\nDone.")
				return "I did not follow the step " + line + "."
			}
			return answer(p, prompt)
		}
	})
	var unsure []string
	for _, r := range rs {
		if r.Label == string(Unsure) {
			unsure = append(unsure, r.Case+" "+r.Harness)
		}
	}
	if !slices.Equal(unsure, []string{"skill-release-notes/release-notes codex"}) {
		t.Errorf("UNSURE results %q, want the codex release-notes example", unsure)
	}
	_, section, _ := strings.Cut(report, "## UNSURE")
	section, _, _ = strings.Cut(section, "## Usefulness")
	if !strings.Contains(section, "- skill-release-notes/release-notes codex: the marker of skill release-notes in a sentence with a negation") {
		t.Errorf("UNSURE section:\n%s\nwant the codex result", section)
	}
	for _, w := range []string{"- claude: pass", "- codex: unsure"} {
		if !strings.Contains(report, w) {
			t.Errorf("report:\n%s\nwant %q", report, w)
		}
	}
}
