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

// installRoot is the checkout of GitHub install n of the plugin in the herdr folder herdr.
func installRoot(herdr, n string) string {
	return filepath.Join(herdr, "plugins", "github-installations", "brain-bbbf7a6412d6", n, "checkout")
}

// newInstall writes the brain binary of GitHub install n in the herdr folder herdr, and returns
// its plugin root.
func newInstall(t *testing.T, herdr, n string) string {
	t.Helper()
	writeBinary(t, filepath.Join(installRoot(herdr, n), "bin", "brain"))
	return installRoot(herdr, n)
}

// linkInstall makes GitHub install n in the herdr folder herdr, with a bin/brain link to target.
func linkInstall(t *testing.T, herdr, n, target string) string {
	t.Helper()
	dir := filepath.Join(installRoot(herdr, n), "bin")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	symlink(t, target, filepath.Join(dir, "brain"))
	return installRoot(herdr, n)
}

func chmod(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

// #468: Sibling gives only the binary of another GitHub install of this plugin that only you can
// change.
func TestSibling(t *testing.T) {
	for _, c := range []struct {
		name string
		make func(t *testing.T, herdr string) string // returns the plugin root
		ok   bool
	}{
		{"new install", func(t *testing.T, herdr string) string { return newInstall(t, herdr, "300-0") }, true},
		{"link to another install", func(t *testing.T, herdr string) string {
			return linkInstall(t, herdr, "400-0", filepath.Join(newInstall(t, herdr, "300-0"), "bin", "brain"))
		}, true},
		{"old fixed path", func(t *testing.T, herdr string) string {
			root := filepath.Join(herdr, "plugins", "github", "brain-bbbf7a6412d6")
			writeBinary(t, filepath.Join(root, "bin", "brain"))
			return root
		}, true},
		{"relative root", func(*testing.T, string) string {
			return "config/herdr/plugins/github-installations/brain-bbbf7a6412d6/300-0/checkout"
		}, false},
		{"no binary", func(_ *testing.T, herdr string) string { return installRoot(herdr, "300-0") }, false},
		{"another plugin hash", func(t *testing.T, herdr string) string {
			root := filepath.Join(herdr, "plugins", "github-installations", "brain-deadbeef", "1-0", "checkout")
			writeBinary(t, filepath.Join(root, "bin", "brain"))
			return root
		}, false},
		{"link to another plugin", func(t *testing.T, herdr string) string {
			other := writeBinary(t, filepath.Join(herdr, "plugins", "github-installations", "other-0a1b", "1-0", "checkout", "bin", "brain"))
			return linkInstall(t, herdr, "300-0", other)
		}, false},
		{"a folder", func(t *testing.T, herdr string) string {
			if err := os.MkdirAll(filepath.Join(installRoot(herdr, "300-0"), "bin", "brain"), 0o755); err != nil {
				t.Fatal(err)
			}
			return installRoot(herdr, "300-0")
		}, false},
		{"not executable", func(t *testing.T, herdr string) string {
			root := newInstall(t, herdr, "300-0")
			chmod(t, filepath.Join(root, "bin", "brain"), 0o644)
			return root
		}, false},
		{"others can write it", func(t *testing.T, herdr string) string {
			root := newInstall(t, herdr, "300-0")
			chmod(t, filepath.Join(root, "bin", "brain"), 0o757)
			return root
		}, false},
		{"the group can write a folder", func(t *testing.T, herdr string) string {
			root := newInstall(t, herdr, "300-0")
			chmod(t, filepath.Dir(root), 0o775)
			return root
		}, false},
		{"others can write the plugin folder", func(t *testing.T, herdr string) string {
			root := newInstall(t, herdr, "300-0")
			chmod(t, filepath.Join(herdr, "plugins"), 0o777)
			return root
		}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, bin, herdr := linkDirs(t)
			root := c.make(t, herdr)
			got, err := Sibling(root, bin)
			if (err == nil) != c.ok {
				t.Fatalf("Sibling(%s) = %q, %v; want ok %t", root, got, err, c.ok)
			}
			if c.ok && !strings.HasSuffix(got, "/bin/brain") {
				t.Errorf("Sibling(%s) = %q, want a brain binary", root, got)
			}
		})
	}
}
