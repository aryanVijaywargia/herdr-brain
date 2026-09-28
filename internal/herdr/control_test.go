package herdr

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestCreateWorkspace(t *testing.T) {
	cmd := "workspace create --cwd /tmp/w --label eval-case --no-focus"
	out := fixture(t, "workspace-created.json")
	c, dir := fakeClient(t, map[string]reply{cmd: {Stdout: out}})
	p, err := c.CreateWorkspace(deadline(t), "/tmp/w", "eval-case")
	want := Pane{ID: "w1:p1", WorkspaceID: "w1", TabID: "w1:t1", Cwd: ptr("/private/tmp/bev-VNlJ/work")}
	if err != nil || !reflect.DeepEqual(p, want) {
		t.Errorf("CreateWorkspace() = %+v, %v, want %+v, nil", p, err, want)
	}
	if cs := calls(t, dir); len(cs) != 1 || cs[0].Session != testSession || cs[0].Socket != testSocket {
		t.Errorf("calls = %+v, want one call to the target", cs)
	}

	c, _ = fakeClient(t, map[string]reply{cmd: {Stdout: strings.Replace(out, `"root_pane"`, `"pane"`, 1)}})
	_, err = c.CreateWorkspace(deadline(t), "/tmp/w", "eval-case")
	wantShape(t, err, "root_pane")
}

func TestSplitPane(t *testing.T) {
	cmd := "pane split w1:p1 --direction right --cwd /tmp/w --no-focus"
	c, _ := fakeClient(t, map[string]reply{cmd: {Stdout: fixture(t, "pane-split.json")}})
	p, err := c.SplitPane(deadline(t), "w1:p1", "/tmp/w")
	if err != nil || p.ID != "w1:p2" || p.WorkspaceID != "w1" {
		t.Errorf("SplitPane() = %+v, %v, want pane w1:p2", p, err)
	}
	if _, err := c.SplitPane(deadline(t), "p1", "/tmp/w"); err == nil {
		t.Error("SplitPane(malformed id) = nil, want an error")
	}
}

func TestClosePane(t *testing.T) {
	cmd := "pane close w1:p2"
	tests := []struct {
		name  string
		reply reply
		check func(error) bool
	}{
		{"closed", reply{Stdout: fixture(t, "pane-closed.json")}, func(err error) bool { return err == nil }},
		{"gone", reply{Stderr: fixture(t, "error-pane-not-found.json"), Code: 1},
			func(err error) bool { return errors.Is(err, ErrPaneNotFound) }},
		{"other type", reply{Stdout: `{"result":{"type":"pane_info"}}`},
			func(err error) bool { var se *ShapeError; return errors.As(err, &se) }},
	}
	for _, tt := range tests {
		c, _ := fakeClient(t, map[string]reply{cmd: tt.reply})
		if err := c.ClosePane(deadline(t), "w1:p2"); !tt.check(err) {
			t.Errorf("%s: ClosePane() = %v", tt.name, err)
		}
	}
}

func TestRunAndWaitOutput(t *testing.T) {
	run := "pane run w1:p1 echo hi; echo brain-eval-done-kest-1234 $?"
	wait := "pane wait-output w1:p1 --regex brain-eval-done-kest-1234 [0-9]+ --timeout 5000"
	out := fixture(t, "output-matched.json")
	c, dir := fakeClient(t, map[string]reply{run: {}, wait: {Stdout: out}})
	if err := c.RunInPane(deadline(t), "w1:p1", "echo hi; echo brain-eval-done-kest-1234 $?"); err != nil {
		t.Fatalf("RunInPane() = %v", err)
	}
	line, err := c.WaitOutput(deadline(t), "w1:p1", "brain-eval-done-kest-1234 [0-9]+", 5*time.Second)
	if err != nil || line != "brain-eval-done-kest-1234 0" {
		t.Errorf("WaitOutput() = %q, %v, want the matched line", line, err)
	}
	if n := len(calls(t, dir)); n != 2 {
		t.Errorf("herdr ran %d times, want 2", n)
	}

	c, _ = fakeClient(t, map[string]reply{wait: {Stdout: strings.Replace(out, `"pane_id":"w1:p1","read"`, `"pane_id":"w1:p9","read"`, 1)}})
	_, err = c.WaitOutput(deadline(t), "w1:p1", "brain-eval-done-kest-1234 [0-9]+", 5*time.Second)
	wantShape(t, err, "pane_id")
}

func TestAgentCalls(t *testing.T) {
	start := "agent start probe-1 --kind claude --pane w1:p2"
	prompt := "agent prompt probe-1 hello --wait --timeout 60000"
	read := "agent read probe-1 --source recent-unwrapped --lines 400"
	c, _ := fakeClient(t, map[string]reply{
		start:  {Stdout: `{"result":{"type":"agent_started","agent":{"name":"probe-1"},"argv":["claude"]}}`},
		prompt: {Stdout: `{"result":{"type":"agent_prompted","agent":{"name":"probe-1"}}}`},
		read:   {Stdout: "> hello\nhi there\n"},
	})
	if err := c.StartAgent(deadline(t), "probe-1", "claude", "w1:p2"); err != nil {
		t.Errorf("StartAgent() = %v", err)
	}
	if err := c.PromptAgent(deadline(t), "probe-1", "hello", time.Minute); err != nil {
		t.Errorf("PromptAgent() = %v", err)
	}
	if got, err := c.ReadAgent(deadline(t), "probe-1", 400); err != nil || got != "> hello\nhi there\n" {
		t.Errorf("ReadAgent() = %q, %v, want the plain text", got, err)
	}

	c, _ = fakeClient(t, map[string]reply{
		start: {Stdout: `{"result":{"type":"agent_started"}}`},
		read:  {Stderr: fixture(t, "error-agent-not-found.json"), Code: 1},
	})
	wantShape(t, c.StartAgent(deadline(t), "probe-1", "claude", "w1:p2"), "agent")
	if _, err := c.ReadAgent(deadline(t), "probe-1", 400); err == nil || !strings.Contains(err.Error(), "agent_not_found") {
		t.Errorf("ReadAgent(no agent) = %v, want agent_not_found", err)
	}
}

func TestPaneProcessInfo(t *testing.T) {
	cmd := "pane process-info --pane w1:p2"
	info := fixture(t, "pane-process-info-sleep.json")
	c, _ := fakeClient(t, map[string]reply{cmd: {Stdout: info}})
	got, err := c.PaneProcessInfo(deadline(t), "w1:p2")
	want := ProcessInfo{ShellPID: 80255, Processes: []Process{{Name: "sleep", Argv: []string{"sleep", "300"}, PID: 80590}}}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("PaneProcessInfo() = %+v, %v, want %+v", got, err, want)
	}
	for field, out := range map[string]string{
		"process_info.shell_pid":                   strings.Replace(info, `"shell_pid":80255`, `"x":1`, 1),
		"process_info.foreground_processes[0].pid": strings.Replace(info, `,"pid":80590`, "", 1),
	} {
		c, _ := fakeClient(t, map[string]reply{cmd: {Stdout: out}})
		_, err := c.PaneProcessInfo(deadline(t), "w1:p2")
		wantShape(t, err, field)
	}
}
