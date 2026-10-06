package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aryanVijaywargia/herdr-brain/internal/adapter"
	"github.com/aryanVijaywargia/herdr-brain/internal/harness"
	"github.com/aryanVijaywargia/herdr-brain/internal/herdr"
	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

// The tests of #249, #250, #251 and #257, from the real-use test on a spare Mac (2026-10-07).

// ps puts ps on the PATH of the fixture, which holds only the fake CLIs.
func (f *doctorFixture) ps(t *testing.T) {
	t.Helper()
	link := filepath.Join(filepath.Dir(f.store), "bin", "ps")
	if _, err := os.Lstat(link); err == nil {
		return
	}
	if err := os.Symlink("/bin/ps", link); err != nil {
		t.Fatal(err)
	}
}

// installedAt sets the time of the last write of the install record: the hook install time
// that harness.StartedBefore compares with the agent start.
func (f *doctorFixture) installedAt(t *testing.T, at time.Time) {
	t.Helper()
	f.ps(t)
	if err := os.Chtimes(filepath.Join(f.data, "install.json"), at, at); err != nil {
		t.Fatal(err)
	}
}

// startedBeforeInstall makes the agent of each pane the test process, and the hook install
// an hour after its start.
func (f *doctorFixture) startedBeforeInstall(t *testing.T, panes ...string) {
	t.Helper()
	f.herdr.procs = map[string][]herdr.Process{}
	for _, id := range panes {
		f.herdr.procs[id] = []herdr.Process{{Name: "claude", PID: os.Getpid()}}
	}
	f.installedAt(t, time.Now().Add(time.Hour))
}

// m2 is the M2 case: claude 2.1.292 against tested 2.1.283, codex 0.160.1 against tested
// 0.154.0, 4 claude sessions that started before the install and one claude session after it
// with a receipt.
func m2(t *testing.T) *doctorFixture {
	t.Helper()
	f := newDoctorFixture(t)
	f.cli(t, "claude", "2.1.292 (Claude Code)")
	f.cli(t, "codex", "codex-cli 0.160.1")
	old := []string{"w2:p1", "w3:p1", "w4:p1", "w5:p1"}
	for i, id := range old {
		f.herdr.panes = append(f.herdr.panes, agent(id, "claude", "s-old-"+string(rune('a'+i))))
	}
	f.herdr.panes = append(f.herdr.panes, agent("w14:p1", "claude", "s-new"))
	receipt(t, f.store, "default", "claude", "w14:p1", "s-new", store.ResultEmitted, "")
	f.startedBeforeInstall(t, old...)
	return f
}

// AC 1, 3 and 6: in the M2 case doctor and doctor --fix pass, so brain update exits 0. They
// show "newer than tested" and the sessions that started before Brain, and no fix needs the
// Brain repo or Go.
func TestDoctorM2(t *testing.T) {
	for _, args := range [][]string{nil, {"--fix"}} {
		t.Run(strings.Join(append([]string{"doctor"}, args...), " "), func(t *testing.T) {
			f := m2(t)
			out, err := f.doctorArgs(t, f.sock, "", args)
			if err != nil {
				t.Fatalf("Doctor(%v) = %v, want nil\n%s", args, err, out)
			}
			for _, want := range []string{
				"note: claude 2.1.292: newer than tested (2.1.283); Brain keeps its hook records and learning on\n",
				"note: codex 0.160.1: newer than tested (0.154.0); Brain keeps its hook records and learning on\n",
				"note: claude: 4 live agent(s) started before Brain installed its hook (w2:p1, w3:p1, w4:p1, w5:p1)",
				"all checks passed\n",
			} {
				if !strings.Contains(out, want) {
					t.Errorf("brain doctor:\n%s\nwant %q", out, want)
				}
			}
			for _, bad := range []string{"retest", "go run", "brain-eval", "brain repo", "not firing"} {
				if strings.Contains(out, bad) {
					t.Errorf("brain doctor:\n%s\nwant no %q", out, bad)
				}
			}
		})
	}
}

// AC 1, 3 and 6: in the M2 case status shows "newer than tested" with the state unchanged, and
// the old sessions as started before Brain, not as unknown.
func TestStatusM2(t *testing.T) {
	f := m2(t)
	out := f.mustStatus(t)
	for _, want := range []string{
		"  claude 2.1.292: configured, unverified; newer than tested (2.1.283)\n",
		"  codex 0.160.1: configured, unverified; newer than tested (0.154.0)\n",
		"  claude: 5 live: 1 emitted, 4 before Brain\n",
		"    w2:p1 before Brain: started before Brain installed its hook; its next session gets Brain items\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("brain status:\n%s\nwant %q", out, want)
		}
	}
	if strings.Contains(out, noReceipt) || strings.Contains(out, "retest") {
		t.Errorf("brain status:\n%s\nwant no %q and no retest", out, noReceipt)
	}
}

// AC 2: a live session that started after the hook entry and has no receipt is still a
// failure, also beside the sessions that started before Brain.
func TestDoctorNoReceiptAfterInstall(t *testing.T) {
	f := m2(t)
	f.herdr.panes = append(f.herdr.panes, agent("w6:p1", "claude", "s-after"))
	f.herdr.procs["w6:p1"] = []herdr.Process{{Name: "claude", PID: os.Getpid()}}
	f.installedAt(t, time.Now().Add(-24*time.Hour))
	out := f.failed(t, "", "claude: hook not trusted or not firing: 5 live agent(s) with no Brain receipt (w2:p1, w3:p1, w4:p1, w5:p1, w6:p1)")
	f.startedBeforeInstall(t, "w2:p1", "w3:p1", "w4:p1", "w5:p1")
	out += f.failed(t, "", "claude: hook not trusted or not firing: 1 live agent(s) with no Brain receipt (w6:p1)",
		"note: claude: 4 live agent(s) started before Brain installed its hook")
	if strings.Contains(out, "go run") {
		t.Errorf("brain doctor:\n%s\nwant no go run", out)
	}
}

// AC 1 (S14): a hook input that the profile cannot parse fails doctor, with a fix that needs
// no repo; a marker of an older profile, as after brain update, passes.
func TestDoctorParseFailure(t *testing.T) {
	f := newDoctorFixture(t)
	p, err := adapter.LoadFS(testProfiles, "claude")
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(f.data, retestDir)
	if err := harness.MarkRetest(dir, "claude", p.SHA256, testNow); err != nil {
		t.Fatal(err)
	}
	out := f.failed(t, "", "claude 2.1.283: the Brain profile could not parse a claude hook input, last at ",
		"herdr capture continues; fix: brain update")
	if strings.Contains(out, "go run") || strings.Contains(out, "brain repo") {
		t.Errorf("brain doctor:\n%s\nwant no repo step", out)
	}
	if !strings.Contains(f.mustStatus(t), "  claude 2.1.283: retest: the profile could not parse a hook input (brain doctor)\n") {
		t.Errorf("brain status does not show the retest of claude")
	}
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if err := harness.MarkRetest(dir, "claude", "older-profile", testNow); err != nil {
		t.Fatal(err)
	}
	if out, err := f.doctor(t, f.sock, ""); err != nil {
		t.Errorf("Doctor() with a marker of an older profile = %v\n%s", err, out)
	}
}

// AC 4 (#250): a CLI that is not installed shows as "not installed", never retest or newer
// than tested, also with a retest marker; doctor does not fail on it.
func TestStatusNotInstalled(t *testing.T) {
	f := newDoctorFixture(t)
	if err := os.Remove(filepath.Join(filepath.Dir(f.store), "bin", "codex")); err != nil {
		t.Fatal(err)
	}
	p, err := adapter.LoadFS(testProfiles, "codex")
	if err != nil {
		t.Fatal(err)
	}
	if err := harness.MarkRetest(filepath.Join(f.data, retestDir), "codex", p.SHA256, testNow); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	e := f.env(f.sock, "w14:p1", "w14", &out)
	e.Data = f.data
	e.Adapters = harness.Files{Install: filepath.Join(f.data, "install.json"),
		Conformance: filepath.Join(f.data, "conformance.json"), Retest: filepath.Join(f.data, retestDir)}
	e.StatusHerdr = func(herdr.Target) (StatusHerdr, error) { return f.herdr, nil }
	e.Now = func() time.Time { return testNow.Add(3 * time.Hour) }
	e.Profiles = fakeProfiles(t) // the fake CLI is not on the PATH, as agy on the M2 Mac
	if err := Status(context.Background(), e, nil); err != nil {
		t.Fatalf("Status() = %v", err)
	}
	for _, want := range []string{"  codex: not installed\n", "  fake: not installed\n"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("brain status:\n%s\nwant %q", out.String(), want)
		}
	}
	if s := out.String(); strings.Contains(s, "retest") || strings.Contains(s, "than tested") {
		t.Errorf("brain status:\n%s\nwant no retest and no version note", s)
	}
	if out, err := f.doctor(t, f.sock, ""); err != nil || strings.Contains(out, "codex") {
		t.Errorf("Doctor() = %v\n%s\nwant a pass with no codex line", err, out)
	}
}

// AC 5 (#257): a codex pane where herdr shows no agent session id names the likely cause and
// the fix in one line.
func TestStatusCodexNoSessionID(t *testing.T) {
	f := newDoctorFixture(t)
	f.herdr.panes = []herdr.Pane{agent("w1N:p1", "codex", ""), agent("w14:p2", "claude", "")}
	out := f.mustStatus(t)
	for _, want := range []string{
		"    w1N:p1 unknown: herdr shows no agent session id; herdr's codex hook may be off (turn it on in codex /hooks), " +
			"or no prompt was sent yet (the id comes after the first prompt)\n",
		"    w14:p2 unknown: herdr shows no agent session id\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("brain status:\n%s\nwant %q", out, want)
		}
	}
}

// AC 1: a CLI version that cannot be read is a note, not a failed check (D13).
func TestDoctorVersionUnknown(t *testing.T) {
	f := newDoctorFixture(t)
	f.cli(t, "claude", "Claude Code")
	out, err := f.doctor(t, f.sock, "")
	if err != nil || !strings.Contains(out, "note: claude: version unknown (claude --version: no version in") {
		t.Errorf("Doctor() = %v\n%s\nwant a pass with a version unknown note", err, out)
	}
}
