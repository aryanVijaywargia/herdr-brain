package main

import (
	"bufio"
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func linkFixture(t *testing.T) (bin, link string) {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	bin = filepath.Join(dir, "plugin", "bin", "brain")
	if err := os.MkdirAll(filepath.Dir(bin), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, filepath.Join(dir, "home", ".local", "bin", "brain")
}

// offer plans the link and, on yes, runs the planned write.
func offer(t *testing.T, bin, link string, yes bool) (string, int, error) {
	t.Helper()
	var out bytes.Buffer
	var p plan
	err := planLink(bin, link, &p, &out)
	text := p.text.String() + out.String()
	if err != nil || !yes {
		return text, len(p.steps), err
	}
	for _, step := range p.steps {
		if err := step(); err != nil {
			return text, len(p.steps), err
		}
	}
	return p.text.String() + out.String(), len(p.steps), nil
}

func TestPlanLinkYes(t *testing.T) {
	bin, link := linkFixture(t)
	out, n, err := offer(t, bin, link, true)
	if err != nil || n != 1 {
		t.Fatalf("%v, %d steps: %s", err, n, out)
	}
	if !strings.Contains(out, "Brain binary: "+bin) || !strings.Contains(out, "Make the symlink "+link) {
		t.Errorf("output does not show the binary path and the link: %q", out)
	}
	if got, err := os.Readlink(link); err != nil || got != bin {
		t.Errorf("link = %q, %v; want %q", got, err, bin)
	}
}

// The plan alone makes no link.
func TestPlanLinkWritesNothing(t *testing.T) {
	bin, link := linkFixture(t)
	if out, _, err := offer(t, bin, link, false); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if _, err := os.Lstat(filepath.Dir(link)); !os.IsNotExist(err) {
		t.Errorf("the plan made %s: %v", filepath.Dir(link), err)
	}
}

func TestPlanLinkKeepsExistingFiles(t *testing.T) {
	bin, link := linkFixture(t)
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}

	// Already linked: no step, nothing changes.
	if err := os.Symlink(bin, link); err != nil {
		t.Fatal(err)
	}
	if out, n, _ := offer(t, bin, link, true); n != 0 || !strings.Contains(out, "already points to it. Already done.") {
		t.Errorf("existing link: %d steps, %q", n, out)
	}

	// Another file or a broken link at the link path: never replaced, and the plan gives the
	// binary path (D3).
	other := filepath.Join(filepath.Dir(link), "other")
	if err := os.WriteFile(other, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, mk := range map[string]func() error{
		"file":   func() error { return os.WriteFile(link, []byte("mine"), 0o755) },
		"other":  func() error { return os.Symlink(other, link) },
		"broken": func() error { return os.Symlink(filepath.Join(filepath.Dir(link), "gone"), link) },
	} {
		if err := os.Remove(link); err != nil { // the link path always holds the previous step's file
			t.Fatal(err)
		}
		if err := mk(); err != nil {
			t.Fatal(err)
		}
		before, _ := os.Readlink(link)
		out, n, err := offer(t, bin, link, true)
		after, _ := os.Readlink(link)
		if err != nil || n != 0 || before != after || !strings.Contains(out, "does not change it") ||
			!strings.Contains(out, "Run Brain with: "+bin) {
			t.Errorf("%s: err %v, %d steps, link %q -> %q, output %q", name, err, n, before, after, out)
		}
	}
}

// `brain setup` through run(): it shows the absolute binary path and makes no link on "no".
func TestSetupCommand(t *testing.T) {
	home := setupHome(t, "0.9.1")
	var out, errOut bytes.Buffer
	if code := run([]string{"setup"}, strings.NewReader("\nn\nn\n"), &out, &errOut); code != 0 {
		t.Fatalf("setup = %d, stderr %q", code, errOut.String())
	}
	if !strings.Contains(out.String(), "Brain binary: /") {
		t.Errorf("setup does not show an absolute binary path: %q", out.String())
	}
	if _, err := os.Lstat(filepath.Join(home, ".local", "bin", "brain")); !os.IsNotExist(err) {
		t.Error("setup made a link after the answer no")
	}
}

type failReader struct{ err error }

func (r failReader) Read([]byte) (int, error) { return 0, r.err }

// Each failure returns one error with the operation as prefix and keeps the cause.
func TestPlanLinkErrors(t *testing.T) {
	cases := map[string]struct {
		prep   func(t *testing.T, link string)
		prefix string
	}{
		"link dir": {
			prep:   func(t *testing.T, link string) { mustMkdir(t, filepath.Dir(filepath.Dir(link)), 0o555) },
			prefix: "create link dir: ",
		},
		"link": {
			prep:   func(t *testing.T, link string) { mustMkdir(t, filepath.Dir(link), 0o555) },
			prefix: "create link: ",
		},
		"lookup": {
			prep:   func(t *testing.T, link string) { mustMkdir(t, filepath.Dir(link), 0o000) },
			prefix: "check link ",
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if os.Geteuid() == 0 {
				t.Skip("root ignores file modes")
			}
			bin, link := linkFixture(t)
			c.prep(t, link)
			_, _, err := offer(t, bin, link, true)
			if err == nil || !strings.HasPrefix(err.Error(), c.prefix) || !errors.Is(err, os.ErrPermission) {
				t.Errorf("err = %v, want prefix %q and cause %v", err, c.prefix, os.ErrPermission)
			}
		})
	}
}

func TestAskReadError(t *testing.T) {
	cause := errors.New("tty gone")
	var out bytes.Buffer
	_, err := ask(bufio.NewReader(failReader{cause}), &out, "Apply all of this? [y/N] ")
	if err == nil || !strings.HasPrefix(err.Error(), "read answer: ") || !errors.Is(err, cause) {
		t.Errorf("err = %v, want prefix %q and cause %v", err, "read answer: ", cause)
	}
}

// run() prints one diagnostic with the command and the operation, and exits 1.
func TestSetupCommandError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores file modes")
	}
	home := setupHome(t, "0.9.1")
	mustMkdir(t, filepath.Join(home, ".local", "bin"), 0o555)
	var out, errOut bytes.Buffer
	// No extractor, then yes: the binding is written, the link fails.
	if code := run([]string{"setup"}, strings.NewReader("\ny\ny\n"), &out, &errOut); code != 1 {
		t.Fatalf("setup = %d, want 1", code)
	}
	lines := strings.Split(strings.TrimSpace(errOut.String()), "\n")
	if len(lines) != 1 || !strings.HasPrefix(lines[0], "brain setup: create link: ") {
		t.Errorf("stderr = %q, want one line starting with %q", errOut.String(), "brain setup: create link: ")
	}
}

// mustMkdir makes dir with mode and gives write access back at cleanup, so TempDir can remove it.
func mustMkdir(t *testing.T, dir string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, mode); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(dir, 0o755); err != nil {
			t.Error(err)
		}
	})
}
