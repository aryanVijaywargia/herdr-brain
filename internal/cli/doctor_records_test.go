package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aryanVijaywargia/herdr-brain/internal/adapter"
	"github.com/aryanVijaywargia/herdr-brain/internal/install"
	"github.com/aryanVijaywargia/herdr-brain/internal/paths"
)

// The doctor repair of a moved binary shows the hook record entries too, and replaces each
// entry on its own (E11-S5): a hook record entry that you edited stays, and the start entry
// and the other hook record entries get the new binary.
func TestDoctorHookRecords(t *testing.T) {
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
	old := f.installer(gone)
	for _, h := range p.Hooks() {
		if _, err := old.InstallHook(p, h); err != nil {
			t.Fatal(err)
		}
	}
	// Edit the Stop entry by hand.
	rec, err := install.Read(old.Record)
	if err != nil {
		t.Fatal(err)
	}
	settings := filepath.Join(f.home, ".claude", "settings.json")
	b, err := os.ReadFile(settings)
	if err != nil {
		t.Fatal(err)
	}
	stop := rec.Records["claude Stop"].Text
	edited := strings.Replace(stop, `"type"`, `"statusMessage":"x","type"`, 1)
	if err := os.WriteFile(settings, []byte(strings.Replace(string(b), stop, edited, 1)), 0o600); err != nil {
		t.Fatal(err)
	}

	out := f.failed(t, "y\n", "Replace it and the hook record entries", "UserPromptSubmit: ", "PostToolUse: ",
		"PostToolUseFailure: ", "Stop: ", "Replaced the Brain SessionStart entry for claude.",
		"Replaced the Brain PostToolUseFailure entry for claude.", "the Brain Stop entry that you edited")
	s := files(t, f.home)[settings]
	if strings.Count(s, bin+" hook --profile claude") != 4 || !strings.Contains(s, edited) {
		t.Errorf("settings.json after yes:\n%s\nwant 4 entries of %s and the edited Stop entry\n%s", s, bin, out)
	}
	rec, err = install.Read(old.Record)
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range p.Hooks() {
		if h.CLIEvent != "Stop" && !rec.Has(p.Kind, h) {
			t.Errorf("install.json has no %s entry after yes", h.CLIEvent)
		}
	}
	if !strings.Contains(rec.Entries["claude"].Command, bin) {
		t.Errorf("start entry = %+v, want the entry of %s", rec.Entries["claude"], bin)
	}
}

// AC (E19-S4): doctor --fix re-points the entries that name an old binary path with no
// question, and an entry that you edited stays. Plain doctor still asks (TestDoctorMissingBinary).
func TestDoctorFix(t *testing.T) {
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
	old := f.installer(gone)
	for _, h := range p.Hooks() {
		if _, err := old.InstallHook(p, h); err != nil {
			t.Fatal(err)
		}
	}
	rec, err := install.Read(old.Record)
	if err != nil {
		t.Fatal(err)
	}
	settings := filepath.Join(f.home, ".claude", "settings.json")
	b, err := os.ReadFile(settings)
	if err != nil {
		t.Fatal(err)
	}
	stop := rec.Records["claude Stop"].Text
	edited := strings.Replace(stop, `"type"`, `"statusMessage":"x","type"`, 1)
	if err := os.WriteFile(settings, []byte(strings.Replace(string(b), stop, edited, 1)), 0o600); err != nil {
		t.Fatal(err)
	}

	// No answer on stdin: --fix needs none.
	out, err := f.doctorArgs(t, f.sock, "", []string{"--fix"})
	if err == nil {
		t.Errorf("Doctor(--fix) = nil, want the failed check of the edited entry\n%s", out)
	}
	for _, w := range []string{"y (--fix)", "Replaced the Brain SessionStart entry for claude.", "the Brain Stop entry that you edited"} {
		if !strings.Contains(out, w) {
			t.Errorf("brain doctor --fix:\n%s\nwant %q", out, w)
		}
	}
	s := files(t, f.home)[settings]
	if strings.Count(s, bin+" hook --profile claude") != 4 || !strings.Contains(s, edited) || strings.Contains(s, gone+" hook --profile claude --event start") {
		t.Errorf("settings.json after --fix:\n%s\nwant 4 entries of %s and the edited Stop entry", s, bin)
	}
}

func TestDoctorFixArgs(t *testing.T) {
	f := newDoctorFixture(t)
	for _, args := range [][]string{{"--fix", "x"}, {"--fx"}} {
		if out, err := f.doctorArgs(t, f.sock, "", args); err == nil || !strings.Contains(err.Error(), "usage: brain doctor [--fix]") {
			t.Errorf("Doctor(%q) = %v, want the usage\n%s", args, err, out)
		}
	}
}

// dropRecords removes the hook record entries of kind, so that the home holds only the Brain
// start entry of kind, as after an update before #315.
func (f *doctorFixture) dropRecords(t *testing.T, kind string) {
	t.Helper()
	p, err := adapter.LoadFS(testProfiles, kind)
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range p.Hooks()[1:] {
		if st, err := f.installer(entryBin(t)).UninstallHook(p, h); err != nil || st != install.Removed {
			t.Fatalf("UninstallHook(%s %s) = %q, %v, want removed", kind, h.CLIEvent, st, err)
		}
	}
}

// AC #315, the M2 case: with only the Brain start entries, status shows hook records off and
// doctor fails with brain update as the fix, also with --fix, which adds no entry. After brain
// update adds the entries, doctor fails on each Codex hook record entry that Codex does not
// trust yet, and passes when Codex trusts them.
func TestDoctorRecordsM2(t *testing.T) {
	f := newDoctorFixture(t)
	settings := filepath.Join(f.home, ".claude", "settings.json")
	hooks := filepath.Join(f.home, ".codex", "hooks.json")
	f.dropRecords(t, "claude")
	f.dropRecords(t, "codex")
	f.trust(t, "")

	off := []string{
		"hook records off for claude: " + settings + " has no Brain entry for UserPromptSubmit, PostToolUse, " +
			"PostToolUseFailure, Stop; fix: brain update\n",
		"hook records off for codex: " + hooks + " has no Brain entry for UserPromptSubmit, PostToolUse, Stop; fix: brain update\n",
	}
	f.failed(t, "", off...)
	before := files(t, f.home)
	if out, err := f.doctorArgs(t, f.sock, "", []string{"--fix"}); err == nil || !strings.Contains(out, off[0]) {
		t.Errorf("Doctor(--fix) = %v\n%s\nwant %q", err, out, off[0])
	}
	diff(t, files(t, f.home), before)
	status := f.mustStatus(t)
	for _, want := range []string{"  claude 2.1.283: configured, unverified; hook records off (brain update adds them)\n",
		"  codex 0.154.0: configured, unverified; hook records off (brain update adds them)\n"} {
		if !strings.Contains(status, want) {
			t.Errorf("brain status:\n%s\nwant %q", status, want)
		}
	}

	// brain update adds the entries through install (cmd/brain TestUpdateHookRecords).
	f.installAll(t, "claude", entryBin(t))
	f.installAll(t, "codex", entryBin(t))
	out := f.failed(t, "", "codex: Codex hook record entries not trusted: UserPromptSubmit (untrusted), "+
		"PostToolUse (untrusted), Stop (untrusted), so Codex skips them; fix: start codex, type /hooks")
	if strings.Contains(out, "hook records off") || strings.Contains(out, "Codex hook not trusted;") {
		t.Errorf("brain doctor:\n%s\nwant no hook records off and a trusted start entry", out)
	}
	if status := f.mustStatus(t); !strings.Contains(status, "  codex 0.154.0: configured, unverified; hook records on\n") {
		t.Errorf("brain status:\n%s\nwant codex hook records on", status)
	}
	f.trust(t, "")
	if out, err := f.doctor(t, f.sock, ""); err != nil || out != "all checks passed\n" {
		t.Errorf("Doctor() after the trust = %v\n%s\nwant all checks passed", err, out)
	}
}

// AC #315: status names the missing CLI events when some hook record entries are in place, and
// doctor names a hook record entry that changed since Codex trusted it.
func TestRecordsPartly(t *testing.T) {
	f := newDoctorFixture(t)
	p, err := adapter.LoadFS(testProfiles, "codex")
	if err != nil {
		t.Fatal(err)
	}
	stop := p.Hooks()[len(p.Hooks())-1]
	if st, err := f.installer(entryBin(t)).UninstallHook(p, stop); err != nil || st != install.Removed {
		t.Fatalf("UninstallHook(codex Stop) = %q, %v", st, err)
	}
	if status := f.mustStatus(t); !strings.Contains(status, "  codex 0.154.0: configured, unverified; hook records off for Stop (brain update adds them)\n") {
		t.Errorf("brain status:\n%s\nwant codex hook records off for Stop", status)
	}
	f.installAll(t, "codex", entryBin(t))
	config := filepath.Join(f.codexHome, "config.toml")
	b, err := os.ReadFile(config)
	if err != nil {
		t.Fatal(err)
	}
	// The trusted hash of PostToolUse belongs to an older entry: Codex sees a changed entry.
	i := strings.Index(string(b), ":post_tool_use:0:0")
	i += strings.Index(string(b[i:]), "sha256:") + len("sha256:")
	if err := os.WriteFile(config, append(append(b[:i:i], "00"...), b[i+2:]...), 0o600); err != nil {
		t.Fatal(err)
	}
	// The Stop entry is back with the same bytes, so Codex still trusts it.
	f.failed(t, "", "codex: Codex hook record entries not trusted: PostToolUse (modified), so Codex skips them")
}

// entryBin is the brain binary of the entries that doctor wants: this test binary.
func entryBin(t *testing.T) string {
	t.Helper()
	bin, err := paths.BinaryPath()
	if err != nil {
		t.Fatal(err)
	}
	return bin
}
