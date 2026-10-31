package install

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"syscall"
)

// LinkState is the state of the ~/.local/bin/brain link that brain setup offers (E1-S2), for
// one brain binary.
type LinkState int

const (
	// LinkMissing is no file at the link path.
	LinkMissing LinkState = iota
	// LinkOK is a link that resolves to the binary.
	LinkOK
	// LinkStale is a link that Brain made, to the brain binary of another GitHub install of
	// this plugin in the herdr plugin folder of the binary, that is broken or runs another brain
	// binary (#459). herdr 0.9.5 puts each GitHub install
	// in a new folder, so a reinstall leaves the link on the old checkout, and herdr later
	// removes that checkout.
	LinkStale
	// LinkOther is a file, or a link that Brain did not make, that resolves.
	LinkOther
	// LinkBroken is a broken link that Brain did not make.
	LinkBroken
)

// pluginBinary matches the brain binary of a GitHub install of the Brain plugin, the target
// that setup gives the link: <herdr>/plugins/github/brain-<hash>/bin/brain before herdr 0.9.5,
// and <herdr>/plugins/github-installations/brain-<hash>/<n>/checkout/bin/brain from it. Its
// groups are the plugin folder <herdr>/plugins/ and brain-<hash>. A local checkout of `herdr
// plugin link` is the folder of its owner, so Brain never takes a link to it as its own.
var pluginBinary = regexp.MustCompile(`^(.+/herdr/plugins/)github(?:-installations)?/(brain-[0-9a-f]+)/(?:[^/]+/)*bin/brain$`)

// own reports whether target is the brain binary of a GitHub install of this plugin in the
// herdr plugin folder of the binary bin: the same plugin folder and the same brain-<hash>. A
// bin outside a plugin folder, such as a build of your own, owns no link.
func own(target, bin string) bool {
	b, t := pluginBinary.FindStringSubmatch(bin), pluginBinary.FindStringSubmatch(target)
	return b != nil && t != nil && t[1] == b[1] && t[2] == b[2]
}

// resolve returns the path p with the symlinks of its longest existing parent resolved, so the
// target in a removed checkout compares with bin, which has no symlink.
func resolve(p string) string {
	rest := ""
	for dir := filepath.Clean(p); dir != filepath.Dir(dir); dir = filepath.Dir(dir) {
		if r, err := filepath.EvalSymlinks(dir); err == nil {
			return filepath.Join(r, rest)
		}
		rest = filepath.Join(filepath.Base(dir), rest)
	}
	return filepath.Clean(p)
}

// Link is the file at the link path.
type Link struct {
	State  LinkState
	Target string // the target of a symlink, as it reads; "" for no symlink
}

// CheckLink returns the state of the file at link for the brain binary bin, a path with no
// symlink (paths.BinaryPath).
func CheckLink(link, bin string) (Link, error) {
	fi, err := os.Lstat(link)
	if errors.Is(err, fs.ErrNotExist) {
		return Link{State: LinkMissing}, nil
	}
	if err != nil {
		return Link{}, fmt.Errorf("check link: %w", err)
	}
	if fi.Mode()&fs.ModeSymlink == 0 {
		return Link{State: LinkOther}, nil
	}
	target, err := os.Readlink(link)
	if err != nil {
		return Link{}, fmt.Errorf("check link: %w", err)
	}
	l := Link{Target: target}
	resolved, err := filepath.EvalSymlinks(link)
	broken := errors.Is(err, fs.ErrNotExist)
	abs := target
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(filepath.Dir(link), abs)
	}
	switch {
	case err != nil && !broken:
		return Link{}, fmt.Errorf("check link: %w", err)
	case !broken && resolved == bin:
		l.State = LinkOK
	case own(resolve(abs), bin):
		l.State = LinkStale
	case broken:
		l.State = LinkBroken
	default:
		l.State = LinkOther
	}
	return l, nil
}

// Relink points the stale link at link to the brain binary bin. It checks the link again
// first, and changes only a link that is still LinkStale. It makes the new link beside the old
// one and renames it over the old one, so the link path always holds a link. The rename would
// replace a file that another user put there after the check, so in a folder that is not yours,
// or that others can write, it changes nothing and gives the command to run.
func Relink(link, bin string) error {
	l, err := CheckLink(link, bin)
	if err != nil {
		return err
	}
	if l.State != LinkStale {
		return fmt.Errorf("link %s changed since the check; Brain does not change it", link)
	}
	dir := filepath.Dir(link)
	fi, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("check link: %w", err)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || int(st.Uid) != os.Getuid() || fi.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("%s is not yours or others can write it, so Brain does not change %s; run: ln -sfn '%s' '%s'",
			dir, link, bin, link)
	}
	tmp := filepath.Join(dir, ".brain-link-"+strconv.Itoa(os.Getpid()))
	if err := os.Symlink(bin, tmp); err != nil {
		return fmt.Errorf("make link: %w", err)
	}
	if err := os.Rename(tmp, link); err != nil {
		_ = os.Remove(tmp) // the rename error is the one to report
		return fmt.Errorf("replace link: %w", err)
	}
	return nil
}
