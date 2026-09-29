package herdr

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

const liveSession = "brain-live-test"

// startScratchServer starts a headless herdr server for a named test session with a scratch
// HOME, so the main herdr server is never touched. It returns the herdr binary and the
// scratch HOME. The server stops at the end of the test.
func startScratchServer(t *testing.T) (bin, home string) {
	t.Helper()
	if os.Getenv("BRAIN_LIVE_HERDR") != "1" {
		t.Skip("set BRAIN_LIVE_HERDR=1 to run against a scratch herdr server")
	}
	bin, err := exec.LookPath("herdr")
	if err != nil {
		t.Skip("herdr is not on PATH")
	}
	// Not t.TempDir: a Unix socket path must stay short (104 bytes on macOS).
	home, err = os.MkdirTemp("/tmp", "brain-live-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(home); err != nil {
			t.Errorf("remove scratch HOME: %v", err)
		}
	})

	cmd := exec.Command(bin, "--session", liveSession, "server")
	cmd.Env = []string{"HOME=" + home, "PATH=/usr/bin:/bin"}
	out, err := cmd.StderrPipe() // herdr prints "herdr server running" on stderr
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
			t.Errorf("stop scratch herdr server: %v", err)
		}
		_ = cmd.Wait() // a server stopped by a signal exits with an error
	})
	s := bufio.NewScanner(out)
	for s.Scan() {
		if strings.HasPrefix(s.Text(), "herdr server running") {
			return bin, home
		}
	}
	t.Fatal("scratch herdr server exited before it was ready")
	return "", ""
}

// herdrRun runs the herdr binary with a scratch environment and reports whether it
// succeeded.
func herdrRun(t *testing.T, bin string, env []string, args ...string) bool {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	t.Logf("herdr %s: %s", strings.Join(args, " "), out)
	return err == nil
}

// TestLiveTarget shows, on a scratch named session only, which target herdr 0.9.1 picks
// when --session, HERDR_SOCKET_PATH and HERDR_SESSION disagree, and that the client always
// reaches its own target.
func TestLiveTarget(t *testing.T) {
	bin, home := startScratchServer(t)
	socket := filepath.Join(home, ".config", "herdr", "sessions", liveSession, "herdr.sock")
	bogus := filepath.Join(home, "none.sock")
	base := []string{"HOME=" + home, "PATH=/usr/bin:/bin"}
	if !herdrRun(t, bin, base, "--session", liveSession, "workspace", "create", "--cwd", home, "--label", "brain-live", "--no-focus") {
		t.Fatal("create a workspace in the scratch session")
	}

	tests := []struct {
		name     string
		flag     string // --session value; empty: no flag
		socket   string
		session  string
		wantLive bool
	}{
		{name: "flag wins over both variables", flag: liveSession, socket: bogus, session: "other", wantLive: true},
		{name: "flag of another session wins over the socket", flag: "other", socket: socket, wantLive: false},
		{name: "socket wins over HERDR_SESSION", socket: socket, session: "other", wantLive: true},
		{name: "bad socket wins over HERDR_SESSION", socket: bogus, session: liveSession, wantLive: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := slices.Concat(base, []string{"HERDR_SOCKET_PATH=" + tt.socket})
			if tt.session != "" {
				env = append(env, "HERDR_SESSION="+tt.session)
			}
			var args []string
			if tt.flag != "" {
				args = []string{"--session", tt.flag}
			}
			if got := herdrRun(t, bin, env, append(args, "workspace", "list")...); got != tt.wantLive {
				t.Errorf("reached the live session = %v, want %v", got, tt.wantLive)
			}
		})
	}

	parent := slices.Concat(base, []string{"HERDR_SESSION=other", "HERDR_SOCKET_PATH=" + bogus, "HERDR_BIN_PATH=" + bin})
	c, err := New(parent, Target{Session: liveSession, Socket: socket})
	if err != nil {
		t.Fatal(err)
	}
	ws, err := c.Workspaces(deadline(t))
	if err != nil || len(ws) != 1 || ws[0].Label != "brain-live" {
		t.Errorf("Workspaces() = %+v, %v, want the brain-live workspace", ws, err)
	}
	if _, _, err := c.CheckServer(deadline(t)); err != nil {
		t.Errorf("CheckServer() = %v", err)
	}
	ss, err := Sessions(deadline(t), parent)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range ss {
		if s.Name == liveSession && (s.Socket != socket || !s.Running) {
			t.Errorf("session %q = %+v, want running with socket %q", liveSession, s, socket)
		}
	}
}

// TestLiveServerControl starts a named server with StartServer in a scratch HOME, opens a
// workspace, runs a shell step and waits for its output, splits a pane, reads its processes,
// closes the panes, then stops and deletes the session (E10-S1).
func TestLiveServerControl(t *testing.T) {
	environ, work := liveEnviron(t)
	s, err := StartServer(deadline(t), environ, liveSession)
	if err != nil {
		t.Fatal(err)
	}
	ss, err := Sessions(deadline(t), environ)
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(ss, func(x Session) bool { return x.Name == liveSession && x.Running })
	if i < 0 {
		t.Fatalf("sessions = %+v, want %s running", ss, liveSession)
	}
	c, err := New(environ, Target{Session: liveSession, Socket: ss[i].Socket})
	if err != nil {
		t.Fatal(err)
	}
	root, err := c.CreateWorkspace(deadline(t), work, "eval-case")
	if err != nil {
		t.Fatal(err)
	}
	if err := c.RunInPane(deadline(t), root.ID, "echo hi; echo live-done-kest $?"); err != nil {
		t.Fatal(err)
	}
	if line, err := c.WaitOutput(deadline(t), root.ID, "live-done-kest [0-9]+", 10*time.Second); err != nil || line != "live-done-kest 0" {
		t.Errorf("WaitOutput() = %q, %v", line, err)
	}
	p, err := c.SplitPane(deadline(t), root.ID, work)
	if err != nil {
		t.Fatal(err)
	}
	if pi, err := c.PaneProcessInfo(deadline(t), p.ID); err != nil || pi.ShellPID <= 0 {
		t.Errorf("PaneProcessInfo() = %+v, %v", pi, err)
	}
	for _, id := range []string{p.ID, root.ID} {
		if err := c.ClosePane(deadline(t), id); err != nil {
			t.Errorf("ClosePane(%s) = %v", id, err)
		}
	}
	// The eval guard counts on this: herdr closes a workspace with its last pane.
	if ws, err := c.Workspaces(deadline(t)); err != nil || slices.ContainsFunc(ws, func(w Workspace) bool { return w.Label == "eval-case" }) {
		t.Errorf("Workspaces() after the close = %+v, %v, want no eval-case", ws, err)
	}
	if err := s.Stop(deadline(t)); err != nil {
		t.Fatal(err)
	}
	ss, err = Sessions(deadline(t), environ)
	if err != nil || slices.ContainsFunc(ss, func(x Session) bool { return x.Name == liveSession }) {
		t.Errorf("sessions after Stop = %+v, %v, want no %s", ss, err, liveSession)
	}
}

// liveEnviron returns a scratch environment with a short scratch HOME, and a work folder in
// it. Its HERDR_ variables name another session, which StartServer must not reach.
func liveEnviron(t *testing.T) (environ []string, work string) {
	t.Helper()
	if os.Getenv("BRAIN_LIVE_HERDR") != "1" {
		t.Skip("set BRAIN_LIVE_HERDR=1 to run against a scratch herdr server")
	}
	bin, err := exec.LookPath("herdr")
	if err != nil {
		t.Skip("herdr is not on PATH")
	}
	home, err := os.MkdirTemp("/tmp", "brain-live-") // short: a socket path
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	work = filepath.Join(home, "work")
	if err := os.Mkdir(work, 0o700); err != nil {
		t.Fatal(err)
	}
	return []string{"HOME=" + home, "PATH=/usr/bin:/bin", "HERDR_BIN_PATH=" + bin,
		"HERDR_SOCKET_PATH=/tmp/not-this.sock", "HERDR_SESSION=default"}, work
}
