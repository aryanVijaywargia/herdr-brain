// Package scripts tests build.sh and check-tag.sh in a throwaway git copy of the plugin.
package scripts

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
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
// BRAIN_NO_AUTO_SETUP=1 keeps the builds of the other tests away from a real herdr on PATH;
// buildDetached clears it.
func TestMain(m *testing.M) {
	for k, v := range map[string]string{
		"BRAIN_NO_AUTO_SETUP": "1",
		"GIT_CONFIG_COUNT":    "2",
		"GIT_CONFIG_KEY_0":    "maintenance.auto",
		"GIT_CONFIG_VALUE_0":  "false",
		"GIT_CONFIG_KEY_1":    "gc.auto",
		"GIT_CONFIG_VALUE_1":  "0",
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

// copyTracked copies one tracked path into the test repo. A symlink stays a symlink.
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

// The release asset THIRD_PARTY_LICENSES.txt has a license of each module in the binary.
func TestThirdPartyLicenses(t *testing.T) {
	out, err := exec.Command("sh", "third-party-licenses.sh").Output()
	if err != nil {
		t.Fatalf("third-party-licenses.sh: %v", err)
	}
	for _, want := range []string{
		"Go standard library ",
		"modernc.org/sqlite v",
		"github.com/BurntSushi/toml v",
		"golang.org/x/text v",
		"Redistribution and use in source and binary forms",
	} {
		if !strings.Contains(string(out), want) {
			t.Errorf("THIRD_PARTY_LICENSES.txt has no %q", want)
		}
	}
	if strings.Contains(string(out), "github.com/aryanVijaywargia/herdr-brain") {
		t.Error("THIRD_PARTY_LICENSES.txt lists the main module")
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

// fakeHerdr is a herdr that appends each command line to $HERDR_DIR/log. plugin list prints
// $HERDR_DIR/list.json, or no plugin when that file is missing, and does not answer while
// $HERDR_DIR/hang exists. session list has four sessions: default and work run, old is
// stopped, and herdr cannot reach gone.
const fakeHerdr = `#!/bin/sh
echo "$*" >> "$HERDR_DIR/log"
case "$*" in
"plugin list --json --plugin brain")
	[ -e "$HERDR_DIR/hang" ] && exec /bin/sleep 60
	if [ -f "$HERDR_DIR/list.json" ]; then /bin/cat "$HERDR_DIR/list.json"
	else echo '{"id":"cli:plugin","result":{"plugins":[],"type":"plugin_list"}}'; fi ;;
"session list --json")
	echo '{"sessions":[{"name":"default","running":true},{"name":"work","running":true},' \
		'{"name":"old","running":false},{"name":"gone","running":true,"connection_error":"refused"}]}' ;;
esac
`

// herdrDir returns a new folder for the files of fakeHerdr and a path with fakeHerdr in front
// of path.
func herdrDir(t *testing.T, path string) (dir, withHerdr string) {
	t.Helper()
	dir = t.TempDir()
	bin := t.TempDir()
	writeFile(t, filepath.Join(bin, "herdr"), fakeHerdr)
	if err := os.Chmod(filepath.Join(bin, "herdr"), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir, bin + string(os.PathListSeparator) + path
}

// entry is the Brain entry of `herdr plugin list --json` (herdr v0.9.1 and v0.9.5) with source
// kind kind, plugin root root, commit commit and install time ms (0 for none).
type entry struct {
	kind, root, commit string
	ms                 uint64
}

// list writes the reply of `herdr plugin list` with e for fakeHerdr in dir. It writes a new
// file and renames it, so herdr never prints half a file.
func list(t *testing.T, dir string, e entry) {
	t.Helper()
	src := `{"kind":"local"}`
	if e.kind == "github" {
		ms := "" // herdr leaves out a field with no value
		if e.ms != 0 {
			ms = fmt.Sprintf(`,"installed_unix_ms":%d`, e.ms)
		}
		src = fmt.Sprintf(`{"kind":"github","owner":"aryanVijaywargia","repo":"herdr-brain","managed_path":%q,`+
			`"resolved_commit":%q%s}`, e.root, e.commit, ms)
	}
	writeFile(t, filepath.Join(dir, "list.new"), fmt.Sprintf(`{"id":"cli:plugin","result":{"type":"plugin_list",`+
		`"plugins":[{"plugin_id":"brain","version":"0.1.0","plugin_root":%q,"enabled":true,"source":%s}]}}`, e.root, src))
	if err := os.Rename(filepath.Join(dir, "list.new"), filepath.Join(dir, "list.json")); err != nil {
		t.Fatal(err)
	}
}

// installCheckout puts a release checkout at rel in a new herdr config folder and returns it.
func installCheckout(t *testing.T, rel string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), rel)
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(newRepo(t), dir); err != nil {
		t.Fatal(err)
	}
	return dir
}

// head is the commit of HEAD in dir.
func head(t *testing.T, dir string) string {
	t.Helper()
	return strings.TrimSpace(gitOut(t, dir, "rev-parse", "HEAD"))
}

// brainRelease is fakeRelease with a real brain of this checkout as the asset, so the
// post-install of the asset runs.
func brainRelease(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go is not on PATH")
	}
	bin := filepath.Join(t.TempDir(), "brain")
	cmd := exec.Command("go", "build", "-ldflags", "-X main.version="+manifestVersion(t), "-o", bin, "./cmd/brain")
	cmd.Dir = ".."
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	b, err := os.ReadFile(bin)
	if err != nil {
		t.Fatal(err)
	}
	return fakeReleaseOf(t, string(b), "")
}

// buildDetached runs build.sh in dir with PATH path and returns when build.sh exits. Its
// stdout and stderr are pipes, so the return also means that no child holds them. done
// closes when every process that holds fd 3, which build.sh and any child of it inherit, has
// exited. fakeHerdr uses the folder herdr.
func buildDetached(t *testing.T, dir, path, herdr string, env ...string) (done <-chan struct{}) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("/bin/sh", "scripts/build.sh")
	cmd.Dir = dir
	// herdr gives a build no HERDR_ variable, so the test clears those of a herdr pane.
	cmd.Env = append(os.Environ(), append([]string{"PATH=" + path, "HERDR_DIR=" + herdr, "HERDR_ENV=",
		"HERDR_BIN_PATH=", "BRAIN_NO_AUTO_SETUP=", "BRAIN_RELEASE_BASE_URL=" + brainRelease(t)}, env...)...)
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

// calls returns the herdr calls in the log of fakeHerdr in dir, with no `plugin list` calls:
// the child polls it a number of times. polls is the number of those.
func calls(t *testing.T, dir string) (rest string, polls int) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "log"))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Fatal(err)
	}
	var keep []string
	for _, l := range strings.SplitAfter(string(b), "\n") {
		if l == "plugin list --json --plugin brain\n" {
			polls++
		} else if l != "" {
			keep = append(keep, l)
		}
	}
	return strings.Join(keep, ""), polls
}

// nonce writes a nonce file with text n to the bin folder of the plugin root root, as a build
// of post-install does.
func nonce(t *testing.T, root, n string) {
	t.Helper()
	writeFile(t, filepath.Join(root, "bin", ".install-nonce"), n)
}

func rename(t *testing.T, from, to string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(from, to); err != nil {
		t.Fatal(err)
	}
}

// #427 AC: on herdr 0.9.1 and on 0.9.5, an install build returns while its child still runs,
// and the child keeps no build output open. When herdr lists the install of this build, the
// child invokes brain.start in each running session. herdr 0.9.1 builds in a temp folder and
// then moves the checkout to plugins/github/<id>, the root of the old install; herdr 0.9.5
// builds in place. herdr lists the old install until the build ends. The install time of the
// new entry can be lower than the old one, or absent: it does not count.
func TestBuildInstallStartsBrain(t *testing.T) {
	type layout struct {
		name, rel string
		old       bool // herdr lists an older install of the same commit during the build
		inPlace   bool // the new plugin root is the build folder
	}
	for _, l := range []layout{
		{"herdr 0.9.1 first install", "plugins/.tmp-install-123-456/checkout", false, false},
		{"herdr 0.9.1 reinstall", "plugins/.tmp-install-123-456/checkout", true, false},
		{"herdr 0.9.5 first install", "plugins/github-installations/brain-0123/123-0/checkout", false, true},
		{"herdr 0.9.5 reinstall", "plugins/github-installations/brain-0123/123-0/checkout", true, true},
	} {
		for _, b := range []struct {
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
			t.Run(l.name+"/"+b.name, func(t *testing.T) {
				assetName(t)
				dir := installCheckout(t, l.rel)
				commit := head(t, dir)
				hd, path := herdrDir(t, b.path(t, dir))
				old := filepath.Join(t.TempDir(), "plugins", "github", "brain-0123")
				if l.inPlace {
					old = filepath.Join(t.TempDir(), "plugins", "github-installations", "brain-0123", "99-0", "checkout")
				}
				if l.old {
					nonce(t, old, "old")
					list(t, hd, entry{"github", old, commit, 2000})
				}
				done := buildDetached(t, dir, path, hd)
				select {
				case <-done:
					t.Fatal("no child runs after the install build, want one that waits for the install")
				default:
				}
				if got, polls := calls(t, hd); got != "" || polls == 0 {
					t.Errorf("herdr calls before herdr lists the install = %q and %d plugin lists, want only plugin lists", got, polls)
				}
				root := dir
				if !l.inPlace {
					if l.old {
						rename(t, old, old+".previous")
					}
					rename(t, dir, old)
					root = old
				}
				var ms uint64 // a first install with no install time
				if l.old {
					ms = 1000 // lower than the old entry
				}
				list(t, hd, entry{"github", root, commit, ms})
				waitDone(t, done)
				want := "session list --json\n" +
					"--session default plugin action invoke brain.start\n--session work plugin action invoke brain.start\n"
				if got, _ := calls(t, hd); got != want {
					t.Errorf("herdr calls = %q, want %q", got, want)
				}
			})
		}
	}
}

// #427 AC: a build with BRAIN_NO_AUTO_SETUP=1, a build with no herdr on PATH, a build by hand in
// a herdr pane (HERDR_ENV is set) and a build when herdr does not answer start no child.
func TestBuildStartsNoChild(t *testing.T) {
	for _, c := range []struct {
		name  string
		herdr bool
		hang  bool
		env   []string
		polls int // the plugin list calls of the build
	}{
		{"BRAIN_NO_AUTO_SETUP=1", true, false, []string{"BRAIN_NO_AUTO_SETUP=1"}, 0},
		{"no herdr on PATH", false, false, nil, 0},
		{"in a herdr pane", true, false, []string{"HERDR_ENV=1"}, 0},
		{"herdr does not answer", true, true, nil, 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			assetName(t)
			dir := installCheckout(t, "plugins/github-installations/brain-0123/123-0/checkout")
			hd, path := herdrDir(t, noGoPath(t))
			if !c.herdr {
				path = noGoPath(t)
			}
			if c.hang {
				writeFile(t, filepath.Join(hd, "hang"), "")
			}
			select {
			case <-buildDetached(t, dir, path, hd, c.env...):
			case <-time.After(15 * time.Second): // [NEW] above the 5 s deadline of one herdr call
				t.Fatal("a child of the build still runs, want none")
			}
			if got, polls := calls(t, hd); got != "" || polls != c.polls {
				t.Errorf("herdr calls = %q and %d plugin lists, want none and %d", got, polls, c.polls)
			}
		})
	}
}

// #427 AC: herdr lists this checkout as the plugin root before the build. An install in place
// changes the registration of the entry (the commit, the install time, also to a lower one, or
// the source kind), so the child invokes brain.start. A build by hand of the plugin root or of
// a linked checkout changes nothing, so the child invokes nothing and stops at its deadline.
func TestBuildSameRoot(t *testing.T) {
	other := strings.Repeat("0", 40)
	for _, c := range []struct {
		name          string
		before, after func(dir, commit string) entry // after: a zero entry for no change
		start         bool
	}{
		{"install in place over another commit",
			func(dir, _ string) entry { return entry{"github", dir, other, 1000} },
			func(dir, commit string) entry { return entry{"github", dir, commit, 2000} }, true},
		{"reinstall in place, later install time",
			func(dir, commit string) entry { return entry{"github", dir, commit, 1000} },
			func(dir, commit string) entry { return entry{"github", dir, commit, 2000} }, true},
		{"reinstall in place, lower install time",
			func(dir, commit string) entry { return entry{"github", dir, commit, 2000} },
			func(dir, commit string) entry { return entry{"github", dir, commit, 1000} }, true},
		{"install in place over a link",
			func(dir, _ string) entry { return entry{"local", dir, "", 0} },
			func(dir, commit string) entry { return entry{"github", dir, commit, 1000} }, true},
		{"rebuild of the plugin root",
			func(dir, commit string) entry { return entry{"github", dir, commit, 1000} },
			func(string, string) entry { return entry{} }, false},
		{"rebuild of a linked checkout",
			func(dir, _ string) entry { return entry{"local", dir, "", 0} },
			func(string, string) entry { return entry{} }, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			assetName(t)
			dir := installCheckout(t, "plugins/github-installations/brain-0123/123-0/checkout")
			commit := head(t, dir)
			hd, path := herdrDir(t, noGoPath(t))
			list(t, hd, c.before(dir, commit))
			done := buildDetached(t, dir, path, hd, "BRAIN_AUTO_SETUP_WAIT=2")
			if e := c.after(dir, commit); e.kind != "" {
				list(t, hd, e)
			}
			_, early := calls(t, hd)
			waitDone(t, done)
			want := ""
			if c.start {
				want = "session list --json\n" +
					"--session default plugin action invoke brain.start\n--session work plugin action invoke brain.start\n"
			}
			if got, polls := calls(t, hd); got != want || (!c.start && polls <= early) {
				t.Errorf("herdr calls = %q and %d plugin lists (%d before the change), want %q, "+
					"and with no start, plugin lists also after the change", got, polls, early, want)
			}
		})
	}
}

// #427 AC: the child invokes nothing and stops after BRAIN_AUTO_SETUP_WAIT for a plain build, a
// linked checkout, an install of another commit, an old install of this commit, an install of
// this commit from another checkout (a build by hand, then `herdr plugin install` of the same
// ref), and a herdr that does not answer after the build.
func TestBuildChildGivesUp(t *testing.T) {
	for _, c := range []struct {
		name   string
		before bool // herdr lists an old install of this commit before the build
		hang   bool // herdr does not answer after the build
		// after returns the entry that herdr lists after the build; a zero entry for none. It
		// runs before the build, so the child sees the entry before its deadline.
		after func(t *testing.T, dir, commit string) entry
	}{
		{"plain build", false, false, nil},
		{"linked checkout", false, false, func(_ *testing.T, dir, _ string) entry { return entry{"local", dir, "", 0} }},
		{"other commit", false, false, func(_ *testing.T, dir, _ string) entry {
			return entry{"github", dir, strings.Repeat("0", 40), 2000}
		}},
		{"old install of this commit", true, false, nil},
		{"install with BRAIN_NO_AUTO_SETUP=1 of this commit", false, false, func(t *testing.T, dir, commit string) entry {
			other := herdrCheckout(t, dir, "v"+manifestVersion(t))
			if out, err := buildNoGo(t, other, fakeRelease(t, "")); err != nil { // TestMain sets BRAIN_NO_AUTO_SETUP=1
				t.Fatalf("build.sh of the install: %v\n%s", err, out)
			}
			return entry{"github", other, commit, 3000}
		}},
		{"install of this commit by another build", false, false, func(t *testing.T, _, commit string) entry {
			other := t.TempDir()
			nonce(t, other, "another build")
			return entry{"github", other, commit, 3000}
		}},
		{"herdr stops answering", false, true, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			assetName(t)
			dir := newRepo(t)
			commit := head(t, dir)
			hd, path := herdrDir(t, noGoPath(t))
			if c.before {
				old := t.TempDir()
				nonce(t, old, "old")
				list(t, hd, entry{"github", old, commit, 1000})
			}
			var e entry
			if c.after != nil {
				e = c.after(t, dir, commit)
			}
			done := buildDetached(t, dir, path, hd, "BRAIN_AUTO_SETUP_WAIT=2")
			if e.kind != "" {
				list(t, hd, e)
			}
			if c.hang {
				writeFile(t, filepath.Join(hd, "hang"), "")
			}
			_, early := calls(t, hd)
			waitDone(t, done)
			// The child asks herdr again after the change, so it sees the change.
			if got, polls := calls(t, hd); got != "" || polls <= early {
				t.Errorf("herdr calls = %q and %d plugin lists (%d before the change), want only plugin lists, "+
					"also after the change", got, polls, early)
			}
		})
	}
}
