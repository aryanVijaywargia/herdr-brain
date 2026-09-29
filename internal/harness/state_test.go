package harness

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aryanVijaywargia/herdr-brain/internal/adapter"
	"github.com/aryanVijaywargia/herdr-brain/internal/herdr"
	"github.com/aryanVijaywargia/herdr-brain/internal/install"
)

// fakeHerdr is a herdr client that returns fixed panes.
type fakeHerdr struct {
	panes []herdr.Pane
	err   error
}

func (f fakeHerdr) Panes(context.Context) ([]herdr.Pane, error) { return f.panes, f.err }

func agentPane(kind string) herdr.Pane {
	return herdr.Pane{ID: "w1:p1", WorkspaceID: "w1", TabID: "w1:t1", Agent: &kind}
}

// env is one test home: the data home files, the hook files and a PATH with only fake CLIs.
type env struct {
	home  string
	files Files
	bin   string // the folder of the fake CLIs, the whole PATH
}

func newEnv(t *testing.T) env {
	t.Helper()
	home := t.TempDir()
	data := filepath.Join(home, ".local", "share", "brain")
	e := env{
		home: home,
		files: Files{Install: filepath.Join(data, "install.json"), Conformance: filepath.Join(data, "conformance.json"),
			Retest: filepath.Join(data, "retest")},
		bin: filepath.Join(home, "bin"),
	}
	if err := os.MkdirAll(e.bin, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", e.bin)
	return e
}

// cli writes a fake CLI named kind that prints out for --version.
func (e env) cli(t *testing.T, kind, out string) {
	t.Helper()
	script := "#!/bin/sh\nprintf '%s\\n' '" + out + "'\n"
	if err := os.WriteFile(filepath.Join(e.bin, kind), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
}

// install adds the Brain entry of p to its hook file in the test home.
func (e env) install(t *testing.T, p *adapter.Profile) install.Entry {
	t.Helper()
	dir := filepath.Join(e.home, filepath.Dir(strings.TrimPrefix(p.InstallTarget, "~/")))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	in := install.Installer{Home: e.home, Record: e.files.Install, Binary: "/opt/brain/bin/brain"}
	if st, err := in.Install(p); err != nil || st != install.Added {
		t.Fatalf("Install() = %v, %v, want added", st, err)
	}
	rec, err := install.Read(e.files.Install)
	if err != nil {
		t.Fatal(err)
	}
	return rec.Entries[p.Kind]
}

func (e env) conformance(t *testing.T, results ...Result) {
	t.Helper()
	data, err := json.Marshal(Conformance{Results: results})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(e.files.Conformance, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func load(t *testing.T, harness string) *adapter.Profile {
	t.Helper()
	p, err := adapter.Load(harness)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func check(t *testing.T, e env, h Panes, p *adapter.Profile) Status {
	t.Helper()
	got, err := Check(context.Background(), h, e.files, []*adapter.Profile{p})
	if err != nil {
		t.Fatalf("Check() = %v", err)
	}
	return got[0]
}

// TestStates walks each state change of one harness with a fake CLI and a fake herdr client.
func TestStates(t *testing.T) {
	for _, tc := range []struct{ kind, version string }{
		{"claude", "2.1.283 (Claude Code)"},
		{"codex", "codex-cli 0.154.0"},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			e := newEnv(t)
			p := load(t, tc.kind)
			e.cli(t, tc.kind, tc.version)
			none := fakeHerdr{}
			seen := fakeHerdr{panes: []herdr.Pane{agentPane("other"), agentPane(tc.kind)}}

			if s := check(t, e, none, p); s.State() != None || s.CLIVersion != p.TestedVersion {
				t.Fatalf("no pane, no entry: %+v, want none at %s", s, p.TestedVersion)
			}
			if s := check(t, e, seen, p); s.State() != Detected {
				t.Fatalf("pane of the kind: state %s, want detected", s.State())
			}
			entry := e.install(t, p)
			s := check(t, e, seen, p)
			if s.State() != Configured || !s.Detected || s.Verified {
				t.Fatalf("installed: %+v, want configured, detected, not verified", s)
			}
			pass := Result{Harness: tc.kind, CLIVersion: p.TestedVersion, ProfileSHA256: p.SHA256,
				EntrySHA256: entry.SHA256, RunID: "run-1", Result: Pass}
			e.conformance(t, pass)
			if s := check(t, e, seen, p); s.State() != Verified {
				t.Fatalf("conformance pass: state %s, want verified", s.State())
			}
			e.cli(t, tc.kind, strings.Replace(tc.version, p.TestedVersion, "9.9.9", 1))
			s = check(t, e, seen, p)
			if s.State() != Retest || s.CLIVersion != "9.9.9" || s.Verified || !s.Configured {
				t.Fatalf("new CLI version: %+v, want retest at 9.9.9, configured, not verified", s)
			}
			pass.CLIVersion, pass.RunID = "9.9.9", "run-2"
			e.conformance(t, pass)
			if s := check(t, e, seen, p); s.State() != Verified || s.Retest {
				t.Fatalf("conformance pass on 9.9.9: %+v, want verified, no retest", s)
			}
			// The last pass tested 9.9.9, not the profile version.
			e.cli(t, tc.kind, tc.version)
			s = check(t, e, seen, p)
			if s.State() != Retest || s.CLIVersion != p.TestedVersion || s.Verified {
				t.Fatalf("back to %s after a pass on 9.9.9: %+v, want retest", p.TestedVersion, s)
			}
			e.cli(t, tc.kind, strings.Replace(tc.version, p.TestedVersion, "9.9.10", 1))
			if s := check(t, e, seen, p); s.State() != Retest {
				t.Fatalf("second new CLI version: state %s, want retest", s.State())
			}
		})
	}
}

// TestNotVerified checks that only a completed pass with all current values is verified.
func TestNotVerified(t *testing.T) {
	e := newEnv(t)
	p := load(t, "codex")
	e.cli(t, "codex", "codex-cli 0.154.0")
	entry := e.install(t, p)
	pass := Result{Harness: "codex", CLIVersion: "0.154.0", ProfileSHA256: p.SHA256,
		EntrySHA256: entry.SHA256, RunID: "run-1", Result: Pass}
	with := func(f func(*Result)) Result {
		r := pass
		f(&r)
		return r
	}
	tests := []struct {
		name    string
		results []Result
		want    State
	}{
		{"no conformance file", nil, Configured},
		{"pending", []Result{with(func(r *Result) { r.Result = "pending" })}, Configured},
		{"fail", []Result{with(func(r *Result) { r.Result = "fail" })}, Configured},
		{"inconclusive", []Result{with(func(r *Result) { r.Result = "inconclusive" })}, Configured},
		// The pass tested another version than the current one (§8.5).
		{"other CLI version", []Result{with(func(r *Result) { r.CLIVersion = "0.153.0" })}, Retest},
		{"other profile", []Result{with(func(r *Result) { r.ProfileSHA256 = "00" })}, Configured},
		{"other entry", []Result{with(func(r *Result) { r.EntrySHA256 = "00" })}, Configured},
		{"other harness", []Result{with(func(r *Result) { r.Harness = "claude" })}, Configured},
		{"pass then a pending run", []Result{pass, with(func(r *Result) { r.RunID, r.Result = "run-2", "pending" })}, Configured},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := os.Remove(e.files.Conformance); err != nil && !errors.Is(err, os.ErrNotExist) {
				t.Fatal(err)
			}
			if tt.results != nil {
				e.conformance(t, tt.results...)
			}
			if s := check(t, e, fakeHerdr{}, p); s.State() != tt.want {
				t.Errorf("state = %s, want %s", s.State(), tt.want)
			}
		})
	}
	e.conformance(t, with(func(r *Result) { r.Result = "fail" }), pass)
	if s := check(t, e, fakeHerdr{}, p); s.State() != Verified {
		t.Errorf("fail then pass: state = %s, want verified", s.State())
	}
}

// TestRetestMarker checks that hook input of an unknown shape sets retest on the tested CLI
// version, and that only a pass that completed after the marker clears it (S14).
func TestRetestMarker(t *testing.T) {
	t0 := time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC)
	at := func(m int) time.Time { return t0.Add(time.Duration(m) * time.Minute) }
	e := newEnv(t)
	p := load(t, "claude")
	e.cli(t, "claude", "2.1.283 (Claude Code)")
	entry := e.install(t, p)
	pass := func(m int) Result {
		return Result{Harness: "claude", CLIVersion: p.TestedVersion, ProfileSHA256: p.SHA256,
			EntrySHA256: entry.SHA256, RunID: "run", Result: Pass, At: at(m)}
	}
	mark := func(m int) {
		t.Helper()
		if err := MarkRetest(e.files.Retest, "claude", at(m)); err != nil {
			t.Fatalf("MarkRetest() = %v", err)
		}
	}
	steps := []struct {
		name string
		do   func()
		want State
	}{
		{"marker, no pass", func() { mark(1) }, Retest},
		{"pass before the marker", func() { e.conformance(t, pass(0)) }, Retest},
		{"pass at the marker time", func() { e.conformance(t, pass(1)) }, Retest},
		{"pass after the marker", func() { e.conformance(t, pass(2)) }, Verified},
		{"newer marker", func() { mark(3) }, Retest},
		{"fail after the marker", func() {
			r := pass(4)
			r.Result = "fail"
			e.conformance(t, pass(2), r)
		}, Retest},
		{"pass of another profile after the newer marker", func() {
			r := pass(5)
			r.ProfileSHA256 = "00"
			e.conformance(t, r)
		}, Retest},
		{"pass after the newer marker", func() { e.conformance(t, pass(6)) }, Verified},
	}
	for _, st := range steps {
		st.do()
		s := check(t, e, fakeHerdr{}, p)
		if s.State() != st.want || s.CLIVersion != p.TestedVersion || s.Retest != (st.want == Retest) {
			t.Fatalf("%s: %+v, want %s at %s", st.name, s, st.want, p.TestedVersion)
		}
	}
	// The marker of one harness leaves the others alone.
	e.cli(t, "codex", "codex-cli "+load(t, "codex").TestedVersion)
	if s := check(t, e, fakeHerdr{}, load(t, "codex")); s.Retest {
		t.Errorf("codex with a claude marker: %+v, want no retest", s)
	}
}

// TestRetestMarkerOlderLater checks that an older marker written after a newer one does not
// replace it (#118): two hooks can fail to parse at about the same time, and the later write
// can hold the older time.
func TestRetestMarkerOlderLater(t *testing.T) {
	t0 := time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC)
	e := newEnv(t)
	p := load(t, "claude")
	e.cli(t, "claude", "2.1.283 (Claude Code)")
	entry := e.install(t, p)
	for _, at := range []time.Time{t0.Add(2 * time.Second), t0} {
		if err := MarkRetest(e.files.Retest, "claude", at); err != nil {
			t.Fatalf("MarkRetest(%s) = %v", at, err)
		}
	}
	// A pass between the two marker times is older than the newer marker.
	e.conformance(t, Result{Harness: "claude", CLIVersion: p.TestedVersion, ProfileSHA256: p.SHA256,
		EntrySHA256: entry.SHA256, RunID: "run", Result: Pass, At: t0.Add(time.Second)})
	if s := check(t, e, fakeHerdr{}, p); s.State() != Retest {
		t.Errorf("pass between an older marker written last and a newer one: %+v, want retest", s)
	}
}

// rewrite returns a change that replaces the first from with to in the hook file.
func rewrite(from, to string) func(t *testing.T, target string, data []byte) {
	return func(t *testing.T, target string, data []byte) {
		t.Helper()
		if err := os.WriteFile(target, []byte(strings.Replace(string(data), from, to, 1)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// TestNotConfigured checks that the entry counts only while the hook file holds it unchanged.
func TestNotConfigured(t *testing.T) {
	tests := []struct {
		name   string
		change func(t *testing.T, target string, data []byte)
	}{
		{"file deleted", func(t *testing.T, target string, _ []byte) {
			if err := os.Remove(target); err != nil {
				t.Fatal(err)
			}
		}},
		{"entry edited", rewrite(`"timeout":5`, `"timeout":9`)},
		{"entry moved to another event", rewrite(`"SessionStart"`, `"Stop"`)},
		{"file not JSON", rewrite(`{`, `{{`)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			p := load(t, "claude")
			e.cli(t, "claude", "2.1.283 (Claude Code)")
			entry := e.install(t, p)
			data, err := os.ReadFile(entry.Target)
			if err != nil {
				t.Fatal(err)
			}
			tt.change(t, entry.Target, data)
			if s := check(t, e, fakeHerdr{}, p); s.Configured || s.State() != None {
				t.Errorf("%+v, want not configured", s)
			}
		})
	}
}

// TestVersionFailure checks that a version that cannot be read or parsed sets retest.
func TestVersionFailure(t *testing.T) {
	tests := []struct {
		name string
		out  string // empty: no CLI on the PATH
		want string
	}{
		{"no CLI", "", "claude --version: exec"},
		{"no version word", "Claude Code", `claude --version: no version in "Claude Code\n"`},
		{"two-part version", "2.1 (Claude Code)", "no version in"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			if tt.out != "" {
				e.cli(t, "claude", tt.out)
			}
			s := check(t, e, fakeHerdr{}, load(t, "claude"))
			if s.State() != Retest || s.CLIVersion != "" || s.VersionErr == nil ||
				!strings.Contains(s.VersionErr.Error(), tt.want) {
				t.Errorf("%+v, want retest with an error that contains %q", s, tt.want)
			}
		})
	}
}

func TestCheckErrors(t *testing.T) {
	herdrDown := errors.New("herdr down")
	e := newEnv(t)
	if _, err := Check(context.Background(), fakeHerdr{err: herdrDown}, e.files, []*adapter.Profile{load(t, "claude")}); !errors.Is(err, herdrDown) {
		t.Errorf("Check() with a herdr error = %v, want %v", err, herdrDown)
	}
	tests := []struct {
		name  string
		setup func(t *testing.T, e env)
		want  string
	}{
		{name: "bad conformance file", setup: func(t *testing.T, e env) {
			if err := os.MkdirAll(filepath.Dir(e.files.Conformance), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(e.files.Conformance, []byte("{"), 0o600); err != nil {
				t.Fatal(err)
			}
		}, want: "conformance results"},
		{name: "bad retest marker", setup: func(t *testing.T, e env) {
			if err := os.MkdirAll(e.files.Retest, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(e.files.Retest, "claude.json"), []byte("{"), 0o600); err != nil {
				t.Fatal(err)
			}
		}, want: "retest marker"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			if tt.setup != nil {
				tt.setup(t, e)
			}
			_, err := Check(context.Background(), fakeHerdr{}, e.files, []*adapter.Profile{load(t, "claude")})
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("Check() = %v, want an error with %q", err, tt.want)
			}
		})
	}
}

// TestMarkRetestError checks that a marker that cannot be written, or a current marker that
// cannot be read, is an error.
func TestMarkRetestError(t *testing.T) {
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := MarkRetest(filepath.Join(file, "retest"), "claude", time.Now()); err == nil || !strings.Contains(err.Error(), "retest marker") {
		t.Errorf("MarkRetest(under a file) = %v, want a retest marker error", err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "claude.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := MarkRetest(dir, "claude", time.Now()); err == nil || !strings.Contains(err.Error(), "retest marker") {
		t.Errorf("MarkRetest(over a bad marker) = %v, want a retest marker error", err)
	}
}
