package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// fakeNewPaneHerdr writes a fake herdr into dir. It saves the args of each call, NUL-ended, in
// the file "$LOG/<arg 1>-<arg 2>", and answers a split with the new pane w1:p2. With FAIL_SPLIT
// set, the split fails as herdr fails it.
func fakeNewPaneHerdr(t *testing.T, dir string) string {
	t.Helper()
	bin := filepath.Join(dir, "herdr")
	script := `#!/bin/sh
printf '%s\0' "$@" > "$LOG/$1-$2"
[ "$1 $2" = "pane split" ] || exit 0
if [ -n "$FAIL_SPLIT" ]; then
	echo '{"error":{"code":"pane_not_found","message":"pane not found"}}' >&2
	exit 1
fi
echo '{"id":"cli:pane:split","result":{"type":"pane_info","pane":{"pane_id":"w1:p2","workspace_id":"w1","tab_id":"w1:t1","cwd":"/elsewhere"}}}'
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

// herdrCall returns the args of the fake herdr call "<a1>-<a2>", or nil when there was none.
func herdrCall(t *testing.T, log, name string) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(log, name))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSuffix(string(b), "\x00"), "\x00")
}

// E24-S7 AC 1: brain new-pane splits the focused pane of the action context with an explicit
// --cwd, the cwd of that pane after JSON decoding, then types `<bin> claude|codex` into the new
// pane. Without --cwd herdr uses its [terminal] new_cwd setting, which can be the home folder,
// so every split carries the cwd. The brain path is a whole word in the pane's shell: a ' or a
// $( in it runs nothing.
func TestNewPane(t *testing.T) {
	cases := []struct {
		name    string
		args    []string
		context string
		wantCwd string // "" means no herdr call and an error
	}{
		{"claude", []string{"claude"}, `{"focused_pane_id":"w1:p1","focused_pane_cwd":"/work/my proj","workspace_id":"w1"}`, "/work/my proj"},
		{"codex with JSON escapes", []string{"codex"}, `{"focused_pane_id":"w1:p1","focused_pane_cwd":"/w/q\"d\\b\nné\/x"}`, "/w/q\"d\\b\nné/x"},
		{"no context", []string{"claude"}, "", ""},
		{"bad context", []string{"claude"}, `{"focused_pane_id":`, ""},
		{"no cwd", []string{"claude"}, `{"focused_pane_id":"w1:p1"}`, ""},
		{"no focused pane", []string{"claude"}, `{"focused_pane_cwd":"/work"}`, ""},
		{"other CLI", []string{"gemini"}, `{"focused_pane_id":"w1:p1","focused_pane_cwd":"/work"}`, ""},
		{"no CLI", nil, `{"focused_pane_id":"w1:p1","focused_pane_cwd":"/work"}`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			bin := filepath.Join(dir, `plug'in; touch "$MARK"; : '$(touch "$MARK")\`, "brain")
			environ := []string{"PATH=" + os.Getenv("PATH"), "LOG=" + dir, "SHELL=/bin/zsh",
				"HERDR_BIN_PATH=" + fakeNewPaneHerdr(t, dir), "HERDR_SOCKET_PATH=/tmp/h.sock"}
			if tc.context != "" {
				environ = append(environ, "HERDR_PLUGIN_CONTEXT_JSON="+tc.context)
			}
			err := newPane(context.Background(), tc.args, environ, bin)
			split, run := herdrCall(t, dir, "pane-split"), herdrCall(t, dir, "pane-run")
			if tc.wantCwd == "" {
				if err == nil || split != nil || run != nil {
					t.Errorf("newPane() = %v with split %q and run %q, want an error and no herdr call", err, split, run)
				}
				return
			}
			if err != nil {
				t.Fatalf("newPane() = %v", err)
			}
			wantSplit := []string{"pane", "split", "w1:p1", "--direction", "right", "--cwd", tc.wantCwd, "--focus"}
			if strings.Join(split, "\x00") != strings.Join(wantSplit, "\x00") {
				t.Errorf("split = %q, want %q", split, wantSplit)
			}
			if len(run) != 4 || run[2] != "w1:p2" {
				t.Fatalf("run = %q, want pane run w1:p2 <command>", run)
			}
			checkPaneCommand(t, bin, run[3], tc.args[0])
		})
	}
}

// checkPaneCommand runs command, the text that brain new-pane types into the new pane, in each
// POSIX shell on PATH, with a fake brain at bin. The fake must get the one arg cli, and no
// text of bin may run.
func checkPaneCommand(t *testing.T, bin, command, cli string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(bin), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$OUT\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, shell := range []string{"sh", "bash", "zsh"} {
		path, err := exec.LookPath(shell)
		if err != nil {
			continue
		}
		dir := t.TempDir()
		mark, out := filepath.Join(dir, "mark"), filepath.Join(dir, "out")
		cmd := exec.Command(path, "-c", command)
		// HOME and ZDOTDIR keep zsh away from the rc files of the real home.
		cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + dir, "ZDOTDIR=" + dir, "MARK=" + mark, "OUT=" + out}
		if b, err := cmd.CombinedOutput(); err != nil {
			t.Errorf("%s -c %q = %v, output %q", shell, command, err, b)
			continue
		}
		if b, err := os.ReadFile(out); err != nil || string(b) != cli+"\n" {
			t.Errorf("%s: brain got args %q, %v, want %q", shell, b, err, cli)
		}
		if _, err := os.Stat(mark); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s: text of the brain path ran as a command (mark: %v)", shell, err)
		}
	}
}

// A failed split types nothing into a pane.
func TestNewPaneFailedSplit(t *testing.T) {
	dir := t.TempDir()
	environ := []string{"PATH=" + os.Getenv("PATH"), "LOG=" + dir, "FAIL_SPLIT=1",
		"HERDR_BIN_PATH=" + fakeNewPaneHerdr(t, dir), "HERDR_SOCKET_PATH=/tmp/h.sock",
		`HERDR_PLUGIN_CONTEXT_JSON={"focused_pane_id":"w1:p1","focused_pane_cwd":"/work"}`}
	if err := newPane(context.Background(), []string{"claude"}, environ, "/b/brain"); err == nil {
		t.Error("newPane() = nil, want the split error")
	}
	if run := herdrCall(t, dir, "pane-run"); run != nil {
		t.Errorf("run = %q, want no pane run", run)
	}
}

// fish reads \\ and \' in single quotes, so its quoting differs from the POSIX shells.
func TestShellQuote(t *testing.T) {
	for _, tc := range []struct{ s, shell, want string }{
		{`/a b/brain`, "/bin/zsh", `'/a b/brain'`},
		{`/a'b\c/brain`, "/bin/bash", `'/a'\''b\c/brain'`},
		{`/a'b\c/brain`, "/opt/homebrew/bin/fish", `'/a\'b\\c/brain'`},
	} {
		if got := shellQuote(tc.s, tc.shell); got != tc.want {
			t.Errorf("shellQuote(%q, %q) = %s, want %s", tc.s, tc.shell, got, tc.want)
		}
	}
}
