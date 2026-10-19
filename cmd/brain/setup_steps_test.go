package main

import (
	"bytes"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aryanVijaywargia/herdr-brain/internal/adapter"
	"github.com/aryanVijaywargia/herdr-brain/internal/bindings"
	"github.com/aryanVijaywargia/herdr-brain/internal/herdr"
	"github.com/aryanVijaywargia/herdr-brain/internal/install"
	"github.com/aryanVijaywargia/herdr-brain/internal/store"
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
	code, out, errOut := runSetup(t, "\ny\ny\n")
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
	for _, want := range []string{"- Session \"default\" runs herdr 0.9.1.\n", "- Brain is tested with this version. OK.\n",
		"brain uninstall --apply", "press t on the Brain hook", "\nFirst use\n", "brain status"} {
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

// AC 1: a fresh home with Claude and Codex: one question, and "y" writes the binding, the
// link, both hook entries, the hook records of Claude and the extractor setting. AC 3: a
// second setup asks nothing and changes nothing.
func TestSetupOneYes(t *testing.T) {
	home := setupHome(t, "0.9.1")
	p, err := adapter.Load("claude")
	if err != nil {
		t.Fatal(err)
	}
	fakeClaude(t, home, p.TestedVersion)
	code, out, errOut := runSetup(t, "claude\n\ny\ny\n")
	if code != 0 {
		t.Fatalf("setup = %d: %s\n%s", code, errOut, out)
	}
	if n := strings.Count(out, "Apply all of this?"); n != 1 {
		t.Errorf("setup asked %d times to apply its plan, want 1:\n%s", n, out)
	}
	if bs := readBindings(t, home); len(bs) != 1 || bs[0].Session != "default" {
		t.Errorf("bindings = %+v", bs)
	}
	if got, err := os.Readlink(filepath.Join(home, ".local", "bin", "brain")); err != nil || !filepath.IsAbs(got) {
		t.Errorf("link = %q, %v", got, err)
	}
	if b := mustRead(t, filepath.Join(home, ".codex", "hooks.json")); !bytes.Contains(b, []byte(" hook --profile codex --event start")) {
		t.Errorf("hooks.json has no Brain hook entry:\n%s", b)
	}
	if b := mustRead(t, filepath.Join(home, ".claude", "settings.json")); !bytes.Contains(b, []byte(" hook --profile claude --event start")) ||
		!bytes.Contains(b, []byte("--event tool")) {
		t.Errorf("settings.json has no Brain hook entry or no hook record entry:\n%s", b)
	}
	rec, err := install.Read(filepath.Join(home, ".local", "share", "brain", "install.json"))
	if err != nil || rec.Entries["claude"].Target == "" || rec.Entries["codex"].Target == "" {
		t.Errorf("install record = %+v, %v", rec, err)
	}
	if x := extractorOf(t, home); x.CLI != store.ExtractorClaude {
		t.Errorf("extractor = %+v, want claude", x)
	}
	for _, want := range []string{"Step 4: plan\n", "- Binding: bind session \"default\" to the main store ", "Make the symlink ",
		"Add this entry to the SessionStart array of ", "Add these entries to ", "brain uninstall --apply",
		"Setup lists each hook entry it adds in " + filepath.Join(home, ".local", "share", "brain", "install.json"),
		"- Extractor: record claude", "Bound session", "Linked ", "Added the Brain hook for codex",
		"Added the hook record entries for claude", "Step 5: trust the Codex hook\n"} {
		if !strings.Contains(out, want) {
			t.Errorf("output does not contain %q:\n%s", want, out)
		}
	}

	before := snapshot(t, home)
	code, out, errOut = runSetup(t, "")
	if code != 0 {
		t.Fatalf("second setup = %d: %s\n%s", code, errOut, out)
	}
	if strings.Contains(out, "[y/N]") || strings.Contains(out, "Which CLI") || !strings.Contains(out, "Every step is already done") {
		t.Errorf("the second setup asked a question:\n%s", out)
	}
	if after := snapshot(t, home); !maps.Equal(before, after) {
		t.Errorf("the second setup changed files:\nbefore %v\nafter  %v", before, after)
	}
}

// AC 2: "n", an empty answer or EOF writes nothing: no binding, no link, no hook entry, no
// install.json, no store and no extractor setting.
func TestSetupNo(t *testing.T) {
	for name, answers := range map[string]string{"n": "claude\n\nn\nn\n", "empty": "claude\n\n\n\n", "EOF": "claude\n\n"} {
		t.Run(name, func(t *testing.T) {
			home := setupHome(t, "0.9.1")
			p, err := adapter.Load("claude")
			if err != nil {
				t.Fatal(err)
			}
			fakeClaude(t, home, p.TestedVersion)
			before := snapshot(t, home)
			code, out, errOut := runSetup(t, answers)
			if code != 0 {
				t.Fatalf("setup = %d: %s\n%s", code, errOut, out)
			}
			if n := strings.Count(out, "Apply all of this?"); n != 1 || !strings.Contains(out, "Setup wrote nothing.") {
				t.Errorf("setup asked %d times to apply its plan, want 1:\n%s", n, out)
			}
			if after := snapshot(t, home); !maps.Equal(before, after) {
				t.Errorf("setup changed files after the answer %s:\nbefore %v\nafter  %v", name, before, after)
			}
		})
	}
}

// AC 4: a hook entry that you edited stays as it is, and the plan says so.
func TestSetupKeepsEditedEntry(t *testing.T) {
	home := setupHome(t, "0.9.1")
	if code, out, errOut := runSetup(t, "\ny\ny\n"); code != 0 {
		t.Fatalf("setup = %d: %s\n%s", code, errOut, out)
	}
	file := filepath.Join(home, ".codex", "hooks.json")
	edited := bytes.Replace(mustRead(t, file), []byte("--event start"), []byte("--event start --mine"), 1)
	if err := os.WriteFile(file, edited, 0o600); err != nil {
		t.Fatal(err)
	}
	// A missing link gives the plan a step, so setup asks.
	if err := os.Remove(filepath.Join(home, ".local", "bin", "brain")); err != nil {
		t.Fatal(err)
	}
	// The first setup recorded the extractor choice none, so the one answer is the yes.
	code, out, errOut := runSetup(t, "y\n")
	if code != 0 {
		t.Fatalf("setup = %d: %s\n%s", code, errOut, out)
	}
	if _, err := os.Readlink(filepath.Join(home, ".local", "bin", "brain")); err != nil {
		t.Errorf("setup did not apply its plan: no link: %v\n%s", err, out)
	}
	if !strings.Contains(out, file+" holds a Brain hook entry for codex that you edited. Brain keeps it.") ||
		strings.Contains(out, "Add this entry to the SessionStart array of "+file) {
		t.Errorf("the plan does not keep the edited entry:\n%s", out)
	}
	if now := mustRead(t, file); !bytes.Equal(now, edited) {
		t.Errorf("setup changed the edited entry:\n%s", now)
	}
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

// S54: a herdr version that is not tested shows the tested versions and the warning, and
// setup goes on.
func TestSetupUntestedHerdr(t *testing.T) {
	// 0.9.2 came before a tested release, so it stays untested.
	setupHome(t, "0.9.2")
	code, out, errOut := runSetup(t, "\nn\nn\n")
	if code != 0 {
		t.Fatalf("setup = %d: %s\n%s", code, errOut, out)
	}
	tested := strings.Join(herdr.TestedVersions(), ", ")
	for _, want := range []string{"Step 1: herdr version\n- Session \"default\" runs herdr 0.9.2.\n",
		"- 0.9.2 is not one of the tested versions: " + tested + ".\n", "- Brain works. brain status shows a warning.\n",
		"Apply all of this? [y/N] "} {
		if !strings.Contains(out, want) {
			t.Errorf("output does not contain %q:\n%s", want, out)
		}
	}
}

// A test binding gets its own store folder, beside the main binding.
func TestSetupTestBinding(t *testing.T) {
	home := setupHome(t, "0.9.1")
	if code, out, errOut := runSetup(t, "\ny\ny\n"); code != 0 {
		t.Fatalf("setup = %d: %s\n%s", code, errOut, out)
	}
	store := filepath.Join(home, "eval", "store")
	code, out, errOut := runSetup(t, "\ny\ny\n", "--session", "brain-test", "--test", store)
	if code != 0 {
		t.Fatalf("setup --test = %d: %s\n%s", code, errOut, out)
	}
	// AC 5: the same one question.
	if n := strings.Count(out, "[y/N]"); n != 1 || !strings.Contains(out, "Apply all of this? [y/N]") {
		t.Errorf("setup --test asked %d yes or no questions, want 1:\n%s", n, out)
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
				if code, out, errOut := runSetup(t, "\ny\ny\n"); code != 0 {
					t.Fatalf("setup = %d: %s\n%s", code, errOut, out)
				}
			}
			other := filepath.Join(home, "other-test")
			if code, out, errOut := runSetup(t, "\ny\ny\n", "--session", "brain-test2", "--test", other); code != 0 {
				t.Fatalf("setup --test = %d: %s\n%s", code, errOut, out)
			}
			folder := c.prep(t, home, main, other)
			file := filepath.Join(home, ".local", "share", "brain", "bindings.json")
			before := mustRead(t, file)
			session := cmpOr(c.session, "brain-test")
			code, out, errOut := runSetup(t, "\ny\ny\n", "--session", session, "--test", folder)
			// A store folder that does not exist yet is checked after the answer: the binding is
			// the first write, so setup stops before any other write.
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
	code, out, errOut := runSetup(t, "\ny\ny\n")
	if code != 0 {
		t.Fatalf("setup = %d: %s\n%s", code, errOut, out)
	}
	if !strings.Contains(out, "codex is not installed") || strings.Contains(out, "press t on the Brain hook") {
		t.Errorf("output:\n%s", out)
	}
	if after := mustRead(t, filepath.Join(home, ".codex", "hooks.json")); !bytes.Equal(before, after) {
		t.Error("setup changed the Codex hook file")
	}
}

// A CLI on PATH that has no config folder yet gets the folder and the hook on yes, and
// nothing on no.
func TestSetupMissingConfigFolder(t *testing.T) {
	for _, c := range []struct{ kind, dir, file, answers string }{
		{"claude", ".claude", "settings.json", "\ny\ny\n"},
		{"codex", ".codex", "hooks.json", "\ny\ny\n"},
		{"claude", ".claude", "settings.json", "\nn\nn\n"},
		{"codex", ".codex", "hooks.json", "\nn\nn\n"},
	} {
		yes := !strings.Contains(c.answers, "n")
		t.Run(fmt.Sprintf("%s yes=%t", c.kind, yes), func(t *testing.T) {
			home := setupHome(t, "0.9.1")
			if err := os.RemoveAll(filepath.Join(home, c.dir)); err != nil {
				t.Fatal(err)
			}
			code, out, errOut := runSetup(t, c.answers)
			if code != 0 {
				t.Fatalf("setup = %d: %s\n%s", code, errOut, out)
			}
			b, err := os.ReadFile(filepath.Join(home, c.dir, c.file))
			switch {
			case yes && (err != nil || !bytes.Contains(b, []byte(" hook --profile "))):
				t.Errorf("%s: %v\n%s", c.file, err, b)
			case !yes:
				if _, err := os.Lstat(filepath.Join(home, c.dir)); !os.IsNotExist(err) {
					t.Errorf("setup made %s after the answer no: %v", c.dir, err)
				}
			}
		})
	}
}

// hookReader runs fn at the first read, while setup waits at its first question.
type hookReader struct {
	fn func()
	r  io.Reader
}

func (h *hookReader) Read(p []byte) (int, error) {
	if h.fn != nil {
		h.fn()
		h.fn = nil
	}
	return h.r.Read(p)
}

// A setup that binds another session while this setup waits for its answer keeps its
// binding: this setup refuses and changes nothing.
func TestSetupOverlap(t *testing.T) {
	home := setupHome(t, "0.9.1")
	file := filepath.Join(home, ".local", "share", "brain", "bindings.json")
	var other []byte
	in := &hookReader{r: strings.NewReader("\ny\ny\n"), fn: func() {
		if code, out, errOut := runSetup(t, "\ny\ny\n", "--session", "brain-test2"); code != 0 {
			t.Fatalf("other setup = %d: %s\n%s", code, errOut, out)
		}
		other = mustRead(t, file)
	}}
	var out, errOut bytes.Buffer
	if code := run([]string{"setup"}, in, &out, &errOut); code != 1 || !strings.Contains(errOut.String(), "changed while setup waited") {
		t.Fatalf("setup = %d, stderr %q, want 1 and changed", code, errOut.String())
	}
	if now := mustRead(t, file); !bytes.Equal(now, other) {
		t.Errorf("bindings file changed:\n%s\n->\n%s", other, now)
	}
	// The other setup recorded the extractor choice none, so the one answer is the yes.
	if code, out, errOut := runSetup(t, "y\n"); code != 0 {
		t.Fatalf("setup again = %d: %s\n%s", code, errOut, out)
	}
	if bs := readBindings(t, home); len(bs) != 2 {
		t.Errorf("bindings = %+v, want 2", bs)
	}
}
