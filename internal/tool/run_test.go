package tool

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
)

// fillContract has a string input package in two steps and an integer input count.
const fillContract = head + `steps = [["pnpm", "install"], ["pnpm", "test", "--filter", "{package}", "--count", "{count}"]]
` + input + `
[inputs.count]
type = "integer"
pattern = "^[0-9]{1,3}$"
`

func TestFill(t *testing.T) {
	c, err := Parse(fillContract)
	if err != nil {
		t.Fatalf("Parse() = %v", err)
	}
	got, err := c.Fill(map[string]string{"package": "shop/api", "count": "12"})
	want := [][]string{{"pnpm", "install"}, {"pnpm", "test", "--filter", "shop/api", "--count", "12"}}
	if err != nil || !slices.EqualFunc(got, want, slices.Equal) {
		t.Errorf("Fill() = %q, %v, want %q", got, err, want)
	}
	for name, tc := range map[string]struct {
		values map[string]string
		want   string
	}{
		"missing":     {map[string]string{"package": "shop"}, "input count is missing"},
		"unknown":     {map[string]string{"package": "shop", "count": "1", "other": "x"}, "has no input other"},
		"not integer": {map[string]string{"package": "shop", "count": "1e3"}, "is not an integer"},
		"pattern":     {map[string]string{"package": "shop", "count": "1234"}, "does not match the pattern"},
		// S58: a value that holds shell text fails the pattern.
		"semicolon": {map[string]string{"package": "; rm -rf ~", "count": "1"}, "does not match the pattern"},
		"subshell":  {map[string]string{"package": "$(touch x)", "count": "1"}, "does not match the pattern"},
	} {
		if _, err := c.Fill(tc.values); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("Fill(%s) = %v, want %q", name, err, tc.want)
		}
	}
}

func TestDir(t *testing.T) {
	for cwd, want := range map[string]string{"~": "/home/u", "~/src/shop": "/home/u/src/shop", "/srv/x": "/srv/x", "~other": "~other"} {
		if got := (Contract{Cwd: cwd}).Dir("/home/u"); got != want {
			t.Errorf("Dir(%q) = %q, want %q", cwd, got, want)
		}
	}
}

// script writes run.sh with text into a new folder, and returns the folder.
func script(t *testing.T, text string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ScriptName), []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// Steps run in order, in the cwd, and stop at the first step that fails. run.sh gets the
// inputs as "$1", "$2".
func TestExecSteps(t *testing.T) {
	dir := t.TempDir()
	sd := script(t, "printf '%s|%s\\n' \"$1\" \"$2\"\nexit 3\n")
	var out bytes.Buffer
	r := Run{Commands: [][]string{{"run.sh", "a b", "$(c)"}, {"/usr/bin/touch", "second"}}, Dir: dir, ScriptDir: sd,
		Timeout: 10 * time.Second, Stdout: &out, Stderr: &out}
	code, err := r.Exec(context.Background())
	if code != 3 || err == nil || !strings.Contains(err.Error(), "step 1") {
		t.Errorf("Exec() = %d, %v, want exit code 3 from step 1", code, err)
	}
	if out.String() != "a b|$(c)\n" {
		t.Errorf("output = %q, want the two inputs as they are", out.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "second")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("second: %v, want no file: step 2 ran after step 1 failed", err)
	}
	r.Commands = [][]string{{"/usr/bin/touch", "first"}, {"/usr/bin/touch", "second"}}
	if code, err := r.Exec(context.Background()); code != 0 || err != nil {
		t.Errorf("Exec() = %d, %v, want 0", code, err)
	}
	for _, f := range []string{"first", "second"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Errorf("%s: %v, want the file in the cwd", f, err)
		}
	}
}

func TestExecNotStart(t *testing.T) {
	r := Run{Commands: [][]string{{filepath.Join(t.TempDir(), "none")}}, Dir: t.TempDir(), Timeout: time.Second}
	if code, err := r.Exec(context.Background()); code != ExitNotStart || err == nil {
		t.Errorf("Exec() = %d, %v, want %d", code, err, ExitNotStart)
	}
}

// The timeout stops the step and its children, and returns ExitTimeout.
func TestExecTimeout(t *testing.T) {
	dir := t.TempDir()
	sd := script(t, "(sleep 2; touch late) &\nsleep 30\n")
	r := Run{Commands: [][]string{{"run.sh"}}, Dir: dir, ScriptDir: sd, Timeout: 300 * time.Millisecond}
	start := time.Now()
	code, err := r.Exec(context.Background())
	if code != ExitTimeout || !errors.Is(err, ErrTimeout) {
		t.Errorf("Exec() = %d, %v, want the timeout", code, err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("Exec() took %s, want it stopped at the timeout", d)
	}
	time.Sleep(2500 * time.Millisecond)
	if _, err := os.Stat(filepath.Join(dir, "late")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("late: %v, want no file: a child kept running after the timeout", err)
	}
}
