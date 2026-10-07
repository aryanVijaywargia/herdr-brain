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
	if rs, err := Reverts(ctx, dir); err != nil || rs != nil {
		t.Errorf("Reverts(repo with no commit) = %v, %v, want none", rs, err)
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
	rs, err := Reverts(ctx, dir)
	if err != nil {
		t.Fatalf("Reverts = %v", err)
	}
	type got struct {
		Commit   string
		Reverted []string
		Subject  string
		Paths    []string
	}
	var gots []got
	for _, r := range rs {
		if r.At.IsZero() {
			t.Errorf("revert %s has no commit time", r.Commit)
		}
		gots = append(gots, got{r.Commit, r.Reverted, r.Subject, r.Paths})
	}
	want := []got{{undo, []string{bad}, `Revert "add b"`, []string{"b.txt"}},
		{hand, nil, `Revert "add c by hand"`, []string{"c.txt"}},
		{back, []string{addA}, "tidy up", []string{"a.txt"}}}
	if !reflect.DeepEqual(gots, want) {
		t.Errorf("Reverts = %+v, want %+v", gots, want)
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
	if rs, err := Reverts(context.Background(), t.TempDir()); err != nil || rs != nil {
		t.Errorf("Reverts(no repo) = %v, %v, want none", rs, err)
	}
	if now, back, err := WorkChanges(context.Background(), t.TempDir(), Changes{"a.txt": "1111111"}); err != nil ||
		now != nil || back != nil {
		t.Errorf("WorkChanges(no repo) = %v, %v, %v, want none", now, back, err)
	}
}

// AC #283: a file that an agent changed and that goes back to its committed content with no
// commit is a revert (the S7 shape: `git checkout -- <file>`). A further edit, a commit of the
// change and an untracked file are not.
func TestWorkChanges(t *testing.T) {
	gitEnv(t)
	ctx := context.Background()
	dir := t.TempDir()
	run(t, dir, "init", "-q")
	write := func(name, content string) { writeIn(t, dir, name, content) }
	write("src/notes.js", "slow")
	write("src/edit.js", "e1")
	write("src/commit.js", "c1")
	write("src/gone.js", "g1")
	run(t, dir, "add", ".")
	run(t, dir, "commit", "-q", "-m", "init")
	head := map[string]string{}
	for _, p := range []string{"src/notes.js", "src/edit.js", "src/commit.js", "src/gone.js"} {
		head[p] = run(t, dir, "rev-parse", "HEAD:"+p)
	}

	// The agent's turn: it changes four files, deletes one and adds an untracked one.
	write("src/notes.js", "cached")
	write("src/edit.js", "e2")
	write("src/commit.js", "c2")
	write("src/new.js", "n")
	if err := os.Remove(filepath.Join(dir, "src/gone.js")); err != nil {
		t.Fatal(err)
	}
	before, back, err := WorkChanges(ctx, dir, nil)
	if err != nil || back != nil {
		t.Fatalf("WorkChanges(first) = %v, %v, want no back", back, err)
	}
	if !reflect.DeepEqual(before, Changes(head)) {
		t.Fatalf("WorkChanges = %v, want %v", before, head)
	}

	// The user reverts notes.js and gone.js, edits edit.js again and commits commit.js.
	run(t, dir, "checkout", "--", "src/notes.js", "src/gone.js")
	write("src/edit.js", "e3")
	run(t, dir, "commit", "-q", "-m", "keep commit.js", "src/commit.js")
	now, back, err := WorkChanges(ctx, filepath.Join(dir, "src"), before)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"src/gone.js", "src/notes.js"}; !reflect.DeepEqual(back, want) {
		t.Errorf("back = %v, want %v", back, want)
	}
	if want := (Changes{"src/edit.js": head["src/edit.js"]}); !reflect.DeepEqual(now, want) {
		t.Errorf("now = %v, want %v", now, want)
	}

	// The next call finds the same revert no more.
	if _, back, err := WorkChanges(ctx, dir, now); err != nil || back != nil {
		t.Errorf("WorkChanges(again) = %v, %v, want no back", back, err)
	}
}

// writeIn writes content to name in dir, with its folders.
func writeIn(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
