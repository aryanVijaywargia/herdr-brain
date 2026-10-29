package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aryanVijaywargia/herdr-brain/internal/paths"
)

// E24-S8 AC: brain doctor checks, with fake CLI binaries on the PATH of the call, that Brain
// finds the real claude and codex, that each is at the min_version of its launch data or later,
// and that the Codex hook hash rule gives the pinned hash. A CLI that is not on the PATH is no
// failure.
func TestDoctorLaunchCLIs(t *testing.T) {
	f := newDoctorFixture(t)
	bin := filepath.Join(filepath.Dir(f.store), "bin")
	f.environ = []string{"PATH=" + bin}

	// The fixture codex is 0.154.0; claude is 2.1.283, its min_version.
	out := f.failed(t, "", "codex: brain codex starts the plain "+filepath.Join(bin, "codex")+
		" with no Brain flags: codex 0.154.0 is older than 0.165.0; fix: update codex to 0.165.0 or later\n")
	if strings.Contains(out, "brain claude") || strings.Contains(out, "hook hash") {
		t.Errorf("brain doctor:\n%s\nwant no claude line and no hook hash line", out)
	}

	f.cli(t, "codex", "codex-cli 0.165.0")
	if out, err := f.doctor(t, f.sock, ""); err != nil || strings.Contains(out, "brain codex") {
		t.Errorf("Doctor() with codex 0.165.0 = %v\n%s\nwant a pass", err, out)
	}

	f.cli(t, "claude", "Claude Code")
	f.failed(t, "", "claude: brain claude starts the plain "+filepath.Join(bin, "claude")+" with no Brain flags: "+
		filepath.Join(bin, "claude")+" --version: no version in \"Claude Code\\n\"; fix: update claude to 2.1.283 or later\n")

	// A claude on the PATH that is the brain binary: brain claude would run itself.
	self, err := paths.BinaryPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(bin, "claude")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(self, filepath.Join(bin, "claude")); err != nil {
		t.Fatal(err)
	}
	f.failed(t, "", "claude: claude not found on PATH (Brain skips its own binary and its copies), so brain claude "+
		"cannot start it; fix: put the real claude on PATH\n")

	// No claude at all.
	if err := os.Remove(filepath.Join(bin, "claude")); err != nil {
		t.Fatal(err)
	}
	if out, err := f.doctor(t, f.sock, ""); err != nil || strings.Contains(out, "claude") {
		t.Errorf("Doctor() with no claude = %v\n%s\nwant a pass with no claude line", err, out)
	}
}

// E24-S8 AC: brain doctor lists the old launch folders: the folder of an ended launch, and a
// folder with a record id as name but no record. The folder of a running launch, and a folder
// that Brain did not name, are not listed. The list is a note, no failure.
func TestDoctorOldLaunchFolders(t *testing.T) {
	f := newDoctorFixture(t)
	testLaunch(t, f.store, f.sock, "w14:p1", "claude", "", "", true)  // record 1: ended
	testLaunch(t, f.store, f.sock, "w14:p2", "claude", "", "", false) // record 2: running
	for _, name := range []string{"1", "2", "99", "notes"} {
		if err := os.MkdirAll(filepath.Join(paths.Launches(f.store), name), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	out, err := f.doctor(t, f.sock, "")
	if err != nil {
		t.Fatalf("Doctor() = %v\n%s\nwant a pass", err, out)
	}
	want := "note: 2 old launch folder(s), of a launch that ended or with no launch record: " +
		paths.Launch(f.store, 1) + ", " + paths.Launch(f.store, 99) + "; brained deletes the folder of an ended launch " +
		"at the next launch end, and you can delete the others\n"
	if !strings.Contains(out, want) {
		t.Errorf("brain doctor:\n%s\nwant %q", out, want)
	}
}
