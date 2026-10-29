package launch

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aryanVijaywargia/herdr-brain/internal/bindings"
	"github.com/aryanVijaywargia/herdr-brain/internal/herdr"
	"github.com/aryanVijaywargia/herdr-brain/internal/paths"
	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

// cmdEnv is a PATH folder with fake claude and codex CLIs that print a version, a store bound
// to herdr session s1, and a pane w1:p1 that herdr confirms.
type cmdEnv struct {
	bin, bindings, sock, store string
	stderr                     bytes.Buffer
	paneErr                    error // the error of herdr pane get; nil for the pane
	execErr                    error // the error of the exec
	// The file, argv and env of the exec; exec is nil before it.
	execBin   string
	exec, env []string
}

func newCmdEnv(t *testing.T, claudeVersion string) *cmdEnv {
	t.Helper()
	e := &cmdEnv{bin: t.TempDir(), store: t.TempDir()}
	for kind, v := range map[string]string{"claude": claudeVersion + " (Claude Code)", "codex": "codex-cli 0.165.0"} {
		if err := os.WriteFile(filepath.Join(e.bin, kind), []byte("#!/bin/sh\necho '"+v+"'\n"), 0o700); err != nil { //nolint:gosec // a test CLI must be executable
			t.Fatal(err)
		}
	}
	e.sock = filepath.Join(t.TempDir(), "herdr.sock")
	if err := os.WriteFile(e.sock, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	e.bindings = filepath.Join(t.TempDir(), "bindings.json")
	b := bindings.Binding{Session: "s1", Socket: e.sock, Store: e.store, Kind: bindings.KindMain, Created: time.Now()}
	if err := bindings.Write(e.bindings, []bindings.Binding{b}); err != nil {
		t.Fatal(err)
	}
	return e
}

// herdrEnv is the env of pane w1:p1 of session s1.
func (e *cmdEnv) herdrEnv() []string {
	return []string{"PATH=" + e.bin, "HERDR_ENV=1", "HERDR_SESSION=s1", "HERDR_SOCKET_PATH=" + e.sock,
		"HERDR_PANE_ID=w1:p1", "HERDR_WORKSPACE_ID=w1"}
}

func (e *cmdEnv) run(t *testing.T, environ []string, kind string, args ...string) error {
	t.Helper()
	self := filepath.Join(t.TempDir(), "brain")
	if err := os.WriteFile(self, []byte("brain binary"), 0o700); err != nil { //nolint:gosec // a test binary must be executable
		t.Fatal(err)
	}
	d := Deps{
		Environ: environ, PID: testPID, Self: self, Bindings: e.bindings, Now: time.Now, Stderr: &e.stderr,
		Pane: func(_ context.Context, _ herdr.Target, id string) (herdr.Pane, error) {
			switch {
			case e.paneErr != nil:
				return herdr.Pane{}, e.paneErr
			case id != "w1:p1":
				return herdr.Pane{}, herdr.ErrPaneNotFound
			}
			return herdr.Pane{ID: id, WorkspaceID: "w1"}, nil
		},
		Exec: func(bin string, argv, env []string) error {
			e.execBin = bin
			e.exec = argv
			e.env = env
			return e.execErr
		},
	}
	return Main(context.Background(), d, kind, args)
}

// testPID is the process ID of the Brain of the tests.
const testPID = 4242

// checkEnv checks that the exec got environ, with the launch chain of one more launch that
// runs the CLI file bin.
func (e *cmdEnv) checkEnv(t *testing.T, environ []string) {
	t.Helper()
	c := readChain(environ, testPID)
	chain := fmt.Sprintf("%s=%d:%d:%s", chainEnv, testPID, c.depth+1, strings.Join(append(c.paths, e.execBin), ":"))
	want := append(slices.DeleteFunc(slices.Clone(environ), func(kv string) bool { return strings.HasPrefix(kv, chainEnv+"=") }), chain)
	if !slices.Equal(e.env, want) {
		t.Errorf("exec env = %q, want %q", e.env, want)
	}
}

// A form that gets Brain flags in a confirmed pane makes a launch record and its folder,
// and execs the CLI with the Brain flags, then the user's args. Every other call execs the
// plain CLI with the user's args and prints one stderr line with the reason. A refuse conflict
// execs nothing.
func TestLaunchMain(t *testing.T) {
	tests := []struct {
		name     string
		version  string
		herdr    bool
		pane     string
		kind     string
		args     []string
		want     []string // the argv of the exec, after the kind
		stderr   string   // a part of the one stderr line; "" for none
		launched bool
		paneErr  error // the error of herdr pane get
	}{
		{name: "brain codex", version: "2.1.312", herdr: true, kind: "codex", args: []string{"--x", "y"},
			want: []string{"--no-daemon", "--x", "y"}, launched: true},
		{name: "brain claude", version: "2.1.312", herdr: true, kind: "claude", args: []string{"--x", "y"},
			want: []string{"--x", "y"}, launched: true},
		{name: "not in herdr", version: "2.1.312", kind: "codex", args: []string{"--x", "y"},
			want: []string{"--x", "y"}, stderr: "brain codex: starting the plain codex with no Brain flags: not in herdr\n"},
		{name: "subcommand", version: "2.1.312", herdr: true, kind: "codex", args: []string{"exec", "hi"},
			want: []string{"exec", "hi"}, stderr: "the subcommand exec"},
		{name: "no brain conflict", version: "2.1.312", herdr: true, kind: "codex", args: []string{"--disable", "hooks"},
			want: []string{"--disable", "hooks"}, stderr: "it turns off the hooks"},
		{name: "old CLI", version: "2.1.282", herdr: true, kind: "claude", args: []string{"hi"},
			want: []string{"hi"}, stderr: "claude 2.1.282 is older than 2.1.283"},
		{name: "pane not confirmed", version: "2.1.312", herdr: true, pane: "w1:p9", kind: "claude",
			want: []string{}, stderr: "herdr does not confirm pane w1:p9 in workspace w1 (pane not live)"},
		{name: "hooks off in a TOML table", version: "2.1.312", herdr: true, kind: "codex", args: []string{"-c", `features={"hooks"=false}`},
			want: []string{"-c", `features={"hooks"=false}`}, stderr: "it turns off the hooks"},
		{name: "a herdr error of two lines", version: "2.1.312", herdr: true, paneErr: errors.New("herdr unavailable\nsecond detail"),
			kind: "claude", args: []string{"hello"}, want: []string{"hello"}, stderr: "herdr failed (herdr unavailable second detail)\n"},
		{name: "refuse", version: "2.1.312", herdr: true, kind: "codex", args: []string{"-c", "hooks={}"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newCmdEnv(t, tt.version)
			e.paneErr = tt.paneErr
			cli := filepath.Join(e.bin, tt.kind)
			environ := []string{"PATH=" + e.bin}
			if tt.herdr {
				environ = e.herdrEnv()
			}
			if tt.pane != "" {
				environ = append(environ, "HERDR_PANE_ID="+tt.pane)
			}
			err := e.run(t, environ, tt.kind, tt.args...)
			if tt.want == nil {
				if err == nil || e.exec != nil || !strings.Contains(err.Error(), "Brain starts nothing") {
					t.Errorf("Main() = %v, exec %q, want a refusal and no exec", err, e.exec)
				}
				return
			}
			if err != nil {
				t.Fatalf("Main() = %v", err)
			}
			if want := append([]string{tt.kind}, tt.want...); !slices.Equal(e.exec, want) || e.execBin != cli {
				t.Errorf("exec %s argv = %q, want %s %q", e.execBin, e.exec, cli, want)
			}
			e.checkEnv(t, environ)
			out := e.stderr.String()
			if tt.stderr == "" && out != "" || !strings.Contains(out, tt.stderr) || strings.Count(out, "\n") > 1 {
				t.Errorf("stderr = %q, want one line with %q", out, tt.stderr)
			}
			_, err = os.Stat(paths.Launch(e.store, 1))
			if launched := err == nil; launched != tt.launched {
				t.Errorf("launch folder exists = %t (%v), want %t", launched, err, tt.launched)
			}
		})
	}
}

// findCLI skips the brain binary, a link to it, a hard link and a copy, a folder, a file that
// is not executable and a relative PATH folder. A file of the same size with other bytes is a CLI.
func TestFindCLI(t *testing.T) {
	root := t.TempDir()
	self := filepath.Join(root, "self", "brain")
	write := func(rel, data string, mode os.FileMode) {
		t.Helper()
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(data), mode); err != nil {
			t.Fatal(err)
		}
	}
	write("self/brain", "brain binary", 0o700)
	write("copy/claude", "brain binary", 0o700)
	write("plain/claude", "not executable", 0o600)
	write("other/claude", "brain other!", 0o700) // same size, other bytes
	for _, dir := range []string{"link", "hard", "folder/claude"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(self, filepath.Join(root, "link", "claude")); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(self, filepath.Join(root, "hard", "claude")); err != nil {
		t.Fatal(err)
	}
	var dirs []string
	for _, d := range []string{"link", "hard", "copy", "plain", "folder"} {
		dirs = append(dirs, filepath.Join(root, d))
	}
	dirs = append(dirs, "other", filepath.Join(root, "other"))
	got, err := findCLI([]string{"PATH=" + strings.Join(dirs, string(os.PathListSeparator))}, "claude", self, nil)
	if want := filepath.Join(root, "other", "claude"); err != nil || got != want {
		t.Errorf("findCLI() = %q, %v, want %q", got, err, want)
	}
	_, err = findCLI([]string{"PATH=" + filepath.Join(root, "link")}, "claude", self, nil)
	if err == nil || errors.Is(err, os.ErrNotExist) {
		t.Errorf("findCLI() with only a link to brain = %v, want not found", err)
	}
}

// A failed exec returns its error and ends only its own launch: its folder goes, and an
// earlier launch of the same pane keeps running.
func TestLaunchExecFailure(t *testing.T) {
	e := newCmdEnv(t, "2.1.312")
	b := bindings.Binding{Session: "s1", Socket: e.sock, Store: e.store, Kind: bindings.KindMain}
	older, err := Create(context.Background(), b, "w1", "w1:p1", "codex", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	e.execErr = errors.New("exec format error")
	if err := e.run(t, e.herdrEnv(), "codex", "hi"); !errors.Is(err, e.execErr) {
		t.Fatalf("Main() = %v, want the exec error", err)
	}
	if _, err := os.Stat(paths.Launch(e.store, 2)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the folder of the failed launch: %v, want none", err)
	}
	st, err := store.Open(context.Background(), e.store, busyTimeout)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close() //nolint:errcheck // test
	var running []store.Launch
	err = st.Write(context.Background(), func(tx *sql.Tx) error {
		running, err = store.RunningLaunches(context.Background(), tx, e.sock)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(running) != 1 || paths.Launch(e.store, running[0].ID) != older {
		t.Errorf("running launches = %+v, want only the earlier launch %s", running, older)
	}
	if _, err := os.Stat(older); err != nil {
		t.Errorf("the folder of the earlier launch: %v", err)
	}
}

// The launch chain: a Brain that a CLI file of an earlier Brain of the same process ran again
// skips that file. Another process, such as a nested `brain claude` in the session of the real
// CLI, skips nothing. The chain stops at maxDepth.
func TestLaunchChain(t *testing.T) {
	e := newCmdEnv(t, "2.1.312")
	shim := t.TempDir()
	if err := os.WriteFile(filepath.Join(shim, "claude"), []byte("#!/bin/sh\nexec brain claude \"$@\"\n"), 0o700); err != nil { //nolint:gosec // a test CLI must be executable
		t.Fatal(err)
	}
	path := "PATH=" + shim + string(os.PathListSeparator) + e.bin
	tests := []struct {
		chain string
		want  string // the CLI file of the exec; "" for an error
	}{
		{"", filepath.Join(shim, "claude")},
		{fmt.Sprintf("%d:1:%s", testPID, filepath.Join(shim, "claude")), filepath.Join(e.bin, "claude")},
		{fmt.Sprintf("%d:1:%s", testPID+1, filepath.Join(shim, "claude")), filepath.Join(shim, "claude")},
		{fmt.Sprintf("%d:%d:%s", testPID+1, maxDepth, filepath.Join(shim, "claude")), ""},
		{"nonsense", filepath.Join(shim, "claude")},
	}
	for _, tt := range tests {
		e.execBin, e.exec = "", nil
		environ := []string{path, chainEnv + "=" + tt.chain}
		err := e.run(t, environ, "claude", "hi")
		if tt.want == "" {
			if err == nil || !strings.Contains(err.Error(), "started brain again") || e.exec != nil {
				t.Errorf("chain %q: Main() = %v, exec %q, want a loop error", tt.chain, err, e.exec)
			}
			continue
		}
		if err != nil || e.execBin != tt.want {
			t.Errorf("chain %q: Main() = %v, exec %s, want %s", tt.chain, err, e.execBin, tt.want)
		}
		e.checkEnv(t, environ)
	}
	// After a shim of this process, the chain holds the shim and the real CLI.
	want := fmt.Sprintf("%s=%d:2:%s:%s", chainEnv, testPID, filepath.Join(shim, "claude"), filepath.Join(e.bin, "claude"))
	if err := e.run(t, []string{path, chainEnv + "=" + tests[1].chain}, "claude"); err != nil || !slices.Contains(e.env, want) {
		t.Errorf("Main() = %v, env %q, want %s", err, e.env, want)
	}
}

// A child of the CLI that holds the output pipe open does not hold the version check past its
// deadline.
func TestCLIVersionDeadline(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "codex")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n/bin/sleep 2 &\nwait\n"), 0o700); err != nil { //nolint:gosec // a test CLI must be executable
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := cliVersion(ctx, bin, []string{"PATH=/usr/bin:/bin"})
	if elapsed := time.Since(start); err == nil || elapsed > waitDelay+time.Second {
		t.Errorf("cliVersion() with a 50 ms deadline = %v after %v, want an error by %v", err, elapsed, waitDelay+time.Second)
	}
}
