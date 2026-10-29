package launch

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aryanVijaywargia/herdr-brain/internal/bindings"
	"github.com/aryanVijaywargia/herdr-brain/internal/herdr"
	"github.com/aryanVijaywargia/herdr-brain/internal/paths"
)

// cmdEnv is a PATH folder with fake claude and codex CLIs that print a version, a store bound
// to herdr session s1, and a pane w1:p1 that herdr confirms.
type cmdEnv struct {
	bin, bindings, sock, store string
	exec                       []string // the argv of the exec; nil before it
	stderr                     bytes.Buffer
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
		Environ: environ, Self: self, Bindings: e.bindings, Now: time.Now, Stderr: &e.stderr,
		Pane: func(_ context.Context, _ herdr.Target, id string) (herdr.Pane, error) {
			if id != "w1:p1" {
				return herdr.Pane{}, herdr.ErrPaneNotFound
			}
			return herdr.Pane{ID: id, WorkspaceID: "w1"}, nil
		},
		Exec: func(bin string, argv, env []string) error {
			if bin != filepath.Join(e.bin, kind) || !slices.Equal(env, environ) {
				t.Errorf("exec %s with env %q, want %s with the env of brain", bin, env, filepath.Join(e.bin, kind))
			}
			e.exec = argv
			return nil
		},
	}
	return Main(context.Background(), d, kind, args)
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
		{name: "refuse", version: "2.1.312", herdr: true, kind: "codex", args: []string{"-c", "hooks={}"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newCmdEnv(t, tt.version)
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
			if want := append([]string{tt.kind}, tt.want...); !slices.Equal(e.exec, want) {
				t.Errorf("exec argv = %q, want %q", e.exec, want)
			}
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
	got, err := findCLI([]string{"PATH=" + strings.Join(dirs, string(os.PathListSeparator))}, "claude", self)
	if want := filepath.Join(root, "other", "claude"); err != nil || got != want {
		t.Errorf("findCLI() = %q, %v, want %q", got, err, want)
	}
	_, err = findCLI([]string{"PATH=" + filepath.Join(root, "link")}, "claude", self)
	if err == nil || errors.Is(err, os.ErrNotExist) {
		t.Errorf("findCLI() with only a link to brain = %v, want not found", err)
	}
}
