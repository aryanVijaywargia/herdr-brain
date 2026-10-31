package install

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// linkDirs returns a scratch link path ~/.local/bin/brain, this brain binary bin, and the
// herdr config folder, all in t.TempDir() with no symlink in their paths.
func linkDirs(t *testing.T) (link, bin, herdr string) {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	herdr = filepath.Join(dir, "config", "herdr")
	bin = writeBinary(t, filepath.Join(herdr, "plugins", "github-installations", "brain-bbbf7a6412d6", "200-0", "checkout", "bin", "brain"))
	link = filepath.Join(dir, "home", ".local", "bin", "brain")
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	return link, bin, herdr
}

// writeBinary writes an executable file at path and returns path.
func writeBinary(t *testing.T, path string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil { //nolint:gosec // a test binary must be executable
		t.Fatal(err)
	}
	return path
}

// #459: only a link to the brain binary of a GitHub plugin checkout is Brain's own; the
// others stay LinkOther or LinkBroken, which Brain never changes.
func TestCheckLink(t *testing.T) {
	for _, c := range []struct {
		name string
		make func(t *testing.T, link, bin, herdr string)
		want LinkState
	}{
		{"missing", func(*testing.T, string, string, string) {}, LinkMissing},
		{"this binary", func(t *testing.T, link, bin, _ string) { symlink(t, bin, link) }, LinkOK},
		{"removed checkout", func(t *testing.T, link, _, herdr string) {
			symlink(t, filepath.Join(herdr, "plugins", "github-installations", "brain-bbbf7a6412d6", "100-0", "checkout", "bin", "brain"), link)
		}, LinkStale},
		{"old checkout", func(t *testing.T, link, _, herdr string) {
			symlink(t, writeBinary(t, filepath.Join(herdr, "plugins", "github-installations", "brain-bbbf7a6412d6", "100-0", "checkout", "bin", "brain")), link)
		}, LinkStale},
		{"old fixed path", func(t *testing.T, link, _, herdr string) {
			symlink(t, filepath.Join(herdr, "plugins", "github", "brain-bbbf7a6412d6", "bin", "brain"), link)
		}, LinkStale},
		{"relative link to a checkout", func(t *testing.T, link, _, _ string) {
			symlink(t, "../../../config/herdr/plugins/github-installations/brain-bbbf7a6412d6/1-0/checkout/bin/brain", link)
		}, LinkStale},
		{"another herdr folder", func(t *testing.T, link, _, herdr string) {
			symlink(t, filepath.Join(filepath.Dir(herdr), "unrelated", "herdr", "plugins", "github-installations", "brain-bbbf7a6412d6", "1-0", "checkout", "bin", "brain"), link)
		}, LinkBroken},
		{"another plugin hash", func(t *testing.T, link, _, herdr string) {
			symlink(t, filepath.Join(herdr, "plugins", "github-installations", "brain-deadbeef", "1-0", "checkout", "bin", "brain"), link)
		}, LinkBroken},
		{"your own binary", func(t *testing.T, link, _, herdr string) {
			symlink(t, writeBinary(t, filepath.Join(filepath.Dir(herdr), "src", "brain")), link)
		}, LinkOther},
		{"a local plugin checkout", func(t *testing.T, link, _, herdr string) {
			symlink(t, filepath.Join(filepath.Dir(herdr), "projects", "herdr-brain", "bin", "brain"), link)
		}, LinkBroken},
		{"another plugin", func(t *testing.T, link, _, herdr string) {
			symlink(t, filepath.Join(herdr, "plugins", "github-installations", "other-0a1b", "1-0", "checkout", "bin", "brain"), link)
		}, LinkBroken},
		{"a file", func(t *testing.T, link, _, _ string) { writeBinary(t, link) }, LinkOther},
	} {
		t.Run(c.name, func(t *testing.T) {
			link, bin, herdr := linkDirs(t)
			c.make(t, link, bin, herdr)
			got, err := CheckLink(link, bin)
			if err != nil || got.State != c.want {
				t.Errorf("CheckLink() = %+v, %v; want state %d", got, err, c.want)
			}
		})
	}
}

// #459: Relink points a stale link to the binary and changes no other link.
func TestRelink(t *testing.T) {
	link, bin, herdr := linkDirs(t)
	symlink(t, filepath.Join(herdr, "plugins", "github-installations", "brain-bbbf7a6412d6", "100-0", "checkout", "bin", "brain"), link)
	if err := Relink(link, bin); err != nil {
		t.Fatalf("Relink() = %v", err)
	}
	if got, err := os.Readlink(link); err != nil || got != bin {
		t.Errorf("link after Relink = %q, %v; want %q", got, err, bin)
	}
	entries, err := os.ReadDir(filepath.Dir(link))
	if err != nil || len(entries) != 1 {
		t.Errorf("link folder after Relink = %v, %v; want only the link", entries, err)
	}

	link, bin, herdr = linkDirs(t)
	mine := filepath.Join(filepath.Dir(herdr), "src", "brain")
	symlink(t, mine, link)
	if err := Relink(link, bin); err == nil {
		t.Error("Relink of a link that Brain did not make = nil, want an error")
	}
	if got, err := os.Readlink(link); err != nil || got != mine {
		t.Errorf("link after a refused Relink = %q, %v; want %q", got, err, mine)
	}
}

// #459 review: Relink changes no link to a binary outside the plugin folder of bin, absolute or
// relative, no regular file, and no link in a folder that others can write; the last one gets
// the command to run.
func TestRelinkPreservesForeignLinks(t *testing.T) {
	for _, name := range []string{"unrelated binary", "another herdr folder", "relative link to another herdr folder", "regular file", "writable by others"} {
		t.Run(name, func(t *testing.T) {
			link, bin, herdr := linkDirs(t)
			target := filepath.Join(filepath.Dir(herdr), "unrelated", "herdr", "plugins", "github-installations", "brain-deadbeef", "7-0", "checkout", "bin", "brain")
			switch name {
			case "unrelated binary":
				target = filepath.Join(filepath.Dir(herdr), "other-program")
			case "regular file":
				writeBinary(t, link)
				if err := Relink(link, bin); err == nil {
					t.Fatal("Relink(regular file) = nil, want an error")
				}
				return
			case "writable by others":
				target = filepath.Join(herdr, "plugins", "github-installations", "brain-bbbf7a6412d6", "1-0", "checkout", "bin", "brain")
				if err := os.Chmod(filepath.Dir(link), 0o777); err != nil { //nolint:gosec // the test needs a folder that others can write
					t.Fatal(err)
				}
			}
			writeBinary(t, target)
			if name == "relative link to another herdr folder" {
				var err error
				if target, err = filepath.Rel(filepath.Dir(link), target); err != nil {
					t.Fatal(err)
				}
			}
			symlink(t, target, link)
			err := Relink(link, bin)
			if name == "writable by others" && (err == nil || !strings.Contains(err.Error(), "run: ln -sfn '"+bin+"' '"+link+"'")) {
				t.Errorf("Relink in a folder that others can write = %v, want the ln command", err)
			}
			if got, readErr := os.Readlink(link); readErr != nil || got != target {
				t.Errorf("Relink() = %v; link = %q, %v; want it kept on %q", err, got, readErr, target)
			}
		})
	}
}

func symlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}
