package update

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// nonceRoot returns a new plugin root whose bin/.install-nonce holds nonce.
func nonceRoot(t *testing.T, nonce string) string {
	t.Helper()
	root := t.TempDir()
	mkdir(t, filepath.Join(root, "bin"))
	write(t, filepath.Join(root, "bin", nonceFile), nonce, 0o600)
	return root
}

// #427: only a GitHub install of the built commit whose plugin root holds the nonce of the
// build is the install of the build. The install time does not count.
func TestOurs(t *testing.T) {
	const commit = "c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1"
	ours1 := nonceRoot(t, "n1")
	for _, c := range []struct {
		name, kind, commit, root string
		want                     bool
	}{
		{"install of this build", "github", commit, ours1, true},
		{"install of another build", "github", commit, nonceRoot(t, "n2"), false},
		{"install with no nonce", "github", commit, t.TempDir(), false},
		{"root that does not exist", "github", commit, filepath.Join(t.TempDir(), "gone"), false},
		{"other commit", "github", "d2", ours1, false},
		{"linked checkout", "local", commit, ours1, false},
		{"relative root", "github", commit, "bin/..", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			var p plugin
			p.Root, p.Source.Kind, p.Source.Commit = c.root, c.kind, c.commit
			if got := ours(p, commit, "n1"); got != c.want {
				t.Errorf("ours(%s %s %s, nonce n1) = %v, want %v", c.kind, c.commit, c.root, got, c.want)
			}
		})
	}
}

// postInstallHerdr writes a fake herdr that logs each call to the returned log and returns the
// environment that uses it. plugin list lists a GitHub install of commit c1 at root. session
// list has sessions running sessions s0, s1, ... and one stopped session, old. With stall, each
// invoke does not answer; else the invoke in s1 fails.
func postInstallHerdr(t *testing.T, root string, sessions int, stall bool) (environ []string, log string) {
	t.Helper()
	dir := t.TempDir()
	log = filepath.Join(dir, "log")
	list := `{"sessions":[{"name":"old","running":false}`
	for i := range sessions {
		list += fmt.Sprintf(`,{"name":"s%d","running":true}`, i)
	}
	invoke := `"--session s1 "*) echo "no server" >&2; exit 1 ;;`
	if stall {
		invoke = `"--session "*) exec sleep 60 ;;`
	}
	herdr := `#!/bin/sh
echo "$*" >> ` + log + `
case "$*" in
"plugin list --json --plugin brain")
	echo '{"result":{"type":"plugin_list","plugins":[{"plugin_id":"brain","plugin_root":"` + root + `",` +
		`"source":{"kind":"github","resolved_commit":"c1"}}]}}' ;;
"session list --json") echo '` + list + `]}' ;;
` + invoke + `
esac
`
	write(t, filepath.Join(dir, "herdr"), herdr, 0o700)
	return []string{"PATH=" + os.Getenv("PATH"), "HERDR_BIN_PATH=" + filepath.Join(dir, "herdr")}, log
}

// childArgs are the arguments of the child of post-install with nonce and a deadline wait from now.
func childArgs(nonce string, wait time.Duration) []string {
	return []string{"--commit", "c1", "--nonce", nonce, "--deadline", strconv.FormatInt(time.Now().Add(wait).UnixMilli(), 10)}
}

func readLog(t *testing.T, log string) string {
	t.Helper()
	b, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// #427 AC: when herdr lists the install of this build, the child invokes brain.start in each
// running session, also after an invoke fails, and returns the error of that invoke.
func TestPostInstallStartsSessions(t *testing.T) {
	environ, log := postInstallHerdr(t, nonceRoot(t, "n1"), 2, false)
	err := PostInstall(context.Background(), childArgs("n1", 10*time.Second), environ)
	if err == nil || !strings.Contains(err.Error(), "no server") {
		t.Errorf("PostInstall = %v, want the error of the invoke in s1", err)
	}
	want := "plugin list --json --plugin brain\nsession list --json\n" +
		"--session s0 plugin action invoke brain.start\n--session s1 plugin action invoke brain.start\n"
	if got := readLog(t, log); got != want {
		t.Errorf("herdr calls = %q, want %q", got, want)
	}
}

// #427: when herdr lists only the install of another build until the deadline, the child
// invokes nothing and returns no error.
func TestPostInstallGivesUp(t *testing.T) {
	environ, log := postInstallHerdr(t, nonceRoot(t, "n2"), 2, false)
	if err := PostInstall(context.Background(), childArgs("n1", time.Second), environ); err != nil {
		t.Errorf("PostInstall = %v, want nil", err)
	}
	got := readLog(t, log)
	if strings.ReplaceAll(got, "plugin list --json --plugin brain\n", "") != "" || got == "" {
		t.Errorf("herdr calls = %q, want only plugin list", got)
	}
}

// #427: one deadline covers the session list and every invoke. 13 invokes that do not answer
// would take 65 s with only the 5 s deadline of each call.
func TestPostInstallDeadline(t *testing.T) {
	environ, _ := postInstallHerdr(t, nonceRoot(t, "n1"), 13, true)
	start := time.Now()
	done := make(chan error, 1)
	go func() { done <- PostInstall(context.Background(), childArgs("n1", 2*time.Second), environ) }()
	select {
	case err := <-done:
		if err == nil {
			t.Error("PostInstall = nil, want the deadline errors of the invokes")
		}
	case <-time.After(30 * time.Second):
		t.Fatal("PostInstall still runs after 30 s")
	}
	// [NEW] 2 s of deadline, 500 ms of WaitDelay and a margin for a slow machine.
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("PostInstall took %v, want the 2 s deadline", d)
	}
}

// #427: the deadline of a herdr call also ends a child of herdr that holds the output pipes,
// in the process group of herdr or out of it.
func TestCallKillsHerdrChildren(t *testing.T) {
	if _, err := exec.LookPath("perl"); err != nil {
		t.Skip("perl is not on PATH")
	}
	for _, c := range []struct{ name, child string }{
		{"child in the group", "sleep 20 &"},
		{"child in its own session", "perl -e 'use POSIX; POSIX::setsid(); sleep 20' &"},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			write(t, filepath.Join(dir, "herdr"), "#!/bin/sh\n"+c.child+"\nexec sleep 20\n", 0o700)
			u := &updater{environ: []string{"PATH=" + os.Getenv("PATH")}, herdr: filepath.Join(dir, "herdr")}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			start := time.Now()
			if _, err := u.call(ctx, "session", "list"); err == nil {
				t.Error("call = nil, want the deadline error")
			}
			// [NEW] 1 s of deadline, 500 ms of WaitDelay and a margin for a slow machine.
			if d := time.Since(start); d > 4*time.Second {
				t.Errorf("call took %v, want the 1 s deadline", d)
			}
		})
	}
}

func TestPostInstallErrors(t *testing.T) {
	for _, c := range []struct {
		name    string
		args    []string
		environ []string
		want    string
	}{
		{"no commit", nil, nil, "usage: brain post-install"},
		{"extra argument", []string{"--commit", "c1", "x"}, nil, "usage: brain post-install"},
		{"unknown flag", []string{"--commit", "c1", "--x"}, nil, "usage: brain post-install"},
		{"nonce with no deadline", []string{"--commit", "c1", "--nonce", "n1"}, nil, "usage: brain post-install"},
		{"deadline with no nonce", []string{"--commit", "c1", "--deadline", "1"}, nil, "usage: brain post-install"},
		{"bad deadline", []string{"--commit", "c1", "--nonce", "n1", "--deadline", "x"}, nil, "--deadline"},
		{"bad wait", []string{"--commit", "c1"}, []string{"BRAIN_AUTO_SETUP_WAIT=x"}, "BRAIN_AUTO_SETUP_WAIT"},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := PostInstall(context.Background(), c.args, c.environ)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("PostInstall(%q) = %v, want an error with %q", c.args, err, c.want)
			}
		})
	}
}
