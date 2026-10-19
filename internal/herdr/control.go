package herdr

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// The calls in this file open, drive and close panes and agents, and start a named herdr
// server. Only the eval runner (E10) uses them, and only in its test session.

// CreateWorkspace opens a workspace with its root pane in folder cwd, from `herdr workspace
// create`. It never takes focus. It returns the root pane.
func (c *Client) CreateWorkspace(ctx context.Context, cwd, label string) (Pane, error) {
	var r struct {
		Root *rawPane `json:"root_pane"`
	}
	err := c.result(ctx, "workspace_created", &r, "workspace", "create", "--cwd", cwd, "--label", label, "--no-focus")
	if err != nil {
		return Pane{}, fmt.Errorf("herdr workspace create: %w", err)
	}
	p, err := r.Root.parse("root_pane")
	if err != nil {
		return Pane{}, fmt.Errorf("herdr workspace create: %w", err)
	}
	return p, nil
}

// SplitPane opens a pane to the right of the pane with id, in folder cwd, from `herdr pane
// split`. It never takes focus.
func (c *Client) SplitPane(ctx context.Context, id, cwd string) (Pane, error) {
	if err := checkPaneID(id); err != nil {
		return Pane{}, fmt.Errorf("herdr pane split: %w", err)
	}
	var r struct {
		Pane *rawPane `json:"pane"`
	}
	err := c.result(ctx, "pane_info", &r, "pane", "split", id, "--direction", "right", "--cwd", cwd, "--no-focus")
	if err != nil {
		return Pane{}, fmt.Errorf("herdr pane split: %w", err)
	}
	p, err := r.Pane.parse("pane")
	if err != nil {
		return Pane{}, fmt.Errorf("herdr pane split: %w", err)
	}
	return p, nil
}

// ClosePane closes the pane with id, from `herdr pane close`. herdr ends the whole process
// group of the pane, and closes the workspace with its last pane.
func (c *Client) ClosePane(ctx context.Context, id string) error {
	if err := checkPaneID(id); err != nil {
		return fmt.Errorf("herdr pane close: %w", err)
	}
	if err := c.result(ctx, "ok", &struct{}{}, "pane", "close", id); err != nil {
		return fmt.Errorf("herdr pane close: %w", err)
	}
	return nil
}

// RunInPane types command and Enter into the shell of the pane with id, from `herdr pane
// run`. It does not wait for the command.
func (c *Client) RunInPane(ctx context.Context, id, command string) error {
	if err := checkPaneID(id); err != nil {
		return fmt.Errorf("herdr pane run: %w", err)
	}
	if _, err := c.run(ctx, "pane", "run", id, command); err != nil {
		return fmt.Errorf("herdr pane run: %w", err)
	}
	return nil
}

// WaitOutput waits until a line of the pane with id matches the regular expression re, from
// `herdr pane wait-output`, and returns that line. herdr stops the wait after timeout.
func (c *Client) WaitOutput(ctx context.Context, id, re string, timeout time.Duration) (string, error) {
	if err := checkPaneID(id); err != nil {
		return "", fmt.Errorf("herdr pane wait-output: %w", err)
	}
	var r struct {
		Line   *string `json:"matched_line"`
		PaneID *string `json:"pane_id"`
	}
	err := c.result(ctx, "output_matched", &r, "pane", "wait-output", id, "--regex", re, "--timeout", millis(timeout))
	if err != nil {
		return "", fmt.Errorf("herdr pane wait-output: %w", err)
	}
	var ck check
	line := req(&ck, r.Line, "matched_line")
	if got := req(&ck, r.PaneID, "pane_id"); ck.err == nil && got != id {
		ck.fail("pane_id", fmt.Sprintf("%q, want %q", got, id))
	}
	if ck.err != nil {
		return "", fmt.Errorf("herdr pane wait-output: %w", ck.err)
	}
	return line, nil
}

// StartAgent starts the official agent CLI of kind (claude or codex) in the pane with id, from
// `herdr agent start`. name is the agent target of the later calls. herdr waits up to 30 s
// for the agent to be ready (herdr --skill); ctx bounds the call.
func (c *Client) StartAgent(ctx context.Context, name, kind, id string) error {
	if err := checkPaneID(id); err != nil {
		return fmt.Errorf("herdr agent start: %w", err)
	}
	var r struct {
		Agent *struct{} `json:"agent"`
	}
	err := c.result(ctx, "agent_started", &r, "agent", "start", name, "--kind", kind, "--pane", id)
	if err != nil {
		return fmt.Errorf("herdr agent start: %w", err)
	}
	var ck check
	if req(&ck, r.Agent, "agent"); ck.err != nil {
		return fmt.Errorf("herdr agent start: %w", ck.err)
	}
	return nil
}

// PromptAgent sends text to the agent target and waits until the agent is done, from `herdr
// agent prompt --wait`.
func (c *Client) PromptAgent(ctx context.Context, target, text string, timeout time.Duration) error {
	var r struct {
		Agent *struct{} `json:"agent"`
	}
	err := c.result(ctx, "agent_prompted", &r, "agent", "prompt", target, text, "--wait", "--timeout", millis(timeout))
	if err != nil {
		return fmt.Errorf("herdr agent prompt: %w", err)
	}
	var ck check
	if req(&ck, r.Agent, "agent"); ck.err != nil {
		return fmt.Errorf("herdr agent prompt: %w", ck.err)
	}
	return nil
}

// ReadAgent returns the last lines of the agent target's screen as plain text, from `herdr
// agent read --source recent-unwrapped`. herdr 0.9.1 prints text here, not JSON.
func (c *Client) ReadAgent(ctx context.Context, target string, lines int) (string, error) {
	out, err := c.run(ctx, "agent", "read", target, "--source", "recent-unwrapped", "--lines", strconv.Itoa(lines))
	if err != nil {
		return "", fmt.Errorf("herdr agent read: %w", err)
	}
	return string(out), nil
}

// ReadAgentVisible returns the screen of the agent target as plain text, from `herdr agent
// read --source visible`. An alternate-screen agent (Claude) has only its screen: a longer
// read fails while it works and scrolls the pane at idle (E11-S1).
func (c *Client) ReadAgentVisible(ctx context.Context, target string) (string, error) {
	out, err := c.run(ctx, "agent", "read", target, "--source", "visible")
	if err != nil {
		return "", fmt.Errorf("herdr agent read: %w", err)
	}
	return string(out), nil
}

func millis(d time.Duration) string { return strconv.FormatInt(d.Milliseconds(), 10) }

// ProcessInfo is the process state of a pane.
type ProcessInfo struct {
	ShellPID int
	// Group is the id of the foreground process group, the pid of its leader: the first process
	// of the command line that the shell runs. 0 when herdr gives none.
	Group     int
	Processes []Process // the foreground processes
}

// PaneProcessInfo returns the shell and the foreground processes of the pane with id, from
// `herdr pane process-info`. It returns ErrPaneNotFound when the pane closed.
func (c *Client) PaneProcessInfo(ctx context.Context, id string) (ProcessInfo, error) {
	if err := checkPaneID(id); err != nil {
		return ProcessInfo{}, fmt.Errorf("herdr pane process-info: %w", err)
	}
	var r struct {
		Info *struct {
			PaneID    *string `json:"pane_id"`
			ShellPID  *int    `json:"shell_pid"`
			Group     *int    `json:"foreground_process_group_id"`
			Processes []struct {
				Name *string  `json:"name"`
				Argv []string `json:"argv"`
				PID  *int     `json:"pid"`
			} `json:"foreground_processes"`
		} `json:"process_info"`
	}
	if err := c.result(ctx, "pane_process_info", &r, "pane", "process-info", "--pane", id); err != nil {
		return ProcessInfo{}, fmt.Errorf("herdr pane process-info: %w", err)
	}
	var ck check
	info := req(&ck, r.Info, "process_info")
	if got := req(&ck, info.PaneID, "process_info.pane_id"); ck.err == nil && got != id {
		ck.fail("process_info.pane_id", fmt.Sprintf("%q, want %q", got, id))
	}
	pi := ProcessInfo{ShellPID: req(&ck, info.ShellPID, "process_info.shell_pid"), Processes: make([]Process, len(info.Processes))}
	if info.Group != nil {
		pi.Group = *info.Group
	}
	for i, p := range info.Processes {
		f := fmt.Sprintf("process_info.foreground_processes[%d]", i)
		pi.Processes[i] = Process{Name: req(&ck, p.Name, f+".name"), Argv: p.Argv, PID: req(&ck, p.PID, f+".pid")}
	}
	if ck.err != nil {
		return ProcessInfo{}, fmt.Errorf("herdr pane process-info: %w", ck.err)
	}
	return pi, nil
}

// Server is a named herdr server that StartServer started.
type Server struct {
	cmd     *exec.Cmd
	bin     string
	environ []string
	name    string
	done    chan struct{} // closed when the server process exits
	exit    error         // the exit of the server process; read it after done
}

// serverPoll is how often StartServer asks herdr whether the new server runs. [NEW]
const serverPoll = 100 * time.Millisecond

// StartServer starts a headless server of the named session, `herdr --session name server`,
// beside the other sessions, and waits until `herdr session list` shows it running. The
// server gets environ without the HERDR_ variables of the caller's pane, so it cannot reach
// another session. It runs in its own process group, so a Ctrl-C of the caller does not
// stop it before the caller's clean-up does. ctx needs a deadline; it bounds only the start.
func StartServer(ctx context.Context, environ []string, name string) (*Server, error) {
	if _, ok := ctx.Deadline(); !ok {
		return nil, errors.New("context has no deadline")
	}
	bin, err := binary(environ)
	if err != nil {
		return nil, err
	}
	env := slices.DeleteFunc(slices.Clone(environ), func(kv string) bool {
		k, _, _ := strings.Cut(kv, "=")
		return strings.HasPrefix(k, "HERDR_") && k != "HERDR_BIN_PATH"
	})
	cmd := exec.Command(bin, "--session", name, "server")
	cmd.Env = env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil { // nil stdio: the server reads and writes /dev/null
		return nil, fmt.Errorf("start herdr server %q: %w", name, err)
	}
	s := &Server{cmd: cmd, bin: bin, environ: env, name: name, done: make(chan struct{})}
	go func() { s.exit = cmd.Wait(); close(s.done) }()
	if err := s.waitRunning(ctx); err != nil {
		_ = cmd.Process.Kill() // the start error is the one to report
		<-s.done
		return nil, fmt.Errorf("start herdr server %q: %w", name, err)
	}
	return s, nil
}

func (s *Server) waitRunning(ctx context.Context) error {
	tick := time.NewTicker(serverPoll)
	defer tick.Stop()
	for {
		ss, err := Sessions(ctx, s.environ)
		if err == nil && slices.ContainsFunc(ss, func(x Session) bool { return x.Name == s.name && x.Running }) {
			select {
			case <-s.done: // another server of that name runs, not this one
				return fmt.Errorf("the server exited: %w", s.exit)
			default:
				return nil
			}
		}
		select {
		case <-s.done:
			return fmt.Errorf("the server exited: %w", s.exit)
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
	}
}

// ErrServerGone says that the server process exited before Stop. The session of that name,
// if one runs, is not this server's, so Stop leaves it.
var ErrServerGone = errors.New("the server process exited before its stop")

// Stop stops the server with `herdr session stop`, waits for its process, and deletes the
// session with `herdr session delete`. ctx needs a deadline. When the process already exited,
// for example because another server of the same name won the start, Stop changes nothing.
func (s *Server) Stop(ctx context.Context) error {
	select {
	case <-s.done:
		return fmt.Errorf("herdr session %s: %w", s.name, errors.Join(ErrServerGone, s.exit))
	default:
	}
	if _, err := run(ctx, s.bin, s.environ, "session", "stop", s.name); err != nil {
		return fmt.Errorf("herdr session stop %s: %w", s.name, err)
	}
	select {
	case <-s.done:
	case <-ctx.Done():
		return fmt.Errorf("herdr session stop %s: the server process did not exit: %w", s.name, ctx.Err())
	}
	if _, err := run(ctx, s.bin, s.environ, "session", "delete", s.name); err != nil {
		return fmt.Errorf("herdr session delete %s: %w", s.name, err)
	}
	return nil
}
