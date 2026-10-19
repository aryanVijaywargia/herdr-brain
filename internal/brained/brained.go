// Package brained is the herdr listener of Brain. herdr runs it as the plugin [[startup]]
// process, once for each herdr session. It follows the panes of its session over the herdr
// event socket and gives each pane step to a Handler. herdr does not supervise it: it exits
// when the event socket closes, or when Brain is disabled or unlinked.
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

	"github.com/aryanVijaywargia/herdr-brain/internal/bindings"
	"github.com/aryanVijaywargia/herdr-brain/internal/extract"
	"github.com/aryanVijaywargia/herdr-brain/internal/herdr"
	"github.com/aryanVijaywargia/herdr-brain/internal/paths"
	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

const (
	// connectTries and connectPause retry the first connect. herdr starts brained after its
	// socket is ready, so one try is enough; the rest cover a slow start. 20 × 250 ms.
	connectTries = 20
	connectPause = 250 * time.Millisecond
	// checkEvery is the pause between two checks of the plugin, the binding and the learning
	// switches. A switch takes effect in brained at the next check; the handler checks it
	// again before it writes. 2 s.
	checkEvery = 2 * time.Second
	// queueSize is the number of events that can wait for the handler. When it is full,
	// brained drops events and reconciles. 256: half of herdr's ring of 512.
	queueSize = 256
	// callTimeout is the deadline of one herdr request or one store transaction. 5 s.
	callTimeout = 5 * time.Second
	// busyTimeout is how long a store transaction waits for the lock. 5 s, as brain
	// event: nobody waits for brained.
	busyTimeout = 5 * time.Second
	// extractEvery is the least pause between the starts of two extraction passes. 1
	// minute: a session is finished after 10 minutes idle, so a pass each minute is soon enough.
	extractEvery = time.Minute
)

// ErrStop means that brained stops on purpose: herdr closed the event socket, or Brain is
// disabled or unlinked. It is not a failure.
var ErrStop = errors.New("brained stops")

// ErrRunning means that another brained holds the lock of the same herdr session.
var ErrRunning = errors.New("brained already runs")

// errRestart means that the brained binary file changed: brain update, or a new install or
// build, put a new binary at its path (E19-S4).
var errRestart = errors.New("the brained binary changed")

// Main runs brained for the herdr session of environ, the environment that herdr gives a
// [[startup]] process. It reads the panes with Capture and writes its errors to log. text is
// the notification text of the history scan. When its binary file changes, it drops the lock
// and runs the new binary in its place (E19-S4).
func Main(ctx context.Context, environ []string, log io.Writer, text ScanText) error {
	// The path comes from the start: on Linux, os.Executable of a replaced file ends in
	// " (deleted)".
	bin, err := paths.BinaryPath()
	if err != nil {
		return err
	}
	fi, err := os.Stat(bin)
	if err != nil {
		return err
	}
	err = serve(ctx, environ, log, text, func() bool { return changed(bin, fi) })
	if !errors.Is(err, errRestart) {
		return err
	}
	_, _ = fmt.Fprintf(log, "brained: %s changed; brained runs it again\n", bin) // a failed log write has no other place to go
	// The method: exec in place keeps the pid and the environment. The lock file closes
	// on exec (O_CLOEXEC), and the new binary takes the brained lock again at its start.
	return syscall.Exec(bin, []string{bin, "brained"}, environ)
}

// changed reports whether the file at bin is no longer the file fi: a new file (inode), or
// the same file with a new time. A missing file is no change yet: a reinstall swaps the plugin
// folder, and for a moment bin/brain does not exist.
func changed(bin string, fi os.FileInfo) bool {
	now, err := os.Stat(bin)
	if err != nil {
		return false // the next check looks again; Main keeps the old binary until then
	}
	return !os.SameFile(fi, now) || !now.ModTime().Equal(fi.ModTime())
}

// serve runs brained with the brained lock until it stops. text is the notification text of the
// history scan, and moved reports a change of the binary.
func serve(ctx context.Context, environ []string, log io.Writer, text ScanText, moved func() bool) error {
	e, err := parseEnv(environ)
	if err != nil {
		return err
	}
	bf, err := paths.BindingsFile()
	if err != nil {
		return err
	}
	config, err := paths.ConfigFile()
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
	reverts := NewReverts(time.Now())
	d := &daemon{
		env: e, sock: herdr.Socket{Path: e.socket}, bindingsFile: bf,
		handler: NewCapture(herdr.Socket{Path: e.socket}, environ), log: log, tick: t.C,
		environ: environ, launch: StartLaunch, now: time.Now, moved: moved,
		tries: connectTries, pause: connectPause, queue: queueSize,
		extract: func(ctx context.Context, g Gate) error {
			return extractPass(ctx, g, config, environ, reverts, log, Start)
		},
		stale: func(ctx context.Context, g Gate) error { return Stale(ctx, g, "", time.Now) },
		merge: func(ctx context.Context, g Gate) error {
			return mergePass(ctx, g, config, log, extract.ExecRunner{}, time.Now)
		},
		scan: newHistoryScan(environ, config, text).run,
	}
	return d.run(ctx)
}

// extractPass runs one extraction pass of the store of g with the extractor CLI, with the
// gates and caps of the config file config, read at each pass so an edit needs no restart. start
// starts `brain suggest` for each new lesson proposal (Start), with the herdr variables of the
// agent pane where you said the lesson, so the process resolves that pane again (#253).
// The revert rule r runs first, so the pass sees the reverts that git shows (#283). A batch that
// dropped claims gets one line in log (#310).
func extractPass(ctx context.Context, g Gate, config string, environ []string, r *Reverts, log io.Writer,
	start func(ctx context.Context, environ []string, d Dispatch) error) (err error) {
	gates, caps, err := extract.ReadConfig(config)
	if err != nil {
		return fmt.Errorf("extract: %w", err)
	}
	st, err := store.Open(ctx, g.Binding.Store, busyTimeout)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, st.Close()) }()
	defaultOn := g.Binding.Kind != bindings.KindTest
	// A git failure does not stop the pass: the other evidence still goes to the extractor.
	gitErr := r.Pass(ctx, st, g.Binding.Session, defaultOn, time.Now())
	if gitErr != nil {
		gitErr = fmt.Errorf("revert rule: %w", gitErr)
	}
	p := extract.Pass{Store: st, Runner: extract.ExecRunner{}, HerdrSession: g.Binding.Session,
		DefaultOn: defaultOn, Now: time.Now, Gates: gates, Caps: caps,
		Suggest: func(ctx context.Context, s extract.Suggestion) error {
			return start(ctx, paneEnv(environ, g.Binding, s.AgentPaneID, s.WorkspaceID), LessonDispatch(g.Binding.Store, s))
		},
		Note: func(line string) {
			_, _ = fmt.Fprintf(log, "brained: extract: %s\n", line) // a failed log write has no other place to go
		}}
	if err := p.Run(ctx); err != nil {
		return errors.Join(gitErr, fmt.Errorf("extract: %w", err))
	}
	return gitErr
}

// LessonDispatch is the Dispatch of the suggestion s of a lesson proposal in the store folder
// dir.
func LessonDispatch(dir string, s extract.Suggestion) Dispatch {
	return Dispatch{Suggestion: s.ID, Binding: s.HerdrSession, Store: dir, Workspace: s.WorkspaceKey,
		Session: s.SessionID, Occurrence: s.ObservationID, Generation: s.Generation, Proposal: s.ProposalID}
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
		e.session = "default" // the default session has no HERDR_SESSION
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
	if err := os.MkdirAll(dir, 0o700); err != nil { // 0o700, as the data home
		return nil, err
	}
	path := filepath.Join(dir, "brained-"+session+".lock")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600) // 0o600, as the data home files
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

// Running reports whether a brained holds the lock of the herdr session of environ. It
// takes the lock for a moment: a brained that starts in that moment stops with ErrRunning,
// and the caller, which then sees no brained, starts one (brain start, E19-S3).
func Running(environ []string) (bool, error) {
	e, err := parseEnv(environ)
	if err != nil {
		return false, err
	}
	unlock, err := lock(e.stateDir, e.session)
	if errors.Is(err, ErrRunning) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	unlock()
	return false, nil
}
