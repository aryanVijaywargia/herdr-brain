package main

import (
	"bufio"
	"bytes"
	"errors"
	"io"
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

func offer(t *testing.T, bin, link, answer string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := offerLink(bin, link, bufio.NewReader(strings.NewReader(answer)), &out)
	return out.String(), err
}

func TestOfferLinkYes(t *testing.T) {
	bin, link := linkFixture(t)
	out, err := offer(t, bin, link, "y\n")
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if !strings.Contains(out, "Brain binary: "+bin) {
		t.Errorf("output does not show the binary path: %q", out)
	}
	if got, err := os.Readlink(link); err != nil || got != bin {
		t.Errorf("link = %q, %v; want %q", got, err, bin)
	}
}

func TestOfferLinkNo(t *testing.T) {
	for _, answer := range []string{"n\n", "\n", ""} {
		bin, link := linkFixture(t)
		out, err := offer(t, bin, link, answer)
		if err != nil {
			t.Fatalf("answer %q: %v: %s", answer, err, out)
		}
		if _, err := os.Lstat(link); !os.IsNotExist(err) {
			t.Errorf("answer %q: link was made", answer)
		}
		if !strings.Contains(out, "Run Brain with: "+bin) {
			t.Errorf("answer %q: output does not give the path to use: %q", answer, out)
		}
	}
}

func TestOfferLinkKeepsExistingFiles(t *testing.T) {
	bin, link := linkFixture(t)
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}

	// Already linked: no question, nothing changes.
	if err := os.Symlink(bin, link); err != nil {
		t.Fatal(err)
	}
	if out, _ := offer(t, bin, link, ""); !strings.Contains(out, "already points to it") {
		t.Errorf("existing link: %q", out)
	}

	// Another file or a broken link at the link path: never replaced.
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
		out, err := offer(t, bin, link, "y\n")
		after, _ := os.Readlink(link)
		if err != nil || before != after || !strings.Contains(out, "did not change it") {
			t.Errorf("%s: err %v, link %q -> %q, output %q", name, err, before, after, out)
		}
	}
}

// `brain setup` through run(): it shows the absolute binary path and makes no link on "no".
func TestSetupCommand(t *testing.T) {
	home := setupHome(t, "0.9.1")
	var out, errOut bytes.Buffer
	if code := run([]string{"setup"}, strings.NewReader("n\nn\n"), &out, &errOut); code != 0 {
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
func TestOfferLinkErrors(t *testing.T) {
	cause := errors.New("tty gone")
	cases := map[string]struct {
		prep   func(t *testing.T, link string)
		in     io.Reader
		prefix string
		cause  error
	}{
		"link dir": {
			prep:   func(t *testing.T, link string) { mustMkdir(t, filepath.Dir(filepath.Dir(link)), 0o555) },
			in:     strings.NewReader("y\n"),
			prefix: "create link dir: ",
			cause:  os.ErrPermission,
		},
		"link": {
			prep: func(t *testing.T, link string) {
				mustMkdir(t, filepath.Dir(link), 0o555)
			},
			in:     strings.NewReader("y\n"),
			prefix: "create link: ",
			cause:  os.ErrPermission,
		},
		"input": {
			in:     failReader{cause},
			prefix: "read answer: ",
			cause:  cause,
		},
		"lookup": {
			prep: func(t *testing.T, link string) {
				mustMkdir(t, filepath.Dir(link), 0o000)
			},
			in:     strings.NewReader("y\n"),
			prefix: "check link ",
			cause:  os.ErrPermission,
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if os.Geteuid() == 0 && errors.Is(c.cause, os.ErrPermission) {
				t.Skip("root ignores file modes")
			}
			bin, link := linkFixture(t)
			if c.prep != nil {
				c.prep(t, link)
			}
			var out bytes.Buffer
			err := offerLink(bin, link, bufio.NewReader(c.in), &out)
			if err == nil || !strings.HasPrefix(err.Error(), c.prefix) || !errors.Is(err, c.cause) {
				t.Errorf("err = %v, want prefix %q and cause %v", err, c.prefix, c.cause)
			}
		})
	}
}

// run() prints one diagnostic with the command and the operation, and exits 1.
func TestSetupCommandError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores file modes")
	}
	home := setupHome(t, "0.9.1")
	mustMkdir(t, filepath.Join(home, ".local"), 0o555)
	var out, errOut bytes.Buffer
	// No binding, then yes to the link.
	if code := run([]string{"setup"}, strings.NewReader("n\ny\n"), &out, &errOut); code != 1 {
		t.Fatalf("setup = %d, want 1", code)
	}
	lines := strings.Split(strings.TrimSpace(errOut.String()), "\n")
	if len(lines) != 1 || !strings.HasPrefix(lines[0], "brain setup: create link dir: ") {
		t.Errorf("stderr = %q, want one line starting with %q", errOut.String(), "brain setup: create link dir: ")
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
