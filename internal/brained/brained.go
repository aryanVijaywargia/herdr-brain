// Package brained is the herdr listener of Brain (HLD §4.1, §6.2, D7). herdr runs it as the
// plugin [[startup]] process, once for each herdr session. It follows the panes of its
// session over the herdr event socket and gives each pane step to a Handler. herdr does not
// supervise it: it exits when the event socket closes, or when Brain is disabled or unlinked.
package brained

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/aryanVijaywargia/herdr-brain/internal/herdr"
	"github.com/aryanVijaywargia/herdr-brain/internal/paths"
)

const (
	// connectTries and connectPause retry the first connect. herdr starts brained after its
	// socket is ready, so one try is enough; the rest cover a slow start. [NEW] 20 × 250 ms.
	connectTries = 20
	connectPause = 250 * time.Millisecond
	// checkEvery is the pause between two checks of the plugin, the binding and the learning
	// switches. A switch takes effect in brained at the next check; the handler checks it
	// again before it writes. [NEW] 2 s.
	checkEvery = 2 * time.Second
	// queueSize is the number of events that can wait for the handler. When it is full,
	// brained drops events and reconciles (S39). [NEW] 256: half of herdr's ring of 512.
	queueSize = 256
	// callTimeout is the deadline of one herdr request or one store transaction. [NEW] 5 s.
	callTimeout = 5 * time.Second
	// busyTimeout is how long a store transaction waits for the lock. [NEW] 5 s, as brain
	// event: nobody waits for brained.
	busyTimeout = 5 * time.Second
)

// ErrStop means that brained stops as D7 wants: herdr closed the event socket, or Brain is
// disabled or unlinked. It is not a failure.
var ErrStop = errors.New("brained stops")

// ErrRunning means that another brained holds the lock of the same herdr session.
var ErrRunning = errors.New("brained already runs")

// Main runs brained for the herdr session of environ, the environment that herdr gives a
// [[startup]] process. It writes one line for each step to log.
func Main(ctx context.Context, environ []string, log io.Writer) error {
	e, err := parseEnv(environ)
	if err != nil {
		return err
	}
	bf, err := paths.BindingsFile()
	if err != nil {
		return err
	}
	unlock, err := lock(e.stateDir, e.session)
	if err != nil {
		return err
	}
	defer unlock()
	t := time.NewTicker(checkEvery)
	defer t.Stop()
	d := &daemon{
		env: e, sock: herdr.Socket{Path: e.socket}, bindingsFile: bf,
		handler: LogHandler{W: log}, log: log, tick: t.C,
		tries: connectTries, pause: connectPause, queue: queueSize,
	}
	return d.run(ctx)
}

// env is what brained reads from the [[startup]] environment.
type env struct {
	session  string // HERDR_SESSION; "default" when it is empty
	socket   string // HERDR_SOCKET_PATH
	stateDir string // HERDR_PLUGIN_STATE_DIR
	pluginID string // HERDR_PLUGIN_ID
}

func parseEnv(environ []string) (env, error) {
	e := env{
		session:  lookup(environ, "HERDR_SESSION"),
		socket:   lookup(environ, "HERDR_SOCKET_PATH"),
		stateDir: lookup(environ, "HERDR_PLUGIN_STATE_DIR"),
		pluginID: lookup(environ, "HERDR_PLUGIN_ID"),
	}
	if e.session == "" {
		e.session = "default" // the default session has no HERDR_SESSION (M0)
	}
	switch {
	case !filepath.IsAbs(e.socket):
		return env{}, fmt.Errorf("HERDR_SOCKET_PATH %q is not an absolute path", e.socket)
	case !filepath.IsAbs(e.stateDir):
		return env{}, fmt.Errorf("HERDR_PLUGIN_STATE_DIR %q is not an absolute path", e.stateDir)
	case e.pluginID == "":
		return env{}, errors.New("HERDR_PLUGIN_ID is not set")
	}
	return e, nil
}

// lock takes the brained lock of session in dir, so one brained runs for each herdr session.
// The lock stays until unlock, or until the process ends.
func lock(dir, session string) (unlock func(), err error) {
	if err := os.MkdirAll(dir, 0o700); err != nil { // [NEW] 0o700, as the data home
		return nil, err
	}
	path := filepath.Join(dir, "brained-"+session+".lock")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600) // [NEW] 0o600, as the data home files
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close() // the lock error is the one to report
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("%w for herdr session %q (lock %s)", ErrRunning, session, path)
		}
		return nil, fmt.Errorf("lock %s: %w", path, err)
	}
	return func() { _ = f.Close() }, nil // the close drops the lock
}

// lookup returns the value of key in environ; the last entry wins, as in os/exec.
func lookup(environ []string, key string) string {
	var v string
	for _, kv := range environ {
		if k, val, ok := strings.Cut(kv, "="); ok && k == key {
			v = val
		}
	}
	return v
}
