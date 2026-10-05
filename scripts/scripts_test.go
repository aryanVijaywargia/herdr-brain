// Package scripts tests build.sh and check-tag.sh in a throwaway git copy of the plugin.
package scripts

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestMain turns off git auto maintenance and auto gc for the git processes of this package:
// the test helpers and build.sh inherit the environment. The file:// transport clears
// GIT_CONFIG_COUNT for upload-pack, but upload-pack starts no auto maintenance.
// Since Git 2.47, git commit and git fetch run auto maintenance detached, and the
// background process can still write in a repo when t.TempDir removes it (#224).
func TestMain(m *testing.M) {
	for k, v := range map[string]string{
		"GIT_CONFIG_COUNT":   "2",
		"GIT_CONFIG_KEY_0":   "maintenance.auto",
		"GIT_CONFIG_VALUE_0": "false",
		"GIT_CONFIG_KEY_1":   "gc.auto",
		"GIT_CONFIG_VALUE_1": "0",
	} {
		if err := os.Setenv(k, v); err != nil {
			panic(err)
		}
	}
	os.Exit(m.Run())
}

func manifestVersion(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("../herdr-plugin.toml")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^version = "(.*)"`).FindSubmatch(b)
	if m == nil {
		t.Fatal("no version in herdr-plugin.toml")
	}
	return string(m[1])
}

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return string(out)
}

// newRepo copies the tracked tree of this checkout (as it is on disk) into a new git repo,
// commits it and tags the commit v<manifest version>. tagArgs are extra `git tag` flags.
func newRepo(t *testing.T, tagArgs ...string) string {
	t.Helper()
	dir := t.TempDir()
	for _, f := range strings.Split(gitOut(t, "..", "ls-files", "-z"), "\x00") {
		if f != "" {
			copyTracked(t, filepath.Join("..", f), filepath.Join(dir, f))
		}
	}
	git(t, dir, "init", "-q")
	git(t, dir, "add", ".")
	git(t, dir, "commit", "-q", "-m", "release")
	git(t, dir, append(append([]string{"tag"}, tagArgs...), "v"+manifestVersion(t))...)
	return dir
}

// copyTracked copies one tracked path into the test repo. A symlink, such as
// .agents/skills/go-quality, stays a symlink.
func copyTracked(t *testing.T, src, dst string) {
	t.Helper()
	fi, err := os.Lstat(src)
	if errors.Is(err, fs.ErrNotExist) {
		return // deleted in the working copy
	}
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&fs.ModeSymlink == 0 {
		b, err := os.ReadFile(src)
		if err != nil {
			t.Fatal(err)
		}
		writeFile(t, dst, string(b))
		return
	}
	target, err := os.Readlink(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, dst); err != nil {
		t.Fatal(err)
	}
}

// herdrCheckout makes a checkout of ref from src the way `herdr plugin install` does:
// a shallow fetch, detached at FETCH_HEAD, with no local refs or tags.
func herdrCheckout(t *testing.T, src, ref string) string {
	t.Helper()
	dir := t.TempDir()
	git(t, dir, "init", "-q")
	git(t, dir, "remote", "add", "origin", "file://"+src)
	git(t, dir, "fetch", "-q", "--depth", "1", "origin", ref)
	git(t, dir, "checkout", "-q", "FETCH_HEAD")
	if tags := gitOut(t, dir, "tag", "-l"); tags != "" {
		t.Fatalf("herdr-like checkout has local tags %q, want none", tags)
	}
	return dir
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func assetName(t *testing.T) string {
	t.Helper()
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("release assets exist for darwin and linux only")
	}
	return "brain_" + runtime.GOOS + "_" + runtime.GOARCH
}

// fakeAsset is a release asset script that prints "brain <manifest version>", as `brain version` does.
func fakeAsset(t *testing.T) string {
	t.Helper()
	return "#!/bin/sh\necho brain " + manifestVersion(t) + "\n"
}

// fakeRelease writes a release folder with fakeAsset and checksums.txt, and
// returns its file:// base URL for BRAIN_RELEASE_BASE_URL. sum overrides the checksum when not empty.
func fakeRelease(t *testing.T, sum string) string {
	t.Helper()
	return fakeReleaseOf(t, fakeAsset(t), sum)
}

// fakeReleaseOf is fakeRelease with the asset content body.
func fakeReleaseOf(t *testing.T, body, sum string) string {
	t.Helper()
	base := t.TempDir()
	dir := filepath.Join(base, "v"+manifestVersion(t))
	asset := assetName(t)
	if sum == "" {
		h := sha256.Sum256([]byte(body))
		sum = hex.EncodeToString(h[:])
	}
	writeFile(t, filepath.Join(dir, asset), body)
	writeFile(t, filepath.Join(dir, "checksums.txt"), sum+"  "+asset+"\n")
	return "file://" + base
}

// noGoPath returns a PATH with only the tools that build.sh uses, and no go.
func noGoPath(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, tool := range []string{"sed", "git", "mkdir", "dirname", "uname", "curl", "awk", "cut", "mktemp", "rm", "chmod", "mv", "sha256sum", "shasum", "perl", "sleep"} {
		p, err := exec.LookPath(tool)
		if err != nil {
			continue
		}
		if err := os.Symlink(p, filepath.Join(dir, tool)); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func runScript(t *testing.T, dir, path string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command("/bin/sh", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "PATH="+path)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func buildNoGo(t *testing.T, dir, releaseURL string) (string, error) {
	t.Helper()
	t.Setenv("BRAIN_RELEASE_BASE_URL", releaseURL)
	return runScript(t, dir, noGoPath(t), "scripts/build.sh")
}

func assertNoBinary(t *testing.T, dir string) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(dir, "bin/brain")); err == nil {
		t.Error("bin/brain exists, want no binary")
	}
}

// buildWithGo runs build.sh with the test PATH, which has go. It returns stdout and stderr apart.
func buildWithGo(t *testing.T, dir, releaseURL string) (stdout, stderr string, err error) {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go is not on PATH")
	}
	var o, e strings.Builder
	cmd := exec.Command("/bin/sh", "scripts/build.sh")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "BRAIN_RELEASE_BASE_URL="+releaseURL)
	cmd.Stdout, cmd.Stderr = &o, &e
	err = cmd.Run()
	return o.String(), e.String(), err
}

// assertSourceBuild checks that bin/brain is a Go build, not the release asset, and that
// its `version` prints want.
func assertSourceBuild(t *testing.T, dir, want string) {
	t.Helper()
	bin := filepath.Join(dir, "bin/brain")
	if got, _ := os.ReadFile(bin); strings.HasPrefix(string(got), "#!/bin/sh") {
		t.Fatalf("bin/brain = %q, want a source build", got)
	}
	out, err := exec.Command(bin, "version").CombinedOutput()
	if err != nil {
		t.Fatalf("bin/brain version: %v\n%s", err, out)
	}
	if got := strings.TrimSpace(string(out)); got != want {
		t.Errorf("bin/brain version = %q, want %q", got, want)
	}
}

// changeVersionText makes `brain version` print "brain changed <version>".
func changeVersionText(t *testing.T, dir string) {
	t.Helper()
	main := filepath.Join(dir, "cmd/brain/main.go")
	b, err := os.ReadFile(main)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, main, strings.Replace(string(b), `"brain %s\n"`, `"brain changed %s\n"`, 1))
}

// #237 AC: with Go on PATH, at the release tag commit, build.sh installs the checked asset and does not compile.
func TestBuildWithGoOnTagInstallsAsset(t *testing.T) {
	tag := "v" + manifestVersion(t)
	for _, c := range []struct {
		name     string
		checkout func(t *testing.T, src string) string
	}{
		{"local tag", func(_ *testing.T, src string) string { return src }},
		{"herdr checkout", func(t *testing.T, src string) string { return herdrCheckout(t, src, tag) }},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := c.checkout(t, newRepo(t))
			stdout, stderr, err := buildWithGo(t, dir, fakeRelease(t, ""))
			if err != nil || stderr != "" {
				t.Fatalf("build.sh = %v\nstdout: %s\nstderr: %s\nwant success and no stderr", err, stdout, stderr)
			}
			if !strings.Contains(stdout, "release asset "+assetName(t)) {
				t.Errorf("stdout = %q, want the release asset line", stdout)
			}
			if got, _ := os.ReadFile(filepath.Join(dir, "bin/brain")); string(got) != fakeAsset(t) {
				t.Errorf("bin/brain = %q, want the release asset", got)
			}
		})
	}
}

// #237 AC: with Go on PATH, a missing asset, a changed checksum or a wrong version from the
// asset each give one stderr line and a source build.
func TestBuildWithGoBadAssetBuildsSource(t *testing.T) {
	missing := func(t *testing.T) string {
		url := fakeRelease(t, "")
		path := filepath.Join(strings.TrimPrefix(url, "file://"), "v"+manifestVersion(t), assetName(t))
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		return url
	}
	for _, c := range []struct {
		name, reason string
		release      func(t *testing.T) string
	}{
		{"missing asset", "cannot download", missing},
		{"changed checksum", "checksum mismatch for", func(t *testing.T) string { return fakeRelease(t, strings.Repeat("0", 64)) }},
		{"wrong version", "version printed \"brain 0.0.0-wrong\"", func(t *testing.T) string {
			return fakeReleaseOf(t, "#!/bin/sh\necho brain 0.0.0-wrong\n", "")
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := newRepo(t)
			stdout, stderr, err := buildWithGo(t, dir, c.release(t))
			if err != nil {
				t.Fatalf("build.sh: %v\nstdout: %s\nstderr: %s", err, stdout, stderr)
			}
			lines := strings.Split(strings.TrimSuffix(stderr, "\n"), "\n")
			if len(lines) != 1 || !strings.Contains(lines[0], c.reason) || !strings.Contains(lines[0], "building from source") {
				t.Errorf("stderr = %q, want one line with %q and \"building from source\"", stderr, c.reason)
			}
			if !strings.Contains(stdout, "built from source") {
				t.Errorf("stdout = %q, want the source build line", stdout)
			}
			assertSourceBuild(t, dir, "brain "+manifestVersion(t))
		})
	}
}

// #237 AC (and #4): with Go on PATH, on a commit after the tag or with local changes,
// build.sh builds from source and the source change is in bin/brain.
func TestBuildWithGoUsesSource(t *testing.T) {
	for _, c := range []struct {
		name   string
		commit bool
	}{
		{"commit after the tag", true},
		{"local changes", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := newRepo(t)
			changeVersionText(t, dir)
			if c.commit {
				git(t, dir, "commit", "-q", "-am", "source change")
			}
			stdout, stderr, err := buildWithGo(t, dir, fakeRelease(t, ""))
			if err != nil || stderr != "" {
				t.Fatalf("build.sh = %v\nstdout: %s\nstderr: %s\nwant success and no stderr", err, stdout, stderr)
			}
			assertSourceBuild(t, dir, "brain changed "+manifestVersion(t))
		})
	}
}

// AC (logic only): with no Go, on the release tag commit, build.sh installs the checked asset.
func TestBuildWithoutGoOnTagInstallsAsset(t *testing.T) {
	dir := newRepo(t)
	out, err := buildNoGo(t, dir, fakeRelease(t, ""))
	if err != nil {
		t.Fatalf("build.sh: %v\n%s", err, out)
	}
	bin := filepath.Join(dir, "bin/brain")
	got, err := os.ReadFile(bin)
	if err != nil || string(got) != fakeAsset(t) {
		t.Fatalf("bin/brain = %q, %v; want the release asset", got, err)
	}
	if fi, _ := os.Stat(bin); fi.Mode()&0o111 == 0 {
		t.Errorf("bin/brain mode = %v, want executable", fi.Mode())
	}
}

// AC: with no Go, on a commit after the tag, build.sh stops with the "needs Go" error.
func TestBuildWithoutGoAfterTagNeedsGo(t *testing.T) {
	dir := newRepo(t)
	writeFile(t, filepath.Join(dir, "NEXT"), "after the tag\n")
	git(t, dir, "add", ".")
	git(t, dir, "commit", "-q", "-m", "after the tag")
	out, err := buildNoGo(t, dir, fakeRelease(t, ""))
	if err == nil || !strings.Contains(out, "this ref needs Go 1.23 or later") {
		t.Fatalf("build.sh = %v\n%s\nwant the needs-Go error", err, out)
	}
	assertNoBinary(t, dir)
}

// With no Go, local changes on the tag commit are not the release either.
func TestBuildWithoutGoDirtyNeedsGo(t *testing.T) {
	dir := newRepo(t)
	writeFile(t, filepath.Join(dir, "cmd/brain/extra.go"), "package main\n")
	out, err := buildNoGo(t, dir, fakeRelease(t, ""))
	if err == nil || !strings.Contains(out, "this ref needs Go 1.23 or later") {
		t.Fatalf("build.sh = %v\n%s\nwant the needs-Go error", err, out)
	}
	assertNoBinary(t, dir)
}

// C1: a herdr managed checkout has no tags, so build.sh asks origin for the tag commit.
func TestBuildWithoutGoHerdrCheckout(t *testing.T) {
	tag := "v" + manifestVersion(t)
	for _, c := range []struct {
		name    string
		tagArgs []string
	}{
		{"lightweight tag", nil},
		{"annotated tag", []string{"-a", "-m", "release"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			src := newRepo(t, c.tagArgs...)
			dir := herdrCheckout(t, src, tag)
			out, err := buildNoGo(t, dir, fakeRelease(t, ""))
			if err != nil {
				t.Fatalf("build.sh: %v\n%s", err, out)
			}
			if got, _ := os.ReadFile(filepath.Join(dir, "bin/brain")); string(got) != fakeAsset(t) {
				t.Errorf("bin/brain = %q, want the release asset", got)
			}
		})
	}
	t.Run("ref after the tag", func(t *testing.T) {
		src := newRepo(t)
		writeFile(t, filepath.Join(src, "NEXT"), "after the tag\n")
		git(t, src, "add", ".")
		git(t, src, "commit", "-q", "-m", "after the tag")
		dir := herdrCheckout(t, src, "HEAD")
		out, err := buildNoGo(t, dir, fakeRelease(t, ""))
		if err == nil || !strings.Contains(out, "this ref needs Go 1.23 or later") {
			t.Fatalf("build.sh = %v\n%s\nwant the needs-Go error", err, out)
		}
		assertNoBinary(t, dir)
	})
}

// AC: a changed checksum stops the build with a clear error.
func TestBuildWithoutGoChecksumMismatch(t *testing.T) {
	dir := newRepo(t)
	out, err := buildNoGo(t, dir, fakeRelease(t, strings.Repeat("0", 64)))
	if err == nil || !strings.Contains(out, "checksum mismatch for "+assetName(t)) {
		t.Fatalf("build.sh = %v\n%s\nwant the checksum mismatch error", err, out)
	}
	assertNoBinary(t, dir)
}

// With no Go, an asset that prints a wrong version stops the build.
func TestBuildWithoutGoWrongVersion(t *testing.T) {
	dir := newRepo(t)
	out, err := buildNoGo(t, dir, fakeReleaseOf(t, "#!/bin/sh\necho brain 0.0.0-wrong\n", ""))
	if err == nil || !strings.Contains(out, "version printed \"brain 0.0.0-wrong\"") {
		t.Fatalf("build.sh = %v\n%s\nwant the version error", err, out)
	}
	assertNoBinary(t, dir)
}

// AC: a tag with a wrong manifest version fails the release job (release.yml runs check-tag.sh).
func TestCheckTag(t *testing.T) {
	dir := newRepo(t)
	path := os.Getenv("PATH")
	if out, err := runScript(t, dir, path, "scripts/check-tag.sh", "v"+manifestVersion(t)); err != nil {
		t.Errorf("check-tag.sh with the manifest version: %v\n%s", err, out)
	}
	for _, tag := range []string{"v9.9.9", manifestVersion(t), "v" + manifestVersion(t) + "-rc1"} {
		out, err := runScript(t, dir, path, "scripts/check-tag.sh", tag)
		if err == nil || !strings.Contains(out, "does not match the manifest version") {
			t.Errorf("check-tag.sh %s = %v\n%s\nwant a failure", tag, err, out)
		}
	}
}

// #224 AC: no git process of the tests runs auto maintenance or auto gc.
func TestGitAutoMaintenanceOff(t *testing.T) {
	dir := newRepo(t)
	for key, want := range map[string]string{"maintenance.auto": "false", "gc.auto": "0"} {
		if got := strings.TrimSpace(gitOut(t, dir, "config", "--get", key)); got != want {
			t.Errorf("git config %s = %q, want %q", key, got, want)
		}
	}
	out, err := runScript(t, dir, os.Getenv("PATH"), "-c", "git config --get maintenance.auto; git config --get gc.auto")
	if err != nil || out != "false\n0\n" {
		t.Errorf("git config in a script = %v %q, want false and 0", err, out)
	}
}

// fakeHerdr is a herdr that appends each command line to $HERDR_LOG. It lists the brain
// plugin, or no plugin when HERDR_NO_BRAIN is set, and three herdr sessions: default and work
// run, old is stopped. With HERDR_HANG set, plugin list never answers.
const fakeHerdr = `#!/bin/sh
echo "$*" >> "$HERDR_LOG"
[ "$*" = "plugin list --plugin brain" ] && [ -n "${HERDR_HANG:-}" ] && exec sleep 60
case "$*" in
"plugin list --plugin brain")
	if [ -n "${HERDR_NO_BRAIN:-}" ]; then echo "No plugins installed."; else
		printf '1 plugin installed:\n- brain (Herdr Brain) enabled [github]\n'; fi ;;
"session list") printf 'name status directory socket\ndefault running /d /d.sock\nwork running /w /w.sock\nold stopped /o /o.sock\n' ;;
esac
`

// withFakeHerdr returns path with a folder that holds fakeHerdr in front.
func withFakeHerdr(t *testing.T, path string) string {
	t.Helper()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "herdr"), fakeHerdr)
	if err := os.Chmod(filepath.Join(dir, "herdr"), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir + string(os.PathListSeparator) + path
}

// installCheckout puts a release checkout where herdr 0.9.1 builds an install:
// <herdr config>/plugins/.tmp-install-<pid>-<ms>/checkout. It returns the checkout and the
// staging folder.
func installCheckout(t *testing.T) (dir, staging string) {
	t.Helper()
	staging = filepath.Join(t.TempDir(), "plugins", ".tmp-install-123-456")
	if err := os.MkdirAll(staging, 0o755); err != nil {
		t.Fatal(err)
	}
	dir = filepath.Join(staging, "checkout")
	if err := os.Rename(newRepo(t), dir); err != nil {
		t.Fatal(err)
	}
	return dir, staging
}

// buildDetached runs build.sh in dir with PATH path and returns when build.sh exits. Its
// stdout and stderr are pipes, so the return also means that no child holds them. done
// closes when every process that holds fd 3, which build.sh and any child of it inherit, has
// exited. The fake herdr logs to log.
func buildDetached(t *testing.T, dir, path, log string, env ...string) (done <-chan struct{}) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("/bin/sh", "scripts/build.sh")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), append([]string{"PATH=" + path, "HERDR_LOG=" + log,
		"BRAIN_NO_AUTO_SETUP=", "BRAIN_RELEASE_BASE_URL=" + fakeRelease(t, "")}, env...)...)
	cmd.ExtraFiles = []*os.File{w}
	out, err := cmd.CombinedOutput()
	_ = w.Close() // the build has its own copy; the read end gets EOF when all copies close
	if err != nil {
		t.Fatalf("build.sh: %v\n%s", err, out)
	}
	ch := make(chan struct{})
	go func() {
		_, _ = io.Copy(io.Discard, r) // only the EOF matters
		_ = r.Close()                 // the read end of a pipe: Close cannot lose data
		close(ch)
	}()
	return ch
}

// waitDone waits until done closes: the child of the build has exited.
func waitDone(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(30 * time.Second): // [NEW] far above the 1 s polls of the child
		t.Fatal("the build child still runs after 30 s")
	}
}

func readLog(t *testing.T, log string) string {
	t.Helper()
	b, err := os.ReadFile(log)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Fatal(err)
	}
	return string(b)
}

// #239 AC: an install build returns while its child still runs, and the child keeps no build
// output open. When the install ends, the child invokes brain.start in each running session.
func TestBuildInstallStartsBrain(t *testing.T) {
	for _, c := range []struct {
		name string
		path func(t *testing.T, dir string) string
	}{
		{"release asset", func(t *testing.T, _ string) string { return noGoPath(t) }},
		{"source build", func(t *testing.T, dir string) string {
			if _, err := exec.LookPath("go"); err != nil {
				t.Skip("go is not on PATH")
			}
			changeVersionText(t, dir) // local changes: build.sh builds from source
			return os.Getenv("PATH")
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			assetName(t)
			dir, staging := installCheckout(t)
			log := filepath.Join(t.TempDir(), "herdr.log")
			done := buildDetached(t, dir, withFakeHerdr(t, c.path(t, dir)), log)
			select {
			case <-done:
				t.Fatal("no child runs after the install build, want one that waits for the install")
			default:
			}
			if got := readLog(t, log); got != "" {
				t.Errorf("herdr calls before the install ends = %q, want none", got)
			}
			// herdr moves the checkout away and removes the staging folder at the end.
			if err := os.RemoveAll(staging); err != nil {
				t.Fatal(err)
			}
			waitDone(t, done)
			want := "plugin list --plugin brain\nsession list\n" +
				"--session default plugin action invoke brain.start\n--session work plugin action invoke brain.start\n"
			if got := readLog(t, log); got != want {
				t.Errorf("herdr calls = %q, want %q", got, want)
			}
		})
	}
}

// #239 AC: a build outside a herdr install, a build with BRAIN_NO_AUTO_SETUP=1 and an install
// build with no herdr on PATH start no child and call no herdr.
func TestBuildStartsNoChild(t *testing.T) {
	for _, c := range []struct {
		name    string
		install bool
		herdr   bool
		env     []string
	}{
		{"not an install", false, true, nil},
		{"BRAIN_NO_AUTO_SETUP=1", true, true, []string{"BRAIN_NO_AUTO_SETUP=1"}},
		{"no herdr on PATH", true, false, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			assetName(t)
			dir := newRepo(t)
			if c.install {
				dir, _ = installCheckout(t)
			}
			path := noGoPath(t)
			if c.herdr {
				path = withFakeHerdr(t, path)
			}
			log := filepath.Join(t.TempDir(), "herdr.log")
			select {
			case <-buildDetached(t, dir, path, log, c.env...):
			case <-time.After(5 * time.Second): // [NEW] a build with no child closes fd 3 at its exit
				t.Fatal("a child of the build still runs, want none")
			}
			if got := readLog(t, log); got != "" {
				t.Errorf("herdr calls = %q, want none", got)
			}
		})
	}
}

// #239: when the install does not end in time, herdr does not list brain or herdr does not
// answer, the child stops and invokes nothing.
func TestBuildChildGivesUp(t *testing.T) {
	for _, c := range []struct {
		name       string
		endInstall bool
		calls      string // the herdr calls of the child, each line once or more
		env        string
	}{
		{"install does not end", false, "", "HERDR_NO_BRAIN=1"},
		{"brain not listed", true, "plugin list --plugin brain\n", "HERDR_NO_BRAIN=1"},
		{"herdr does not answer", true, "plugin list --plugin brain\n", "HERDR_HANG=1"},
	} {
		t.Run(c.name, func(t *testing.T) {
			assetName(t)
			dir, staging := installCheckout(t)
			log := filepath.Join(t.TempDir(), "herdr.log")
			done := buildDetached(t, dir, withFakeHerdr(t, noGoPath(t)), log,
				"BRAIN_AUTO_SETUP_WAIT=1", c.env)
			if c.endInstall {
				if err := os.RemoveAll(staging); err != nil {
					t.Fatal(err)
				}
			}
			waitDone(t, done)
			got := readLog(t, log)
			if strings.ReplaceAll(got, c.calls, "") != "" || (c.calls != "" && got == "") {
				t.Errorf("herdr calls = %q, want only %q", got, c.calls)
			}
		})
	}
}
