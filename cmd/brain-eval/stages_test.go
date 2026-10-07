package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aryanVijaywargia/herdr-brain/internal/adapter"
	"github.com/aryanVijaywargia/herdr-brain/internal/brief"
	"github.com/aryanVijaywargia/herdr-brain/internal/harness"
	"github.com/aryanVijaywargia/herdr-brain/internal/install"
	"github.com/aryanVijaywargia/herdr-brain/internal/paths"
	"github.com/aryanVijaywargia/herdr-brain/internal/store"
	"github.com/aryanVijaywargia/herdr-brain/internal/workspace"
)

// AC 1: each §12.1 stage has a probe for Claude and one for Codex, and each exclusion mode has
// one for each of them. Every other CLI with a profile has a delivery probe: only that stage
// sets its state to verified (§8.5, E16-S12).
func TestStageProbes(t *testing.T) {
	cases, err := loadProbes("../../evals/probes")
	if err != nil {
		t.Fatal(err)
	}
	has := func(match func(Case) bool, h string) bool {
		return slices.ContainsFunc(cases, func(c Case) bool {
			return !c.Control && match(c) && slices.ContainsFunc(c.Probes, func(p Probe) bool { return p.Harness == h })
		})
	}
	for _, h := range []string{"claude", "codex"} {
		for _, s := range stages {
			if !has(func(c Case) bool { return c.Stage == s }, h) {
				t.Errorf("stage %s has no %s probe", s, h)
			}
		}
		for _, m := range hookModes {
			if !has(func(c Case) bool { return c.Hook == m }, h) {
				t.Errorf("exclusion mode %s has no %s probe", m, h)
			}
		}
		if !has(func(c Case) bool { return c.FullBrief }, h) {
			t.Errorf("no full-brief delivery probe for %s", h)
		}
	}
	for _, h := range adapter.Kinds(nil) {
		if !has(func(c Case) bool { return c.Stage == "delivery" }, h) {
			t.Errorf("stage delivery has no %s probe", h)
		}
	}
}

// The setup of the full-brief probe gives a brief within the budgets that passes the Codex
// inline check: full size, with the marker in its middle third (E8-S2).
func TestFullBriefProbeSize(t *testing.T) {
	cases, err := loadProbes("../../evals/probes")
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(cases, func(c Case) bool { return c.FullBrief && !c.Control })
	if i < 0 {
		t.Fatal("no full-brief probe")
	}
	markers, err := newMarkers(cases[i])
	if err != nil {
		t.Fatal(err)
	}
	// A shell brain that writes what each remember call saves, one item for each line: the
	// scope, then the text.
	dir := t.TempDir()
	log := filepath.Join(dir, "items")
	fn := `brain() { if [ "$1" = remember ]; then printf '%s\t%s\n' "${3:-workspace}" "$2" >> ` + shellQuote(log) + `; fi; }; `
	for _, step := range cases[i].Setup {
		if out, err := exec.Command("sh", "-c", fn+fill(step, markers)).CombinedOutput(); err != nil {
			t.Fatalf("step %q: %v: %s", step, err, out)
		}
	}
	data, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	var global, ws []brief.Item
	for n, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		scope, text, _ := strings.Cut(line, "\t")
		it := brief.Item{ID: int64(n + 1), Text: text}
		if scope == "--global" {
			global = append(global, it)
		} else {
			ws = append(ws, it)
		}
	}
	g, err := brief.Global(global)
	if err != nil {
		t.Fatal(err)
	}
	w, err := brief.Workspace(ws)
	if err != nil {
		t.Fatal(err)
	}
	text := brief.Text(g, w)
	if why := briefCheck(text, markers["marker"]); why != "" || len(text) > 9500 {
		t.Errorf("brief of %d bytes: %q; want a full brief within 9,500 bytes", len(text), why)
	}
}

func TestBriefCheck(t *testing.T) {
	pad := strings.Repeat("x", 4600)
	tests := []struct {
		text, want string
	}{
		{pad + "MARK" + pad, ""},
		{"MARK" + pad + pad, "not in the middle third"},
		{pad + pad, "does not hold the marker"},
		{"MARK", "below the full size"},
	}
	for _, tt := range tests {
		if got := briefCheck(tt.text, "MARK"); !strings.Contains(got, tt.want) || (tt.want == "") != (got == "") {
			t.Errorf("briefCheck(%d bytes) = %q, want %q", len(tt.text), got, tt.want)
		}
	}
}

// fullBrief is the global part of a full-size brief with the fact of the case in its middle:
// the fact.txt of the case's scratch folder, which a setup step wrote.
func (e *env) fullBrief(t *testing.T) func() string {
	return func() string {
		names, err := filepath.Glob(filepath.Join(e.r.Scratch, "*", "fact.txt"))
		if err != nil || len(names) != 1 {
			return "facts\n"
		}
		data, err := os.ReadFile(names[0])
		if err != nil {
			t.Fatal(err)
		}
		pad := strings.Repeat("padding item for a size test\n", 160)
		return pad + "The release code word is " + strings.TrimSpace(string(data)) + ".\n" + pad
	}
}

// fullBriefCase is deliveryCase with a full-size brief: two setup steps, so the second Brain
// call sees the fact of the first.
func fullBriefCase(harness string) []Case {
	cs := deliveryCase(harness)
	cs[0].ID, cs[1].ID = "full", "full"
	cs[0].FullBrief, cs[1].FullBrief = true, true
	cs[0].Setup = append(cs[0].Setup, "true")
	return cs
}

// cliOnPath puts a fake CLI of kind on the PATH that prints version.
func cliOnPath(t *testing.T, bin, kind, version string) {
	t.Helper()
	write(t, filepath.Join(bin, kind), "#!/bin/sh\necho '"+version+"'\n")
	if err := os.Chmod(filepath.Join(bin, kind), 0o755); err != nil {
		t.Fatal(err)
	}
}

func readConformance(t *testing.T, e *env) map[string]harness.Result {
	t.Helper()
	data, err := os.ReadFile(e.r.Conformance)
	if err != nil {
		t.Fatal(err)
	}
	var c harness.Conformance
	if err := json.Unmarshal(data, &c); err != nil {
		t.Fatal(err)
	}
	m := map[string]harness.Result{}
	for _, r := range c.Results {
		if _, ok := m[r.Harness]; ok {
			t.Errorf("two records of %s", r.Harness)
		}
		m[r.Harness] = r
	}
	return m
}

// A delivery run writes a pending record before its cases and the result of the completed
// run after them, with the CLI version, the profile hash, the entry hash and the run id. A
// Codex pass needs a passed full-size brief (E8-S2). The record of the other harness stays.
func TestConformanceRecord(t *testing.T) {
	tests := []struct {
		name    string
		harness string
		cases   func(string) []Case
		set     func(t *testing.T, e *env)
		want    string
	}{
		{"claude pass", "claude", deliveryCase, func(t *testing.T, e *env) { e.hook(t, store.ResultEmitted, "") }, harness.Pass},
		{"withheld receipt", "claude", deliveryCase, func(t *testing.T, e *env) { e.hook(t, store.ResultWithheld, "") }, resultFail},
		{"reply incomplete", "claude", deliveryCase, func(t *testing.T, e *env) {
			e.hook(t, store.ResultEmitted, "")
			e.fake.notIdle = true
		}, resultInconclusive},
		{"codex without a full brief", "codex", deliveryCase, func(t *testing.T, e *env) { e.hook(t, store.ResultEmitted, "") }, resultInconclusive},
		{"codex with a full brief", "codex", fullBriefCase, func(t *testing.T, e *env) {
			e.hookText(t, store.ResultEmitted, "", e.fullBrief(t))
		}, harness.Pass},
		{"codex with a small brief", "codex", fullBriefCase, func(t *testing.T, e *env) { e.hook(t, store.ResultEmitted, "") }, resultInconclusive},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			e.fake.answer = knows
			tt.set(t, e)
			other := "codex"
			if tt.harness == "codex" {
				other = "claude"
			}
			kept := harness.Result{Harness: other, CLIVersion: "1.0.0", RunID: "old", Result: harness.Pass}
			if err := harness.WriteConformance(e.r.Conformance, kept); err != nil {
				t.Fatal(err)
			}
			var before map[string]harness.Result
			run := e.fake.onRun
			e.fake.onRun = func() {
				if before == nil {
					before = readConformance(t, e)
				}
				run()
			}
			if _, err := e.r.Run(context.Background(), tt.cases(tt.harness)); err != nil {
				t.Fatal(err)
			}
			if before[tt.harness].Result != resultPending {
				t.Errorf("record before the cases %+v, want pending", before[tt.harness])
			}
			got := readConformance(t, e)
			p, err := adapter.Load(tt.harness)
			if err != nil {
				t.Fatal(err)
			}
			rec, err := install.Read(e.r.Install)
			if err != nil {
				t.Fatal(err)
			}
			want := harness.Result{Harness: tt.harness, CLIVersion: "2.1.283", ProfileSHA256: p.SHA256,
				EntrySHA256: rec.Entries[tt.harness].SHA256, RunID: runID(e.r.Now()), Result: tt.want, At: e.r.Now()}
			if g := got[tt.harness]; g != want {
				t.Errorf("record %+v, want %+v", g, want)
			}
			if got[other] != kept {
				t.Errorf("record of %s %+v, want %+v kept", other, got[other], kept)
			}
		})
	}
}

// A run that stops at a failed case completed, but gives no pass: its delivery probes that
// passed before the failure make it inconclusive, not pending.
func TestConformanceFailedRun(t *testing.T) {
	e := newEnv(t)
	e.fake.answer = knows
	e.hook(t, store.ResultEmitted, "")
	cs := append(deliveryCase("claude"), Case{ID: "broken", Stage: "update", Setup: []string{"false"}})
	if _, err := e.r.Run(context.Background(), cs); err == nil {
		t.Fatal("Run() = nil, want the failed setup step")
	}
	if got := readConformance(t, e)["claude"].Result; got != resultInconclusive {
		t.Errorf("record after a failed run %q, want inconclusive", got)
	}
}

// A run of another stage, or of the delivery stage of the other CLI, leaves the record alone.
func TestConformanceUnrequested(t *testing.T) {
	e := newEnv(t)
	e.fake.answer = knows
	e.hook(t, store.ResultEmitted, "")
	cs := deliveryCase("claude")
	for i := range cs {
		cs[i].Stage = "update"
	}
	if _, err := e.r.Run(context.Background(), cs); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(e.r.Conformance); !os.IsNotExist(err) {
		t.Fatalf("conformance file after an update run: %v, want none", err)
	}
	kept := harness.Result{Harness: "codex", RunID: "old", Result: harness.Pass}
	if err := harness.WriteConformance(e.r.Conformance, kept); err != nil {
		t.Fatal(err)
	}
	if _, err := e.r.Run(context.Background(), deliveryCase("claude")); err != nil {
		t.Fatal(err)
	}
	if got := readConformance(t, e); got["codex"] != kept || got["claude"].Result != harness.Pass {
		t.Errorf("records %+v, want codex kept and a claude pass", got)
	}
}

// AC 2 and 3 through the runner and the E8-S4 check: with the same hashes, a pass is verified;
// a later fail, incomplete reply or interrupted run is not; a pass after it is verified again.
// A new CLI version after a pass stays verified, with a warning only (D13).
func TestConformanceSequence(t *testing.T) {
	e := newEnv(t)
	e.fake.answer = knows
	bin := t.TempDir()
	cliOnPath(t, bin, "claude", "2.1.283 (Claude Code)")
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	p, err := adapter.Load("claude")
	if err != nil {
		t.Fatal(err)
	}
	files := harness.Files{Install: e.r.Install, Conformance: e.r.Conformance, Retest: t.TempDir()}
	state := func() harness.State { return checkState(t, e, files, p) }
	run := func(res store.Result, notIdle, interrupt bool) {
		t.Helper()
		e.hook(t, res, "")
		e.fake.notIdle = notIdle
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		if interrupt {
			start := e.fake.onStart
			e.fake.onStart = func(pane, kind string) { start(pane, kind); cancel() }
		}
		_, _ = e.r.Run(ctx, deliveryCase("claude")) // an interrupted run can end with an error
	}
	pass := func() { run(store.ResultEmitted, false, false) }
	for _, bad := range []struct {
		name string
		run  func()
	}{
		{"fail", func() { run(store.ResultWithheld, false, false) }},
		{"reply incomplete", func() { run(store.ResultEmitted, true, false) }},
		{"interrupted", func() { run(store.ResultEmitted, false, true) }},
	} {
		pass()
		if s := state(); s != harness.Verified {
			t.Fatalf("pass before %s: state %s, want verified", bad.name, s)
		}
		bad.run()
		if s := state(); s == harness.Verified {
			t.Fatalf("pass then %s: state %s, want not verified", bad.name, s)
		}
	}
	if got := readConformance(t, e)["claude"].Result; got != resultPending {
		t.Errorf("record after the interrupted run %q, want pending", got)
	}
	pass()
	if s := state(); s != harness.Verified {
		t.Fatalf("a later pass: state %s, want verified", s)
	}
	cliOnPath(t, bin, "claude", "9.9.9 (Claude Code)")
	if s := state(); s != harness.Verified {
		t.Fatalf("new CLI version after a pass: state %s, want verified", s)
	}
}

// checkState is the state of p by the E8-S4 check.
func checkState(t *testing.T, e *env, files harness.Files, p *adapter.Profile) harness.State {
	t.Helper()
	ss, err := harness.Check(context.Background(), e.fake, files, []*adapter.Profile{p})
	if err != nil {
		t.Fatal(err)
	}
	return ss[0].State()
}

// Codex gets the trust of its scratch folder on the command line, as a quoted key. A setup
// agent of Claude may run `brain remember`. A setup CLI that does not get ready skips the
// probes of its case.
func TestAgentArgs(t *testing.T) {
	e := newEnv(t)
	e.fake.answer = knows
	c := Case{ID: "transfer", Stage: "transfer", Setup: []string{"agent claude: remember {{marker}}"},
		Probes: []Probe{{Harness: "codex", Prompt: "what?"}}, Check: Check{Expect: []string{"{{marker}}"}}}
	if _, err := e.r.Run(context.Background(), []Case{c}); err != nil {
		t.Fatal(err)
	}
	scratch, err := filepath.EvalSymlinks(e.r.Scratch)
	if err != nil {
		t.Fatal(err)
	}
	var claude, codex []string
	for _, p := range e.fake.panes {
		switch {
		case p.pane.Agent == nil:
		case *p.pane.Agent == "claude":
			claude = p.args
		case *p.pane.Agent == "codex":
			codex = p.args
		}
	}
	if !slices.Equal(claude, []string{"--allowedTools", "Bash(brain remember:*)"}) {
		t.Errorf("claude setup args %q", claude)
	}
	if len(codex) < 2 || codex[0] != "-c" || !strings.HasPrefix(codex[1], `projects={"`+scratch+string(filepath.Separator)) ||
		!strings.HasSuffix(codex[1], `"={trust_level="trusted"}}`) {
		t.Errorf("codex args %q, want the trust of a folder in %s", codex, scratch)
	}

	e = newEnv(t)
	e.fake.notReady = map[string]bool{"claude": true}
	if _, err := e.r.Run(context.Background(), []Case{c}); err != nil {
		t.Fatal(err)
	}
	rs := readResults(t, e)
	if len(rs) != 1 || Status(rs[0].Status) != Skipped || !strings.Contains(rs[0].Why, "setup CLI claude did not get ready") {
		t.Errorf("results %+v, want the codex probe skipped", rs)
	}
}

// brainOnPath puts a fake brain on the PATH. Its hook writes its arguments and herdr
// variables to log, and prints print.txt of its folder when HERDR_ENV is 1.
func brainOnPath(t *testing.T) (log string) {
	t.Helper()
	bin := t.TempDir()
	log = filepath.Join(bin, "log")
	write(t, filepath.Join(bin, "brain"), `#!/bin/sh
cat > /dev/null
echo "$*|${HERDR_ENV-}|${HERDR_PANE_ID-}|${HERDR_WORKSPACE_ID-}|${HERDR_SOCKET_PATH-}|${HERDR_BIN_PATH-}" >> `+shellQuote(log)+`
if [ "${HERDR_ENV-}" = 1 ]; then cat print.txt; fi
`)
	if err := os.Chmod(filepath.Join(bin, "brain"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	for k, v := range map[string]string{"HERDR_ENV": "1", "HERDR_PANE_ID": "w1:p1", "HERDR_WORKSPACE_ID": "w1",
		"HERDR_SOCKET_PATH": filepath.Join(bin, "herdr.sock"), "HERDR_BIN_PATH": "/opt/herdr"} {
		t.Setenv(k, v)
	}
	return log
}

// An exclusion case calls `brain hook` with the herdr state of its mode, and labels what the
// hook printed: a marker that the case withholds is INVENTED.
func TestExclusionHook(t *testing.T) {
	tests := []struct {
		mode   string
		print  string // what the fake hook prints
		expect []string
		check  func(e *env, f []string) bool // the fields of the log line
		label  Label
		status Status
	}{
		{"no-herdr-env", "", nil, func(_ *env, f []string) bool { return f[1] == "" }, Pass, NotDelivered},
		{"unbound-session", "", nil, func(_ *env, f []string) bool { return strings.HasSuffix(f[4], "/unbound.sock") }, Pass, NotDelivered},
		{"no-herdr-answer", "", nil, func(_ *env, f []string) bool { return strings.HasSuffix(f[5], "/.brain-eval-slow-herdr") }, Pass, NotDelivered},
		{"closed-pane", "", nil, func(e *env, f []string) bool { p := e.fake.panes[f[2]]; return p != nil && p.closed && f[2] != "w1:p1" }, Pass, NotDelivered},
		{"other-workspace", "{{marker}}", []string{"{{marker}}"}, func(e *env, f []string) bool {
			return f[3] != "w1" && strings.HasPrefix(f[2], f[3]+":") && strings.HasSuffix(e.fake.labels[f[3]], "-other")
		}, Pass, Delivered},
		{"other-workspace", "{{marker}} {{marker2}}", []string{"{{marker}}"}, func(*env, []string) bool { return true }, Invented, Delivered},
	}
	for _, tt := range tests {
		t.Run(tt.mode+" "+string(tt.label), func(t *testing.T) {
			log := brainOnPath(t)
			e := newEnv(t)
			c := Case{ID: "ex", Stage: "exclusion", Hook: tt.mode, Check: Check{Expect: tt.expect},
				Setup:  []string{"printf '%s' " + shellQuote(tt.print) + " > print.txt; echo {{marker2}} > withheld.txt"},
				Probes: []Probe{{Harness: "claude"}, {Harness: "codex"}}}
			if _, err := e.r.Run(context.Background(), []Case{c}); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(log)
			if err != nil {
				t.Fatal(err)
			}
			lines := strings.Split(strings.TrimSpace(string(data)), "\n")
			if len(lines) != 2 {
				t.Fatalf("hook calls %q, want one for each CLI", lines)
			}
			for i, h := range []string{"claude", "codex"} {
				f := strings.Split(lines[i], "|")
				if f[0] != "hook --profile "+h+" --event start" || !tt.check(e, f) {
					t.Errorf("hook call %q, want the %s state", lines[i], tt.mode)
				}
			}
			for _, r := range readResults(t, e) {
				if Label(r.Label) != tt.label || Status(r.Status) != tt.status {
					t.Errorf("%s: %s, %s (%s), want %s, %s", r.Harness, r.Status, r.Label, r.Rule, tt.status, tt.label)
				}
			}
		})
	}
}

// The output_none of a profile, for example agy's "{}", is the hook printing nothing: an
// exclusion probe of that CLI is not delivered (E16-S12). For a CLI with no output_none, "{}"
// is output.
func TestExclusionHookOutputNone(t *testing.T) {
	log := brainOnPath(t)
	e, _ := fakeEnvWith(t, "output_none = \"{}\"\n")
	c := Case{ID: "ex", Stage: "exclusion", Hook: "other-workspace",
		Setup:  []string{"printf '{}\\n' > print.txt"},
		Probes: []Probe{{Harness: "fake"}, {Harness: "claude"}}}
	if _, err := e.r.Run(context.Background(), []Case{c}); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(log); err != nil || strings.Count(string(data), "\n") != 2 {
		t.Fatalf("hook calls %q, %v, want one for each CLI", data, err)
	}
	want := map[string]Status{"fake": NotDelivered, "claude": Delivered}
	for _, r := range readResults(t, e) {
		if Status(r.Status) != want[r.Harness] {
			t.Errorf("%s: %s (%s), want %s", r.Harness, r.Status, r.Why, want[r.Harness])
		}
	}
}

// AC 4: a usefulness case has a record of what the CLI changed, and the report lists it.
func TestUsefulnessRecord(t *testing.T) {
	e := newEnv(t)
	e.hook(t, store.ResultEmitted, "")
	e.fake.answer = func(p *fakePane, _ string) string {
		write(t, filepath.Join(p.cwd, "new.txt"), "x")
		return "done"
	}
	c := Case{ID: "pm", Stage: "usefulness", Setup: []string{"echo {{marker}} > fact.txt; git add fact.txt"},
		Probes: []Probe{{Harness: "claude", Prompt: "add a file"}}, Check: Check{Expect: []string{"done"}}}
	if _, err := e.r.Run(context.Background(), []Case{c}); err != nil {
		t.Fatal(err)
	}
	rs := readResults(t, e)
	if len(rs) != 1 || !strings.Contains(rs[0].Why, "record: git status: A fact.txt; ?? new.txt") {
		t.Fatalf("results %+v, want the git status record", rs)
	}
	if v := verdicts(rs); v[0] != verdictRecord {
		t.Errorf("verdict %q, want record", v[0])
	}
	data, err := os.ReadFile(paths.EvalReport(e.testStore))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "## Usefulness records\n\n- pm claude: record: git status:") {
		t.Errorf("report %s, want the usefulness record", data)
	}
}

// A forget case whose forgotten answer is still in the output files is STALE, also when the
// reply passes (§12.1 forget).
func TestForgetLeak(t *testing.T) {
	e := newEnv(t)
	e.fake.answer = knows
	e.hook(t, store.ResultEmitted, "")
	c := Case{ID: "gone", Stage: "forget", Forgotten: []string{"{{marker2}}"},
		Setup:  []string{"echo {{marker}} > fact.txt", "mkdir -p " + e.testStore + "/out/x && echo {{marker2}} > " + e.testStore + "/out/x/left.txt"},
		Probes: []Probe{{Harness: "claude", Prompt: "what?"}}, Check: Check{Expect: []string{"{{marker}}"}}}
	out, err := e.r.Run(context.Background(), []Case{c})
	if err != nil {
		t.Fatal(err)
	}
	rs := readResults(t, e)
	want := "the store still holds forgotten " + out[0].Markers["marker2"] + " in out/x/left.txt"
	if len(rs) != 1 || Label(rs[0].Label) != Stale || rs[0].Rule != want {
		t.Errorf("results %+v, want STALE with %q", rs, want)
	}
}

func TestTableLeaks(t *testing.T) {
	dir := t.TempDir()
	s, err := store.Open(context.Background(), dir, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close() //nolint:errcheck // test
	err = s.Write(context.Background(), func(tx *sql.Tx) error {
		if _, err := store.InsertDelivery(context.Background(), tx, store.Delivery{Profile: "claude", PaneID: "w1:p1-abcd-1234",
			Result: store.ResultEmitted, At: time.Now()}); err != nil {
			return err
		}
		found, err := tableLeaks(context.Background(), tx, []string{"abcd-1234", "zzzz-0000"})
		if err != nil {
			return err
		}
		if len(found) != 1 || !strings.HasPrefix(found[0], "abcd-1234 in table ") {
			t.Errorf("tableLeaks() = %q, want the one table with abcd-1234", found)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// A restraint case passes only when its control passes too, and fails when its control was
// skipped; a result without a label fails; a control and an ad-hoc case get no verdict.
func TestVerdicts(t *testing.T) {
	r := func(c, stage string, control bool, status Status, label Label) store.EvalResult {
		return store.EvalResult{Case: c, Stage: stage, Control: control, Harness: "claude", Status: string(status), Label: string(label)}
	}
	rs := []store.EvalResult{
		r("d", "delivery", false, Delivered, Pass),
		r("d", "delivery", true, NotDelivered, Miss),
		r("n", "delivery", false, NotDelivered, ""),
		r("q", "restraint", false, Delivered, Pass),
		r("q", "restraint", true, NotDelivered, Pass),
		r("l", "restraint", false, Delivered, Pass),
		r("l", "restraint", true, NotDelivered, Stale),
		r("s", "restraint", false, Delivered, Pass),
		r("s", "restraint", true, Skipped, ""),
		r("w", "restraint", false, Delivered, Pass),
		r("w", "restraint", true, Incomplete, Unsure),
		r("u", "update", false, Incomplete, Unsure),
		r("k", "update", false, Skipped, ""),
		r("pm", "usefulness", false, Delivered, Miss),
		r("adhoc", "", false, Delivered, Pass),
	}
	want := []string{verdictPass, "", verdictFail, verdictPass, "", verdictFail, "", verdictFail, "", verdictUnsure, "",
		verdictUnsure, verdictSkipped, verdictRecord, ""}
	if got := verdicts(rs); !slices.Equal(got, want) {
		t.Errorf("verdicts() = %q, want %q", got, want)
	}
	if got := score(want); got != "score: 2 pass, 3 fail, 2 unsure, 1 skipped, 1 record, of 9 probes" {
		t.Errorf("score() = %q", got)
	}
}

// The report lists each result with its verdict and each UNSURE to resolve; an ad-hoc run has
// none.
func TestWriteReport(t *testing.T) {
	dir := t.TempDir()
	var out strings.Builder
	if err := writeReport(dir, []Case{{ID: "adhoc"}}, nil, &out); err != nil || out.Len() != 0 {
		t.Fatalf("writeReport(adhoc) = %v, %q; want no report", err, out.String())
	}
	if _, err := os.Stat(paths.EvalReport(dir)); !os.IsNotExist(err) {
		t.Fatalf("report of an ad-hoc run: %v, want none", err)
	}
	rs := []store.EvalResult{
		{Case: "u", Stage: "update", Harness: "codex", CLIVersion: "0.154.0", Status: string(Incomplete), Label: string(Unsure),
			Rule: "the runner did not see the whole reply", Reply: "bun"},
	}
	if err := writeReport(dir, []Case{{ID: "u", Stage: "update"}}, rs, &out); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(paths.EvalReport(dir))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"score: 0 pass, 0 fail, 1 unsure, 0 skipped, 0 record, of 1 probes",
		"| u | update | codex 0.154.0 | reply incomplete | UNSURE | unsure |",
		"## UNSURE: resolve each to pass or fail\n\n- u codex: the runner did not see the whole reply; reply \"bun\"",
		"## Usefulness records\n\nNone.",
	} {
		if !strings.Contains(string(data), want) {
			t.Errorf("report %s\nwant %q", data, want)
		}
	}
	if !strings.Contains(out.String(), "report: "+paths.EvalReport(dir)) {
		t.Errorf("stdout %q, want the report path", out.String())
	}
}

// #277: herdr saves a new case workspace about 5 s after it creates it (HLD §11). The runner
// maps the case workspace before the setup steps, also when herdr saves it late, so a
// workspace `brain remember` can run and the hook receipt is emitted, not unmapped. A case
// workspace that herdr does not save in time stops the run before its setup steps.
func TestCaseWorkspaceMapped(t *testing.T) {
	tests := []struct {
		name      string
		saveAfter int
		noSave    bool
		err       string
	}{
		{"saved at once", 0, false, ""},
		{"saved late", 3, false, ""},
		{"not saved", 0, true, "w1 is unmapped after"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			e.fake.answer = knows
			e.fake.saveAfter, e.fake.noSave = tt.saveAfter, tt.noSave
			e.r.Timeouts.Map = 200 * time.Millisecond
			e.hook(t, store.ResultEmitted, "")
			var states []workspace.State // the state of the case workspace at each setup step
			run := e.fake.onRun
			e.fake.onRun = func() {
				states = append(states, e.lookup(t, "w1").State)
				run()
			}
			_, err := e.r.Run(context.Background(), deliveryCase("codex")[:1])
			if tt.err != "" {
				if err == nil || !strings.Contains(err.Error(), tt.err) {
					t.Errorf("Run() = %v, want %q", err, tt.err)
				}
				if len(states) != 0 {
					t.Errorf("setup steps ran at states %v, want none", states)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(states, []workspace.State{workspace.Mapped}) {
				t.Errorf("case workspace at the setup step: %v, want mapped", states)
			}
			if rs := readResults(t, e); len(rs) != 1 || Status(rs[0].Status) != Delivered || rs[0].Label != string(Pass) {
				t.Errorf("results %+v, want one delivered PASS", rs)
			}
		})
	}
}

// #276: Claude Code 2.1.292 asks for folder trust in each new git folder. The runner answers
// that dialog once in the case folder that it made, closes that CLI and starts a fresh one in
// a new pane, so the probe passes or fails on delivery. A setup agent gets the same answer.
// The runner answers no dialog of another folder or another CLI, and no dialog twice.
func TestTrustDialog(t *testing.T) {
	transfer := Case{ID: "transfer", Stage: "transfer", Setup: []string{"agent claude: save {{marker}}", "echo {{marker}} > fact.txt"},
		Probes: []Probe{{Harness: "codex", Prompt: "What is the deploy code word?"}}, Check: Check{Expect: []string{"{{marker}}"}}}
	tests := []struct {
		name     string
		c        Case
		set      func(e *env)
		status   Status
		answered bool
	}{
		{"probe", deliveryCase("claude")[0], func(*env) {}, Delivered, true},
		{"setup agent", transfer, func(*env) {}, Delivered, true},
		{"not ready after the answer", deliveryCase("claude")[0], func(e *env) { e.fake.notReady = map[string]bool{"claude": true} }, Skipped, true},
		{"dialog of another folder", deliveryCase("claude")[0], func(e *env) { e.fake.dialogDir = "/private/tmp/other" }, Skipped, false},
		{"dialog of another CLI", deliveryCase("codex")[0], func(e *env) { e.fake.trust = map[string]bool{"codex": true} }, Skipped, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			e.fake.answer = knows
			e.fake.trust = map[string]bool{"claude": true}
			e.hook(t, store.ResultEmitted, "")
			tt.set(e)
			if _, err := e.r.Run(context.Background(), []Case{tt.c}); err != nil {
				t.Fatal(err)
			}
			if rs := readResults(t, e); len(rs) != 1 || Status(rs[0].Status) != tt.status {
				t.Errorf("results %+v, want one %s", rs, tt.status)
			}
			var keyed []string // the panes that got keys
			for _, id := range e.fake.order {
				p := e.fake.panes[id]
				if len(p.keys) == 0 {
					continue
				}
				keyed = append(keyed, id)
				if !slices.Equal(p.keys, []string{"Down", "Enter"}) || len(p.prompts) != 0 || !p.closed {
					t.Errorf("pane %s: keys %v, prompts %v, closed %v; want the trust answer only, then a close", id, p.keys, p.prompts, p.closed)
				}
			}
			if (len(keyed) == 1) != tt.answered || len(keyed) > 1 {
				t.Errorf("panes with keys %v, want an answer: %v", keyed, tt.answered)
			}
			for dir, ok := range e.fake.trusted {
				if !tt.answered || !ok || filepath.Dir(dir) != e.r.Scratch {
					t.Errorf("trusted folder %s (%v), want only the case folder in %s after an answer", dir, ok, e.r.Scratch)
				}
			}
		})
	}
}
