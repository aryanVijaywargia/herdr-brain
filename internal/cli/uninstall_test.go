package cli

import (
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/aryanVijaywargia/herdr-brain/internal/adapter"
	"github.com/aryanVijaywargia/herdr-brain/internal/install"
	"github.com/aryanVijaywargia/herdr-brain/internal/paths"
)

// userHome is a temp HOME with the Claude and Codex hook files and files of other tools. HOME
// points there, so no test reaches the real home.
func userHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", "")
	files := map[string]string{
		".claude/settings.json":                     string(readFile(t, "../install/testdata/claude-settings.json")),
		".codex/hooks.json":                         string(readFile(t, "../install/testdata/codex-hooks.json")),
		".local/bin/other-tool":                     "#!/bin/sh\n",
		".local/share/other/data.db":                "other data",
		".local/state/herdr/plugins/other/log":      "other state",
		".config/herdr/config.toml":                 "theme = \"dark\"\n",
		".config/herdr/plugins/config/other/a.toml": "x = 1\n",
	}
	for rel, data := range files {
		putFile(t, filepath.Join(home, rel), data)
	}
	return home
}

// installBrain adds every Brain file to home: the hook entries of Claude and Codex with their
// install record, the ~/.local/bin/brain link to this binary, the bindings file with a main
// and a test binding, the stores, and herdr's plugin config and state folders.
func installBrain(t *testing.T, home string) {
	t.Helper()
	record, err := paths.InstallFile()
	if err != nil {
		t.Fatal(err)
	}
	in := &install.Installer{Home: home, Record: record, Binary: "/opt/brain/bin/brain"}
	for _, harness := range []string{"claude", "codex"} {
		p, err := adapter.Load(harness)
		if err != nil {
			t.Fatal(err)
		}
		if st, err := in.Install(p); err != nil || st != install.Added {
			t.Fatalf("Install(%s) = %q, %v", harness, st, err)
		}
	}
	bin, err := paths.BinaryPath()
	if err != nil {
		t.Fatal(err)
	}
	link, err := paths.LinkPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(bin, link); err != nil {
		t.Fatal(err)
	}
	data, err := paths.DataHome()
	if err != nil {
		t.Fatal(err)
	}
	putFile(t, filepath.Join(data, "bindings.json"), fmt.Sprintf(`{"bindings":[`+
		`{"session":"default","socket":"/tmp/main.sock","store":%q,"kind":"main","created":"2026-09-28T12:00:00Z"},`+
		`{"session":"brain-test","socket":"/tmp/test.sock","store":%q,"kind":"test","created":"2026-09-28T12:00:00Z"}]}`,
		filepath.Join(data, "main"), filepath.Join(data, "test")))
	putFile(t, filepath.Join(data, "main", "brain.db"), "main store")
	putFile(t, filepath.Join(data, "test", "brain.db"), "test store")
	config, state, err := paths.PluginDirs()
	if err != nil {
		t.Fatal(err)
	}
	putFile(t, filepath.Join(config, "config.toml"), "x = 1\n")
	putFile(t, filepath.Join(state, "state.json"), "{}\n")
}

func putFile(t *testing.T, path, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0o640); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// tree returns every folder, file and link under home, with the mode and bytes of each file
// and the target of each link.
func tree(t *testing.T, home string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(home, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(home, path)
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case d.IsDir():
			out[rel] = "dir " + info.Mode().String()
		case d.Type()&fs.ModeSymlink != 0:
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			out[rel] = "link " + target
		default:
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			out[rel] = "file " + info.Mode().String() + " " + string(b)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func diffTrees(t *testing.T, got, want map[string]string) {
	t.Helper()
	for _, k := range slices.Sorted(maps.Keys(got)) {
		if w, ok := want[k]; !ok {
			t.Errorf("left behind: %s (%.60s)", k, got[k])
		} else if got[k] != w {
			t.Errorf("changed: %s\ngot  %s\nwant %s", k, got[k], w)
		}
	}
	for _, k := range slices.Sorted(maps.Keys(want)) {
		if _, ok := got[k]; !ok {
			t.Errorf("removed: %s", k)
		}
	}
}

// runUninstall runs `brain uninstall args` from a pane on the herdr socket socket.
func runUninstall(t *testing.T, socket string, args ...string) string {
	t.Helper()
	file, err := paths.BindingsFile()
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	e := Env{
		Environ:  []string{"HERDR_ENV=1", "HERDR_SOCKET_PATH=" + socket, "HERDR_PANE_ID=w1:p1"},
		Bindings: file,
		Stdout:   &out,
		Stderr:   &bytes.Buffer{},
	}
	if err := Uninstall(context.Background(), e, args); err != nil {
		t.Fatalf("Uninstall(%q) = %v\n%s", args, err, out.String())
	}
	return out.String()
}

// AC: install, then `uninstall --apply --data` leaves no Brain file and no Brain entry, and
// every other byte is unchanged.
func TestUninstallApplyData(t *testing.T) {
	home := userHome(t)
	before := tree(t, home)
	installBrain(t, home)

	out := runUninstall(t, "/tmp/main.sock", "--apply", "--data")
	diffTrees(t, tree(t, home), before)
	for _, want := range []string{
		"removed the Brain hook entry for claude in " + filepath.Join(home, ".claude", "settings.json"),
		"removed the Brain hook entry for codex in " + filepath.Join(home, ".codex", "hooks.json"),
		"removed " + filepath.Join(home, ".local", "bin", "brain"),
		"removed " + filepath.Join(home, ".local", "share", "brain"),
		"Next: herdr plugin uninstall brain",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}

	if out := runUninstall(t, "/tmp/main.sock", "--apply", "--data"); out != "Brain has nothing to remove.\n" {
		t.Errorf("second uninstall = %q, want nothing to remove", out)
	}
}

// Without --data the stores stay, and only the files outside them go.
func TestUninstallApplyKeepsStores(t *testing.T) {
	home := userHome(t)
	installBrain(t, home)
	data, err := paths.DataHome()
	if err != nil {
		t.Fatal(err)
	}

	out := runUninstall(t, "/tmp/main.sock", "--apply")
	for _, gone := range []string{"install.json", "install.json.lock", "bindings.json"} {
		if _, err := os.Lstat(filepath.Join(data, gone)); !os.IsNotExist(err) {
			t.Errorf("%s: Lstat = %v, want it removed", gone, err)
		}
	}
	for _, kept := range []string{
		filepath.Join(data, "main", "brain.db"),
		filepath.Join(data, "test", "brain.db"),
		filepath.Join(home, ".config", "herdr", "plugins", "config", "brain", "config.toml"),
		filepath.Join(home, ".local", "state", "herdr", "plugins", "brain", "state.json"),
	} {
		if _, err := os.Stat(kept); err != nil {
			t.Errorf("%s: %v, want it kept", kept, err)
		}
	}
	if !strings.Contains(out, "The stores stay in "+data) {
		t.Errorf("output lacks the stores line:\n%s", out)
	}
}

// AC: a dry run changes nothing.
func TestUninstallDryRun(t *testing.T) {
	home := userHome(t)
	installBrain(t, home)
	before := tree(t, home)

	for _, args := range [][]string{nil, {"--data"}} {
		out := runUninstall(t, "/tmp/main.sock", args...)
		diffTrees(t, tree(t, home), before)
		for _, want := range []string{
			"would remove the Brain hook entry for claude in ",
			"would remove the Brain hook entry for codex in ",
			"would remove " + filepath.Join(home, ".local", "share", "brain", "install.json"),
			"Dry run: nothing changed.",
		} {
			if !strings.Contains(out, want) {
				t.Errorf("dry run %q: output lacks %q:\n%s", args, want, out)
			}
		}
	}
}

// AC: an uninstall from a test binding and from the main binding find the same install record.
func TestUninstallSameRecordFromEachBinding(t *testing.T) {
	home := userHome(t)
	installBrain(t, home)

	main := runUninstall(t, "/tmp/main.sock")
	test := runUninstall(t, "/tmp/test.sock")
	if main != test {
		t.Errorf("from the main binding:\n%s\nfrom the test binding:\n%s", main, test)
	}
	if !strings.Contains(main, "the Brain hook entry for claude in "+filepath.Join(home, ".claude", "settings.json")) {
		t.Errorf("output lacks the recorded Claude entry:\n%s", main)
	}
}

// A hook entry that you edited stays, and uninstall says so (§8.1).
func TestUninstallKeepsEditedEntry(t *testing.T) {
	home := userHome(t)
	installBrain(t, home)
	hooks := filepath.Join(home, ".codex", "hooks.json")
	edited := strings.Replace(string(readFile(t, hooks)), `"timeout":5`, `"timeout":9`, 1)
	putFile(t, hooks, edited)

	out := runUninstall(t, "/tmp/main.sock", "--apply")
	if got := string(readFile(t, hooks)); got != edited {
		t.Errorf("edited hook file changed:\n%s", got)
	}
	if !strings.Contains(out, "keep the Brain hook entry for codex in "+hooks) {
		t.Errorf("output lacks the kept entry:\n%s", out)
	}
}

func TestUninstallArgs(t *testing.T) {
	userHome(t)
	for _, args := range [][]string{{"--nope"}, {"x"}} {
		err := Uninstall(context.Background(), Env{Stdout: &bytes.Buffer{}}, args)
		if err == nil || !strings.Contains(err.Error(), uninstallUsage) {
			t.Errorf("Uninstall(%q) = %v, want the usage", args, err)
		}
	}
}
