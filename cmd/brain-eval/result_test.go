package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aryanVijaywargia/herdr-brain/internal/brief"
	"github.com/aryanVijaywargia/herdr-brain/internal/paths"
	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

// hook is a fake Brain hook: at an agent start it builds generation 1 of the test store with
// global part global, and writes a receipt of the pane with result res and brief hash hash.
// hash "" is the hash of the brief of that generation.
func (e *env) hook(t *testing.T, global string, res store.Result, hash string) {
	e.fake.onStart = func(pane, kind string) {
		out := filepath.Join(e.testStore, "out")
		sum := sha256.Sum256([]byte(global))
		m, err := json.Marshal(map[string]any{"generation": 1, "built_at": time.Now().UTC(), "files": map[string]any{
			"global.md": map[string]any{"sha256": hex.EncodeToString(sum[:]), "revisions": []store.RevisionRef{{Item: 1, Revision: 2}}},
		}})
		if err != nil {
			t.Fatal(err)
		}
		write(t, filepath.Join(out, "gen-1", "global.md"), global)
		write(t, filepath.Join(out, "gen-1", "manifest.json"), string(m))
		write(t, filepath.Join(out, "current"), "gen-1\n")
		if hash == "" {
			sum := sha256.Sum256([]byte(brief.Text(global, "")))
			hash = hex.EncodeToString(sum[:])
		}
		sess, gen, version := session, int64(1), "2.1.0"
		d := store.Delivery{Profile: kind, HerdrSession: &sess, PaneID: pane, BriefHash: &hash, OutputGeneration: &gen,
			ItemRevisions: []store.RevisionRef{{Item: 1, Revision: 2}}, CLIVersion: &version, Result: res, At: time.Now()}
		s, err := store.Open(context.Background(), e.testStore, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close() //nolint:errcheck // test
		if err := s.Write(context.Background(), func(tx *sql.Tx) error {
			_, err := store.InsertDelivery(context.Background(), tx, d)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
}

// knows answers with the fact that the setup steps wrote to fact.txt in the scratch folder, and
// declines without it: the part that Brain plays in a real case.
func knows(p *fakePane, _ string) string {
	data, err := os.ReadFile(filepath.Join(p.cwd, "fact.txt"))
	if err != nil {
		return "I don't know the deploy code word."
	}
	return "The deploy code word is " + strings.TrimSpace(string(data)) + "."
}

func readResults(t *testing.T, e *env) []store.EvalResult {
	t.Helper()
	run, err := store.ReadEvalRun(paths.EvalResults(e.testStore))
	if err != nil {
		t.Fatal(err)
	}
	return run.Results
}

// deliveryCase is a delivery probe of one CLI and its control.
func deliveryCase(harness string) []Case {
	c := Case{
		ID: "code-word", Stage: "delivery", Setup: []string{"echo {{marker}} > fact.txt"},
		Probes: []Probe{{Harness: harness, Prompt: "What is the deploy code word?"}},
		Check:  Check{Expect: []string{"{{marker}}"}},
	}
	ctl := c
	ctl.Control, ctl.Setup = true, nil
	return []Case{c, ctl}
}

// The run status is apart from the label (E10-S2): delivered only with an emitted receipt of
// the expected brief hash; a reply that the runner cannot see whole is UNSURE (AC); a CLI that
// does not get ready is skipped, never a pass. The results go to the results file of the test
// store, with the receipt fields.
func TestResultStatus(t *testing.T) {
	tests := []struct {
		name      string
		harness   string
		set       func(t *testing.T, e *env)
		status    Status
		label     Label
		why       string
		ctlStatus Status
		ctlLabel  Label
	}{
		{"delivered", "claude", func(t *testing.T, e *env) { e.hook(t, "facts\n", store.ResultEmitted, "") },
			Delivered, Pass, "", Delivered, Miss},
		{"another brief hash", "claude", func(t *testing.T, e *env) { e.hook(t, "facts\n", store.ResultEmitted, strings.Repeat("0", 64)) },
			NotDelivered, "", "another brief hash", NotDelivered, Miss},
		{"withheld receipt", "codex", func(t *testing.T, e *env) { e.hook(t, "facts\n", store.ResultWithheld, "") },
			NotDelivered, "", "the receipt is withheld", NotDelivered, Miss},
		{"no receipt", "codex", func(*testing.T, *env) {},
			NotDelivered, "", "no receipt", NotDelivered, Miss},
		{"reply incomplete", "claude", func(_ *testing.T, e *env) { e.fake.notIdle = true },
			Incomplete, Unsure, "agent_not_idle", Incomplete, Unsure},
		{"not ready: folder-trust dialog", "codex", func(_ *testing.T, e *env) { e.fake.notReady = map[string]bool{"codex": true} },
			Skipped, "", "folder-trust dialog", Skipped, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			e.fake.answer = knows
			tt.set(t, e)
			out, err := e.r.Run(context.Background(), deliveryCase(tt.harness))
			if err != nil {
				t.Fatal(err)
			}
			rs := readResults(t, e)
			if len(rs) != 2 {
				t.Fatalf("got %d results, want the case and its control", len(rs))
			}
			got, ctl := rs[0], rs[1]
			if Status(got.Status) != tt.status || Label(got.Label) != tt.label || !strings.Contains(got.Why, tt.why) {
				t.Errorf("result %s, %s (%s); want %s, %s with %q", got.Status, got.Label, got.Why, tt.status, tt.label, tt.why)
			}
			if Status(ctl.Status) != tt.ctlStatus || Label(ctl.Label) != tt.ctlLabel || !ctl.Control {
				t.Errorf("control %s, %s (%s); want %s, %s", ctl.Status, ctl.Label, ctl.Why, tt.ctlStatus, tt.ctlLabel)
			}
			if got.Case != "code-word" || got.Stage != "delivery" || got.Harness != tt.harness || got.Profile != tt.harness ||
				len(got.ProfileSHA256) != 64 || got.Session != session || got.Store != e.testStore ||
				got.Markers["marker"] != out[0].Marker {
				t.Errorf("result %+v, want the case, the profile and its hash, the store binding and the marker", got)
			}
			if tt.status == Delivered && (got.BriefHash == "" || got.Generation != 1 || got.CLIVersion != "2.1.0" ||
				!slices.Equal(got.ItemRevisions, []store.RevisionRef{{Item: 1, Revision: 2}}) ||
				got.Reply != "The deploy code word is "+out[0].Marker+".") {
				t.Errorf("result %+v, want the reply and the receipt fields", got)
			}
		})
	}
}

// Claude draws in the alternate screen, so the runner reads its visible screen; Codex draws
// inline and gets the long read (E11-S1).
func TestVisibleReadForClaude(t *testing.T) {
	for _, tt := range []struct {
		harness string
		visible int
	}{{"claude", 1}, {"codex", 0}} {
		e := newEnv(t)
		e.fake.answer = knows
		if _, err := e.r.Run(context.Background(), deliveryCase(tt.harness)[:1]); err != nil {
			t.Fatal(err)
		}
		if e.fake.visible != tt.visible {
			t.Errorf("%s: %d visible reads, want %d", tt.harness, e.fake.visible, tt.visible)
		}
	}
}

// A probe of a CLI whose Brain hook is missing is skipped with the reason, and never starts
// its CLI; the other probes run.
func TestMissingHookSkips(t *testing.T) {
	tests := []struct {
		name   string
		change func(t *testing.T, e *env)
	}{
		{"no hook of a probe", func(t *testing.T, e *env) {
			rm(t, filepath.Join(e.data, "install.json"))
			e.install(t, "claude")
		}},
		{"hook taken out of its file", func(t *testing.T, e *env) {
			write(t, filepath.Join(e.home, ".codex", "hooks.json"), `{}`)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			tt.change(t, e)
			c := Case{ID: "c", Check: Check{Expect: []string{"x"}},
				Probes: []Probe{{Harness: "codex", Prompt: "p"}, {Harness: "claude", Prompt: "p"}}}
			if _, err := e.r.Run(context.Background(), []Case{c}); err != nil {
				t.Fatal(err)
			}
			rs := readResults(t, e)
			if len(rs) != 2 || rs[0].Status != string(Skipped) || rs[0].Label != "" ||
				!strings.Contains(rs[0].Why, "hook of codex is not installed") || rs[1].Status == string(Skipped) {
				t.Errorf("results %+v, want codex skipped with the reason and claude run", rs)
			}
			for _, p := range e.fake.panes {
				if p.pane.Agent != nil && *p.pane.Agent == "codex" {
					t.Errorf("the run started codex in pane %s", p.pane.ID)
				}
			}
		})
	}
}

// A failed case stops the run, but the results file still holds each probe: the probes of
// the cases that did not run are skipped.
func TestResultsOfFailedRun(t *testing.T) {
	e := newEnv(t)
	cases := []Case{
		{ID: "one", Probes: []Probe{{Harness: "claude", Prompt: "p"}}},
		{ID: "two", Setup: []string{"false"}, Probes: []Probe{{Harness: "codex", Prompt: "p"}}},
	}
	if _, err := e.r.Run(context.Background(), cases); err == nil {
		t.Fatal("Run() = nil, want the failed step")
	}
	rs := readResults(t, e)
	if len(rs) != 2 || rs[0].Case != "one" || rs[0].Status == string(Skipped) ||
		rs[1].Case != "two" || rs[1].Status != string(Skipped) || rs[1].Why != "the case did not run" {
		t.Errorf("results %+v, want case one's result and case two skipped", rs)
	}
}

// The reset empties the test store but keeps the results file.
func TestResetKeepsResults(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "brain.db"), "rows")
	write(t, filepath.Join(dir, "out", "current"), "gen-1")
	write(t, paths.EvalResults(dir), "{}")
	if err := reset(dir); err != nil {
		t.Fatal(err)
	}
	left, err := os.ReadDir(dir)
	if err != nil || len(left) != 1 || left[0].Name() != "eval-results.json" {
		t.Errorf("store holds %v (%v), want only the results file", left, err)
	}
}

// A meaning case that passes in its control too is flagged control_pass and kept (AC). A
// restraint case must pass in its control, so it gets no flag. A reply without its brief gets
// no label, except in a control and an exclusion case.
func TestControlPass(t *testing.T) {
	check := Check{Expect: []string{"bun"}}
	cases := []Case{
		{ID: "pm", Stage: "update", Check: check, Probes: []Probe{{Harness: "claude"}, {Harness: "codex"}}},
		{ID: "pm", Stage: "update", Control: true, Check: check, Probes: []Probe{{Harness: "claude"}, {Harness: "codex"}}},
		{ID: "quiet", Stage: "restraint", Check: check, Probes: []Probe{{Harness: "claude"}}},
		{ID: "quiet", Stage: "restraint", Control: true, Check: check, Probes: []Probe{{Harness: "claude"}}},
		{ID: "gone", Stage: "exclusion", Check: Check{Stale: []string{"pnpm"}}, Probes: []Probe{{Harness: "claude"}}},
	}
	delivered := func(harness, text string) Reply { return Reply{Harness: harness, Status: Delivered, Text: text} }
	outs := []Outcome{
		{Replies: []Reply{delivered("claude", "bun"), {Harness: "codex", Status: NotDelivered, Text: "bun"}}},
		{Replies: []Reply{{Harness: "claude", Status: NotDelivered, Text: "bun"}, {Harness: "codex", Status: NotDelivered, Text: "npm"}}},
		{Replies: []Reply{delivered("claude", "bun")}},
		{Replies: []Reply{{Harness: "claude", Status: NotDelivered, Text: "bun"}}},
		{Replies: []Reply{{Harness: "claude", Status: NotDelivered, Text: "pnpm"}}},
	}
	type got struct {
		label       Label
		controlPass bool
	}
	want := []got{{Pass, true}, {"", false}, {Pass, false}, {Miss, false}, {Pass, false}, {Pass, false}, {Stale, false}}
	rs := results(cases, outs)
	if len(rs) != len(want) {
		t.Fatalf("got %d results, want %d", len(rs), len(want))
	}
	for i, r := range rs {
		if g := (got{Label(r.Label), r.ControlPass}); g != want[i] {
			t.Errorf("result %d (%s %s control %v) = %+v, want %+v", i, r.Case, r.Harness, r.Control, g, want[i])
		}
	}
}

func TestReplyAfter(t *testing.T) {
	tests := []struct {
		name, screen, ask, want string
		found                   bool
	}{
		{"one line", "> What is it?\nIt is bun.\n", "What is it?", "It is bun.", true},
		{"wrapped ask", "> What package\n  manager is it?\n\nbun\n", "What package manager is it?", "bun", true},
		{"last copy of the ask", "> ask\nold\n> ask\nnew\n", "ask", "new", true},
		{"no ask on the screen", "some reply\n", "What is it?", "", false},
		{"nothing after the ask", "> What is it?\n\n", "What is it?", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, found := replyAfter(tt.screen, tt.ask)
			if got != tt.want || found != tt.found {
				t.Errorf("replyAfter() = %q, %v; want %q, %v", got, found, tt.want, tt.found)
			}
		})
	}
}

// Each marker placeholder gets its own fresh value, in every part of the case.
func TestMarkers(t *testing.T) {
	c := Case{Setup: []string{"echo {{marker}} {{marker2}}"}, Probes: []Probe{{Prompt: "{{marker3}}?"}},
		Check: Check{Expect: []string{"{{marker2}}"}}}
	m, err := newMarkers(c)
	if err != nil {
		t.Fatal(err)
	}
	if len(m) != 3 || m["marker"] == m["marker2"] || m["marker2"] == m["marker3"] || m["marker"] == m["marker3"] {
		t.Fatalf("markers %v, want three different values", m)
	}
	if got := fill("{{marker2}} and {{marker}}", m); got != m["marker2"]+" and "+m["marker"] {
		t.Errorf("fill() = %q", got)
	}
}
