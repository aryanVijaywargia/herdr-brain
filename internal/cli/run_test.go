package cli

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aryanVijaywargia/herdr-brain/internal/herdr"
	"github.com/aryanVijaywargia/herdr-brain/internal/output"
	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

// runTool is a tool contract named name that runs in folder dir: run.sh writes its input
// word to the file "ran" there, so a test sees whether the tool ran.
func runTool(name, dir, timeout, script string) string {
	return "name = \"" + name + "\"\ndescription = \"Write one word\"\ncwd = \"" + dir + "\"\n" +
		"timeout_s = " + timeout + "\neffects = \"writes the file ran in a temp folder\"\nread_only = false\n" +
		"argv = [\"run.sh\", \"{word}\"]\n\n[inputs.word]\ntype = \"string\"\npattern = \"^[a-z]{1,16}$\"\n" +
		"--- run.sh ---\n" + script
}

// writeWord is the run.sh of runTool: it writes "$1" to ran and prints it.
const writeWord = "printf '%s' \"$1\" > ran\necho \"word $1\"\n"

// runFixture is a fixture with a temp folder that its tools run in.
type runFixture struct {
	*fixture
	dir string
}

func newRunFixture(t *testing.T) *runFixture {
	t.Helper()
	f := newFixture(t)
	f.panes["w15:p1"] = herdr.Pane{ID: "w15:p1", WorkspaceID: "w15", TabID: "w15:t1"}
	return &runFixture{fixture: f, dir: t.TempDir()}
}

// run runs brain run with args from pane in workspace ws.
func (f *runFixture) run(t *testing.T, pane, ws string, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	e := f.env(f.sock, pane, ws, &out)
	e.Environ = append(e.Environ, "HOME="+f.dir)
	err := Run(context.Background(), e, args)
	return out.String(), err
}

// ran is the word that the last run wrote, or "" when no run wrote one; it removes the file.
func (f *runFixture) ran(t *testing.T) string {
	t.Helper()
	path := filepath.Join(f.dir, "ran")
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return ""
	}
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func (f *runFixture) calls(t *testing.T) []store.ToolCall {
	t.Helper()
	s := openStore(t, f.store)
	defer closeStore(t, s)
	var cs []store.ToolCall
	write(t, s, func(tx *sql.Tx) (err error) {
		cs, err = store.ToolCalls(context.Background(), tx)
		return err
	})
	return cs
}

// The agent runs an approved tool of its workspace: the input reaches run.sh as "$1", the
// tool runs in its cwd, its output is the output of brain run, and one ran row logs the call
// with the contract hash, the session of the pane, the exit code and the duration.
func TestRunTool(t *testing.T) {
	f := newRunFixture(t)
	id := f.approvedTool(t, runTool("write-word", f.dir, "10", writeWord))
	hash := f.mustMayRun(t, id).ContractHash
	s := openStore(t, f.store)
	var session int64
	write(t, s, func(tx *sql.Tx) (err error) {
		session, err = store.OpenSession(context.Background(), tx, store.SessionKey{HerdrSession: "default",
			Harness: "claude", Kind: store.KeyAgent, ID: "a1"}, "w14:p1", "ws_a14", testNow)
		return err
	})
	closeStore(t, s)
	out, err := f.run(t, "w14:p1", "w14", "write-word", "--word", "hello")
	if err != nil || out != "word hello\n" {
		t.Fatalf("Run() = %q, %v, want the tool output", out, err)
	}
	if got := f.ran(t); got != "hello" {
		t.Errorf("ran = %q, want hello", got)
	}
	cs := f.calls(t)
	if len(cs) != 1 {
		t.Fatalf("tool_calls = %+v, want one row", cs)
	}
	c := cs[0]
	if c.ToolID != id || c.ContractHash != hash || c.SessionID != session || c.Origin != store.OriginAgent ||
		c.Result != store.CallRan || !c.At.Equal(testNow) || c.ExitCode == nil || *c.ExitCode != 0 {
		t.Errorf("tool_calls[0] = %+v, want a ran row of tool %d with hash %s, session %d and exit code 0", c, id, hash, session)
	}
	// The --<input>=<value> form is the same call.
	if _, err := f.run(t, "w14:p1", "w14", "write-word", "--word=again"); err != nil || f.ran(t) != "again" {
		t.Errorf("Run(--word=again) = %v, want a run", err)
	}
}

// AC S58: an input of `; rm -rf ~` or `$(…)` fails the pattern, and a refused row logs the
// call. With a pattern that lets it pass, it stays one argv element: run.sh gets it as "$1",
// and nothing runs it as shell text.
func TestRunInjection(t *testing.T) {
	for _, v := range []string{"; rm -rf ~", "$(touch pwned)", "`touch pwned`"} {
		t.Run(v, func(t *testing.T) {
			f := newRunFixture(t)
			f.approvedTool(t, runTool("write-word", f.dir, "10", writeWord))
			if _, err := f.run(t, "w14:p1", "w14", "write-word", "--word", v); !errors.Is(err, ErrRefused) ||
				!strings.Contains(err.Error(), "does not match the pattern") {
				t.Errorf("Run(%q) = %v, want the pattern refusal", v, err)
			}
			if got := f.ran(t); got != "" {
				t.Errorf("ran = %q, want no run", got)
			}
			if cs := f.calls(t); len(cs) != 1 || cs[0].Result != store.CallRefused {
				t.Errorf("tool_calls = %+v, want one refused row", cs)
			}

			open := strings.Replace(runTool("write-any", f.dir, "10", writeWord), "^[a-z]{1,16}$", "^.{1,64}$", 1)
			f.approvedTool(t, open)
			out, err := f.run(t, "w14:p1", "w14", "write-any", "--word", v)
			if err != nil || out != "word "+v+"\n" {
				t.Errorf("Run(%q) = %q, %v, want the value as one word", v, out, err)
			}
			if got := f.ran(t); got != v {
				t.Errorf("ran = %q, want %q", got, v)
			}
			if _, err := os.Stat(filepath.Join(f.dir, "pwned")); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("pwned: %v, want no file: the value ran as a command", err)
			}
		})
	}
}

// AC: a tool.toml changed after approval stops the run: brain run refuses it with a refused
// row that holds the approved hash and no exit code, and status shows the tool as stopped.
func TestRunChangedFile(t *testing.T) {
	f := newStatusFixture(t)
	rf := &runFixture{fixture: f.fixture, dir: t.TempDir()}
	rf.panes["w15:p1"] = herdr.Pane{ID: "w15:p1", WorkspaceID: "w15", TabID: "w15:t1"}
	id := rf.approvedTool(t, runTool("write-word", rf.dir, "10", writeWord))
	hash := rf.mustMayRun(t, id).ContractHash
	if out := f.mustStatus(t); !strings.Contains(out, "\ntools: 1 active, none stopped\n") {
		t.Errorf("Status() = %q, want the tool line with none stopped", out)
	}
	path := rf.toolFile(id, 2)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Replace(string(b), "{1,16}", "{1,64}", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = rf.run(t, "w14:p1", "w14", "write-word", "--word", "hello")
	if !errors.Is(err, ErrRefused) || !errors.Is(err, output.ErrMayNotRun) {
		t.Errorf("Run() = %v, want the refusal of a changed tool", err)
	}
	var x *ExitError
	if !errors.As(err, &x) || x.Code != 1 {
		t.Errorf("Run() = %#v, want exit code 1", err)
	}
	if got := rf.ran(t); got != "" {
		t.Errorf("ran = %q, want no run", got)
	}
	cs := rf.calls(t)
	if len(cs) != 1 || cs[0].Result != store.CallRefused || cs[0].ContractHash != hash || cs[0].ExitCode != nil ||
		cs[0].Duration != 0 {
		t.Errorf("tool_calls = %+v, want one refused row with the approved hash and no exit code", cs)
	}
	out := f.mustStatus(t)
	if !strings.Contains(out, "\ntools: 1 active, 1 stopped; brain run refuses them\n") ||
		!strings.Contains(out, fmt.Sprintf("  stopped: tool write-word (item %d): ", id)) ||
		!strings.Contains(out, "changed outside Brain") {
		t.Errorf("Status() = %q, want the tool shown as stopped", out)
	}
}

// AC: a pane in workspace B cannot run a workspace tool of A. It finds no such tool, runs
// nothing and writes no row; a global tool of the same name runs there instead.
func TestRunOtherWorkspace(t *testing.T) {
	f := newRunFixture(t)
	f.approvedTool(t, runTool("write-word", f.dir, "10", writeWord))
	_, err := f.run(t, "w15:p1", "w15", "write-word", "--word", "hello")
	if err == nil || !strings.Contains(err.Error(), "no tool write-word in workspace w15") {
		t.Errorf("Run() from w15 = %v, want no tool", err)
	}
	if got := f.ran(t); got != "" {
		t.Errorf("ran = %q, want no run", got)
	}
	if cs := f.calls(t); len(cs) != 0 {
		t.Errorf("tool_calls = %+v, want none", cs)
	}
	// A global tool of the same name runs in both workspaces, and the workspace tool wins in A.
	s := openStore(t, f.store)
	p, err := saveDraft(context.Background(), s, draft{text: strings.Replace(runTool("write-word", f.dir, "10", writeWord),
		"printf '%s' \"$1\"", "printf 'global %s' \"$1\"", 1), kind: store.KindTool, scope: store.ScopeGlobal, target: "global"}, testNow)
	closeStore(t, s)
	if err != nil {
		t.Fatal(err)
	}
	f.mustReview(t, fmt.Sprint(p), "--approve")
	if _, err := f.run(t, "w15:p1", "w15", "write-word", "--word", "hello"); err != nil || f.ran(t) != "global hello" {
		t.Errorf("Run() from w15 = %v, want the global tool", err)
	}
	if _, err := f.run(t, "w14:p1", "w14", "write-word", "--word", "hello"); err != nil || f.ran(t) != "hello" {
		t.Errorf("Run() from w14 = %v, want the workspace tool", err)
	}
}

// AC: a timeout stops the run and its children, and a ran row logs the call with exit code
// 124.
func TestRunTimeout(t *testing.T) {
	f := newRunFixture(t)
	// run.sh starts a child that would write the file after 3 s, and waits for it.
	f.approvedTool(t, runTool("slow", f.dir, "1", "(sleep 3; printf late > ran) &\nwait\n"))
	_, err := f.run(t, "w14:p1", "w14", "slow", "--word", "hello")
	var x *ExitError
	if !errors.As(err, &x) || x.Code != 124 || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("Run() = %v, want the timeout with exit code 124", err)
	}
	cs := f.calls(t)
	if len(cs) != 1 || cs[0].Result != store.CallRan || cs[0].ExitCode == nil || *cs[0].ExitCode != 124 ||
		cs[0].Duration < 900*time.Millisecond {
		t.Errorf("tool_calls = %+v, want one ran row with exit code 124 and about 1 s", cs)
	}
	waitGone(t, filepath.Join(f.dir, "ran"))
}

// AC: run contract A, approve contract B, and run again: the two rows keep hash A and hash B.
func TestRunKeepsHash(t *testing.T) {
	f := newRunFixture(t)
	a := runTool("write-word", f.dir, "10", writeWord)
	id := f.approvedTool(t, a)
	hashA := f.mustMayRun(t, id).ContractHash
	if _, err := f.run(t, "w14:p1", "w14", "write-word", "--word", "one"); err != nil {
		t.Fatalf("Run(A) = %v", err)
	}
	b := strings.Replace(a, "timeout_s = 10", "timeout_s = 20", 1)
	f.mustReview(t, "--item", fmt.Sprint(id), "--edit", b)
	r := f.rows(t)
	f.mustReview(t, fmt.Sprint(r.proposals[len(r.proposals)-1].ID), "--approve")
	hashB := f.mustMayRun(t, id).ContractHash
	if _, err := f.run(t, "w14:p1", "w14", "write-word", "--word", "two"); err != nil {
		t.Fatalf("Run(B) = %v", err)
	}
	cs := f.calls(t)
	if len(cs) != 2 || hashA == hashB || cs[0].ContractHash != hashA || cs[1].ContractHash != hashB {
		t.Errorf("tool_calls = %+v, want hash A %s, then hash B %s", cs, hashA, hashB)
	}
}

// AC: a refused call writes a refused row, not a ran row: a missing, unknown or wrong input,
// each with the approved hash. (TestRunChangedFile covers a tool that may not run.) A failing tool is a ran row with its
// exit code, and brain run exits with that code.
func TestRunRefused(t *testing.T) {
	f := newRunFixture(t)
	f.approvedTool(t, runTool("write-word", f.dir, "10", writeWord))
	for _, args := range [][]string{
		{"write-word"},
		{"write-word", "--word", "hello", "--other", "x"},
		{"write-word", "--word", "UPPER"},
	} {
		if _, err := f.run(t, "w14:p1", "w14", args...); !errors.Is(err, ErrRefused) {
			t.Errorf("Run(%q) = %v, want a refusal", args, err)
		}
	}
	if got := f.ran(t); got != "" {
		t.Errorf("ran = %q, want no run", got)
	}
	cs := f.calls(t)
	if len(cs) != 3 {
		t.Fatalf("tool_calls = %+v, want 3 rows", cs)
	}
	for _, c := range cs {
		if c.Result != store.CallRefused || c.ExitCode != nil || c.ContractHash == "" {
			t.Errorf("tool_calls row = %+v, want refused with no exit code", c)
		}
	}

	g := newRunFixture(t)
	g.approvedTool(t, runTool("fail", g.dir, "10", "echo no >&2\nexit 3\n"))
	_, err := g.run(t, "w14:p1", "w14", "fail", "--word", "hello")
	var x *ExitError
	if !errors.As(err, &x) || x.Code != 3 || errors.Is(err, ErrRefused) {
		t.Errorf("Run(fail) = %v, want exit code 3", err)
	}
	if cs := g.calls(t); len(cs) != 1 || cs[0].Result != store.CallRan || *cs[0].ExitCode != 3 {
		t.Errorf("tool_calls = %+v, want one ran row with exit code 3", cs)
	}
}

// Outside herdr, in an unbound session, or from a pane that herdr does not confirm, brain run
// refuses, runs nothing and writes nothing.
func TestRunNoStore(t *testing.T) {
	f := newRunFixture(t)
	f.approvedTool(t, runTool("write-word", f.dir, "10", writeWord))
	for name, e := range map[string]Env{
		"outside herdr": func() Env {
			e := f.env(f.sock, "w14:p1", "w14", &bytes.Buffer{})
			e.Environ = []string{"HOME=" + f.dir}
			return e
		}(),
		"unbound":   f.env(filepath.Join(f.dir, "other.sock"), "w14:p1", "w14", &bytes.Buffer{}),
		"pane gone": f.env(f.sock, "w14:p9", "w14", &bytes.Buffer{}),
		"moved":     f.env(f.sock, "w14:p2", "w14", &bytes.Buffer{}),
	} {
		t.Run(name, func(t *testing.T) {
			err := Run(context.Background(), e, []string{"write-word", "--word", "hello"})
			var x *ExitError
			if err == nil || errors.As(err, &x) {
				t.Errorf("Run() = %v, want a refusal with no call", err)
			}
		})
	}
	if got := f.ran(t); got != "" {
		t.Errorf("ran = %q, want no run", got)
	}
	if cs := f.calls(t); len(cs) != 0 {
		t.Errorf("tool_calls = %+v, want none", cs)
	}
}

// The automation runner (E17-S11) passes the pane of its event, not its own: RunTool checks
// that pane with herdr, and logs the call with the origin automation.
func TestRunToolPane(t *testing.T) {
	f := newRunFixture(t)
	f.approvedTool(t, runTool("write-word", f.dir, "10", writeWord))
	// The process runs in no pane of its own.
	e := f.env(f.sock, "", "", &bytes.Buffer{})
	e.Environ = append(e.Environ, "HOME="+f.dir)
	var out bytes.Buffer
	res, err := RunTool(context.Background(), e, ToolRun{PaneID: "w14:p1", WorkspaceID: "w14", Origin: store.OriginAutomation,
		Tool: "write-word", Inputs: map[string]string{"word": "auto"}, Stdout: &out, Stderr: &out})
	if err != nil || res.ExitCode != 0 || res.CallID == 0 || out.String() != "word auto\n" {
		t.Fatalf("RunTool() = %+v, %q, %v, want a run", res, out.String(), err)
	}
	if cs := f.calls(t); len(cs) != 1 || cs[0].Origin != store.OriginAutomation {
		t.Errorf("tool_calls = %+v, want one automation row", cs)
	}
	// A pane of workspace B gets the same check: no tool of A.
	if _, err := RunTool(context.Background(), e, ToolRun{PaneID: "w15:p1", WorkspaceID: "w15",
		Origin: store.OriginAutomation, Tool: "write-word", Inputs: map[string]string{"word": "auto"},
		Stdout: &out, Stderr: &out}); err == nil {
		t.Errorf("RunTool(w15:p1) = nil, want no tool")
	}
}

// waitGone fails when path appears within 3 s: a child of a stopped run wrote it.
func waitGone(t *testing.T, path string) {
	t.Helper()
	time.Sleep(3 * time.Second)
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("%s: %v, want no file: a child of the stopped run kept running", path, err)
	}
}

func TestParseRun(t *testing.T) {
	for _, args := range [][]string{nil, {"--word", "x"}, {"t", "word"}, {"t", "--word"}, {"t", "--w", "1", "--w=2"}, {"t", "--"}} {
		if _, _, err := parseRun(args); err == nil {
			t.Errorf("parseRun(%q) = nil, want an error", args)
		}
	}
}
