package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aryanVijaywargia/herdr-brain/internal/adapter"
	"github.com/aryanVijaywargia/herdr-brain/internal/harness"
	"github.com/aryanVijaywargia/herdr-brain/internal/herdr"
	"github.com/aryanVijaywargia/herdr-brain/internal/install"
	"github.com/aryanVijaywargia/herdr-brain/internal/paths"
	"github.com/aryanVijaywargia/herdr-brain/internal/resolver"
	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

// doctorFixture is a status fixture with a temp HOME and a temp CODEX_HOME, both CLIs at
// their tested versions, and the Brain hook of each CLI installed and trusted: a clean setup.
type doctorFixture struct {
	*statusFixture
	home, codexHome string
}

func newDoctorFixture(t *testing.T) *doctorFixture {
	t.Helper()
	f := &doctorFixture{statusFixture: newStatusFixture(t)}
	dir := filepath.Dir(f.store)
	f.home = filepath.Join(dir, "home")
	t.Setenv("HOME", f.home)
	f.codexHome = filepath.Join(f.home, ".codex")
	for _, d := range []string{f.codexHome, filepath.Join(f.home, ".claude")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	f.cli(t, "codex", "codex-cli 0.154.0")
	bin, err := paths.BinaryPath()
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range adapter.Kinds(testProfiles) {
		f.install(t, kind, bin)
	}
	f.trust(t, "")
	return f
}

// cli writes a fake agent CLI that prints version.
func (f *doctorFixture) cli(t *testing.T, kind, version string) {
	t.Helper()
	script := "#!/bin/sh\necho '" + version + "'\n"
	if err := os.WriteFile(filepath.Join(filepath.Dir(f.store), "bin", kind), []byte(script), 0o700); err != nil { //nolint:gosec // a test CLI must be executable
		t.Fatal(err)
	}
}

func (f *doctorFixture) installer(bin string) *install.Installer {
	return &install.Installer{Home: f.home, Record: filepath.Join(f.data, "install.json"), Binary: bin}
}

// install adds the Brain hook of kind that runs bin.
func (f *doctorFixture) install(t *testing.T, kind, bin string) {
	t.Helper()
	p, err := adapter.LoadFS(testProfiles, kind)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.installer(bin).Install(p); err != nil {
		t.Fatal(err)
	}
}

// trust writes the Codex config.toml that trusts the recorded Codex entry with hash sum;
// "" is the hash of the entry, as Codex 0.154 computes it (E8-S3).
func (f *doctorFixture) trust(t *testing.T, sum string) {
	t.Helper()
	rec, err := install.Read(filepath.Join(f.data, "install.json"))
	if err != nil {
		t.Fatal(err)
	}
	if sum == "" {
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(map[string]any{"event_name": "session_start", "hooks": []any{map[string]any{
			"type": "command", "command": rec.Entries["codex"].Command, "timeout": 5, "async": false,
			"additionalContextLimit": 0}}}); err != nil {
			t.Fatal(err)
		}
		h := sha256.Sum256(bytes.TrimSuffix(buf.Bytes(), []byte("\n")))
		sum = "sha256:" + hex.EncodeToString(h[:])
	}
	dir, err := filepath.EvalSymlinks(f.codexHome)
	if err != nil {
		t.Fatal(err)
	}
	key := filepath.Join(dir, "hooks.json") + ":session_start:0:0"
	config := fmt.Sprintf("model = \"x\"\n\n[hooks.state.%q]\ntrusted_hash = %q\n", key, sum)
	if err := os.WriteFile(filepath.Join(f.codexHome, "config.toml"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
}

// doctor runs brain doctor from pane w14:p1 with the answers in stdin.
func (f *doctorFixture) doctor(t *testing.T, socket, stdin string) (string, error) {
	t.Helper()
	return f.doctorArgs(t, socket, stdin, nil)
}

// doctorArgs runs brain doctor with args from pane w14:p1 with the answers in stdin.
func (f *doctorFixture) doctorArgs(t *testing.T, socket, stdin string, args []string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	e := f.env(socket, "w14:p1", "w14", &out)
	e.Environ = append(e.Environ, "CODEX_HOME="+f.codexHome)
	e.Data = f.data
	e.Adapters = harness.Files{Install: filepath.Join(f.data, "install.json"),
		Conformance: filepath.Join(f.data, "conformance.json"), Retest: filepath.Join(f.data, retestDir)}
	e.StatusHerdr = func(herdr.Target) (StatusHerdr, error) { return f.herdr, nil }
	e.Now = func() time.Time { return testNow.Add(3 * time.Hour) }
	e.Stdin = strings.NewReader(stdin)
	err := Doctor(context.Background(), e, args)
	return out.String(), err
}

// failed runs brain doctor and wants exit 1 (an error) and an output that holds each of want.
func (f *doctorFixture) failed(t *testing.T, stdin string, want ...string) string {
	t.Helper()
	out, err := f.doctor(t, f.sock, stdin)
	if err == nil {
		t.Errorf("Doctor() = nil, want a failed check\n%s", out)
	}
	for _, w := range want {
		if !strings.Contains(out, w) {
			t.Errorf("brain doctor:\n%s\nwant %q", out, w)
		}
	}
	if strings.Contains(out, "all checks passed") {
		t.Errorf("brain doctor:\n%s\nwant no \"all checks passed\"", out)
	}
	return out
}

// AC: a clean setup prints "all checks passed" and exits 0. A live agent with a receipt is
// no failure.
func TestDoctorClean(t *testing.T) {
	f := newDoctorFixture(t)
	receipt(t, f.store, "default", "claude", "w14:p1", "s-1", store.ResultEmitted, "")
	f.herdr.panes = []herdr.Pane{agent("w14:p1", "claude", "s-1")}
	out, err := f.doctor(t, f.sock, "")
	if err != nil || out != "all checks passed\n" {
		t.Errorf("Doctor() = %v\n%s\nwant all checks passed", err, out)
	}
}

// AC: an untested herdr version is no failure (S54); a herdr that is too old fails with the fix.
func TestDoctorHerdr(t *testing.T) {
	f := newDoctorFixture(t)
	f.herdr.version, f.herdr.untested = "0.9.3", true
	if out, err := f.doctor(t, f.sock, ""); err != nil {
		t.Errorf("Doctor() = %v\n%s\nwant an untested herdr to pass", err, out)
	}
	f.herdr.checkErr = errors.New("herdr 0.9.0 is older than 0.9.1: run `herdr update`, then restart herdr")
	f.failed(t, "", "herdr: herdr 0.9.0 is older than 0.9.1: run `herdr update`")
}

// AC: a failed pane list is a failed check; with no pane list the receipt check does not run.
func TestDoctorPaneListFails(t *testing.T) {
	f := newDoctorFixture(t)
	f.herdr.panesErr = errors.New("timeout")
	f.failed(t, "", "herdr pane list: timeout")
}

// AC: a CLI on PATH with no Brain hook: profile not configured, fix brain setup. Each failed
// check has its own line.
func TestDoctorNotConfigured(t *testing.T) {
	f := newDoctorFixture(t)
	if err := os.Remove(filepath.Join(f.data, "install.json")); err != nil {
		t.Fatal(err)
	}
	out := f.failed(t, "", "claude: no Brain hook in "+filepath.Join(f.home, ".claude"),
		"codex: no Brain hook in "+filepath.Join(f.home, ".codex", "hooks.json"), "; fix: brain setup")
	if _, err := f.doctor(t, f.sock, ""); err == nil || err.Error() != "2 checks failed" {
		t.Errorf("Doctor() = %v, want 2 checks failed\n%s", err, out)
	}
}

// AC E1-S2: the hook entry runs a missing binary. "no" changes no file and the check stays
// failed; "yes" replaces the entry through install, and doctor then passes.
func TestDoctorMissingBinary(t *testing.T) {
	f := newDoctorFixture(t)
	gone := filepath.Join(filepath.Dir(f.store), "gone", "brain")
	p, err := adapter.Load("claude")
	if err != nil {
		t.Fatal(err)
	}
	bin, err := paths.BinaryPath()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.installer(bin).Uninstall(p); err != nil {
		t.Fatal(err)
	}
	f.install(t, "claude", gone)
	before := files(t, f.home)
	record := files(t, f.data)["install.json"]

	f.failed(t, "n\n", "claude: the Brain hook in", "runs "+gone+" (a missing binary)", "[y/N]",
		"fix: run brain doctor again and answer y")
	diff(t, files(t, f.home), before)
	if files(t, f.data)["install.json"] != record {
		t.Error("install.json changed after no")
	}

	out, err := f.doctor(t, f.sock, "y\n")
	if err != nil || !strings.Contains(out, "Replaced the Brain SessionStart entry for claude.") {
		t.Errorf("Doctor(y) = %v\n%s\nwant the replaced entry and all checks passed", err, out)
	}
	settings := files(t, f.home)[filepath.Join(f.home, ".claude", "settings.json")]
	if strings.Contains(settings, gone) || !strings.Contains(settings, bin) {
		t.Errorf("settings.json after yes:\n%s\nwant the entry of %s", settings, bin)
	}
	if out, err := f.doctor(t, f.sock, ""); err != nil {
		t.Errorf("Doctor() after the fix = %v\n%s", err, out)
	}
}

// AC: the Brain hook is in the hook file of another home (the home changed after the
// install). Doctor offers the guarded reinstall into the file that the CLI reads.
func TestDoctorOtherTarget(t *testing.T) {
	f := newDoctorFixture(t)
	bin, err := paths.BinaryPath()
	if err != nil {
		t.Fatal(err)
	}
	p, err := adapter.Load("claude")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.installer(bin).Uninstall(p); err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(filepath.Dir(f.store), "old-home")
	if err := os.MkdirAll(filepath.Join(old, ".claude"), 0o700); err != nil {
		t.Fatal(err)
	}
	in := f.installer(bin)
	in.Home = old
	if _, err := in.Install(p); err != nil {
		t.Fatal(err)
	}
	f.failed(t, "n\n", "claude: the Brain hook is in "+filepath.Join(old, ".claude", "settings.json")+
		", but claude reads "+filepath.Join(f.home, ".claude", "settings.json"))
	if out, err := f.doctor(t, f.sock, "y\n"); err != nil {
		t.Errorf("Doctor(y) = %v\n%s", err, out)
	}
	if s := files(t, f.home)[filepath.Join(f.home, ".claude", "settings.json")]; !strings.Contains(s, bin) {
		t.Errorf("settings.json after yes:\n%s\nwant the Brain entry", s)
	}
}

// AC: the Codex trust states. Doctor reads config.toml and never writes it.
func TestDoctorCodexTrust(t *testing.T) {
	tests := []struct {
		name string
		edit func(f *doctorFixture, t *testing.T)
		want string
	}{
		{"untrusted", func(f *doctorFixture, t *testing.T) {
			if err := os.Remove(filepath.Join(f.codexHome, "config.toml")); err != nil {
				t.Fatal(err)
			}
		}, "codex: Codex hook not trusted; fix: start codex, type /hooks"},
		{"modified", func(f *doctorFixture, t *testing.T) { f.trust(t, "sha256:00") },
			"codex: Codex hook not trusted: the Brain hook changed since you trusted it; fix: start codex, type /hooks"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newDoctorFixture(t)
			tt.edit(f, t)
			before := files(t, f.codexHome)
			f.failed(t, "", tt.want)
			diff(t, files(t, f.codexHome), before)
		})
	}
}

// AC: a CLI version that differs from the tested version needs a retest.
func TestDoctorRetest(t *testing.T) {
	f := newDoctorFixture(t)
	f.cli(t, "codex", "codex-cli 0.155.0")
	f.failed(t, "", "codex 0.155.0: needs a retest: tested on 0.154.0",
		"fix: run the conformance test in the brain repo: go run ./cmd/brain-eval --probes evals/probes --stage delivery --harness codex")
}

// AC §17 item 30: a live agent of a configured kind with no receipt after its first prompt:
// hook not trusted or not firing.
func TestDoctorNoReceipt(t *testing.T) {
	f := newDoctorFixture(t)
	f.herdr.panes = []herdr.Pane{agent("w14:p1", "claude", "s-1"), agent("w14:p2", "claude", "")}
	f.failed(t, "", "claude: hook not trusted or not firing: 1 live agent(s) with no Brain receipt (w14:p1); fix: start a new session")
}

// AC: a Codex agent with no receipt is a note, not a failed check: Codex runs the hook at the
// first prompt, and the trust check fails an untrusted hook. Doctor exits 0 when all else passes.
func TestDoctorCodexNoReceiptNote(t *testing.T) {
	f := newDoctorFixture(t)
	f.herdr.panes = []herdr.Pane{agent("w14:p1", "codex", "s-1"), agent("w14:p2", "codex", "s-2")}
	out, err := f.doctor(t, f.sock, "")
	want := "note: codex: 2 live agent(s) with no Brain receipt (w14:p1, w14:p2); Codex runs the hook at the first prompt."
	if err != nil || !strings.Contains(out, want) || !strings.HasSuffix(out, "all checks passed\n") {
		t.Errorf("Doctor() = %v\n%s\nwant the note and all checks passed", err, out)
	}
}

// AC S66: an output conflict fails with brain repair --confirm.
func TestDoctorConflict(t *testing.T) {
	f := newDoctorFixture(t)
	f.handEdit(t, f.rows(t).newest)
	f.failed(t, "", "output conflict: 1 output generation(s) changed outside Brain", "fix: brain repair --confirm")
}

// AC S84: an unbound session gets the explanation and the setup command, exits 1, and doctor
// reads no store and writes nothing.
func TestDoctorUnbound(t *testing.T) {
	f := newDoctorFixture(t)
	dir := filepath.Dir(f.store)
	before := files(t, dir)
	out, err := f.doctor(t, filepath.Join(dir, "unbound", "herdr.sock"), "")
	if err == nil {
		t.Error("Doctor() = nil, want a failed check")
	}
	if !strings.Contains(out, "has no Brain binding") || !strings.Contains(out, "To bind it, run: brain setup --session") {
		t.Errorf("brain doctor:\n%s\nwant the explanation and the setup command", out)
	}
	diff(t, files(t, dir), before)
	if f.syncs != 0 || f.herdr.lists != 0 {
		t.Errorf("syncs %d, pane lists %d, want none", f.syncs, f.herdr.lists)
	}
}

// AC: doctor runs the deferred session-id check like any store command.
func TestDoctorChecksAgents(t *testing.T) {
	f := newDoctorFixture(t)
	s := openStore(t, f.store)
	write(t, s, func(tx *sql.Tx) error {
		_, err := store.InsertDelivery(context.Background(), tx, store.Delivery{Profile: "claude",
			HerdrSession: ptr("default"), PaneID: "w14:p1", AgentSessionID: ptr("s-1"),
			Result: store.ResultEmitted, At: testNow, AgentCheck: ptr(string(resolver.AgentPending))})
		return err
	})
	closeStore(t, s)
	if out, err := f.doctor(t, f.sock, ""); err != nil {
		t.Fatalf("Doctor() = %v\n%s", err, out)
	}
	s = openStore(t, f.store)
	defer closeStore(t, s)
	ids, err := (commandStore{s}).UncheckedAgentSessions(context.Background(), "default", "w14:p1")
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 0 {
		t.Errorf("unchecked agent sessions %v after doctor, want none", ids)
	}
}

// AC: doctor takes no argument.
func TestDoctorArgs(t *testing.T) {
	f := newDoctorFixture(t)
	var out bytes.Buffer
	if err := Doctor(context.Background(), f.env(f.sock, "w14:p1", "w14", &out), []string{"x"}); err == nil {
		t.Error("Doctor(x) = nil, want the usage")
	}
}

func TestEntryBinary(t *testing.T) {
	for cmd, want := range map[string]string{
		"/opt/brain hook --profile codex --event start":         "/opt/brain",
		"'/my dir/it'\\''s' hook --profile codex --event start": "/my dir/it's",
	} {
		if got := entryBinary(cmd, "codex"); got != want {
			t.Errorf("entryBinary(%q) = %q, want %q", cmd, got, want)
		}
	}
}
