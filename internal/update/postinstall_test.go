package update

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
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
// build is the install of the build. With the registration that herdr listed for this
// checkout before (old), the entry must also differ from it; the order of install times does
// not count.
func TestOurs(t *testing.T) {
	const commit = "c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1"
	ours1 := nonceRoot(t, "n1")
	reg := func(kind, commit, ref string, ms uint64) *source {
		return &source{Kind: kind, Commit: commit, Ref: ref, Installed: ms}
	}
	// meta is the old registration of the entry at time 2000 with only metadata changed by edit.
	meta := func(edit func(*source)) *source {
		s := &source{Kind: "github", Owner: "acme", Repo: "brain", Commit: commit, Installed: 2000}
		edit(s)
		return s
	}
	for _, c := range []struct {
		name, kind, commit, root string
		ms                       uint64
		old                      *source
		want                     bool
	}{
		{"install of this build", "github", commit, ours1, 2000, nil, true},
		{"install of another build", "github", commit, nonceRoot(t, "n2"), 2000, nil, false},
		{"install with no nonce", "github", commit, t.TempDir(), 2000, nil, false},
		{"root that does not exist", "github", commit, filepath.Join(t.TempDir(), "gone"), 2000, nil, false},
		{"other commit", "github", "d2", ours1, 2000, nil, false},
		{"linked checkout", "local", commit, ours1, 0, nil, false},
		{"relative root", "github", commit, "bin/..", 2000, nil, false},
		{"same root, registration unchanged", "github", commit, ours1, 2000, reg("github", commit, "", 2000), false},
		{"same root, later install time", "github", commit, ours1, 2000, reg("github", commit, "", 1000), true},
		{"same root, lower install time", "github", commit, ours1, 1000, reg("github", commit, "", 2000), true},
		{"same root, other commit before", "github", commit, ours1, 2000, reg("github", "d2", "", 2000), true},
		{"same root, other ref before", "github", commit, ours1, 2000, reg("github", commit, "v0.1.0", 2000), true},
		{"same root, linked before", "github", commit, ours1, 2000, reg("local", "", "", 0), true},
		{"same root, owner case changed", "github", commit, ours1, 2000, meta(func(s *source) { s.Owner = "Acme" }), false},
		{"same root, repo changed", "github", commit, ours1, 2000, meta(func(s *source) { s.Repo = "other" }), false},
		{"same root, subdir changed", "github", commit, ours1, 2000, meta(func(s *source) { s.Subdir = "sub" }), false},
	} {
		t.Run(c.name, func(t *testing.T) {
			var p plugin
			p.Root, p.Source.Kind, p.Source.Commit, p.Source.Installed = c.root, c.kind, c.commit, c.ms
			p.Source.Owner, p.Source.Repo = "acme", "brain"
			if got := ours(p, commit, "n1", c.old); got != c.want {
				t.Errorf("ours(%s %s %s %d, nonce n1, old %+v) = %v, want %v", c.kind, c.commit, c.root, c.ms, c.old, got, c.want)
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

// childArgs are the arguments of the child of post-install with nonce n1 and a deadline wait from now.
func childArgs(wait time.Duration) []string {
	return []string{"--commit", "c1", "--nonce", "n1", "--deadline", strconv.FormatInt(time.Now().Add(wait).UnixMilli(), 10)}
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
	err := PostInstall(context.Background(), childArgs(30*time.Second), environ, strings.NewReader(""))
	if err == nil || !strings.Contains(err.Error(), "no server") {
		t.Errorf("PostInstall = %v, want the error of the invoke in s1", err)
	}
	want := "plugin list --json --plugin brain\nsession list --json\n" +
		"--session s0 plugin action invoke brain.start\n--session s1 plugin action invoke brain.start\n"
	if got := readLog(t, log); got != want {
		t.Errorf("herdr calls = %q, want %q", got, want)
	}
}

// runChild runs the child of PostInstall with the old registration old on stdin and a 30 s
// deadline, and returns its error. A child that finds its install asks no more and invokes
// brain.start, so when the log shows two plugin list calls, the first one found no install, and
// runChild cancels the child. So the test has no time race with the deadline of the child.
func runChild(t *testing.T, environ []string, log, old string) error {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- PostInstall(ctx, childArgs(30*time.Second), environ, strings.NewReader(old)) }()
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case err := <-done:
			return err
		case <-tick.C:
			b, _ := os.ReadFile(log) // no log yet: the child has not asked herdr yet
			if strings.Count(string(b), "plugin list --json --plugin brain\n") >= 2 {
				cancel()
			}
		}
	}
}

// #427: the child reads the old registration of this checkout from stdin. A change in a signal
// of an install starts brain; a change in metadata only, or in no field, does not.
func TestPostInstallOldRegistration(t *testing.T) {
	for _, c := range []struct {
		name, old string
		start     bool
	}{
		{"install time changed", `{"kind":"github","resolved_commit":"c1","installed_unix_ms":5}`, true},
		{"metadata only", `{"kind":"github","owner":"Acme","subdir":"x","resolved_commit":"c1","extra":1}`, false},
		{"no change", `{"kind":"github","resolved_commit":"c1"}`, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			environ, log := postInstallHerdr(t, nonceRoot(t, "n1"), 1, false)
			if err := runChild(t, environ, log, c.old); err != nil {
				t.Errorf("PostInstall = %v, want nil", err)
			}
			if got := strings.Contains(readLog(t, log), "brain.start"); got != c.start {
				t.Errorf("brain.start invoked = %v, want %v", got, c.start)
			}
		})
	}
}

// #427: when herdr lists only the install of another build, the child invokes nothing and
// returns no error.
func TestPostInstallGivesUp(t *testing.T) {
	environ, log := postInstallHerdr(t, nonceRoot(t, "n2"), 2, false)
	if err := runChild(t, environ, log, ""); err != nil {
		t.Errorf("PostInstall = %v, want nil", err)
	}
	got := readLog(t, log)
	if strings.ReplaceAll(got, "plugin list --json --plugin brain\n", "") != "" || got == "" {
		t.Errorf("herdr calls = %q, want only plugin list", got)
	}
}

// slack is the time that a bound in these tests allows for the scheduler. [NEW] 300 ms: the
// race detector and a busy CI machine slow each process start.
const slack = 300 * time.Millisecond

// #427: one deadline covers the session list and every invoke, with the kill of each call in
// it. 13 invokes that do not answer would take 65 s with only the 5 s deadline of each call.
func TestPostInstallDeadline(t *testing.T) {
	environ, _ := postInstallHerdr(t, nonceRoot(t, "n1"), 13, true)
	args := childArgs(2 * time.Second)
	start := time.Now()
	done := make(chan error, 1)
	go func() { done <- PostInstall(context.Background(), args, environ, strings.NewReader("")) }()
	select {
	case err := <-done:
		if err == nil {
			t.Error("PostInstall = nil, want the deadline errors of the invokes")
		}
	case <-time.After(30 * time.Second):
		t.Fatal("PostInstall still runs after 30 s")
	}
	if d := time.Since(start); d > 2*time.Second+slack {
		t.Errorf("PostInstall took %v, want the 2 s deadline", d)
	}
}

// gone reports whether the process pid has exited, within 10 s. An orphan child is a zombie
// until init takes it, so the test asks again until then. [NEW] 10 s: a loaded machine can be
// slow to reap; gone returns at once when the process is gone.
func gone(pid int) bool {
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	end := time.After(10 * time.Second)
	for {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return true
		}
		select {
		case <-end:
			return false
		case <-tick.C:
		}
	}
}

// #427: a herdr call ends by its deadline, the deadline of ctx or 5 s, with the kill of herdr
// in it. After the call, no child of herdr is left in its process group, also after a good
// answer. A child that leaves the group (setsid) is out of scope, but the call still ends in
// time.
func TestCallKillsHerdrChildren(t *testing.T) {
	if _, err := exec.LookPath("perl"); err != nil {
		t.Skip("perl is not on PATH")
	}
	for _, c := range []struct {
		name, script string
		ctx          time.Duration // the deadline of ctx, 0 for none; 3 s, so a slow shell start still writes the pid
		bound        time.Duration // the budget of the call: the deadline of ctx, else herdrDeadline
		ok           bool          // the call returns the answer
		inGroup      bool          // the child stays in the process group of herdr
	}{
		{"answer, child holds the pipes", "sleep 20 & echo $! > pid\necho ok\n", 0, herdrDeadline, false, true},
		{"answer, child holds no pipe", "sleep 20 >/dev/null 2>&1 & echo $! > pid\necho ok\n", 0, herdrDeadline, true, true},
		{"no answer, child in the group", "sleep 20 & echo $! > pid\nexec sleep 20\n", 3 * time.Second, 3 * time.Second, false, true},
		{"no answer, 5 s deadline", "sleep 20 & echo $! > pid\nexec sleep 20\n", 0, herdrDeadline, false, true},
		{"no answer, child in its own session",
			"perl -e 'use POSIX; POSIX::setsid(); open(F, \">pid.new\"); print F $$; close(F); rename(\"pid.new\", \"pid\"); sleep 20' &\n" +
				"while [ ! -f pid ]; do sleep 0.01; done\nexec sleep 20\n",
			3 * time.Second, 3 * time.Second, false, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			write(t, filepath.Join(dir, "herdr"), "#!/bin/sh\ncd "+dir+"\n"+c.script, 0o700)
			u := &updater{environ: []string{"PATH=" + os.Getenv("PATH")}, herdr: filepath.Join(dir, "herdr")}
			ctx := context.Background()
			if c.ctx != 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, c.ctx)
				defer cancel()
			}
			start := time.Now()
			out, err := u.call(ctx, "session", "list")
			d := time.Since(start)
			pid := childPID(t, dir)
			if (err == nil) != c.ok || (c.ok && out != "ok") {
				t.Errorf("call = %q, %v, want answer %v", out, err, c.ok)
			}
			if d > c.bound+slack {
				t.Errorf("call took %v, want at most %v", d, c.bound)
			}
			if c.inGroup && !gone(pid) {
				t.Errorf("the child %d of herdr still runs after the call, want it killed", pid)
			}
		})
	}
}

// childPID reads the pid of the child of the fake herdr in dir, and kills that child when the
// test ends, if it still runs.
func childPID(t *testing.T, dir string) int {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "pid"))
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if syscall.Kill(pid, 0) == nil {
			_ = syscall.Kill(pid, syscall.SIGKILL) // the test started this child; it ends with the test
		}
	})
	return pid
}

func TestPostInstallErrors(t *testing.T) {
	for _, c := range []struct {
		name    string
		args    []string
		environ []string
		stdin   string
		want    string
	}{
		{"no commit", nil, nil, "", "usage: brain post-install"},
		{"extra argument", []string{"--commit", "c1", "x"}, nil, "", "usage: brain post-install"},
		{"unknown flag", []string{"--commit", "c1", "--x"}, nil, "", "usage: brain post-install"},
		{"nonce with no deadline", []string{"--commit", "c1", "--nonce", "n1"}, nil, "", "usage: brain post-install"},
		{"deadline with no nonce", []string{"--commit", "c1", "--deadline", "1"}, nil, "", "usage: brain post-install"},
		{"bad deadline", []string{"--commit", "c1", "--nonce", "n1", "--deadline", "x"}, nil, "", "--deadline"},
		{"bad old registration", childArgs(time.Second), nil, "{", "old registration"},
		{"bad wait", []string{"--commit", "c1"}, []string{"BRAIN_AUTO_SETUP_WAIT=x"}, "", "BRAIN_AUTO_SETUP_WAIT"},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := PostInstall(context.Background(), c.args, c.environ, strings.NewReader(c.stdin))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("PostInstall(%q) = %v, want an error with %q", c.args, err, c.want)
			}
		})
	}
}
