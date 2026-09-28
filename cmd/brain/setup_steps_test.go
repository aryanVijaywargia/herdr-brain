package main

import (
	"bytes"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aryanVijaywargia/herdr-brain/internal/bindings"
)

// fakeHerdr answers `session list --json` and `api snapshot` from files in its folder.
const fakeHerdr = `#!/bin/sh
case "$*" in
"session list --json") /bin/cat "$FAKE_HERDR_DIR/sessions.json" ;;
"api snapshot") /bin/cat "$FAKE_HERDR_DIR/snapshot.json" ;;
*) echo "fake herdr: unexpected: $*" >&2; exit 1 ;;
esac
`

// setupHome makes a temp HOME with the Claude and Codex hook files of other tools, a fake
// herdr whose server runs version, and fake claude and codex CLIs on PATH. No test reaches
// the real home or herdr.
func setupHome(t *testing.T, version string) string {
	t.Helper()
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", "")
	for _, k := range []string{"HERDR_ENV", "HERDR_SESSION", "HERDR_SOCKET_PATH", "HERDR_PANE_ID",
		"HERDR_WORKSPACE_ID", "HERDR_PLUGIN_ENTRYPOINT_ID"} {
		t.Setenv(k, "")
	}
	herdrDir := filepath.Join(home, "fake-herdr")
	var sessions []string
	for _, s := range []string{"default", "brain-test", "brain-test2"} {
		dir := filepath.Join(home, ".config", "herdr", "sessions", s)
		sessions = append(sessions, fmt.Sprintf(`{"default":%t,"name":%q,"running":true,"session_dir":%q,"socket_path":%q}`,
			s == "default", s, dir, filepath.Join(dir, "herdr.sock")))
	}
	files := map[string]string{
		"fake-herdr/herdr":         fakeHerdr,
		"fake-herdr/sessions.json": `{"sessions":[` + strings.Join(sessions, ",") + `]}`,
		"fake-herdr/snapshot.json": `{"id":"cli:api:snapshot","result":{"type":"session_snapshot","snapshot":{"version":"` + version + `","protocol":1}}}`,
		"path/claude":              "#!/bin/sh\n",
		"path/codex":               "#!/bin/sh\n",
		".claude/settings.json":    string(mustRead(t, "../../internal/install/testdata/claude-settings.json")),
		".codex/hooks.json":        string(mustRead(t, "../../internal/install/testdata/codex-hooks.json")),
	}
	for rel, data := range files {
		p := filepath.Join(home, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(data), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HERDR_BIN_PATH", filepath.Join(herdrDir, "herdr"))
	t.Setenv("FAKE_HERDR_DIR", herdrDir)
	t.Setenv("PATH", filepath.Join(home, "path"))
	return home
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// runSetup runs `brain setup args...` with answers on stdin.
func runSetup(t *testing.T, answers string, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code = run(append([]string{"setup"}, args...), strings.NewReader(answers), &out, &errOut)
	return code, out.String(), errOut.String()
}

// snapshot returns every folder, file and link under home, with the file bytes and link
// targets.
func snapshot(t *testing.T, home string) map[string]string {
	t.Helper()
	m := map[string]string{}
	err := filepath.WalkDir(home, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(home, p)
		if err != nil {
			return err
		}
		switch {
		case d.Type()&fs.ModeSymlink != 0:
			target, err := os.Readlink(p)
			m[rel] = "link " + target
			return err
		case d.IsDir():
			m[rel] = "dir"
		default:
			b, err := os.ReadFile(p)
			m[rel] = "file " + string(b)
			return err
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func readBindings(t *testing.T, home string) []bindings.Binding {
	t.Helper()
	bs, dropped, err := bindings.Read(filepath.Join(home, ".local", "share", "brain", "bindings.json"))
	if err != nil || len(dropped) != 0 {
		t.Fatalf("bindings.Read: %v, dropped %v", err, dropped)
	}
	return bs
}

// Setup twice gives the same files and one binding; the second run asks nothing.
func TestSetupTwice(t *testing.T) {
	home := setupHome(t, "0.9.1")
	code, out, errOut := runSetup(t, "y\ny\ny\ny\n")
	if code != 0 {
		t.Fatalf("setup = %d: %s\n%s", code, errOut, out)
	}
	bs := readBindings(t, home)
	mainStore := filepath.Join(home, ".local", "share", "brain", "main")
	if len(bs) != 1 || bs[0].Session != "default" || bs[0].Kind != bindings.KindMain || bs[0].Store != mainStore ||
		bs[0].Socket != filepath.Join(home, ".config", "herdr", "sessions", "default", "herdr.sock") {
		t.Errorf("bindings = %+v", bs)
	}
	for _, f := range []string{".claude/settings.json", ".codex/hooks.json"} {
		if b := mustRead(t, filepath.Join(home, f)); !bytes.Contains(b, []byte(" hook --profile ")) {
			t.Errorf("%s has no Brain hook entry:\n%s", f, b)
		}
	}
	for _, want := range []string{"herdr 0.9.1: OK", "brain uninstall --apply", "Press t to trust it", "First use:", "brain status"} {
		if !strings.Contains(out, want) {
			t.Errorf("output does not contain %q:\n%s", want, out)
		}
	}
	first := snapshot(t, home)

	code, out, errOut = runSetup(t, "")
	if code != 0 {
		t.Fatalf("second setup = %d: %s\n%s", code, errOut, out)
	}
	if strings.Contains(out, "[y/N]") {
		t.Errorf("the second setup asked a question:\n%s", out)
	}
	if second := snapshot(t, home); !maps.Equal(first, second) {
		t.Errorf("the second setup changed files:\nbefore %v\nafter  %v", first, second)
	}
	if bs := readBindings(t, home); len(bs) != 1 {
		t.Errorf("bindings after the second setup = %+v", bs)
	}
}

// "No" at each step leaves that step's files unchanged; the other steps still run.
func TestSetupNoAtEachStep(t *testing.T) {
	steps := []struct {
		name  string
		files []string
	}{
		{"binding", []string{".local/share/brain/bindings.json", ".local/share/brain/main"}},
		{"link", []string{".local/bin"}},
		{"claude", []string{".claude"}},
		{"codex", []string{".codex"}},
	}
	for i, step := range steps {
		t.Run(step.name, func(t *testing.T) {
			home := setupHome(t, "0.9.1")
			answers := strings.Repeat("y\n", len(steps))
			answers = answers[:2*i] + "n" + answers[2*i+1:]
			before := snapshot(t, home)
			code, out, errOut := runSetup(t, answers)
			if code != 0 {
				t.Fatalf("setup = %d: %s\n%s", code, errOut, out)
			}
			after := snapshot(t, home)
			changed := false
			for _, m := range []map[string]string{before, after} {
				for rel := range m {
					if !under(rel, step.files) {
						changed = changed || before[rel] != after[rel]
						continue
					}
					if before[rel] != after[rel] {
						t.Errorf("%s changed after the answer no: %q -> %q", rel, before[rel], after[rel])
					}
				}
			}
			if !changed {
				t.Error("the other steps changed nothing")
			}
		})
	}

	home := setupHome(t, "0.9.1")
	before := snapshot(t, home)
	if code, out, errOut := runSetup(t, "n\nn\nn\nn\n"); code != 0 {
		t.Fatalf("setup = %d: %s\n%s", code, errOut, out)
	}
	if after := snapshot(t, home); !maps.Equal(before, after) {
		t.Errorf("no at every step changed files:\nbefore %v\nafter  %v", before, after)
	}
}

func under(rel string, prefixes []string) bool {
	for _, p := range prefixes {
		if rel == filepath.FromSlash(p) || strings.HasPrefix(rel, filepath.FromSlash(p)+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// S81: herdr older than 0.9.1. Setup refuses with the version and the fix, writes nothing,
// and the hook prints nothing.
func TestSetupS81(t *testing.T) {
	home := setupHome(t, "0.8.2")
	before := snapshot(t, home)
	code, out, errOut := runSetup(t, "y\ny\ny\ny\n")
	if code != 1 {
		t.Fatalf("setup = %d, want 1: %s", code, out)
	}
	for _, want := range []string{"herdr 0.8.2 is too old", "0.9.1 or later", "herdr update"} {
		if !strings.Contains(errOut, want) {
			t.Errorf("stderr does not contain %q: %q", want, errOut)
		}
	}
	if after := snapshot(t, home); !maps.Equal(before, after) {
		t.Errorf("setup wrote files:\nbefore %v\nafter  %v", before, after)
	}

	t.Setenv("HERDR_ENV", "1")
	t.Setenv("HERDR_SOCKET_PATH", filepath.Join(home, ".config", "herdr", "sessions", "default", "herdr.sock"))
	t.Setenv("HERDR_PANE_ID", "w1:p1")
	t.Setenv("HERDR_WORKSPACE_ID", "w1")
	var hookOut, hookErr bytes.Buffer
	input := `{"session_id":"s1","cwd":"/tmp","hook_event_name":"SessionStart","source":"startup"}`
	if code := run([]string{"hook", "--profile", "claude", "--event", "start"}, strings.NewReader(input), &hookOut, &hookErr); code != 0 || hookOut.Len() != 0 {
		t.Errorf("hook = %d, stdout %q, want 0 and nothing", code, hookOut.String())
	}
}

// A test binding gets its own store folder, beside the main binding.
func TestSetupTestBinding(t *testing.T) {
	home := setupHome(t, "0.9.1")
	if code, out, errOut := runSetup(t, "y\nn\nn\nn\n"); code != 0 {
		t.Fatalf("setup = %d: %s\n%s", code, errOut, out)
	}
	store := filepath.Join(home, "eval", "store")
	if code, out, errOut := runSetup(t, "y\nn\nn\nn\n", "--session", "brain-test", "--test", store); code != 0 {
		t.Fatalf("setup --test = %d: %s\n%s", code, errOut, out)
	}
	bs := readBindings(t, home)
	if len(bs) != 2 || bs[1].Session != "brain-test" || bs[1].Kind != bindings.KindTest || bs[1].Store != store {
		t.Errorf("bindings = %+v", bs)
	}
	if fi, err := os.Stat(store); err != nil || !fi.IsDir() {
		t.Errorf("test store folder: %v", err)
	}
}

// S76: setup refuses a test store that is, holds or sits inside the main store or another test
// store, after links, and a real session on a test store. The bindings file stays as it was.
func TestSetupS76(t *testing.T) {
	cases := map[string]struct {
		mainBound bool
		prep      func(t *testing.T, home, main, other string) string // returns the --test folder
		session   string
	}{
		"main store through a link": {mainBound: true, prep: func(t *testing.T, home, main, _ string) string {
			return link(t, main, filepath.Join(home, "evil"))
		}},
		"inside the main store": {mainBound: true, prep: func(_ *testing.T, _, main, _ string) string {
			return filepath.Join(main, "sub")
		}},
		"inside the main store through a link": {mainBound: true, prep: func(t *testing.T, home, main, _ string) string {
			return filepath.Join(link(t, main, filepath.Join(home, "evil")), "sub")
		}},
		"holds the main store": {mainBound: true, prep: func(_ *testing.T, home, _, _ string) string {
			return filepath.Join(home, ".local")
		}},
		"main store, not bound yet": {prep: func(t *testing.T, home, main, _ string) string {
			if err := os.MkdirAll(main, 0o700); err != nil {
				t.Fatal(err)
			}
			return link(t, main, filepath.Join(home, "evil"))
		}},
		"another test store through a link": {prep: func(t *testing.T, home, _, other string) string {
			return link(t, other, filepath.Join(home, "evil"))
		}},
		"inside another test store": {prep: func(_ *testing.T, _, _, other string) string {
			return filepath.Join(other, "sub")
		}},
		"real session": {session: "default", mainBound: true, prep: func(_ *testing.T, home, _, _ string) string {
			return filepath.Join(home, "eval")
		}},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			home := setupHome(t, "0.9.1")
			main := filepath.Join(home, ".local", "share", "brain", "main")
			if c.mainBound {
				if code, out, errOut := runSetup(t, "y\nn\nn\nn\n"); code != 0 {
					t.Fatalf("setup = %d: %s\n%s", code, errOut, out)
				}
			}
			other := filepath.Join(home, "other-test")
			if code, out, errOut := runSetup(t, "y\nn\nn\nn\n", "--session", "brain-test2", "--test", other); code != 0 {
				t.Fatalf("setup --test = %d: %s\n%s", code, errOut, out)
			}
			folder := c.prep(t, home, main, other)
			file := filepath.Join(home, ".local", "share", "brain", "bindings.json")
			before := mustRead(t, file)
			session := cmpOr(c.session, "brain-test")
			code, out, errOut := runSetup(t, "y\ny\ny\ny\n", "--session", session, "--test", folder)
			if code != 1 || !strings.Contains(errOut, "refused") {
				t.Errorf("setup = %d, stderr %q, want 1 and refused\n%s", code, errOut, out)
			}
			if after := mustRead(t, file); !bytes.Equal(before, after) {
				t.Errorf("bindings file changed:\n%s\n->\n%s", before, after)
			}
		})
	}
}

func link(t *testing.T, target, path string) string {
	t.Helper()
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	return path
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// A CLI that is not on PATH gets no hook entry, and no Codex trust steps show without Codex.
func TestSetupSkipsMissingCLI(t *testing.T) {
	home := setupHome(t, "0.9.1")
	if err := os.Remove(filepath.Join(home, "path", "codex")); err != nil {
		t.Fatal(err)
	}
	before := mustRead(t, filepath.Join(home, ".codex", "hooks.json"))
	code, out, errOut := runSetup(t, "y\ny\ny\ny\n")
	if code != 0 {
		t.Fatalf("setup = %d: %s\n%s", code, errOut, out)
	}
	if !strings.Contains(out, "codex is not installed") || strings.Contains(out, "Press t to trust it") {
		t.Errorf("output:\n%s", out)
	}
	if after := mustRead(t, filepath.Join(home, ".codex", "hooks.json")); !bytes.Equal(before, after) {
		t.Error("setup changed the Codex hook file")
	}
}
