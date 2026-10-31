package brained

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"

	"github.com/aryanVijaywargia/herdr-brain/internal/install"
)

// lockFDEnv names the descriptor of the brained lock that a brained gives the binary that it runs
// in its place, so the lock is never free during the handoff (#468).
const lockFDEnv = "BRAIN_BRAINED_LOCK_FD"

// reinstallError means that herdr lists Brain in the plugin root root, and bin there is a brain
// binary that brained can run in its place. herdr 0.9.5 puts each install in a new checkout
// folder and keeps the old one, so the file of the old binary does not change (#468). fi is the
// file of bin at the check.
type reinstallError struct {
	root, bin string
	fi        os.FileInfo
}

func (e *reinstallError) Error() string {
	return "herdr lists Brain in " + e.root
}

// handoff is the brain binary that brained runs, and the checks of a binary to run in its place.
// Only one goroutine uses it at a time: the loop during serve, and Main after it.
type handoff struct {
	bin     string      // the brain binary that runs, a path with no symlink
	fi      os.FileInfo // the file at bin at the start, or after a restart that failed
	tried   os.FileInfo // the last new install binary that failed its check or its exec; nil for none
	environ []string    // the brained environment, with no lockFDEnv
	log     io.Writer
	exec    func(argv0 string, argv, envv []string) error // syscall.Exec; tests replace it
}

// changed reports a new file at the path of the binary that runs (E19-S4).
func (h *handoff) changed() bool {
	return changed(h.bin, h.fi)
}

// reinstalled returns a reinstallError when the plugin root root holds a brain binary that brained
// can run in its place: not the binary that runs, a binary of another install of this plugin that
// only you can change (install.Sibling), and one that prints its version. A binary that fails gets
// one line in the log, and no new check until its file changes, so a bad install costs no exec at
// each check. A missing binary gets no line: an install can still be on its way.
func (h *handoff) reinstalled(ctx context.Context, root string) error {
	fi, err := os.Stat(rootBinary(root))
	if err != nil || os.SameFile(fi, h.fi) || (h.tried != nil && sameFile(fi, h.tried)) {
		return nil //nolint:nilerr // no binary to check yet: the next check looks again
	}
	bin, err := install.Sibling(root, h.bin)
	if err == nil {
		err = h.version(ctx, bin)
	}
	if err != nil {
		h.tried = fi
		h.logf("herdr lists Brain in %s, but brained keeps running %s: %v", root, h.bin, err)
		return nil
	}
	return &reinstallError{root: root, bin: bin, fi: fi}
}

// version checks that bin prints a brain version (`brain version`) within callTimeout, so a
// truncated or broken binary does not replace brained.
func (h *handoff) version(ctx context.Context, bin string) error {
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "version")
	cmd.Env = h.environ
	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("%s version: %w", bin, err)
	}
	if !strings.HasPrefix(string(out), "brain ") {
		return fmt.Errorf("%s version printed %q, want a brain version", bin, out)
	}
	return nil
}

// next ends brained with the error err of serve, or runs the binary that err asks for in place of
// brained, with the brained lock of f. It returns nil when brained serves again: the exec failed,
// or the new install changed since its check.
func (h *handoff) next(f *os.File, err error) error {
	var re *reinstallError
	switch {
	case errors.As(err, &re):
		h.reinstall(f, re)
		return nil
	case errors.Is(err, errRestart):
		h.logf("%s changed; brained runs it again", h.bin)
		if err := h.execLocked(f, h.bin, h.environ); err != nil {
			h.logf("run %s: %v; brained keeps running", h.bin, err)
			if fi, err := os.Stat(h.bin); err == nil {
				h.fi = fi // no new try until the file changes again
			}
		}
		return nil
	default:
		return err
	}
}

// reinstall runs the binary of re in place of brained, with the root of re in HERDR_PLUGIN_ROOT.
// It checks the binary again first: the install can change between the check and here.
func (h *handoff) reinstall(f *os.File, re *reinstallError) {
	bin, err := install.Sibling(re.root, h.bin)
	if err != nil || bin != re.bin {
		return // the next check looks at it again
	}
	if fi, err := os.Stat(bin); err != nil || !sameFile(fi, re.fi) {
		return
	}
	environ := append(withoutEnv(h.environ, "HERDR_PLUGIN_ROOT"), "HERDR_PLUGIN_ROOT="+re.root)
	h.logf("herdr lists Brain in %s; brained runs %s", re.root, bin)
	if err := h.execLocked(f, bin, environ); err != nil {
		h.tried = re.fi
		h.logf("run %s: %v; brained keeps running %s", bin, err, h.bin)
	}
}

// execLocked runs bin as `bin brained` in place of this process, with environ, and gives it the
// brained lock of f: a copy of its descriptor stays open across the exec, and lockFDEnv names
// it. So the lock is never free, and a lock check of brain doctor or brain status cannot make
// the new binary stop with ErrRunning. It returns only when the exec fails; f keeps the lock.
func (h *handoff) execLocked(f *os.File, bin string, environ []string) error {
	// The copy does not close on exec: ForkLock keeps it out of a child that starts meanwhile.
	syscall.ForkLock.Lock()
	defer syscall.ForkLock.Unlock()
	fd, err := syscall.Dup(int(f.Fd()))
	if err != nil {
		return fmt.Errorf("copy the lock descriptor: %w", err)
	}
	err = h.exec(bin, []string{bin, "brained"}, append(slices.Clone(environ), lockFDEnv+"="+strconv.Itoa(fd)))
	_ = syscall.Close(fd) // the exec error is the one to report
	return err
}

func (h *handoff) logf(format string, a ...any) {
	_, _ = fmt.Fprintf(h.log, "brained: "+format+"\n", a...) // a failed log write has no other place to go
}

// holdLock returns the open brained lock file of the session of e, with the lock held. It takes
// over the lock that the brained before it in this process gave across the exec (lockFDEnv),
// and takes the lock itself when there is none. A lock that it cannot take over goes to log,
// and then it takes the lock itself.
func holdLock(environ []string, e env, log io.Writer) (*os.File, error) {
	if fd := lookup(environ, lockFDEnv); fd != "" {
		f, err := adopt(fd, lockPath(e.stateDir, e.session))
		if err == nil {
			return f, nil
		}
		_, _ = fmt.Fprintf(log, "brained: take over the brained lock: %v\n", err) // a failed log write has no other place to go
	}
	return lockSession(e.stateDir, e.session)
}

// adopt returns the lock file at path from the inherited descriptor fd, a number in text, and
// holds its lock again. It checks the descriptor before it wraps it in an *os.File, so it never
// closes a descriptor that is not the lock. The descriptor closes on exec again, so no child
// process gets the lock.
func adopt(fd, path string) (*os.File, error) {
	n, err := strconv.Atoi(fd)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", lockFDEnv, err)
	}
	var st syscall.Stat_t
	if err := syscall.Fstat(n, &st); err != nil {
		return nil, fmt.Errorf("%s=%d: %w", lockFDEnv, n, err)
	}
	want, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	ws, ok := want.Sys().(*syscall.Stat_t)
	if !ok || ws.Dev != st.Dev || ws.Ino != st.Ino {
		return nil, fmt.Errorf("%s=%d is not the lock %s", lockFDEnv, n, path)
	}
	syscall.CloseOnExec(n)
	if err := syscall.Flock(n, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return nil, fmt.Errorf("lock %s: %w", path, err)
	}
	return os.NewFile(uintptr(n), path), nil
}

// rootBinary is the brain binary of the plugin root root, the binary of the [[startup]] command.
func rootBinary(root string) string {
	return filepath.Join(root, "bin", "brain")
}

// sameFile reports whether a and b are the same file with the same size and time.
func sameFile(a, b os.FileInfo) bool {
	return os.SameFile(a, b) && a.Size() == b.Size() && a.ModTime().Equal(b.ModTime())
}

// withoutEnv returns a copy of environ with no entry for key.
func withoutEnv(environ []string, key string) []string {
	return slices.DeleteFunc(slices.Clone(environ), func(kv string) bool {
		return strings.HasPrefix(kv, key+"=")
	})
}
