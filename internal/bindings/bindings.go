// Package bindings reads and writes the session bindings file. A binding ties one herdr
// session to one store folder. All real sessions share the main store; a test session has its
// own store.
package bindings

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
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

// Dropped is a test binding that Read left out, and why.
type Dropped struct {
	Binding Binding
	Err     error
}

// Read reads the bindings file once and validates it. A bad test binding does not stop the
// other sessions: Read leaves it out and returns it in dropped.
// A test binding that conflicts with a main binding is dropped; two test bindings that
// conflict are both dropped. A bad main binding, or main bindings that disagree, make Read
// fail. A binding stays in the file when its herdr session is gone, so Read never
// checks the socket against herdr.
func Read(path string) (bs []Binding, dropped []Dropped, err error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		// No file yet: `brain setup` has bound no session, so every session is unbound.
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	// Each entry is decoded on its own, so that a malformed test entry drops only itself.
	var f struct {
		Bindings []json.RawMessage `json:"bindings"`
	}
	if err := dec.Decode(&f); err != nil {
		return nil, nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, nil, fmt.Errorf("parse %s: data after the bindings object", path)
	}
	all := make([]Binding, len(f.Bindings))
	errs := make([]error, len(f.Bindings))
	for i, raw := range f.Bindings {
		all[i], errs[i] = decode(raw)
		if errs[i] != nil && all[i].Kind != KindTest {
			return nil, nil, fmt.Errorf("parse %s: binding %d: %w", path, i, errs[i])
		}
	}
	bs, dropped, err = split(all, errs, filepath.Dir(path))
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %w", path, err)
	}
	return bs, dropped, nil
}

// decode decodes one binding. On an error it returns the fields that it could read, so that
// the caller knows the kind and can name the dropped entry.
func decode(raw json.RawMessage) (Binding, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var b Binding
	if err := dec.Decode(&b); err != nil {
		var fields map[string]json.RawMessage
		var part Binding
		if json.Unmarshal(raw, &fields) == nil {
			// A field that is missing or of the wrong type stays empty.
			_ = json.Unmarshal(fields["kind"], &part.Kind)
			_ = json.Unmarshal(fields["session"], &part.Session)
			_ = json.Unmarshal(fields["socket"], &part.Socket)
			_ = json.Unmarshal(fields["store"], &part.Store)
		}
		return part, err
	}
	return b, nil
}

// split splits bs into the bindings that pass validate and the dropped test bindings. errs
// holds the decode error of each binding, or nil. It fails on a bad main binding or on two
// main bindings that conflict.
func split(bs []Binding, errs []error, dir string) (kept []Binding, dropped []Dropped, err error) {
	for i, b := range bs {
		if errs[i] != nil {
			continue
		}
		errs[i] = checkOne(b, dir)
		if errs[i] != nil && b.Kind != KindTest {
			return nil, nil, errs[i]
		}
	}
	// Each pair of valid bindings is compared, also when a conflict already marked one of
	// them, so the result does not depend on the order of the entries.
	conflicts := make([]error, len(bs))
	for i, b := range bs {
		for j, prev := range bs[:i] {
			if errs[i] != nil || errs[j] != nil {
				continue
			}
			err := conflict(prev, b)
			switch {
			case err == nil:
			case prev.Kind == KindMain && b.Kind == KindMain:
				return nil, nil, err
			case prev.Kind == KindMain:
				conflicts[i] = cmp.Or(conflicts[i], err)
			case b.Kind == KindMain:
				conflicts[j] = cmp.Or(conflicts[j], err)
			default:
				conflicts[i], conflicts[j] = cmp.Or(conflicts[i], err), cmp.Or(conflicts[j], err)
			}
		}
	}
	for i, b := range bs {
		if err := cmp.Or(errs[i], conflicts[i]); err != nil {
			dropped = append(dropped, Dropped{Binding: b, Err: err})
			continue
		}
		kept = append(kept, b)
	}
	return kept, dropped, nil
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

// Check runs the checks of Write on bs for the bindings file at path, and writes nothing.
// `brain setup` refuses a binding with it before it asks (E9-S1).
func Check(path string, bs []Binding) error {
	return validate(bs, filepath.Dir(path))
}

// BySocket returns the binding whose socket is socket, after links and aliases, as validate
// compares them. A herdr pane gives its socket in HERDR_SOCKET_PATH, which may be a link path
// or an alias of the path that `brain setup` stored. validate allows one binding per socket.
func BySocket(bs []Binding, socket string) (Binding, bool, error) {
	// Clean drops "link/.." as text, but the kernel follows the link first (see check).
	if socket != filepath.Clean(socket) {
		return Binding{}, false, fmt.Errorf("socket path %q is not a clean path", socket)
	}
	for _, b := range bs {
		same, err := samePath(b.Socket, socket)
		if err != nil {
			return Binding{}, false, err
		}
		if same {
			return b, true, nil
		}
	}
	return Binding{}, false, nil
}

// validate checks each binding and the rules between them: one binding per session name and
// per socket, one main store for all real sessions, and each test store apart from the main
// store and from every other test store. No store may hold dir, the bindings folder, because
// the file must stay outside every store. Paths compare after links and aliases.
func validate(bs []Binding, dir string) error {
	for _, b := range bs {
		if err := checkOne(b, dir); err != nil {
			return err
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

// checkOne checks one binding on its own. It also resolves the socket, so that a pair check
// never fails on a link of one binding and blames the other. The store folder must exist:
// only then can the checks compare it with other folders by identity, which also catches
// aliases in letter case on a case-insensitive disk. `brain setup` creates it first.
func checkOne(b Binding, dir string) error {
	if err := b.check(); err != nil {
		return err
	}
	if _, err := canonical(b.Store); err != nil {
		return fmt.Errorf("session %q: %w", b.Session, err)
	}
	fi, err := os.Stat(b.Store)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("session %q: store folder %s does not exist", b.Session, b.Store)
	case err != nil:
		return fmt.Errorf("session %q: %w", b.Session, err)
	case !fi.IsDir():
		return fmt.Errorf("session %q: store %s is not a folder", b.Session, b.Store)
	}
	if err := holdsNot(b.Store, dir); err != nil {
		return fmt.Errorf("session %q: %w", b.Session, err)
	}
	if _, err := canonical(b.Socket); err != nil {
		return fmt.Errorf("session %q: %w", b.Session, err)
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
	// A test store is reset before each eval case, so it must not hold another store,
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

// Nested reports whether the existing folders a and b are one folder, or one holds the
// other, after links and aliases. `brain setup` checks a test store against the main store
// folder with it, also before any main binding exists (E9-S1, S76).
func Nested(a, b string) (bool, error) {
	return nested(a, b)
}

// nested reports whether the existing folders a and b are one folder, or one holds the other.
func nested(a, b string) (bool, error) {
	in, err := within(a, b)
	if err != nil || in {
		return in, err
	}
	return within(b, a)
}

// holdsNot returns an error when the existing folder store is dir or holds dir.
func holdsNot(store, dir string) error {
	in, err := within(store, dir)
	if err != nil {
		return err
	}
	if in {
		return fmt.Errorf("store %s holds the bindings folder %s", store, dir)
	}
	return nil
}

// within reports whether path is the existing folder parent or below it. It compares parent
// with each existing folder on the way up from path by identity, not by path text, so links
// and aliases such as another letter case on a case-insensitive disk count as the same.
func within(parent, path string) (bool, error) {
	fp, err := os.Stat(parent)
	if err != nil {
		return false, err
	}
	p, err := canonical(path)
	if err != nil {
		return false, err
	}
	for {
		fi, err := os.Stat(p)
		switch {
		case err == nil:
			if os.SameFile(fp, fi) {
				return true, nil
			}
		case !errors.Is(err, fs.ErrNotExist):
			return false, err
		}
		up := filepath.Dir(p)
		if up == p {
			return false, nil
		}
		p = up
	}
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
