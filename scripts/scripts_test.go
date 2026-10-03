// Package scripts tests build.sh and check-tag.sh in a throwaway git copy of the plugin.
package scripts

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

const fakeAsset = "#!/bin/sh\necho brain release asset\n"

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

// fakeRelease writes a release folder with the asset and checksums.txt, and
// returns its file:// base URL for BRAIN_RELEASE_BASE_URL. sum overrides the checksum when not empty.
func fakeRelease(t *testing.T, sum string) string {
	t.Helper()
	base := t.TempDir()
	dir := filepath.Join(base, "v"+manifestVersion(t))
	asset := assetName(t)
	if sum == "" {
		h := sha256.Sum256([]byte(fakeAsset))
		sum = hex.EncodeToString(h[:])
	}
	writeFile(t, filepath.Join(dir, asset), fakeAsset)
	writeFile(t, filepath.Join(dir, "checksums.txt"), sum+"  "+asset+"\n")
	return "file://" + base
}

// noGoPath returns a PATH with only the tools that build.sh uses, and no go.
func noGoPath(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, tool := range []string{"sed", "git", "mkdir", "dirname", "uname", "curl", "awk", "cut", "mktemp", "rm", "chmod", "mv", "sha256sum", "shasum", "perl"} {
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

// AC: with Go on PATH, a source change with no manifest version change is in bin/brain.
func TestBuildWithGoUsesSource(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go is not on PATH")
	}
	dir := newRepo(t)
	t.Setenv("BRAIN_RELEASE_BASE_URL", fakeRelease(t, ""))
	main := filepath.Join(dir, "cmd/brain/main.go")
	b, _ := os.ReadFile(main)
	writeFile(t, main, strings.Replace(string(b), `"brain %s\n"`, `"brain changed %s\n"`, 1))
	git(t, dir, "commit", "-q", "-am", "source change")

	if out, err := runScript(t, dir, os.Getenv("PATH"), "scripts/build.sh"); err != nil {
		t.Fatalf("build.sh: %v\n%s", err, out)
	}
	out, err := exec.Command(filepath.Join(dir, "bin/brain"), "version").CombinedOutput()
	if err != nil {
		t.Fatalf("bin/brain version: %v\n%s", err, out)
	}
	if want := "brain changed " + manifestVersion(t); !strings.Contains(string(out), want) {
		t.Errorf("bin/brain version = %q, want %q", out, want)
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
	if err != nil || string(got) != fakeAsset {
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
			if got, _ := os.ReadFile(filepath.Join(dir, "bin/brain")); string(got) != fakeAsset {
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
