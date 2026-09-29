package signal

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// gitEnv makes git in the test use no user or system config.
func gitEnv(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
}

// run runs git args in dir and returns its output.
func run(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@example.com",
		"-c", "commit.gpgsign=false"}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// commit writes content to name in dir and commits it with message msg.
func commit(t *testing.T, dir, name, content, msg string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	run(t, dir, "add", name)
	run(t, dir, "commit", "-q", "-m", msg)
	return run(t, dir, "rev-parse", "HEAD")
}

func TestReverts(t *testing.T) {
	gitEnv(t)
	ctx := context.Background()
	dir := t.TempDir()
	run(t, dir, "init", "-q")
	if sigs, err := Reverts(ctx, dir); err != nil || sigs != nil {
		t.Errorf("Reverts(repo with no commit) = %v, %v, want none", sigs, err)
	}
	addA := commit(t, dir, "a.txt", "a", "add a")
	bad := commit(t, dir, "b.txt", "b", "add b")
	run(t, dir, "revert", "--no-edit", bad)
	undo := run(t, dir, "rev-parse", "HEAD")
	hand := commit(t, dir, "c.txt", "c", `Revert "add c by hand"`)
	commit(t, dir, "d.txt", "d", "Reverting is not in this subject")
	commit(t, dir, "a.txt", "a2", "change a")
	back := commit(t, dir, "a.txt", "a", "tidy up")
	commit(t, dir, "a.txt", "a3", "change a again")
	run(t, dir, "rm", "-q", "d.txt")
	run(t, dir, "commit", "-q", "-m", "drop d")

	head, status := run(t, dir, "rev-parse", "HEAD"), run(t, dir, "status", "--porcelain")
	index, err := os.ReadFile(filepath.Join(dir, ".git", "index"))
	if err != nil {
		t.Fatal(err)
	}
	sigs, err := Reverts(ctx, dir)
	if err != nil {
		t.Fatalf("Reverts = %v", err)
	}
	want := []Signal{{Kind: Revert, Commits: []string{undo, bad}}, {Kind: Revert, Commits: []string{hand}},
		{Kind: Revert, Commits: []string{back, addA}}}
	if !reflect.DeepEqual(sigs, want) {
		t.Errorf("Reverts = %+v, want %+v", sigs, want)
	}
	after, err := os.ReadFile(filepath.Join(dir, ".git", "index"))
	if err != nil {
		t.Fatal(err)
	}
	if run(t, dir, "rev-parse", "HEAD") != head || run(t, dir, "status", "--porcelain") != status || !bytes.Equal(index, after) {
		t.Error("Reverts changed the repo, want read-only")
	}
}

// A file that goes back to a content older than the log is a revert with no earlier commit.
// A deleted file is not.
func TestGoesBack(t *testing.T) {
	const old, next, zero = "1111111", "2222222", "0000000"
	blobs := map[string]map[string]string{}
	if back := goesBack("c1", ":100644 100644 "+old+" "+next+" M\ta.txt\n", blobs); back != nil {
		t.Errorf("goesBack(change) = %v, want nil", back)
	}
	if back := goesBack("c2", ":100644 100644 "+next+" "+old+" M\ta.txt\n", blobs); back == nil || len(back) != 0 {
		t.Errorf("goesBack(back to a content older than the log) = %v, want an empty list", back)
	}
	if back := goesBack("c3", ":100644 000000 "+old+" "+zero+" D\ta.txt\n", blobs); back != nil {
		t.Errorf("goesBack(delete) = %v, want nil", back)
	}
}

// A workspace root outside git has no reverts.
func TestRevertsOutsideGit(t *testing.T) {
	gitEnv(t)
	t.Setenv("GIT_CEILING_DIRECTORIES", os.TempDir())
	if sigs, err := Reverts(context.Background(), t.TempDir()); err != nil || sigs != nil {
		t.Errorf("Reverts(no repo) = %v, %v, want none", sigs, err)
	}
}
