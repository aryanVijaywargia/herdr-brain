// Package bindings reads and writes the session bindings file (HLD §8.3 "Session bindings and
// the resolver", F1). A binding ties one herdr session to one store folder. All real sessions
// share the main store; a test session has its own store.
package bindings

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Kind says whether a binding serves a real herdr session or a test session.
type Kind string

// The binding kinds.
const (
	KindMain Kind = "main"
	KindTest Kind = "test"
)

// Binding ties one herdr session to one store folder.
type Binding struct {
	Session string    `json:"session"` // herdr session name
	Socket  string    `json:"socket"`  // herdr socket path, absolute
	Store   string    `json:"store"`   // store folder, absolute
	Kind    Kind      `json:"kind"`
	Created time.Time `json:"created"`
}

type file struct {
	Bindings []Binding `json:"bindings"`
}

// Read reads the bindings file once and validates it. A binding stays in the file when its
// herdr session is gone (§8.3), so Read never checks the socket against herdr.
func Read(path string) ([]Binding, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		// No file yet: `brain setup` has bound no session, so every session is unbound.
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var f file
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("parse %s: data after the bindings object", path)
	}
	if err := validate(f.Bindings, filepath.Dir(path)); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return f.Bindings, nil
}

// Write validates bs and replaces the bindings file atomically: it writes a temp file in the
// same folder and renames it over the old file.
func Write(path string, bs []Binding) error {
	if err := validate(bs, filepath.Dir(path)); err != nil {
		return err
	}
	data, err := json.MarshalIndent(file{Bindings: bs}, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	// [NEW] 0o700: the data home holds your items, so only you may list it.
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	// os.CreateTemp makes the file with mode 0o600 [NEW]; the rename keeps it.
	tmp, err := os.CreateTemp(dir, ".bindings-*.tmp")
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
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close() // the sync error is the one to report
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// validate checks each binding and the rules between them: one binding per session name and
// per socket, one main store for all real sessions, and each test store apart from the main
// store and from every other test store. No store may hold dir, the bindings folder, because
// the file must stay outside every store (§8.3). Paths compare after links and aliases.
func validate(bs []Binding, dir string) error {
	for _, b := range bs {
		if err := b.check(); err != nil {
			return err
		}
		if err := holdsNot(b.Store, dir); err != nil {
			return fmt.Errorf("session %q: %w", b.Session, err)
		}
	}
	for i, b := range bs {
		for _, prev := range bs[:i] {
			if err := conflict(prev, b); err != nil {
				return err
			}
		}
	}
	return nil
}

func (b Binding) check() error {
	switch {
	case b.Session == "":
		return errors.New("binding with no session name")
	case !filepath.IsAbs(b.Socket):
		return fmt.Errorf("session %q: socket path %q is not absolute", b.Session, b.Socket)
	case !filepath.IsAbs(b.Store):
		return fmt.Errorf("session %q: store folder %q is not absolute", b.Session, b.Store)
	// The kernel follows a link before it applies "..", but filepath.Clean drops "link/.."
	// as text. So a path that Clean changes could name another folder than the check sees.
	case b.Socket != filepath.Clean(b.Socket):
		return fmt.Errorf("session %q: socket path %q is not a clean path", b.Session, b.Socket)
	case b.Store != filepath.Clean(b.Store):
		return fmt.Errorf("session %q: store folder %q is not a clean path", b.Session, b.Store)
	case b.Kind != KindMain && b.Kind != KindTest:
		return fmt.Errorf("session %q: kind %q is not %q or %q", b.Session, b.Kind, KindMain, KindTest)
	case b.Created.IsZero():
		return fmt.Errorf("session %q: no created time", b.Session)
	}
	return nil
}

// conflict returns an error when b cannot sit in the same file as prev.
func conflict(prev, b Binding) error {
	if prev.Session == b.Session {
		return fmt.Errorf("session %q is already bound", b.Session)
	}
	same, err := samePath(prev.Socket, b.Socket)
	if err != nil {
		return err
	}
	if same {
		return fmt.Errorf("session %q: socket %s is already bound to session %q", b.Session, b.Socket, prev.Session)
	}
	same, err = samePath(prev.Store, b.Store)
	if err != nil {
		return err
	}
	// A test store is reset before each eval case (§12.1), so it must not hold another store,
	// nor sit inside one.
	nested, err := nested(prev.Store, b.Store)
	if err != nil {
		return err
	}
	bothMain := prev.Kind == KindMain && b.Kind == KindMain
	switch {
	case bothMain && !same:
		return fmt.Errorf("session %q: store %s is not the main store %s", b.Session, b.Store, prev.Store)
	case bothMain || !same && !nested:
		return nil
	case !same:
		return fmt.Errorf("session %q: store %s and the store %s of session %q are inside one another",
			b.Session, b.Store, prev.Store, prev.Session)
	case b.Kind == KindMain:
		return fmt.Errorf("session %q: a real session cannot use the test store %s of session %q", b.Session, b.Store, prev.Session)
	case prev.Kind == KindMain:
		return fmt.Errorf("session %q: test store %s is the main store", b.Session, b.Store)
	default:
		return fmt.Errorf("session %q: test store %s is also the test store of session %q", b.Session, b.Store, prev.Session)
	}
}

// samePath reports whether a and b name the same file after links and aliases. When both
// exist, os.SameFile also catches aliases that path text cannot show, such as another letter
// case on a case-insensitive disk.
func samePath(a, b string) (bool, error) {
	ca, err := canonical(a)
	if err != nil {
		return false, err
	}
	cb, err := canonical(b)
	if err != nil {
		return false, err
	}
	if ca == cb {
		return true, nil
	}
	fa, err := os.Stat(ca)
	if errors.Is(err, fs.ErrNotExist) {
		// A path that does not exist yet has no file to compare; canonical already compared it.
		return false, nil
	}
	if err != nil {
		return false, err
	}
	fb, err := os.Stat(cb)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return os.SameFile(fa, fb), nil
}

// nested reports whether one of a and b is inside the other, after links.
func nested(a, b string) (bool, error) {
	ca, err := canonical(a)
	if err != nil {
		return false, err
	}
	cb, err := canonical(b)
	if err != nil {
		return false, err
	}
	return inside(ca, cb) || inside(cb, ca), nil
}

// holdsNot returns an error when store is dir or holds dir, after links.
func holdsNot(store, dir string) error {
	cs, err := canonical(store)
	if err != nil {
		return err
	}
	cd, err := canonical(dir)
	if err != nil {
		return err
	}
	if cs == cd || inside(cs, cd) {
		return fmt.Errorf("store %s holds the bindings folder %s", store, dir)
	}
	return nil
}

// inside reports whether the clean path child is below parent.
func inside(parent, child string) bool {
	rel, err := filepath.Rel(parent, child)
	return err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// canonical resolves the links in p. A store folder or a socket may not exist (yet), so it
// resolves the longest part that exists and keeps the rest. A link that points nowhere is
// refused: its target could later be the main store.
func canonical(p string) (string, error) {
	p = filepath.Clean(p)
	r, err := filepath.EvalSymlinks(p)
	if err == nil {
		return r, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	if _, lerr := os.Lstat(p); lerr == nil {
		return "", fmt.Errorf("%s is a link to a missing target", p)
	}
	parent := filepath.Dir(p)
	if parent == p {
		return "", err
	}
	rp, err := canonical(parent)
	if err != nil {
		return "", err
	}
	return filepath.Join(rp, filepath.Base(p)), nil
}
