package cli

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/aryanVijaywargia/herdr-brain/internal/paths"
)

// noBrained is a brained check that sees no brained, and records the sessions it checks.
func noBrained(sessions *[]string) func(string) (bool, error) {
	return func(s string) (bool, error) {
		*sessions = append(*sessions, s)
		return false, nil
	}
}

// #460: with no brained for the session, doctor fails with the fix and invokes nothing; a yes or
// --fix invokes brain.start in the session.
func TestDoctorBrainedDead(t *testing.T) {
	f := newDoctorFixture(t)
	var sessions []string
	f.brained = noBrained(&sessions)
	f.failed(t, "", `brained: no brained runs for herdr session "default", so Brain captures nothing and no launch ends`,
		`brained: no brained runs for herdr session "default"; fix: run brain doctor again and answer y, `+
			"or run the herdr action brain.start (herdr plugin action invoke brain.start)")
	if len(f.herdr.actions) != 0 || !slices.Equal(sessions, []string{"default"}) {
		t.Errorf("after no: actions %q, checked sessions %q; want none and [default]", f.herdr.actions, sessions)
	}
	for _, c := range []struct {
		stdin string
		args  []string
	}{{"y\n", nil}, {"", []string{"--fix"}}} {
		f.herdr.actions = nil
		out, err := f.doctorArgs(t, f.sock, c.stdin, c.args)
		if err != nil || !strings.Contains(out, "Invoked the herdr action brain.start, which starts brained.") {
			t.Errorf("Doctor(%q, %q) = %v\n%s\nwant brain.start and no failure", c.args, c.stdin, err, out)
		}
		if !slices.Equal(f.herdr.actions, []string{"brain.start"}) {
			t.Errorf("Doctor(%q, %q) actions = %q, want [brain.start]", c.args, c.stdin, f.herdr.actions)
		}
	}
}

// #460: with herdr down, the herdr check says why and the brained check does not run; a failed
// brained check is a failed check.
func TestDoctorBrainedChecks(t *testing.T) {
	f := newDoctorFixture(t)
	var sessions []string
	f.brained = noBrained(&sessions)
	f.herdr.checkErr = errors.New("herdr does not answer")
	if out := f.failed(t, "", "herdr: herdr does not answer"); strings.Contains(out, "brained") || len(sessions) != 0 {
		t.Errorf("brain doctor with herdr down:\n%s\nchecked sessions %q; want no brained check", out, sessions)
	}
	f.herdr.checkErr = nil
	f.brained = func(string) (bool, error) { return false, errors.New("lock: permission denied") }
	f.failed(t, "", "brained: lock: permission denied")
}

// #460: status shows one line when no brained runs, and none when one runs.
func TestStatusBrained(t *testing.T) {
	f := newStatusFixture(t)
	if out := f.mustStatus(t); strings.Contains(out, "brained") {
		t.Errorf("brain status with a brained:\n%s\nwant no brained line", out)
	}
	var sessions []string
	f.brained = noBrained(&sessions)
	want := `brained: not running for herdr session "default", so Brain captures nothing; fix: herdr plugin action invoke brain.start` + "\n"
	if out := f.mustStatus(t); strings.Count(out, "brained") != 1 || !strings.Contains(out, want) {
		t.Errorf("brain status with no brained:\n%s\nwant one line %q", out, want)
	}
}

// staleTarget is a brain binary in a removed GitHub checkout of the plugin under home.
func staleTarget(home string) string {
	return filepath.Join(home, ".config", "herdr", "plugins", "github-installations", "brain-bbbf7a6412d6", "1-0", "checkout", "bin", "brain")
}

// #459: doctor reports a link of setup to an old checkout, and a yes or --fix points it to this
// binary.
func TestDoctorLinkStale(t *testing.T) {
	bin, err := paths.BinaryPath()
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		stdin string
		args  []string
	}{{"y\n", nil}, {"", []string{"--fix"}}} {
		f := newDoctorFixture(t)
		f.link = filepath.Join(f.home, ".local", "bin", "brain")
		old := staleTarget(f.home)
		mustSymlink(t, old, f.link)
		f.failed(t, "", "link: "+f.link+" points to "+old+" (a missing binary), not "+bin,
			"link: "+f.link+" points to "+old+"; fix: run brain doctor again and answer y, or brain setup")
		if got, _ := os.Readlink(f.link); got != old {
			t.Errorf("link after no = %q, want %q", got, old)
		}
		out, err := f.doctorArgs(t, f.sock, c.stdin, c.args)
		if err != nil || !strings.Contains(out, "Pointed "+f.link+" to "+bin+".") {
			t.Errorf("Doctor(%q, %q) = %v\n%s\nwant the link repaired", c.args, c.stdin, err, out)
		}
		if got, err := os.Readlink(f.link); err != nil || got != bin {
			t.Errorf("link after Doctor(%q, %q) = %q, %v; want %q", c.args, c.stdin, got, err, bin)
		}
	}
}

// #459: a link that Brain did not make, broken or not, and no link at all are no failure, and
// --fix leaves them.
func TestDoctorLinkNotOurs(t *testing.T) {
	f := newDoctorFixture(t)
	f.link = filepath.Join(f.home, ".local", "bin", "brain")
	if out, err := f.doctorArgs(t, f.sock, "", []string{"--fix"}); err != nil || out != "all checks passed\n" {
		t.Errorf("Doctor() with no link = %v\n%s\nwant all checks passed", err, out)
	}
	for _, target := range []string{filepath.Join(f.home, "src", "brain"), filepath.Join(f.home, "bin", "brain-old")} {
		if err := os.RemoveAll(f.link); err != nil {
			t.Fatal(err)
		}
		mustSymlink(t, target, f.link)
		if out, err := f.doctorArgs(t, f.sock, "", []string{"--fix"}); err != nil || out != "all checks passed\n" {
			t.Errorf("Doctor() with a link to %s = %v\n%s\nwant all checks passed", target, err, out)
		}
		if got, _ := os.Readlink(f.link); got != target {
			t.Errorf("link after --fix = %q, want %q", got, target)
		}
	}
}

func mustSymlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}
