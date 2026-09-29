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
