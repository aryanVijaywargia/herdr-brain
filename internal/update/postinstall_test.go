package update

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// #427: only a GitHub install of the built commit with a later install time is the install of
// the build.
func TestFresh(t *testing.T) {
	const commit = "c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1"
	for _, c := range []struct {
		name, kind, commit string
		installed          uint64
		want               bool
	}{
		{"new install", "github", commit, 2000, true},
		{"entry from before the build", "github", commit, 1000, false},
		{"older entry", "github", commit, 999, false},
		{"no install time", "github", commit, 0, false},
		{"other commit", "github", "d2", 2000, false},
		{"linked checkout", "local", "", 0, false},
		{"linked checkout with a time", "local", commit, 2000, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			var p plugin
			p.Source.Kind, p.Source.Commit, p.Source.Installed = c.kind, c.commit, c.installed
			if got := fresh(p, commit, 1000); got != c.want {
				t.Errorf("fresh(%s %s %d, before 1000) = %v, want %v", c.kind, c.commit, c.installed, got, c.want)
			}
		})
	}
}

// postInstallHerdr writes a fake herdr that logs each call to dir/log and returns the
// environment that uses it. plugin list lists a GitHub install of commit c1 at time 2000.
// session list has default and work, which run, and old, which is stopped. The invoke in
// work fails.
func postInstallHerdr(t *testing.T) (environ []string, log string) {
	t.Helper()
	dir := t.TempDir()
	log = filepath.Join(dir, "log")
	herdr := `#!/bin/sh
echo "$*" >> ` + log + `
case "$*" in
"plugin list --json --plugin brain")
	echo '{"result":{"type":"plugin_list","plugins":[{"plugin_id":"brain","plugin_root":"/p",` +
		`"source":{"kind":"github","resolved_commit":"c1","installed_unix_ms":2000}}]}}' ;;
"session list --json")
	echo '{"sessions":[{"name":"default","running":true},{"name":"work","running":true},{"name":"old","running":false}]}' ;;
"--session work "*) echo "no server" >&2; exit 1 ;;
esac
`
	write(t, filepath.Join(dir, "herdr"), herdr, 0o700)
	return []string{"PATH=" + os.Getenv("PATH"), "HERDR_BIN_PATH=" + filepath.Join(dir, "herdr"),
		"BRAIN_AUTO_SETUP_WAIT=1"}, log
}

// #427 AC: when herdr lists the install, the child invokes brain.start in each running session,
// also after an invoke fails, and returns the error of that invoke.
func TestPostInstallStartsSessions(t *testing.T) {
	environ, log := postInstallHerdr(t)
	err := PostInstall(context.Background(), []string{"--commit", "c1", "--before", "1000"}, environ)
	if err == nil || !strings.Contains(err.Error(), "no server") {
		t.Errorf("PostInstall = %v, want the error of the invoke in work", err)
	}
	b, rerr := os.ReadFile(log)
	if rerr != nil {
		t.Fatal(rerr)
	}
	want := "plugin list --json --plugin brain\nsession list --json\n" +
		"--session default plugin action invoke brain.start\n--session work plugin action invoke brain.start\n"
	if string(b) != want {
		t.Errorf("herdr calls = %q, want %q", b, want)
	}
}

// #427: when herdr lists no install of the build until the wait ends, the child invokes
// nothing and returns no error.
func TestPostInstallGivesUp(t *testing.T) {
	environ, log := postInstallHerdr(t)
	if err := PostInstall(context.Background(), []string{"--commit", "c1", "--before", "2000"}, environ); err != nil {
		t.Errorf("PostInstall = %v, want nil", err)
	}
	b, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.ReplaceAll(string(b), "plugin list --json --plugin brain\n", ""); got != "" || len(b) == 0 {
		t.Errorf("herdr calls = %q, want only plugin list", b)
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
		{"bad --before", []string{"--commit", "c1", "--before", "x"}, nil, "--before"},
		{"bad wait", []string{"--commit", "c1", "--before", "1"}, []string{"BRAIN_AUTO_SETUP_WAIT=x"}, "BRAIN_AUTO_SETUP_WAIT"},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := PostInstall(context.Background(), c.args, c.environ)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("PostInstall(%q) = %v, want an error with %q", c.args, err, c.want)
			}
		})
	}
}
