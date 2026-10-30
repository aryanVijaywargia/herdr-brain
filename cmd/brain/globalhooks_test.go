package main

import (
	"bufio"
	"bytes"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aryanVijaywargia/herdr-brain/internal/adapter"
	"github.com/aryanVijaywargia/herdr-brain/internal/install"
)

// cliHookFiles are the hook files of Claude and Codex in a test home.
var cliHookFiles = []string{".claude/settings.json", ".codex/hooks.json"}

// globalHome adds to home every global Brain entry of Claude and Codex, the start entry
// and the hook record entries, as an older Brain setup added them. It returns the home and the
// bytes of the hook files before that install. home is a setupHome or an updateHome.
func globalHome(t *testing.T, home string) (string, map[string][]byte) {
	t.Helper()
	orig := map[string][]byte{}
	for _, f := range cliHookFiles {
		orig[f] = mustRead(t, filepath.Join(home, f))
	}
	inst := testInstaller(t)
	for _, kind := range []string{"claude", "codex"} {
		p, err := adapter.Load(kind)
		if err != nil {
			t.Fatal(err)
		}
		for _, h := range p.Hooks() {
			if _, err := inst.InstallHook(p, h); err != nil {
				t.Fatal(err)
			}
		}
	}
	return home, orig
}

// question is the global hooks question of kind.
func question(kind string) string {
	return "Remove the global Brain hooks for " + kind + "? A plain " + kind + " session then gets pane-screen capture only. [y/N] "
}

// wantRemoved checks that the hook files of Claude and Codex are back to orig, byte for byte,
// and that the install record holds no entry of them.
func wantRemoved(t *testing.T, home string, orig map[string][]byte) {
	t.Helper()
	for _, f := range cliHookFiles {
		if got := mustRead(t, filepath.Join(home, f)); !bytes.Equal(got, orig[f]) {
			t.Errorf("%s after the removal:\n%s\nwant the file before the install:\n%s", f, got, orig[f])
		}
	}
	rec, err := install.Read(testInstaller(t).Record)
	if err != nil || len(rec.Entries) != 0 || len(rec.Records) != 0 {
		t.Errorf("install record = %+v, %v; want no entry", rec, err)
	}
}

// E24-S8 AC: an existing install. brain setup asks once for each CLI with launch data whether
// Brain may remove its global hooks; a yes removes each unchanged Brain entry through the install
// record after the yes to the plan, and a no keeps them. No answer, as in a run with no terminal,
// changes nothing.
func TestSetupGlobalHooks(t *testing.T) {
	t.Run("yes", func(t *testing.T) {
		home, orig := globalHome(t, setupHome(t, "0.9.1"))
		// No extractor, no history scan, yes for claude and codex, then yes to the plan.
		code, out, errOut := runSetup(t, "\nn\ny\ny\ny\n")
		if code != 0 {
			t.Fatalf("setup = %d: %s\n%s", code, errOut, out)
		}
		for _, want := range []string{"Step 4: global hooks\n", question("claude"), question("codex"),
			"- " + filepath.Join(home, ".claude", "settings.json") + " holds the global Brain hooks for claude. They run in each claude session, also outside herdr.\n",
			"- brain codex, or the herdr action \"Brain: new Codex pane\", gives Brain to one session with no global hook.\n",
			"- Global hooks: remove the unchanged Brain hook entries for claude from ",
			"Removed the global Brain hooks for claude from " + filepath.Join(home, ".claude", "settings.json") + ".\n",
			"Removed the global Brain hooks for codex from " + filepath.Join(home, ".codex", "hooks.json") + ".\n"} {
			if !strings.Contains(out, want) {
				t.Errorf("output does not contain %q:\n%s", want, out)
			}
		}
		if strings.Contains(out, "trust the Codex hook") {
			t.Errorf("setup shows the Codex trust steps after the removal:\n%s", out)
		}
		wantRemoved(t, home, orig)
		// Nothing is left to ask.
		if code, out, _ := runSetup(t, ""); code != 0 || strings.Contains(out, "global hooks") {
			t.Errorf("second setup = %d:\n%s\nwant no global hooks step", code, out)
		}
	})
	t.Run("no", func(t *testing.T) {
		home, _ := globalHome(t, setupHome(t, "0.9.1"))
		files := map[string][]byte{}
		for _, f := range cliHookFiles {
			files[f] = mustRead(t, filepath.Join(home, f))
		}
		code, out, errOut := runSetup(t, "\nn\nn\nn\ny\n")
		if code != 0 {
			t.Fatalf("setup = %d: %s\n%s", code, errOut, out)
		}
		for _, f := range cliHookFiles {
			if got := mustRead(t, filepath.Join(home, f)); !bytes.Equal(got, files[f]) {
				t.Errorf("setup changed %s after the answer no:\n%s", f, got)
			}
		}
		for _, want := range []string{"- Global hooks: keep the Brain hooks for claude in ", "The next brain setup asks again.",
			"trust the Codex hook"} {
			if !strings.Contains(out, want) {
				t.Errorf("output does not contain %q:\n%s", want, out)
			}
		}
	})
	t.Run("yes, then no to the plan", func(t *testing.T) {
		home, _ := globalHome(t, setupHome(t, "0.9.1"))
		before := snapshot(t, home)
		if code, out, errOut := runSetup(t, "\nn\ny\ny\nn\n"); code != 0 || !strings.Contains(out, "Setup wrote nothing.") {
			t.Fatalf("setup = %d: %s\n%s", code, errOut, out)
		}
		if after := snapshot(t, home); !maps.Equal(before, after) {
			t.Error("setup changed the home after the answer no to the plan")
		}
	})
	t.Run("no answer", func(t *testing.T) {
		home, _ := globalHome(t, setupHome(t, "0.9.1"))
		before := snapshot(t, home)
		if code, out, errOut := runSetup(t, ""); code != 0 || !strings.Contains(out, question("claude")) {
			t.Fatalf("setup = %d: %s\n%s", code, errOut, out)
		}
		if after := snapshot(t, home); !maps.Equal(before, after) {
			t.Error("setup with no answer changed the home")
		}
	})
}

// E24-S8 AC: a Brain entry that you edited stays after the yes, and setup says so, in the
// question and after the removal. The unchanged entries of the same file go.
func TestSetupGlobalHooksEdited(t *testing.T) {
	home, _ := globalHome(t, setupHome(t, "0.9.1"))
	file := filepath.Join(home, ".claude", "settings.json")
	edited := bytes.Replace(mustRead(t, file), []byte("--event start"), []byte("--event start --mine"), 1)
	if err := os.WriteFile(file, edited, 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, errOut := runSetup(t, "\nn\ny\ny\ny\n")
	if code != 0 {
		t.Fatalf("setup = %d: %s\n%s", code, errOut, out)
	}
	for _, want := range []string{
		"- " + file + " holds a Brain hook entry for claude that you edited. Brain keeps it.\n" + question("claude"),
		file + " holds the Brain SessionStart entry for claude that you edited. Brain keeps it: remove it by hand.\n",
		"Removed the global Brain hooks for claude from " + file + ".\n"} {
		if !strings.Contains(out, want) {
			t.Errorf("output does not contain %q:\n%s", want, out)
		}
	}
	got := string(mustRead(t, file))
	if !strings.Contains(got, "--event start --mine") || strings.Contains(got, "--event prompt") || strings.Contains(got, "--event stop") {
		t.Errorf("%s after the removal:\n%s\nwant the edited start entry only", file, got)
	}
	rec, err := install.Read(testInstaller(t).Record)
	if err != nil || len(rec.Entries) != 1 || rec.Entries["claude"].Target != file || len(rec.Records) != 0 {
		t.Errorf("install record = %+v, %v; want the edited claude start entry only", rec, err)
	}
}

// E24-S8 AC: brain update asks the same question for each CLI. A no keeps the hooks, a yes
// removes them through the install record.
func TestUpdateGlobalHooks(t *testing.T) {
	home, orig := globalHome(t, updateHome(t))
	before := snapshot(t, home)
	code, out, errOut := runUpdate(t, "n\nn\n")
	if code != 0 {
		t.Fatalf("update (no) = %d: %s\n%s", code, errOut, out)
	}
	for _, want := range []string{question("claude"), question("codex"),
		"brain update keeps the global Brain hooks for claude. To remove them later, run brain update again.\n",
		"Hook records: no hook record entry to add.\n"} {
		if !strings.Contains(out, want) {
			t.Errorf("update (no):\n%s\nwant %q", out, want)
		}
	}
	if after := snapshot(t, home); !maps.Equal(before, after) {
		t.Errorf("update (no) changed the home:\n%s", out)
	}

	code, out, errOut = runUpdate(t, "y\ny\n")
	if code != 0 {
		t.Fatalf("update (yes) = %d: %s\n%s", code, errOut, out)
	}
	if !strings.Contains(out, "Removed the global Brain hooks for codex from "+filepath.Join(home, ".codex", "hooks.json")+".\n") {
		t.Errorf("update (yes):\n%s\nwant the removal", out)
	}
	wantRemoved(t, home, orig)
}

// Review of #448, finding 1: the question and the messages name the hook files of the install
// record, the files that the removal changes, also when the CLI now reads another file.
func TestGlobalHooksRecordedTarget(t *testing.T) {
	home, orig := globalHome(t, updateHome(t))
	inst := testInstaller(t)
	inst.Home = t.TempDir() // the record comes from another home
	p, err := adapter.Load("claude")
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := updateGlobalHooks(inst, []*adapter.Profile{p}, bufio.NewReader(strings.NewReader("y\n")), &out); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(home, ".claude", "settings.json")
	if !bytes.Equal(mustRead(t, file), orig[".claude/settings.json"]) {
		t.Errorf("%s still holds Brain entries:\n%s", file, mustRead(t, file))
	}
	for _, want := range []string{"- " + file + " holds the global Brain hooks for claude.",
		"Removed the global Brain hooks for claude from " + file + ".\n"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output:\n%s\nwant %q", out.String(), want)
		}
	}
	if strings.Contains(out.String(), inst.Home) {
		t.Errorf("output names the hook file of the new home, which the removal does not change:\n%s", out.String())
	}
}

// Review of #448, finding 2 (a known limit): when Brain cannot save the install record after it
// removed entries from a hook file, the error says that the hook file changed and the install
// record did not.
func TestGlobalHooksRecordWriteFailure(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores file modes")
	}
	home, _ := globalHome(t, updateHome(t))
	inst := testInstaller(t)
	ledger := mustRead(t, inst.Record)
	p, err := adapter.Load("claude")
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(inst.Record)
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(dir, 0o700); err != nil {
			t.Error(err)
		}
	})
	var out bytes.Buffer
	err = updateGlobalHooks(inst, []*adapter.Profile{p}, bufio.NewReader(strings.NewReader("y\n")), &out)
	file := filepath.Join(home, ".claude", "settings.json")
	want := "removed Brain entries from " + file + ", but " + inst.Record + " did not change and still lists them: "
	if err == nil || !strings.Contains(err.Error(), want) || !errors.Is(err, os.ErrPermission) {
		t.Errorf("updateGlobalHooks() = %v, want %q and a permission error", err, want)
	}
	if !bytes.Equal(ledger, mustRead(t, inst.Record)) {
		t.Error("the install record changed")
	}
}

// Review of #448, finding 3: an entry whose fixed arguments you edited is no Brain entry for
// install, so the removal cannot find it. Brain keeps its record and says so; the other
// entries go.
func TestGlobalHooksEditedIdentity(t *testing.T) {
	home, _ := globalHome(t, updateHome(t))
	file := filepath.Join(home, ".claude", "settings.json")
	b := bytes.ReplaceAll(mustRead(t, file), []byte("--profile claude --event start"), []byte("--profile renamed --event start"))
	if err := os.WriteFile(file, b, 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, errOut := runUpdate(t, "y\nn\n", "--records")
	if code != 0 {
		t.Fatalf("update = %d: %s\n%s", code, errOut, out)
	}
	want := "Brain could not find its SessionStart entry for claude in " + file +
		". If you edited it, remove it by hand. Brain keeps it in its install record.\n"
	if !strings.Contains(out, want) || !strings.Contains(out, "Removed the global Brain hooks for claude from "+file+".\n") {
		t.Errorf("update:\n%s\nwant %q and the removal of the others", out, want)
	}
	got := string(mustRead(t, file))
	if !strings.Contains(got, "--profile renamed --event start") || strings.Contains(got, "--profile claude") {
		t.Errorf("%s:\n%s\nwant the renamed entry only", file, got)
	}
	rec, err := install.Read(testInstaller(t).Record)
	if err != nil || rec.Entries["claude"].Target != file || len(rec.Records) != len(mustProfile(t, "codex").Hooks())-1 {
		t.Errorf("install record = %+v, %v; want the claude start entry and the codex entries", rec, err)
	}
}

func mustProfile(t *testing.T, kind string) *adapter.Profile {
	t.Helper()
	p, err := adapter.Load(kind)
	if err != nil {
		t.Fatal(err)
	}
	return p
}
