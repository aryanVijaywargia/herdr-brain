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
