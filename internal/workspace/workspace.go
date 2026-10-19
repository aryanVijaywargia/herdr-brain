// Package workspace keeps the workspace map of a store: it maps a (herdr session, workspace id)
// pair to Brain's own workspace key.
//
// A `brain` call refreshes the map from `herdr workspace list`. The first mapping records the
// project folder: the git top-level of the saved root (identity_cwd in herdr's session.json),
// or the root itself outside git. Each use, the hook included, checks that the saved root is
// still the project folder or lies beneath it. The hook only reads files and compares paths.
package workspace

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/aryanVijaywargia/herdr-brain/internal/bindings"
	"github.com/aryanVijaywargia/herdr-brain/internal/herdr"
	"github.com/aryanVijaywargia/herdr-brain/internal/paths"
	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

// mapVersion is the version of the map file format.
const mapVersion = 1

// Entry maps one (herdr session, workspace id) pair to a key. A remap keeps the old entry,
// so that its items wait for a relink; ReplacedBy then names the new key. A
// close keeps the entry too, with Closed set: herdr can reuse the id after a restart.
type Entry struct {
	Key         string `json:"key"`
	Session     string `json:"session"`      // herdr session name
	WorkspaceID string `json:"workspace_id"` // for example w14
	Label       string `json:"label"`
	Project     string `json:"project"` // canonical project folder, recorded at the first mapping
	ReplacedBy  string `json:"replaced_by,omitempty"`
	Closed      bool   `json:"closed,omitempty"` // optional, so the map file stays version 1
}

type mapFile struct {
	Version int     `json:"version"`
	Entries []Entry `json:"entries"`
}

// Session is the workspace map of one bound herdr session.
type Session struct {
	Name    string // herdr session name
	Socket  string // socket of the binding; its folder is herdr's session_dir
	MapFile string // the map file of the store
}

// For returns the workspace map of the herdr session of binding b.
func For(b bindings.Binding) Session {
	return Session{Name: b.Session, Socket: b.Socket, MapFile: paths.WorkspaceMap(b.Store)}
}

// sessionFile is herdr's session.json of s: <session_dir>/session.json, read only (E2-S5).
func (s Session) sessionFile() string {
	return filepath.Join(filepath.Dir(s.Socket), "session.json")
}

// State says what a call gets for its workspace.
type State string

// The states of a workspace.
const (
	Mapped   State = "mapped"             // the call gets the workspace part of Entry.Key
	Unmapped State = "unmapped"           // no entry yet; the next `brain` call maps it
	Withheld State = "withheld: identity" // an entry, but the root check failed
)

// Mapping is the result of a lookup.
type Mapping struct {
	State State
	Entry Entry  // the current entry; empty when Unmapped
	Root  string // the canonical saved root; empty when it cannot be read
	Err   error  // why Withheld
}

// Lookup maps workspace id of s to its key. The hook calls it, so it never runs git and never
// asks herdr: it reads the map file and session.json and compares canonical paths. The error
// is for a map file that cannot be read.
func (s Session) Lookup(id string) (Mapping, error) {
	m, err := readMap(s.MapFile)
	if err != nil {
		return Mapping{}, err
	}
	i, ok := m.current(s.Name, id)
	if !ok {
		return Mapping{State: Unmapped}, nil
	}
	return s.check(m.Entries[i], id), nil
}

// check reads the saved root of workspace id and checks that it lies in the project folder of
// e. A withheld result holds the reason in Err.
func (s Session) check(e Entry, id string) Mapping {
	r, err := root(s.sessionFile(), id)
	if err != nil {
		return Mapping{State: Withheld, Entry: e, Err: err}
	}
	in, err := within(e.Project, r)
	switch {
	case err != nil:
		return Mapping{State: Withheld, Entry: e, Root: r, Err: fmt.Errorf("%w: %w", ErrIdentity, err)}
	case !in:
		return Mapping{State: Withheld, Entry: e, Root: r,
			Err: fmt.Errorf("%w: root %s is outside the project folder %s", ErrIdentity, r, e.Project)}
	}
	return Mapping{State: Mapped, Entry: e, Root: r}
}

// Remappable reports whether a remap can help: the saved root is readable, but it lies
// outside the project folder of the entry.
func (m Mapping) Remappable() bool {
	return m.State == Withheld && m.Root != ""
}

// Status is the line that `brain status` shows for workspace id. For a root outside the
// project folder it offers the remap.
func (m Mapping) Status(id string) string {
	switch {
	case m.State == Mapped:
		return fmt.Sprintf("workspace %s (%s): key %s, project %s", id, m.Entry.Label, m.Entry.Key, m.Entry.Project)
	case m.State == Unmapped:
		return fmt.Sprintf("workspace %s: unmapped; the next brain call maps it", id)
	case m.Remappable():
		return fmt.Sprintf("workspace %s (%s): %v\n  remap: a new key for project %s; the items of key %s stay for relink",
			id, m.Entry.Label, m.Err, m.Root, m.Entry.Key)
	default:
		return fmt.Sprintf("workspace %s (%s): %v", id, m.Entry.Label, m.Err)
	}
}

// Sync runs `herdr workspace list` for s and refreshes the map and the store with it. Each
// `brain` call runs it; the hook never does. environ is the process environment, for herdr
// and git.
func (s Session) Sync(ctx context.Context, environ []string, st *store.Store, now time.Time) error {
	c, err := herdr.New(environ, herdr.Target{Session: s.Name, Socket: s.Socket})
	if err != nil {
		return err
	}
	ws, err := c.Workspaces(ctx)
	if err != nil {
		return err
	}
	return s.Refresh(ctx, environ, st, ws, now)
}

// Refresh maps each workspace in ws that has no entry yet, and updates the label of each one
// that has. A known pair keeps its key: a rename changes only the label, and a root outside
// the project folder is never remapped here. A workspace whose root cannot be read yet (herdr
// saves a new workspace about 5 s after it creates it) stays unmapped until a later call.
// Each mapped workspace has its row in st, with the label and the last seen time now: the
// compiler builds one workspace part for each row.
func (s Session) Refresh(ctx context.Context, environ []string, st *store.Store, ws []herdr.Workspace, now time.Time) error {
	return s.update(func(m *mapFile) (bool, error) {
		var seen, added []Entry
		var rs map[string][]string // read once, and only when a workspace is new
		var rootsErr error
		for _, w := range ws {
			if i, ok := m.current(s.Name, w.ID); ok {
				m.Entries[i].Label = w.Label
				seen = append(seen, m.Entries[i])
				continue
			}
			if rs == nil && rootsErr == nil {
				rs, rootsErr = roots(s.sessionFile())
			}
			r, err := rootOf(rs, w.ID)
			if err != nil {
				continue // an unreadable root: this workspace stays unmapped
			}
			e, err := s.newEntry(ctx, environ, w.ID, w.Label, r)
			if err != nil {
				return false, err
			}
			added = append(added, e)
		}
		if err := st.Write(ctx, func(tx *sql.Tx) error {
			return writeRows(ctx, tx, seen, added, now)
		}); err != nil {
			return false, err
		}
		m.Entries = append(m.Entries, added...)
		return len(seen)+len(added) > 0, nil
	})
}

// writeRows updates the rows of the seen entries and inserts the rows of the added ones. The
// rows go in before the map file, so that no mapped key lacks its row.
func writeRows(ctx context.Context, tx *sql.Tx, seen, added []Entry, now time.Time) error {
	for _, e := range seen {
		if err := store.SeeWorkspace(ctx, tx, e.Key, e.Label, now); err != nil {
			return err
		}
	}
	for _, e := range added {
		if err := store.InsertWorkspace(ctx, tx, e.row(now)); err != nil {
			return err
		}
	}
	return nil
}

func (e Entry) row(now time.Time) store.Workspace {
	return store.Workspace{Key: e.Key, HerdrSession: e.Session, HerdrWorkspaceID: e.WorkspaceID,
		Label: e.Label, RootFolder: &e.Project, Status: store.WorkspaceLive, LastSeen: now}
}

// Remapped is the result of a remap.
type Remapped struct {
	New Entry
	Old []Entry // earlier entries of the pair; their items wait for a relink
}

// Remap gives workspace id of s a new key for the project folder of its saved root. The old
// entry and its store row stay, so its items stay on the old key for a relink; nothing moves.
// The new key gets its own row. Remap refuses a workspace whose root still lies in its project
// folder.
func (s Session) Remap(ctx context.Context, environ []string, st *store.Store, id string, now time.Time) (Remapped, error) {
	var res Remapped
	err := s.update(func(m *mapFile) (bool, error) {
		i, ok := m.current(s.Name, id)
		if !ok {
			return false, fmt.Errorf("workspace %s of session %s is not mapped", id, s.Name)
		}
		r, err := root(s.sessionFile(), id)
		if err != nil {
			return false, err
		}
		old := m.Entries[i]
		if in, err := within(old.Project, r); err == nil && in {
			return false, fmt.Errorf("workspace %s: root %s is in its project folder %s; nothing to remap", id, r, old.Project)
		}
		e, err := s.newEntry(ctx, environ, id, old.Label, r)
		if err != nil {
			return false, err
		}
		if err := st.Write(ctx, func(tx *sql.Tx) error {
			return store.InsertWorkspace(ctx, tx, e.row(now))
		}); err != nil {
			return false, err
		}
		m.Entries[i].ReplacedBy = e.Key
		m.Entries = append(m.Entries, e)
		res.New = e
		for _, o := range m.Entries {
			if o.Session == s.Name && o.WorkspaceID == id && o.ReplacedBy != "" && !o.Closed {
				res.Old = append(res.Old, o)
			}
		}
		return true, nil
	})
	return res, err
}

// Relink is an old entry whose items wait for a relink, and the current entry that it
// could link to.
type Relink struct {
	Old, New Entry
}

// Relinks lists the old entries of s whose items wait for a relink, in map order: each entry
// that a remap replaced, and each closed entry with the project folder of a current entry.
// Brain links nothing by itself; `brain status` only lists them. It reads the map file only.
func (s Session) Relinks() ([]Relink, error) {
	m, err := readMap(s.MapFile)
	if err != nil {
		return nil, err
	}
	var cur []Entry
	byKey := map[string]Entry{}
	for _, e := range m.Entries {
		byKey[e.Key] = e
		if e.Session == s.Name && e.ReplacedBy == "" && !e.Closed {
			cur = append(cur, e)
		}
	}
	var rs []Relink
	for _, e := range m.Entries {
		switch {
		case e.Session != s.Name:
		case e.Closed:
			for _, c := range cur {
				if c.Project == e.Project {
					rs = append(rs, Relink{Old: e, New: c})
					break
				}
			}
		case e.ReplacedBy != "":
			rs = append(rs, Relink{Old: e, New: byKey[e.ReplacedBy]})
		}
	}
	// A new herdr id with the project folder of an older current entry: the map
	// appends entries, so the older one comes first.
	for i, e := range cur {
		for _, old := range cur[:i] {
			if old.Project == e.Project {
				rs = append(rs, Relink{Old: old, New: e})
				break
			}
		}
	}
	return rs, nil
}

// Entry returns the map entry of key in the herdr session of s, and false when there is none.
// It reads the map file only.
func (s Session) Entry(key string) (Entry, bool, error) {
	m, err := readMap(s.MapFile)
	if err != nil {
		return Entry{}, false, err
	}
	for _, e := range m.Entries {
		if e.Key == key && e.Session == s.Name {
			return e, true, nil
		}
	}
	return Entry{}, false, nil
}

// Close marks workspace id of s closed: each entry of the pair, the remapped ones too, and
// their store rows. The items stay in the store, without delivery. A later workspace
// with the same id is then unmapped, and the next `brain` call gives it a new key. An
// unknown pair changes nothing.
//
// The map goes first: herdr does not replay an event, and a map that still holds the pair
// would give a reused id the old items. The rows follow, for every entry of the pair, so a
// repeated close finishes rows that an earlier one left live.
func (s Session) Close(ctx context.Context, st *store.Store, id string) error {
	var keys []string
	err := s.update(func(m *mapFile) (bool, error) {
		changed := false
		for i, e := range m.Entries {
			if e.Session == s.Name && e.WorkspaceID == id {
				keys = append(keys, e.Key)
				changed = changed || !e.Closed
				m.Entries[i].Closed = true
			}
		}
		return changed, nil
	})
	if err != nil || len(keys) == 0 {
		return err
	}
	return st.Write(ctx, func(tx *sql.Tx) error {
		for _, k := range keys {
			if err := store.CloseWorkspace(ctx, tx, k); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s Session) newEntry(ctx context.Context, environ []string, id, label, root string) (Entry, error) {
	p, err := projectFolder(ctx, environ, root)
	if err != nil {
		return Entry{}, err
	}
	k, err := newKey()
	if err != nil {
		return Entry{}, err
	}
	return Entry{Key: k, Session: s.Name, WorkspaceID: id, Label: label, Project: p}, nil
}

// newKey returns an opaque random key. 8 random bytes, in hex with a "ws_" prefix.
func newKey() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "ws_" + hex.EncodeToString(b), nil
}

// current returns the index of the entry of the pair that no remap replaced and no close
// ended.
func (m *mapFile) current(session, id string) (int, bool) {
	for i, e := range m.Entries {
		if e.Session == session && e.WorkspaceID == id && e.ReplacedBy == "" && !e.Closed {
			return i, true
		}
	}
	return 0, false
}

// readMap reads the map file at path.
func readMap(path string) (*mapFile, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		// No `brain` call has mapped a workspace of this store yet: every workspace is unmapped.
		return &mapFile{Version: mapVersion}, nil
	}
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var m mapFile
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("parse %s: data after the map object", path)
	}
	if m.Version != mapVersion {
		return nil, fmt.Errorf("%s: version %d, want %d", path, m.Version, mapVersion)
	}
	return &m, nil
}

// update reads the map file, lets f change it and writes it back when f reports a change.
// Two `brain` calls can map at the same time, so it holds a lock file for the whole
// read-change-write; otherwise one call could drop the key that the other just made. The hook
// takes no lock: the write is a rename, so a reader sees the whole old or the whole new file.
func (s Session) update(f func(m *mapFile) (bool, error)) (err error) {
	// 0o700: the store folder holds your items, so only you may list it.
	if err := os.MkdirAll(filepath.Dir(s.MapFile), 0o700); err != nil {
		return err
	}
	lock, err := os.OpenFile(s.MapFile+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, lock.Close()) }() // Close also drops the lock
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("lock %s: %w", lock.Name(), err)
	}
	m, err := readMap(s.MapFile)
	if err != nil {
		return err
	}
	changed, err := f(m)
	if err != nil || !changed {
		return err
	}
	return writeMap(s.MapFile, m)
}

// writeMap replaces the map file atomically: it writes a temp file in the same folder and
// renames it over the old file.
func writeMap(path string, m *mapFile) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	// os.CreateTemp makes the file with mode 0o600; the rename keeps it.
	tmp, err := os.CreateTemp(filepath.Dir(path), ".workspaces-*.tmp")
	if err != nil {
		return err
	}
	if err := writeAndRename(tmp, path, append(data, '\n')); err != nil {
		// The temp file is an orphan now; the error from its removal adds nothing.
		_ = os.Remove(tmp.Name())
		return err
	}
	return nil
}

func writeAndRename(tmp *os.File, path string, data []byte) error {
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close() // the write error is the one to report
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
