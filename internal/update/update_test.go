package update

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// repo is the GitHub repo of the fake install. The test HOME's git config sends its URL to a
// local bare clone, so no test uses the network.
const repo = "acme/brain"

// fixture is a local git remote, a fake herdr and a plugin root with a fake bin/brain.
type fixture struct {
	dir, remote, src, root, log string
	environ                     []string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	dir := t.TempDir()
	f := &fixture{dir: dir, remote: filepath.Join(dir, "remote.git"), src: filepath.Join(dir, "src"),
		root: filepath.Join(dir, "plugins", "github", "brain-x"), log: filepath.Join(dir, "log")}
	home := filepath.Join(dir, "home")
	mkdir(t, home)
	gitconfig := fmt.Sprintf("[url \"file://%s\"]\n\tinsteadOf = https://github.com/%s.git\n"+
		"[user]\n\tname = t\n\temail = t@example.com\n[init]\n\tdefaultBranch = main\n", f.remote, repo)
	write(t, filepath.Join(home, ".gitconfig"), gitconfig, 0o600)
	f.environ = []string{"HOME=" + home, "PATH=" + os.Getenv("PATH"), "GIT_CONFIG_NOSYSTEM=1",
		"HERDR_BIN_PATH=" + filepath.Join(dir, "herdr")}

	f.git(t, "", "init", "-q", "--bare", f.remote)
	f.git(t, "", "init", "-q", f.src)
	write(t, filepath.Join(f.src, ".gitignore"), "bin/\n", 0o600)
	mkdir(t, filepath.Join(f.src, "scripts"))
	// The build of a linked checkout: a new bin/brain.
	write(t, filepath.Join(f.src, "scripts", "build.sh"), "mkdir -p bin\nprintf '#!/bin/sh\\necho \"brain-new $*\" >> "+
		f.log+"\\n' > bin/brain\nchmod +x bin/brain\necho build >> "+f.log+"\n", 0o600)
	f.git(t, f.src, "add", ".")
	f.git(t, f.src, "commit", "-q", "-m", "c1")
	f.git(t, f.src, "push", "-q", f.remote, "HEAD:main")

	// The fake herdr: plugin list prints list.json; plugin install fails when the file
	// "fail" exists, else it puts next.json and next-brain in place, as a reinstall does.
	herdr := fmt.Sprintf(`#!/bin/sh
echo "herdr $*" >> %[1]s/log
case "$1 $2" in
"plugin list") cat %[1]s/list.json ;;
"plugin install")
	if [ -f %[1]s/fail ]; then echo "build failed" >&2; exit 1; fi
	cp %[1]s/next.json %[1]s/list.json
	cp %[1]s/next-brain %[2]s/bin/brain ;;
*) exit 2 ;;
esac
`, dir, f.root)
	write(t, filepath.Join(dir, "herdr"), herdr, 0o700)
	write(t, filepath.Join(dir, "next-brain"), "#!/bin/sh\necho \"brain-new $*\" >> "+f.log+"\n", 0o700)
	return f
}

// managed makes root a managed install at the current remote HEAD, with ref ("" for none).
func (f *fixture) managed(t *testing.T, ref string) {
	t.Helper()
	mkdir(t, filepath.Join(f.root, "bin"))
	write(t, filepath.Join(f.root, "bin", "brain"), "#!/bin/sh\necho \"brain-old $*\" >> "+f.log+"\n", 0o700)
	write(t, filepath.Join(f.dir, "list.json"), f.listJSON("github", f.head(t), ref), 0o600)
}

// linked makes root a clone of the remote, linked as a local plugin.
func (f *fixture) linked(t *testing.T) {
	t.Helper()
	f.git(t, "", "clone", "-q", "https://github.com/"+repo+".git", f.root)
	f.git(t, f.root, "config", "pull.ff", "only")
	mkdir(t, filepath.Join(f.root, "bin"))
	write(t, filepath.Join(f.root, "bin", "brain"), "#!/bin/sh\necho \"brain-old $*\" >> "+f.log+"\n", 0o700)
	write(t, filepath.Join(f.dir, "list.json"), f.listJSON("local", "", ""), 0o600)
}

func (f *fixture) listJSON(kind, commit, ref string) string {
	src := fmt.Sprintf(`{"kind":%q}`, kind)
	if kind == "github" {
		src = fmt.Sprintf(`{"kind":"github","owner":"acme","repo":"brain","requested_ref":%q,"resolved_commit":%q,"managed_path":%q}`,
			ref, commit, f.root)
	}
	return fmt.Sprintf(`{"id":"cli:plugin","result":{"type":"plugin_list","plugins":[{"plugin_id":"brain","name":"Herdr Brain",`+
		`"version":"0.0.1","manifest_path":"%s/herdr-plugin.toml","plugin_root":%q,"enabled":true,"source":%s}]}}`,
		f.root, f.root, src)
}

// push adds a commit to the remote: an update is available.
func (f *fixture) push(t *testing.T) string {
	t.Helper()
	f.git(t, f.src, "commit", "-q", "--allow-empty", "-m", "c2")
	f.git(t, f.src, "push", "-q", f.remote, "HEAD:main")
	head := f.head(t)
	write(t, filepath.Join(f.dir, "next.json"), f.listJSON("github", head, ""), 0o600)
	return head
}

func (f *fixture) head(t *testing.T) string {
	t.Helper()
	return f.git(t, f.src, "rev-parse", "HEAD")
}

func (f *fixture) git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir, cmd.Env = dir, f.environ
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// update runs brain update with args and returns its output.
func (f *fixture) update(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := Main(context.Background(), args, f.environ, &out, &out)
	return out.String(), err
}

// calls returns the log of the fake herdr, git build and brain binaries.
func (f *fixture) calls(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(f.log)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Fatal(err)
	}
	return string(b)
}

func (f *fixture) binary(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(f.root, "bin", "brain"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func mkdir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
}

func write(t *testing.T, path, s string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(s), mode); err != nil {
		t.Fatal(err)
	}
}

func contains(t *testing.T, what, got string, want ...string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(got, w) {
			t.Errorf("%s:\n%s\nwant %q", what, got, w)
		}
	}
}

// AC: --check with the newest commit prints "up to date"; with an older install it prints
// "update available". It changes nothing: herdr gets only the list call.
func TestCheck(t *testing.T) {
	f := newFixture(t)
	f.managed(t, "")
	c1 := f.head(t)
	out, err := f.update(t, "--check")
	if err != nil {
		t.Fatalf("update --check = %v\n%s", err, out)
	}
	contains(t, "brain update --check", out, "installed: brain 0.0.1, commit "+c1[:12]+" (github:acme/brain)",
		"newest:    commit "+c1[:12]+" on the default branch of acme/brain", "up to date")

	c2 := f.push(t)
	before := f.binary(t)
	out, err = f.update(t, "--check")
	if err != nil {
		t.Fatalf("update --check = %v\n%s", err, out)
	}
	contains(t, "brain update --check", out, "commit "+c1[:12], "newest:    commit "+c2[:12], "update available")
	if got := f.calls(t); got != "herdr plugin list --json --plugin brain\nherdr plugin list --json --plugin brain\n" {
		t.Errorf("calls = %q, want only the two plugin lists", got)
	}
	if f.binary(t) != before {
		t.Error("bin/brain changed after --check")
	}
}

// AC: on a managed install, update runs `herdr plugin install <repo> --yes` and then the new
// binary's `brain doctor --fix`. Up to date, it installs nothing.
func TestManagedUpdate(t *testing.T) {
	f := newFixture(t)
	f.managed(t, "")
	if out, err := f.update(t); err != nil || !strings.Contains(out, "up to date") {
		t.Fatalf("update (up to date) = %v\n%s", err, out)
	}
	f.push(t)
	out, err := f.update(t)
	if err != nil {
		t.Fatalf("update = %v\n%s", err, out)
	}
	want := "herdr plugin list --json --plugin brain\nherdr plugin list --json --plugin brain\n" +
		"herdr plugin install acme/brain --yes\nbrain-new doctor --fix\n"
	if got := f.calls(t); got != want {
		t.Errorf("calls =\n%s\nwant\n%s", got, want)
	}
	contains(t, "brain update", out, "update available", "$ "+filepath.Join(f.dir, "herdr")+" plugin install acme/brain --yes")
}

// AC: a failed install keeps the old binary, and update returns the reason (exit 1). It runs
// no doctor.
func TestManagedInstallFails(t *testing.T) {
	f := newFixture(t)
	f.managed(t, "")
	c1 := f.head(t)
	f.push(t)
	write(t, filepath.Join(f.dir, "fail"), "", 0o600)
	before := f.binary(t)
	out, err := f.update(t)
	if err == nil || !strings.Contains(err.Error(), "herdr keeps the old checkout, so Brain stays at commit "+c1[:12]) {
		t.Errorf("update = %v, want the failed install\n%s", err, out)
	}
	contains(t, "brain update", out, "build failed")
	if f.binary(t) != before {
		t.Error("bin/brain changed after a failed install")
	}
	if strings.Contains(f.calls(t), "doctor") {
		t.Errorf("calls = %q, want no doctor", f.calls(t))
	}
}

// An install at a --ref is not moved to the default branch.
func TestManagedRef(t *testing.T) {
	f := newFixture(t)
	f.managed(t, "v0.0.1")
	f.push(t)
	out, err := f.update(t)
	if err == nil || !strings.Contains(err.Error(), "this install is at --ref v0.0.1") {
		t.Errorf("update = %v, want the ref reason\n%s", err, out)
	}
	if strings.Contains(f.calls(t), "install") {
		t.Errorf("calls = %q, want no install", f.calls(t))
	}
}

// AC: a linked checkout off main or with local changes stops with the reason and changes
// nothing. On main and clean, update pulls, builds and runs the new binary's doctor --fix.
func TestLinked(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T, f *fixture)
		want  string // "" for a good update
	}{
		{"off main", func(t *testing.T, f *fixture) { f.git(t, f.root, "switch", "-q", "-c", "work") },
			"is on work, not main; brain update changes nothing"},
		{"detached", func(t *testing.T, f *fixture) { f.git(t, f.root, "switch", "-q", "--detach") },
			"is on a detached HEAD, not main"},
		{"local changes", func(t *testing.T, f *fixture) { write(t, filepath.Join(f.root, "new.go"), "x", 0o600) },
			"has local changes; brain update changes nothing"},
		{"main and clean", func(*testing.T, *fixture) {}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			f.linked(t)
			c1 := f.git(t, f.root, "rev-parse", "HEAD")
			tt.setup(t, f)
			c2 := f.push(t)
			out, err := f.update(t)
			contains(t, "brain update", out, "installed: brain 0.0.1, commit "+c1[:12]+" (linked: "+f.root+")",
				"newest:    commit "+c2[:12])
			if tt.want != "" {
				if err == nil || !strings.Contains(err.Error(), tt.want) {
					t.Errorf("update = %v, want %q\n%s", err, tt.want, out)
				}
				if got := f.git(t, f.root, "rev-parse", "HEAD"); got != c1 {
					t.Errorf("HEAD = %s, want %s (no pull)", got, c1)
				}
				if c := f.calls(t); strings.Contains(c, "build") || strings.Contains(c, "doctor") {
					t.Errorf("calls = %q, want no build and no doctor", c)
				}
				return
			}
			if err != nil {
				t.Fatalf("update = %v\n%s", err, out)
			}
			if got := f.git(t, f.root, "rev-parse", "HEAD"); got != c2 {
				t.Errorf("HEAD after update = %s, want %s", got, c2)
			}
			if c := f.calls(t); !strings.HasSuffix(c, "build\nbrain-new doctor --fix\n") {
				t.Errorf("calls = %q, want the build, then the new doctor --fix", c)
			}
		})
	}
}

// A failed doctor --fix after the update is an error that says the update is done.
func TestDoctorFails(t *testing.T) {
	f := newFixture(t)
	f.managed(t, "")
	f.push(t)
	write(t, filepath.Join(f.dir, "next-brain"), "#!/bin/sh\nexit 1\n", 0o700)
	if out, err := f.update(t); err == nil || !strings.Contains(err.Error(), "the update is done, but brain doctor --fix") {
		t.Errorf("update = %v, want the doctor failure\n%s", err, out)
	}
}

func TestErrors(t *testing.T) {
	f := newFixture(t)
	if _, err := f.update(t, "--chek"); err == nil || !strings.Contains(err.Error(), usage) {
		t.Errorf("update --chek = %v, want the usage", err)
	}
	write(t, filepath.Join(f.dir, "list.json"), `{"id":"cli:plugin","result":{"type":"plugin_list","plugins":[]}}`, 0o600)
	if _, err := f.update(t, "--check"); err == nil || !strings.Contains(err.Error(), "herdr lists no brain plugin") {
		t.Errorf("update with no plugin = %v, want no brain plugin", err)
	}
}
