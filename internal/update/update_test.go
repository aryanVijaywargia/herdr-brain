package update

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
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
	answer                      string // the input of update
	records                     int    // the calls of the hook record step of this binary
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
	write(t, filepath.Join(dir, "next-brain"), newBrain(f.log, ""), 0o700)
	return f
}

// newBrain is a fake new bin/brain that logs each call to log. Its hook record step reads one
// answer line and logs it. A call whose first word is fail exits 1.
func newBrain(log, fail string) string {
	return "#!/bin/sh\necho \"brain-new $*\" >> " + log + "\n" +
		"if [ \"$1\" = update ]; then read -r a; echo \"answer $a\" >> " + log + "; fi\n" +
		"if [ \"$1\" = \"" + fail + "\" ]; then exit 1; fi\n"
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
	records := func(in io.Reader, stdout io.Writer) error {
		f.records++
		b, err := io.ReadAll(in)
		_, _ = fmt.Fprintf(stdout, "records step, answer %q\n", b) // the buffer cannot fail
		return err
	}
	err := Main(context.Background(), args, f.environ, strings.NewReader(f.answer), &out, &out, records)
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
	if f.records != 0 {
		t.Errorf("--check ran the hook record step %d times, want none", f.records)
	}
}

// AC: on a managed install, update runs `herdr plugin install <repo> --yes` and then the new
// binary's hook record step, with the input of update, and its `brain doctor --fix` (#315). Up
// to date, it installs nothing and runs the hook record step of this binary.
func TestManagedUpdate(t *testing.T) {
	f := newFixture(t)
	f.managed(t, "")
	f.answer = "y\n"
	if out, err := f.update(t); err != nil || !strings.Contains(out, "up to date\nrecords step, answer \"y\\n\"") ||
		f.records != 1 {
		t.Fatalf("update (up to date) = %v, %d hook record steps\n%s\nwant one with the answer y", err, f.records, out)
	}
	f.push(t)
	out, err := f.update(t)
	if err != nil {
		t.Fatalf("update = %v\n%s", err, out)
	}
	if f.records != 1 {
		t.Errorf("update ran the hook record step of the old binary: %d calls, want 1", f.records)
	}
	want := "herdr plugin list --json --plugin brain\nherdr plugin list --json --plugin brain\n" +
		"herdr plugin install acme/brain --yes\nbrain-new update --records\nanswer y\nbrain-new doctor --fix\n"
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

// tag adds the tags to the remote at the current commit. An annotated tag has its own object
// id, so the tags are annotated and lightweight in turn.
func (f *fixture) tag(t *testing.T, tags ...string) {
	t.Helper()
	for i, tag := range tags {
		args := []string{"tag", tag}
		if i%2 == 1 {
			args = []string{"tag", "-a", "-m", tag, tag}
		}
		f.git(t, f.src, args...)
		f.git(t, f.src, "push", "-q", f.remote, "refs/tags/"+tag)
	}
}

// AC: an install at a release tag follows the newest release tag, compared as a version and
// not as text. Tags that are not release tags and new commits on the default branch do not
// count.
func TestManagedRelease(t *testing.T) {
	ignored := []string{"v9.9", "v10.0.0-rc1", "release-v11.0.0", "V12.0.0", "v99999999999999999999.0.0"}
	tests := []struct {
		name, ref string
		tags      []string
		want      string // the newest release tag
	}{
		{"newer tag", "v0.1.0", []string{"v0.1.0", "v0.2.0"}, "v0.2.0"},
		{"no newer tag", "v0.2.0", []string{"v0.1.0", "v0.2.0"}, "v0.2.0"},
		{"versions, not text", "v0.9.0", []string{"v0.9.0", "v0.10.0", "v0.2.0"}, "v0.10.0"},
		{"newest is two digits", "v0.10.0", []string{"v0.9.0", "v0.10.0"}, "v0.10.0"},
		{"major and patch", "v1.2.3", []string{"v1.2.3", "v1.2.10", "v0.99.0"}, "v1.2.10"},
		{"installed tag not on the remote", "v0.3.0", []string{"v0.2.0"}, "v0.3.0"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			f.managed(t, tt.ref)
			c1 := f.head(t)
			f.tag(t, append(tt.tags, ignored...)...)
			f.push(t)
			write(t, filepath.Join(f.dir, "next.json"), f.listJSON("github", f.head(t), tt.want), 0o600)
			f.answer = "y\n"

			out, err := f.update(t, "--check")
			if err != nil {
				t.Fatalf("update --check = %v\n%s", err, out)
			}
			verdict := "update available"
			if tt.want == tt.ref {
				verdict = "up to date"
			}
			contains(t, "brain update --check", out, "installed: brain 0.0.1, commit "+c1[:12]+" (github:acme/brain --ref "+tt.ref+")",
				"newest:    release tag "+tt.want+" of acme/brain\n"+verdict+"\n")

			out, err = f.update(t)
			if err != nil {
				t.Fatalf("update = %v\n%s", err, out)
			}
			want := "herdr plugin list --json --plugin brain\nherdr plugin list --json --plugin brain\n"
			if tt.want != tt.ref {
				want += "herdr plugin install acme/brain --ref " + tt.want + " --yes\nbrain-new update --records\nanswer y\nbrain-new doctor --fix\n"
			}
			if got := f.calls(t); got != want {
				t.Errorf("calls =\n%s\nwant\n%s", got, want)
			}
		})
	}
}

// AC: an install at a branch, a commit or a tag that is not a release tag is pinned. --check
// says so and prints the command to leave the ref; update says so and installs nothing.
func TestManagedPinned(t *testing.T) {
	for _, ref := range []string{"main", "commit", "v0.1", "nightly"} {
		t.Run(ref, func(t *testing.T) {
			f := newFixture(t)
			if ref == "commit" {
				ref = f.head(t)
			}
			f.managed(t, ref)
			f.tag(t, "v0.1.0", "v0.2.0")
			f.push(t)
			pinned := "this install is pinned to --ref " + ref + ", which is not a release tag, so brain update changes nothing; " +
				"to leave the ref, run: herdr plugin install acme/brain --yes"

			out, err := f.update(t, "--check")
			if err != nil {
				t.Fatalf("update --check = %v\n%s", err, out)
			}
			contains(t, "brain update --check", out, "(github:acme/brain --ref "+ref+")", pinned)
			if strings.Contains(out, "update available") || strings.Contains(out, "up to date") {
				t.Errorf("brain update --check:\n%s\nwant no verdict", out)
			}

			out, err = f.update(t)
			if err == nil || err.Error() != pinned {
				t.Errorf("update = %v, want %q\n%s", err, pinned, out)
			}
			if c := f.calls(t); strings.Contains(c, "install") || strings.Contains(c, "brain-") {
				t.Errorf("calls = %q, want no install and no new binary", c)
			}
			if f.records != 0 {
				t.Errorf("update ran the hook record step %d times, want none", f.records)
			}
		})
	}
}

// AC: a linked checkout off main or with local changes stops with the reason and changes
// nothing. On main and clean, update pulls, builds and runs the new binary's doctor --fix.
func TestLinked(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T, f *fixture)
		want  string // "" for a good update
		ahead bool   // the remote has a newer commit
	}{
		{"off main", func(t *testing.T, f *fixture) { f.git(t, f.root, "switch", "-q", "-c", "work") },
			"is on work, not main; brain update changes nothing", true},
		{"off main at the newest commit", func(t *testing.T, f *fixture) { f.git(t, f.root, "switch", "-q", "-c", "work") },
			"is on work, not main; brain update changes nothing", false},
		{"detached", func(t *testing.T, f *fixture) { f.git(t, f.root, "switch", "-q", "--detach") },
			"is on a detached HEAD, not main", true},
		{"local changes", func(t *testing.T, f *fixture) { write(t, filepath.Join(f.root, "new.go"), "x", 0o600) },
			"has local changes; brain update changes nothing", true},
		{"local changes at the newest commit", func(t *testing.T, f *fixture) { write(t, filepath.Join(f.root, "new.go"), "x", 0o600) },
			"has local changes; brain update changes nothing", false},
		{"main and clean", func(*testing.T, *fixture) {}, "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			f.linked(t)
			c1 := f.git(t, f.root, "rev-parse", "HEAD")
			tt.setup(t, f)
			c2 := c1
			if tt.ahead {
				c2 = f.push(t)
			}
			out, err := f.update(t)
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
			contains(t, "brain update", out, "installed: brain 0.0.1, commit "+c1[:12]+" (linked: "+f.root+")",
				"newest:    commit "+c2[:12])
			if got := f.git(t, f.root, "rev-parse", "HEAD"); got != c2 {
				t.Errorf("HEAD after update = %s, want %s", got, c2)
			}
			if c := f.calls(t); !strings.HasSuffix(c, "build\nbrain-new update --records\nbrain-new doctor --fix\n") {
				t.Errorf("calls = %q, want the build, then the new hook record step and doctor --fix", c)
			}
		})
	}
}

// A failed hook record step or doctor --fix after the update is an error that says the update
// is done. A failed hook record step runs no doctor.
func TestAfterFails(t *testing.T) {
	for _, tt := range []struct{ fail, want string }{
		{"update", "the update is done, but its hook record step reports a problem"},
		{"doctor", "the update is done, but brain doctor --fix reports a problem"},
	} {
		t.Run(tt.fail, func(t *testing.T) {
			f := newFixture(t)
			f.managed(t, "")
			f.push(t)
			write(t, filepath.Join(f.dir, "next-brain"), newBrain(f.log, tt.fail), 0o700)
			if out, err := f.update(t); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("update = %v, want %q\n%s", err, tt.want, out)
			}
			if tt.fail == "update" && strings.Contains(f.calls(t), "doctor") {
				t.Errorf("calls = %q, want no doctor after a failed hook record step", f.calls(t))
			}
		})
	}
}

// AC #315: --records runs only the hook record step of this binary, with the input of update:
// no herdr, no git and no network.
func TestRecordsOnly(t *testing.T) {
	f := newFixture(t)
	f.answer = "n\n"
	out, err := f.update(t, "--records")
	if err != nil || f.records != 1 || out != "records step, answer \"n\\n\"\n" {
		t.Errorf("update --records = %v, %d steps, %q; want one step with the answer n", err, f.records, out)
	}
	if c := f.calls(t); c != "" {
		t.Errorf("calls = %q, want none", c)
	}
}

func TestErrors(t *testing.T) {
	f := newFixture(t)
	if _, err := f.update(t, "--chek"); err == nil || !strings.Contains(err.Error(), Usage) {
		t.Errorf("update --chek = %v, want the usage", err)
	}
	write(t, filepath.Join(f.dir, "list.json"), `{"id":"cli:plugin","result":{"type":"plugin_list","plugins":[]}}`, 0o600)
	if _, err := f.update(t, "--check"); err == nil || !strings.Contains(err.Error(), "herdr lists no brain plugin") {
		t.Errorf("update with no plugin = %v, want no brain plugin", err)
	}
}
