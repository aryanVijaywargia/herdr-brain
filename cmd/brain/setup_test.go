package main

import (
	"bufio"
	"bytes"
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

func offer(t *testing.T, bin, link, answer string) (string, int) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := offerLink(bin, link, bufio.NewReader(strings.NewReader(answer)), &out, &errOut)
	return out.String() + errOut.String(), code
}

func TestOfferLinkYes(t *testing.T) {
	bin, link := linkFixture(t)
	out, code := offer(t, bin, link, "y\n")
	if code != 0 {
		t.Fatalf("code %d: %s", code, out)
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
		out, code := offer(t, bin, link, answer)
		if code != 0 {
			t.Fatalf("answer %q: code %d: %s", answer, code, out)
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
	os.Remove(link)
	other := filepath.Join(filepath.Dir(link), "other")
	os.WriteFile(other, []byte("x"), 0o755)
	for name, mk := range map[string]func() error{
		"file":   func() error { return os.WriteFile(link, []byte("mine"), 0o755) },
		"other":  func() error { return os.Symlink(other, link) },
		"broken": func() error { return os.Symlink(filepath.Join(filepath.Dir(link), "gone"), link) },
	} {
		os.Remove(link)
		if err := mk(); err != nil {
			t.Fatal(err)
		}
		before, _ := os.Readlink(link)
		out, code := offer(t, bin, link, "y\n")
		after, _ := os.Readlink(link)
		if code != 0 || before != after || !strings.Contains(out, "did not change it") {
			t.Errorf("%s: code %d, link %q -> %q, output %q", name, code, before, after, out)
		}
	}
}

// `brain setup` through run(): it shows the absolute binary path and makes no link on "no".
func TestSetupCommand(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	var out, errOut bytes.Buffer
	if code := run([]string{"setup"}, strings.NewReader("n\n"), &out, &errOut); code != 0 {
		t.Fatalf("setup = %d, stderr %q", code, errOut.String())
	}
	if !strings.Contains(out.String(), "Brain binary: /") {
		t.Errorf("setup does not show an absolute binary path: %q", out.String())
	}
	if _, err := os.Lstat(filepath.Join(home, ".local", "bin", "brain")); !os.IsNotExist(err) {
		t.Error("setup made a link after the answer no")
	}
}
