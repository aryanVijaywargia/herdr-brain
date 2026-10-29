package main

import (
	"bytes"
	"context"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aryanVijaywargia/herdr-brain/internal/bindings"
	"github.com/aryanVijaywargia/herdr-brain/internal/launch"
	"github.com/aryanVijaywargia/herdr-brain/internal/paths"
)

// TestMain lets the test binary run as brain when BRAIN_TEST_MAIN is 1, so a test can see the
// real exec of `brain claude` and `brain codex`.
func TestMain(m *testing.M) {
	if os.Getenv("BRAIN_TEST_MAIN") == "1" {
		main()
	}
	os.Exit(m.Run())
}

// launchHome is a test home with the Claude and Codex config files of the user, fake claude and
// codex CLIs that log their args, a fake herdr that confirms pane w1:p1 in workspace w1, and
// a main store bound to herdr session s1.
type launchHome struct {
	home, fakes, store string
	env                []string // the env of pane w1:p1, with only the fake CLIs on PATH
}

func newLaunchHome(t *testing.T) *launchHome {
	t.Helper()
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h := &launchHome{home: home, fakes: filepath.Join(home, "fakes")}
	reply := `{"id":"cli:pane:get","result":{"type":"pane_info","pane":{"pane_id":"w1:p1","tab_id":"w1:t1","workspace_id":"w1"}}}`
	files := map[string]string{
		"fakes/claude":          fakeCLI(home, "claude", "2.1.312 (Claude Code)"),
		"fakes/codex":           fakeCLI(home, "codex", "codex-cli 0.165.0"),
		"herdr/herdr":           "#!/bin/sh\n[ \"$*\" = 'pane get w1:p1' ] || exit 1\necho '" + reply + "'\n",
		"herdr/herdr.sock":      "",
		".claude/settings.json": `{"hooks":{}}`,
		".codex/config.toml":    "model = \"o3\"\n",
		".codex/hooks.json":     `{"hooks":{}}`,
	}
	for rel, data := range files {
		p := filepath.Join(home, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(data), 0o700); err != nil { //nolint:gosec // the fake CLIs must be executable
			t.Fatal(err)
		}
	}
	h.store = filepath.Join(home, ".local", "share", "brain", "main")
	if err := os.MkdirAll(h.store, 0o700); err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(home, "herdr", "herdr.sock")
	b := bindings.Binding{Session: "s1", Socket: sock, Store: h.store, Kind: bindings.KindMain, Created: time.Now()}
	if err := bindings.Write(filepath.Join(home, ".local", "share", "brain", "bindings.json"), []bindings.Binding{b}); err != nil {
		t.Fatal(err)
	}
	h.env = []string{"HOME=" + home, "PATH=" + h.fakes, "BRAIN_TEST_MAIN=1", "HERDR_ENV=1", "HERDR_SESSION=s1",
		"HERDR_SOCKET_PATH=" + sock, "HERDR_PANE_ID=w1:p1", "HERDR_WORKSPACE_ID=w1",
		"HERDR_BIN_PATH=" + filepath.Join(home, "herdr", "herdr")}
	return h
}

// fakeCLI is a fake CLI that prints version, or writes each of its args on a line of
// <home>/<kind>.log.
func fakeCLI(home, kind, version string) string {
	return "#!/bin/sh\nif [ \"$1\" = --version ]; then echo '" + version + "'; exit 0; fi\n" +
		"printf '%s\\n' \"$@\" > '" + filepath.Join(home, kind+".log") + "'\n"
}

// brain runs the test binary as `brain args...` with env, and returns its stderr.
func (h *launchHome) brain(t *testing.T, env []string, args ...string) string {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	// A launch that ran itself would loop until this deadline.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, self, args...)
	cmd.Env = env
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if out, err := cmd.Output(); err != nil {
		t.Fatalf("brain %q: %v\nstdout %s\nstderr %s", args, err, out, stderr.String())
	}
	return stderr.String()
}

// logged returns the args that the fake CLI kind got.
func (h *launchHome) logged(t *testing.T, kind string) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(h.home, kind+".log"))
	if err != nil {
		t.Fatalf("the fake %s did not run: %v", kind, err)
	}
	return strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
}

// configs returns every file under the test home's .claude and .codex.
func (h *launchHome) configs(t *testing.T) map[string]string {
	t.Helper()
	m := snapshot(t, filepath.Join(h.home, ".claude"))
	maps.Copy(m, snapshot(t, filepath.Join(h.home, ".codex")))
	return m
}

// with returns env with the entries of add in place of those of the same names.
func with(env []string, add ...string) []string {
	out := slices.DeleteFunc(slices.Clone(env), func(kv string) bool {
		k, _, _ := strings.Cut(kv, "=")
		return slices.ContainsFunc(add, func(a string) bool { return strings.HasPrefix(a, k+"=") })
	})
	return append(out, add...)
}

// AC #442: in a herdr pane of a bound session, `brain claude --x y` and `brain codex --x y`
// exec the real CLI with the launch flags, then --x y, and print nothing. Each launch has its
// folder (0700) with its ID file (0600). No file under .claude or .codex changes.
func TestLaunchExec(t *testing.T) {
	h := newLaunchHome(t)
	before := h.configs(t)
	for i, tt := range []struct {
		kind string
		want []string
	}{
		{"claude", []string{"--x", "y"}},
		{"codex", []string{"--no-daemon", "--x", "y"}},
	} {
		if stderr := h.brain(t, h.env, tt.kind, "--x", "y"); stderr != "" {
			t.Errorf("brain %s stderr = %q, want none", tt.kind, stderr)
		}
		if got := h.logged(t, tt.kind); !slices.Equal(got, tt.want) {
			t.Errorf("brain %s --x y: %s got %q, want %q", tt.kind, tt.kind, got, tt.want)
		}
		dir := paths.Launch(h.store, int64(i+1))
		files := map[string]os.FileMode{dir: 0o700 | os.ModeDir}
		es, err := os.ReadDir(dir)
		if err != nil || len(es) == 0 {
			t.Fatalf("launch folder %s: %v, %d files", dir, err, len(es))
		}
		for _, e := range es {
			files[filepath.Join(dir, e.Name())] = 0o600
		}
		for path, want := range files {
			fi, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if fi.Mode() != want {
				t.Errorf("mode of %s = %v, want %v", path, fi.Mode(), want)
			}
		}
	}
	if after := h.configs(t); !maps.Equal(after, before) {
		t.Errorf("the files under .claude and .codex changed:\n%v\nwant\n%v", after, before)
	}
}

// AC #442: with HERDR_ENV unset, the fake CLI gets only the user's args, stderr has one line,
// and Brain makes no launch folder.
func TestLaunchPlain(t *testing.T) {
	h := newLaunchHome(t)
	for _, kind := range []string{"claude", "codex"} {
		stderr := h.brain(t, with(h.env, "HERDR_ENV="), kind, "--x", "y")
		if strings.Count(stderr, "\n") != 1 || !strings.Contains(stderr, "not in herdr") {
			t.Errorf("brain %s stderr = %q, want one line with the reason", kind, stderr)
		}
		if got := h.logged(t, kind); !slices.Equal(got, []string{"--x", "y"}) {
			t.Errorf("brain %s --x y: %s got %q, want only the user's args", kind, kind, got)
		}
	}
	if _, err := os.Stat(filepath.Join(h.store, "launches")); !os.IsNotExist(err) {
		t.Errorf("launches folder: %v, want none", err)
	}
}

// AC #442: a link to the brain binary and a copy of it, both named claude and earlier on
// PATH, do not make a loop: the launch skips them and execs the fake claude, in herdr and
// outside.
func TestLaunchNoLoop(t *testing.T) {
	h := newLaunchHome(t)
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	link, cp := filepath.Join(h.home, "link"), filepath.Join(h.home, "copy")
	for _, d := range []string{link, cp} {
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(self, filepath.Join(link, "claude")); err != nil {
		t.Fatal(err)
	}
	copyFile(t, self, filepath.Join(cp, "claude"))
	path := "PATH=" + strings.Join([]string{link, cp, h.fakes}, string(os.PathListSeparator))
	for _, env := range [][]string{with(h.env, path), with(h.env, path, "HERDR_ENV=")} {
		// The args are a brain command line: a launch that ran brain as claude would launch again.
		h.brain(t, env, "claude", "claude", "hi")
		if got := h.logged(t, "claude"); !slices.Equal(got, []string{"claude", "hi"}) {
			t.Errorf("brain claude claude hi: claude got %q, want [claude hi]", got)
		}
	}
}

func copyFile(t *testing.T, from, to string) {
	t.Helper()
	src, err := os.Open(from)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close() //nolint:errcheck // read-only: Close cannot lose data
	dst, err := os.OpenFile(to, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o700)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(dst, src); err != nil {
		t.Fatal(err)
	}
	if err := dst.Close(); err != nil {
		t.Fatal(err)
	}
}

// A CLI kind with launch data is never the name of a Brain command, and `brain <kind>` is its
// launch command. The Brain commands are the herdr commands and each command of the commands
// block of the usage; every other entry of the commands map is a launch.
func TestLaunchKindNames(t *testing.T) {
	names := slices.Collect(maps.Keys(herdrCommands))
	i := strings.Index(usage, "commands:")
	if i < 0 {
		t.Fatal("the usage has no commands block")
	}
	block, _, _ := strings.Cut(usage[i:], "\n\n")
	for _, m := range regexp.MustCompile(`(?m)^  ([a-z][a-z-]*)\s`).FindAllStringSubmatch(block, -1) {
		names = append(names, m[1])
	}
	names = append(names, "help")
	if len(names) < 28 {
		t.Fatalf("found %d Brain commands, want all of them: %v", len(names), names)
	}
	kinds := launch.Kinds()
	for _, k := range kinds {
		if slices.Contains(names, k) {
			t.Errorf("CLI kind %q is the name of a Brain command", k)
		}
		if commands[k] == nil || !strings.Contains(usage, "\n  "+k+" ") {
			t.Errorf("brain %s has no launch command or no usage line", k)
		}
	}
	for name := range commands {
		if !slices.Contains(names, name) && !slices.Contains(kinds, name) {
			t.Errorf("command %q is not in the usage and has no launch data", name)
		}
	}
	if helpFlag("claude", []string{"--help"}) {
		t.Error("brain claude --help prints the Brain usage, want the help of claude")
	}
}
