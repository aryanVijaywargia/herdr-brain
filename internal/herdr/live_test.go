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
	ss, err := c.Sessions(deadline(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range ss {
		if s.Name == liveSession && (s.Socket != socket || !s.Running) {
			t.Errorf("session %q = %+v, want running with socket %q", liveSession, s, socket)
		}
	}
}
