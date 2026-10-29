package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aryanVijaywargia/herdr-brain/internal/bindings"
	"github.com/aryanVijaywargia/herdr-brain/internal/launch"
	"github.com/aryanVijaywargia/herdr-brain/internal/paths"
	"github.com/aryanVijaywargia/herdr-brain/internal/store"
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
	// A Codex launch reads the .codex/config.toml of each folder above its working folder.
	cmd.Dir = h.home
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

// userArgs returns args without the Brain flags at its start: --no-daemon, the -c pairs,
// --settings and --mcp-config.
func userArgs(args []string) []string {
	for len(args) > 0 {
		switch {
		case args[0] == "--no-daemon" || strings.HasPrefix(args[0], "--settings=") || strings.HasPrefix(args[0], "--mcp-config="):
			args = args[1:]
		case args[0] == "-c" && len(args) > 1:
			args = args[2:]
		default:
			return args
		}
	}
	return args
}

// mcpFlags are the Brain flags of the MCP server of launch id for the CLI kind: the server is
// the test binary as `brain mcp --launch <folder>` (E24-S6).
func (h *launchHome) mcpFlags(t *testing.T, kind string, id int64) []string {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := paths.Launch(h.store, id)
	if kind == "claude" {
		return []string{"--mcp-config=" + filepath.Join(dir, "mcp.json")}
	}
	return []string{"-c", `mcp_servers.brain.command="` + self + `"`,
		"-c", `mcp_servers.brain.args=["mcp","--launch","` + dir + `"]`, "-c", `mcp_servers.brain.env_vars=["HERDR_BIN_PATH"]`}
}

// hasRun reports whether want is a run of args in got.
func hasRun(got, want []string) bool {
	for i := 0; i+len(want) <= len(got); i++ {
		if slices.Equal(got[i:i+len(want)], want) {
			return true
		}
	}
	return false
}

// AC #442 and #446: in a herdr pane of a bound session, `brain claude --x y` and `brain codex
// --x y` exec the real CLI with the static flags (Codex --no-daemon) and the flags of the
// plan, with the Brain MCP server, then --x y, and print nothing. Claude gets the server in
// mcp.json and the allow rule of its tools in settings.json; Codex gets -c mcp_servers.brain.*.
// The server is the running brain binary, with the launch folder. Each launch has its folder
// (0700) with its ID file and its other files (0600). No file under .claude or .codex changes.
func TestLaunchStaticFlags(t *testing.T) {
	h := newLaunchHome(t)
	before := h.configs(t)
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for i, tt := range []struct {
		kind  string
		first string
	}{
		{"claude", "--settings=" + filepath.Join(paths.Launch(h.store, 1), "settings.json")},
		{"codex", "--no-daemon"},
	} {
		if stderr := h.brain(t, h.env, tt.kind, "--x", "y"); stderr != "" {
			t.Errorf("brain %s stderr = %q, want none", tt.kind, stderr)
		}
		got := h.logged(t, tt.kind)
		if got[0] != tt.first || !slices.Equal(userArgs(got), []string{"--x", "y"}) {
			t.Errorf("brain %s --x y: %s got %q, want %s first and --x y last", tt.kind, tt.kind, got, tt.first)
		}
		if mcp := h.mcpFlags(t, tt.kind, int64(i+1)); !hasRun(got, mcp) {
			t.Errorf("brain %s --x y: %s got %q, want the MCP flags %q", tt.kind, tt.kind, got, mcp)
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
		if tt.kind == "claude" {
			wantJSON(t, filepath.Join(dir, "mcp.json"), `{"mcpServers":{"brain":{"type":"stdio","command":`+
				strconv.Quote(self)+`,"args":["mcp","--launch",`+strconv.Quote(dir)+`]}}}`)
			var s struct {
				Permissions struct {
					Allow []string `json:"allow"`
				} `json:"permissions"`
			}
			readJSON(t, filepath.Join(dir, "settings.json"), &s)
			if !slices.Equal(s.Permissions.Allow, []string{"mcp__brain"}) {
				t.Errorf("settings.json permissions.allow = %q, want [mcp__brain]", s.Permissions.Allow)
			}
		}
	}
	if after := h.configs(t); !maps.Equal(after, before) {
		t.Errorf("the files under .claude and .codex changed:\n%v\nwant\n%v", after, before)
	}
}

// readJSON reads the JSON file at path into v.
func readJSON(t *testing.T, path string, v any) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatal(err)
	}
}

// wantJSON checks that the file at path holds the JSON value want.
func wantJSON(t *testing.T, path, want string) {
	t.Helper()
	var got, w any
	readJSON(t, path, &got)
	if err := json.Unmarshal([]byte(want), &w); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, w) {
		t.Errorf("%s = %v, want %s", path, got, want)
	}
}

// hookCommand is the command of Brain's hook of event for launch record launch of the CLI kind.
func (h *launchHome) hookCommand(t *testing.T, kind, event string, launch int64) string {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return self + " hook --profile " + kind + " --event " + event + " --launch " + paths.Launch(h.store, launch)
}

// AC E24-S5: the fake claude gets Brain's prompt, tool and stop hooks with the launch folder
// in its private --settings file.
func TestLaunchClaudeCaptureHooks(t *testing.T) {
	h := newLaunchHome(t)
	hook := func(kind, event string, launch int64) string { return h.hookCommand(t, kind, event, launch) }
	h.brain(t, h.env, "claude", "hi")
	args := h.logged(t, "claude")
	settings, ok := strings.CutPrefix(args[0], "--settings=")
	if !ok {
		t.Fatalf("claude got %q, want --settings first", args)
	}
	b, err := os.ReadFile(settings)
	if err != nil {
		t.Fatal(err)
	}
	var s struct {
		Hooks map[string][]struct {
			Matcher string `json:"matcher"`
			Hooks   []struct {
				Type, Command string
			} `json:"hooks"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for event, groups := range s.Hooks {
		for _, g := range groups {
			for _, hd := range g.Hooks {
				got[event] = g.Matcher + " " + hd.Type + " " + hd.Command
			}
		}
	}
	want := map[string]string{
		"UserPromptSubmit":   " command " + hook("claude", "prompt", 1),
		"PostToolUse":        "Bash command " + hook("claude", "tool", 1),
		"PostToolUseFailure": "Bash command " + hook("claude", "tool", 1),
		"Stop":               " command " + hook("claude", "stop", 1),
	}
	if !maps.Equal(got, want) {
		t.Errorf("claude settings hooks = %q, want %q", got, want)
	}
}

// AC E24-S5: the fake codex gets Brain's prompt, tool and stop hooks as -c hooks entries with
// one hooks.state table that trusts each of them. The Codex launch keeps the Codex home of the
// pane.
func TestLaunchCodexCaptureHooks(t *testing.T) {
	h := newLaunchHome(t)
	hook := func(kind, event string, launch int64) string { return h.hookCommand(t, kind, event, launch) }
	h.brain(t, h.env, "codex", "hi")
	args := h.logged(t, "codex")
	var overrides []string
	for i, a := range args {
		if a == "-c" && i+1 < len(args) {
			overrides = append(overrides, args[i+1])
		}
	}
	for _, w := range []struct{ key, command string }{
		{"hooks.UserPromptSubmit=", hook("codex", "prompt", 1)},
		{"hooks.PostToolUse=", hook("codex", "tool", 1)},
		{"hooks.Stop=", hook("codex", "stop", 1)},
	} {
		i := slices.IndexFunc(overrides, func(o string) bool { return strings.HasPrefix(o, w.key) })
		if i < 0 || !strings.Contains(overrides[i], `command="`+w.command+`"`) {
			t.Errorf("codex -c overrides = %q, want %s with %q", overrides, w.key, w.command)
		}
	}
	states := slices.DeleteFunc(slices.Clone(overrides), func(o string) bool { return !strings.HasPrefix(o, "hooks.state=") })
	if len(states) != 1 || strings.Count(states[0], "trusted_hash") != 3 {
		t.Errorf("codex hooks.state overrides = %q, want one table that trusts 3 hooks", states)
	}
	if got := userArgs(args); !slices.Equal(got, []string{"hi"}) {
		t.Errorf("codex user args = %q, want [hi]", got)
	}
	ctx := context.Background()
	st, err := store.Open(ctx, h.store, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close() //nolint:errcheck // test
	err = st.Write(ctx, func(tx *sql.Tx) error {
		for id, want := range map[int64]string{1: filepath.Join(h.home, ".codex")} {
			l, _, err := store.LaunchByID(ctx, tx, id)
			if err != nil {
				return err
			}
			if l.CodexHome != want {
				t.Errorf("Codex home of launch %d = %q, want %q", id, l.CodexHome, want)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// AC #442 and #446: with HERDR_ENV unset, the fake CLI gets only the user's args, so no Brain
// MCP server; stderr has one line, and Brain makes no launch folder.
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
		if got := userArgs(h.logged(t, "claude")); !slices.Equal(got, []string{"claude", "hi"}) {
			t.Errorf("brain claude claude hi: claude got %q, want [claude hi]", got)
		}
	}
}

// AC #442 (review): a shell shim named claude earlier on PATH that runs `exec brain claude`
// does not loop: the Brain that the shim runs again skips it and execs the fake claude, in
// herdr and outside. In herdr it ends the launch whose CLI did not start and launches again
// with the user's args, so one launch stays. A shim that runs brain with no exec stops at the chain limit with
// an error, and starts no CLI.
func TestLaunchShim(t *testing.T) {
	h := newLaunchHome(t)
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	quoted := "'" + strings.ReplaceAll(self, "'", `'\''`) + "'"
	shims := map[string]string{"exec": "exec " + quoted, "fork": quoted}
	for name, line := range shims {
		dir := filepath.Join(h.home, name)
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "claude"), []byte("#!/bin/sh\n"+line+" claude \"$@\"\n"), 0o700); err != nil { //nolint:gosec // a shim must be executable
			t.Fatal(err)
		}
	}
	path := func(name string) string {
		return "PATH=" + filepath.Join(h.home, name) + string(os.PathListSeparator) + h.fakes
	}
	for _, env := range [][]string{with(h.env, path("exec")), with(h.env, path("exec"), "HERDR_ENV=")} {
		h.brain(t, env, "claude", "hi")
		if got := userArgs(h.logged(t, "claude")); !slices.Equal(got, []string{"hi"}) {
			t.Errorf("brain claude hi through an exec shim: claude got %q, want [hi]", got)
		}
	}
	// The Brain that the shim ran again ended launch 1, whose CLI did not start, and made
	// launch 2 for the user's args: one launch stays.
	if _, err := os.Stat(paths.Launch(h.store, 1)); !os.IsNotExist(err) {
		t.Errorf("launch 1 through the shim: %v, want it ended", err)
	}
	if _, err := os.Stat(paths.Launch(h.store, 2)); err != nil {
		t.Errorf("launch 2 through the shim: %v", err)
	}
	if err := os.Remove(filepath.Join(h.home, "claude.log")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, self, "claude", "hi")
	cmd.Env = with(h.env, path("fork"), "HERDR_ENV=")
	out, err := cmd.CombinedOutput()
	if err == nil || ctx.Err() != nil || !strings.Contains(string(out), "started brain again 8 times") {
		t.Errorf("brain claude through a shim with no exec = %v, %v:\n%s\nwant the loop error", err, ctx.Err(), out)
	}
	if _, err := os.Stat(filepath.Join(h.home, "claude.log")); !os.IsNotExist(err) {
		t.Errorf("the fake claude ran through a looping shim: %v", err)
	}
}

// Review of #446: a shim that runs `exec brain claude "$@"` with one more --mcp-config that
// names a server brain is refused: the Brain that the shim runs again checks the args that the
// shim added. The fake claude does not run.
func TestLaunchShimAddsBrainServer(t *testing.T) {
	h := newLaunchHome(t)
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(h.home, "shim")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
	foreign := `--mcp-config={"mcpServers":{"brain":{"command":"foreign"}}}`
	script := "#!/bin/sh\nif [ \"$1\" = --version ]; then echo '2.1.312'; exit 0; fi\nexec " +
		quote(self) + " claude \"$@\" " + quote(foreign) + "\n"
	if err := os.WriteFile(filepath.Join(dir, "claude"), []byte(script), 0o700); err != nil { //nolint:gosec // a shim must be executable
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, self, "claude", "hi")
	cmd.Env, cmd.Dir = with(h.env, "PATH="+dir+string(os.PathListSeparator)+h.fakes), h.home
	out, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(out), "names a server brain") || strings.Contains(string(out), "foreign") {
		t.Errorf("brain claude through a shim that adds a server brain = %v:\n%s\nwant a refusal with no config text", err, out)
	}
	if _, err := os.Stat(filepath.Join(h.home, "claude.log")); !os.IsNotExist(err) {
		t.Errorf("the fake claude ran: %v", err)
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
