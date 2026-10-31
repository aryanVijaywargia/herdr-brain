// Package launch makes, checks and ends the launch records of `brain claude` and `brain codex`
// (E24-S3). A launch has a random 128-bit launch ID. The ID is only in a 0600 file of the
// private launch folder <store>/launches/<record id>, never in a process argument or the
// environment, because other processes can read those. The store keeps only the SHA-256 of the
// ID. A call that gives a launch folder is checked against its record and against herdr on
// each use.
package launch

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/aryanVijaywargia/herdr-brain/internal/bindings"
	"github.com/aryanVijaywargia/herdr-brain/internal/herdr"
	"github.com/aryanVijaywargia/herdr-brain/internal/paths"
	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

const (
	// idFile is the name of the file in the launch folder that holds the launch ID.
	idFile = "id"
	// idBytes is the size of a launch ID: 128 random bits.
	idBytes = 16
	// busyTimeout is how long a store transaction waits for the lock. 5 s, as the CLI; the
	// store waits no longer than the deadline of ctx, so a hook keeps its own cap.
	busyTimeout = 5 * time.Second
)

// idRe is a launch ID as Create writes it: 32 lowercase hex digits.
var idRe = regexp.MustCompile(`^[0-9a-f]{32}$`)

// Create makes the launch record of the CLI of kind cli in pane paneID of workspace
// workspaceID, on the herdr server of binding b, and its private launch folder (0700) with the
// launch ID in a 0600 file. It returns the folder: the launch command writes its other launch
// files there. codexHome is the absolute Codex home of a Codex launch, where brained finds its
// rollouts; "" for none. pid is the process id of the launch: the process that runs the CLI
// in its place. now is the start time of the launch.
func Create(ctx context.Context, b bindings.Binding, workspaceID, paneID, cli, codexHome string, pid int, now time.Time) (dir string, err error) {
	if workspaceID == "" || paneID == "" || cli == "" {
		return "", errors.New("create launch: want a workspace, a pane and a CLI kind")
	}
	raw := make([]byte, idBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("make launch ID: %w", err)
	}
	st, err := store.Open(ctx, b.Store, busyTimeout)
	if err != nil {
		return "", err
	}
	defer func() { err = errors.Join(err, st.Close()) }()
	made := false
	err = st.Write(ctx, func(tx *sql.Tx) error {
		id, err := store.InsertLaunch(ctx, tx, store.Launch{IDHash: hash(raw), Socket: b.Socket,
			WorkspaceID: workspaceID, PaneID: paneID, CLI: cli, Started: now, CodexHome: codexHome, PID: pid})
		if err != nil {
			return err
		}
		dir = paths.Launch(b.Store, id)
		made, err = writeFolder(dir, hex.EncodeToString(raw))
		return err
	})
	if err != nil {
		if made {
			// The record did not commit, so its folder must not stay.
			err = errors.Join(err, os.RemoveAll(dir))
		}
		return "", err
	}
	return dir, nil
}

// writeFolder makes the launch folder dir and writes the launch ID id in it. made reports
// whether it made dir: a folder that was there already is not its own to remove.
func writeFolder(dir, id string) (made bool, err error) {
	// 0o700: the launch folders hold launch IDs, so only you may list them.
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		return false, err
	}
	if err := os.Mkdir(dir, 0o700); err != nil {
		return false, err
	}
	f, err := os.OpenFile(filepath.Join(dir, idFile), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return true, err
	}
	_, err = f.WriteString(id)
	return true, errors.Join(err, f.Close())
}

// hash is the hex SHA-256 of the raw launch ID, as the store keeps it.
func hash(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// Launch is a launch that passed the check.
type Launch struct {
	store.Launch
	Binding bindings.Binding // the binding of the launch's herdr server: its session, socket and store
	Dir     string           // the launch folder
}

// Reason says why the check refused a launch.
type Reason string

// The reasons of a refusal.
const (
	// Unknown: no record has the ID of the folder, or the folder or its ID file is gone, as
	// after the end of the launch. A folder or ID file that another user can open or that
	// another user owns counts as unknown too (#463).
	Unknown Reason = "unknown launch ID"
	Ended   Reason = "launch ended"
	// ProcessGone: the record has no end, but its launch process is gone, as after a herdr
	// restart that restores the pane with its old id while brained has not ended it yet (#458).
	ProcessGone Reason = "launch process gone"
	// Unbound: the folder is in no bound store, or the herdr server of the record is not
	// bound to that store.
	Unbound    Reason = "herdr server not bound"
	PaneGone   Reason = "pane not live"
	Moved      Reason = "pane in another workspace" // herdr answers the pane id with a new id or workspace
	OtherAgent Reason = "another agent in the pane" // herdr names an agent of another CLI kind
	OtherPane  Reason = "call from another pane"    // HERDR_PANE_ID of the call names another pane
)

// RefusedError is the error of a launch that the check refused.
type RefusedError struct {
	Reason Reason
}

func (e *RefusedError) Error() string {
	return "launch refused: " + string(e.Reason)
}

func refuse(r Reason) error {
	return &RefusedError{Reason: r}
}

// Check reads the launch ID in the launch folder dir and checks it on each use: the record
// exists and has no end, its launch process runs, herdr shows its pane live in the same
// workspace, and an agent that herdr names in the pane is of the record's CLI kind. Only you
// may open the folder and its ID file. environ is the environment of the call:
// HERDR_PANE_ID is only a hint, but when it is set, it must name the pane of the record.
// bindingsFile is the bindings file. A refusal is a *RefusedError. The herdr request ends at
// the deadline of ctx.
func Check(ctx context.Context, bindingsFile, dir string, environ []string) (Launch, error) {
	rowID, err := folderID(dir)
	if err != nil {
		return Launch{}, err
	}
	raw, err := readID(dir)
	if err != nil {
		return Launch{}, err
	}
	l, err := running(ctx, bindingsFile, dir, rowID, raw)
	if err != nil {
		return Launch{}, err
	}
	if pane := lookup(environ, "HERDR_PANE_ID"); pane != "" && pane != l.PaneID {
		return Launch{}, refuse(OtherPane)
	}
	if Gone(l.Launch) {
		return Launch{}, refuse(ProcessGone)
	}
	if err := live(ctx, l.Launch); err != nil {
		return Launch{}, err
	}
	// The record can end, or the binding go, while herdr answers.
	return running(ctx, bindingsFile, dir, rowID, raw)
}

// running reads the record of the raw launch ID of the launch folder dir, whose name is the
// record id rowID, and the binding of its herdr server. The record must have no end.
func running(ctx context.Context, bindingsFile, dir string, rowID int64, raw []byte) (Launch, error) {
	b, l, err := record(ctx, bindingsFile, filepath.Dir(filepath.Dir(dir)), raw)
	switch {
	case err != nil:
		return Launch{}, err
	case l.ID != rowID:
		return Launch{}, refuse(Unknown) // the ID of another launch folder
	case !l.Ended.IsZero():
		return Launch{}, refuse(Ended)
	}
	return Launch{Launch: l, Binding: b, Dir: dir}, nil
}

// folderID returns the record id in the name of the launch folder dir.
func folderID(dir string) (int64, error) {
	if !filepath.IsAbs(dir) || dir != filepath.Clean(dir) || filepath.Base(filepath.Dir(dir)) != "launches" {
		return 0, fmt.Errorf("%q is not a launch folder", dir)
	}
	id, err := strconv.ParseInt(filepath.Base(dir), 10, 64)
	if err != nil || id <= 0 {
		return 0, fmt.Errorf("%q is not a launch folder", dir)
	}
	return id, nil
}

// readID reads the raw launch ID in the launch folder dir. The folder and the ID file must be
// yours and only yours: Create makes them 0700 and 0600 (#463).
func readID(dir string) ([]byte, error) {
	fi, err := os.Lstat(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, refuse(Unknown)
	}
	if err != nil {
		return nil, err
	}
	if !fi.IsDir() || !private(fi) {
		return nil, refuse(Unknown)
	}
	f, err := os.Open(filepath.Join(dir, idFile))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, refuse(Unknown)
	}
	if err != nil {
		return nil, err
	}
	defer f.Close() //nolint:errcheck // read-only: Close cannot lose data
	// The stat of the open file: a rename between a path check and the read cannot swap it.
	fi, err = f.Stat()
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() || !private(fi) {
		return nil, refuse(Unknown)
	}
	// One byte more than an ID, so a longer file fails the match.
	text, err := io.ReadAll(io.LimitReader(f, 2*idBytes+1))
	if err != nil {
		return nil, err
	}
	if !idRe.Match(text) {
		return nil, refuse(Unknown)
	}
	return hex.DecodeString(string(text))
}

// private reports whether the file of fi is yours and has no permission bits for the group or
// others. An execute bit on the folder counts too: it lets another user open the ID file, whose
// name is known.
func private(fi fs.FileInfo) bool {
	st, ok := fi.Sys().(*syscall.Stat_t)
	return ok && int(st.Uid) == os.Getuid() && fi.Mode().Perm()&0o077 == 0
}

// Gone reports whether the launch process of l is gone. The process of `brain <kind>` runs the
// CLI in its place, so it lives as long as the CLI. A record with no pid, from before the pid
// column, is never gone by this check. A process of another user (EPERM) is not gone: the pid
// is in use.
func Gone(l store.Launch) bool {
	return l.PID > 0 && errors.Is(syscall.Kill(l.PID, 0), syscall.ESRCH)
}

// record reads the record of the raw launch ID in the store folder storeDir and the binding of
// its herdr server. The store must be bound, and the herdr server of the record must be bound
// to it.
func record(ctx context.Context, bindingsFile, storeDir string, raw []byte) (bindings.Binding, store.Launch, error) {
	bs, _, err := bindings.Read(bindingsFile)
	if err != nil {
		return bindings.Binding{}, store.Launch{}, err
	}
	bound, err := boundStore(bs, storeDir)
	if err != nil {
		return bindings.Binding{}, store.Launch{}, err
	}
	if !bound {
		return bindings.Binding{}, store.Launch{}, refuse(Unbound)
	}
	l, ok, err := read(ctx, storeDir, hash(raw))
	if err != nil {
		return bindings.Binding{}, store.Launch{}, err
	}
	if !ok {
		return bindings.Binding{}, store.Launch{}, refuse(Unknown)
	}
	b, ok, err := bindings.BySocket(bs, l.Socket)
	if err != nil {
		return bindings.Binding{}, store.Launch{}, err
	}
	if !ok {
		return bindings.Binding{}, store.Launch{}, refuse(Unbound)
	}
	same, err := sameDir(b.Store, storeDir)
	if err != nil {
		return bindings.Binding{}, store.Launch{}, err
	}
	if !same {
		return bindings.Binding{}, store.Launch{}, refuse(Unbound)
	}
	return b, l, nil
}

// boundStore reports whether the store folder dir is the store of a binding in bs.
func boundStore(bs []bindings.Binding, dir string) (bool, error) {
	for _, b := range bs {
		same, err := sameDir(b.Store, dir)
		if err != nil || same {
			return same, err
		}
	}
	return false, nil
}

// sameDir reports whether the folders a and b are one folder. A folder that does not exist is
// no store.
func sameDir(a, b string) (bool, error) {
	fa, err := os.Stat(a)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	fb, err := os.Stat(b)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return os.SameFile(fa, fb), nil
}

// read reads the launch record with ID hash h in the store folder dir.
func read(ctx context.Context, dir, h string) (l store.Launch, ok bool, err error) {
	st, err := store.Open(ctx, dir, busyTimeout)
	if err != nil {
		return store.Launch{}, false, err
	}
	defer func() { err = errors.Join(err, st.Close()) }()
	err = st.Write(ctx, func(tx *sql.Tx) error {
		l, ok, err = store.LaunchByHash(ctx, tx, h)
		return err
	})
	return l, ok, err
}

// live asks herdr for the pane of l: it must be live, in the same workspace, and an agent
// that herdr names there must be of the CLI kind of l.
func live(ctx context.Context, l store.Launch) error {
	p, err := herdr.Socket{Path: l.Socket}.Pane(ctx, l.PaneID)
	switch {
	case errors.Is(err, herdr.ErrPaneNotFound):
		return refuse(PaneGone)
	case err != nil:
		return err
	case p.ID != l.PaneID || p.WorkspaceID != l.WorkspaceID:
		return refuse(Moved)
	case p.Agent != nil && *p.Agent != l.CLI:
		return refuse(OtherAgent)
	}
	return nil
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

// End ends the running launch records of the herdr server socket in the store folder dir for
// which gone reports true, with reason r at now, and deletes their launch folders. It also
// deletes the folder of each record that ended before, so a delete that a crash or an error
// stopped runs again at the next End. brained calls it.
func End(ctx context.Context, dir, socket string, gone func(store.Launch) bool, r store.EndReason, now time.Time) (err error) {
	st, err := store.Open(ctx, dir, busyTimeout)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, st.Close()) }()
	var stale []string
	err = st.Write(ctx, func(tx *sql.Tx) error {
		ls, err := store.RunningLaunches(ctx, tx, socket)
		if err != nil {
			return err
		}
		for _, l := range ls {
			if !gone(l) {
				continue
			}
			if err := store.EndLaunch(ctx, tx, l.ID, r, now); err != nil {
				return err
			}
		}
		stale, err = endedFolders(ctx, tx, dir)
		return err
	})
	if err != nil {
		return err
	}
	var errs []error
	for _, f := range stale {
		if err := os.RemoveAll(f); err != nil {
			errs = append(errs, fmt.Errorf("delete launch folder: %w", err))
		}
	}
	return errors.Join(errs...)
}

// endedFolders returns the launch folders in the store folder dir whose record has an end.
// It keeps a folder with no record, or with a name that is no record id: Brain did not make it
// for an ended launch.
func endedFolders(ctx context.Context, tx *sql.Tx, dir string) ([]string, error) {
	return folders(ctx, tx, dir, func(l store.Launch, ok bool) bool { return ok && !l.Ended.IsZero() })
}

// OldFolders returns the launch folders in the store folder dir with a record id as name that
// no running launch uses: the folder of a record that ended, and a folder with no record.
// brain doctor lists them (E24-S8). brained deletes the folder of an ended record at its next
// launch end; Brain never deletes a folder with no record.
func OldFolders(ctx context.Context, tx *sql.Tx, dir string) ([]string, error) {
	return folders(ctx, tx, dir, func(l store.Launch, ok bool) bool { return !ok || !l.Ended.IsZero() })
}

// folders returns the launch folders in the store folder dir with a record id as name for
// which keep reports true. keep gets the record of the folder, and ok false when it has none.
func folders(ctx context.Context, tx *sql.Tx, dir string, keep func(l store.Launch, ok bool) bool) ([]string, error) {
	entries, err := os.ReadDir(paths.Launches(dir))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil // no launch yet in this store
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		id, err := strconv.ParseInt(e.Name(), 10, 64)
		if err != nil || id <= 0 {
			continue
		}
		l, ok, err := store.LaunchByID(ctx, tx, id)
		if err != nil {
			return nil, err
		}
		if keep(l, ok) {
			out = append(out, paths.Launch(dir, id))
		}
	}
	return out, nil
}
