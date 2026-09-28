// Package install adds the one Brain hook entry to a CLI's hook file and takes it out again
// (HLD §8.1, §8.3, E8-S2). It splices bytes: every byte outside the Brain entry stays as it is.
// The install record, <data home>/install.json, holds the entry, its hash and a snapshot of
// each file before its first edit. It sits outside every store, because the CLI files are
// shared by all bindings (§11). `brain setup` (E9) and `brain uninstall` (E8-S5) call it.
package install

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/aryanVijaywargia/herdr-brain/internal/adapter"
)

// entryTimeout is the timeout of the entry in the CLI config, in seconds [NEW] (E8-S2). The
// real limit is the 300 ms cap inside the hook (§8.3).
const entryTimeout = 5

// newFileMode is the mode of a hook file that did not exist before the install. [NEW]
const newFileMode = 0o600

var (
	// ErrLayout says that the splice cannot handle the file's layout. Nothing is written; the
	// error text holds the entry for a manual edit.
	ErrLayout = errors.New("hook file layout not supported")
	// ErrChanged says that the file changed between the read and the write. Nothing is written.
	ErrChanged = errors.New("hook file changed during the edit")
)

// Status is the outcome of Install or Uninstall.
type Status string

// The outcomes.
const (
	Added   Status = "added"   // install added the entry
	Present Status = "present" // install found the same entry in the file
	Removed Status = "removed" // uninstall removed the unchanged entry
	// Edited: the file holds a Brain entry that you changed by hand. Brain keeps it, writes
	// nothing and reports it (§8.1).
	Edited       Status = "edited"
	NotFound     Status = "not found"     // uninstall found no Brain entry in the file
	NotInstalled Status = "not installed" // the record holds no entry for the profile
)

// The wraps say what install created around the entry, so uninstall removes exactly that.
const (
	wrapGroup = "group" // one matcher group in the existing event array
	wrapEvent = "event" // the event member, "SessionStart":[entry], in the hooks object
	wrapHooks = "hooks" // the hooks member, "hooks":{"SessionStart":[entry]}, in the root
)

// Installer edits the hook files of one user. Every path comes from its fields, so tests use
// a temp home.
type Installer struct {
	Home   string // the user's home; "~/" in a profile's install_target means this folder
	Record string // the install record, <data home>/install.json
	Binary string // the absolute path of the brain binary (paths.BinaryPath)

	// beforeCheck runs after the first read of the hook file and before the check read. A
	// test changes the file here.
	beforeCheck func()
}

// Install adds the Brain entry of profile p to its hook file, as the last matcher group of the
// start event. It stores the entry, its hash and, before the first edit of the file, a
// snapshot of the whole file in the install record.
func (in *Installer) Install(p *adapter.Profile) (Status, error) {
	target := in.target(p)
	e, err := in.entry(p, target)
	if err != nil {
		return "", err
	}
	lk, err := lock(in.Record)
	if err != nil {
		return "", err
	}
	defer lk.Close() //nolint:errcheck // the close only drops the lock; the file holds no data
	rec, err := Read(in.Record)
	if err != nil {
		return "", err
	}
	old, err := readFile(target)
	if err != nil {
		return "", err
	}
	data := old.data
	if old.missing {
		data = []byte("{}\n")
	}
	out, wrap, st, err := insert(data, p, e, rec.Entries[p.Kind])
	if err != nil || st == Edited {
		return st, err
	}
	e.Wrap = wrap
	if err := in.check(old); err != nil {
		return "", err
	}
	if _, ok := rec.Snapshots[target]; !ok {
		rec.Snapshots[target] = Snapshot{Missing: old.missing, Data: old.data}
	}
	rec.Entries[p.Kind] = e
	if err := write(in.Record, rec); err != nil {
		return "", err
	}
	if st == Present {
		return Present, nil
	}
	if err := writeFile(old, out); err != nil {
		return "", err
	}
	return Added, nil
}

// Uninstall removes the Brain entry of profile p from its hook file when the entry's hash is
// unchanged. It keeps every other byte as it is now (S45). A hand-edited entry stays, and
// Uninstall returns Edited. The snapshot stays in the record as recovery material (§8.1).
func (in *Installer) Uninstall(p *adapter.Profile) (Status, error) {
	lk, err := lock(in.Record)
	if err != nil {
		return "", err
	}
	defer lk.Close() //nolint:errcheck // the close only drops the lock; the file holds no data
	rec, err := Read(in.Record)
	if err != nil {
		return "", err
	}
	e, ok := rec.Entries[p.Kind]
	if !ok {
		return NotInstalled, nil
	}
	old, out, st, err := removal(p, e)
	if err != nil || st == Edited {
		return st, err
	}
	if st == Removed {
		if err := in.check(old); err != nil {
			return "", err
		}
		if err := writeFile(old, out); err != nil {
			return "", err
		}
	}
	delete(rec.Entries, p.Kind)
	if err := write(in.Record, rec); err != nil {
		return "", err
	}
	return st, nil
}

// WouldUninstall returns what Uninstall would do for profile p now, and changes nothing: it
// takes no lock and writes no file. `brain uninstall` shows it in a dry run (E8-S5).
func (in *Installer) WouldUninstall(p *adapter.Profile) (Status, error) {
	rec, err := Read(in.Record)
	if err != nil {
		return "", err
	}
	e, ok := rec.Entries[p.Kind]
	if !ok {
		return NotInstalled, nil
	}
	_, _, st, err := removal(p, e)
	return st, err
}

// WouldInstall returns the entry that Install would add for profile p, and what Install
// would do now: Present when the hook file and the record already hold that entry, Edited
// when the file holds a Brain entry that you changed, else Added. It changes nothing.
// `brain setup` shows the entry before it asks (E9-S1).
func (in *Installer) WouldInstall(p *adapter.Profile) (Entry, Status, error) {
	target := in.target(p)
	e, err := in.entry(p, target)
	if err != nil {
		return Entry{}, "", err
	}
	rec, err := Read(in.Record)
	if err != nil {
		return Entry{}, "", err
	}
	old, err := readFile(target)
	if err != nil {
		return Entry{}, "", err
	}
	data := old.data
	if old.missing {
		data = []byte("{}\n")
	}
	_, _, st, err := insert(data, p, e, rec.Entries[p.Kind])
	if err != nil {
		return e, "", err
	}
	// Install also records an entry that is in the file but not in the record.
	if prev, ok := rec.Entries[p.Kind]; st == Present && (!ok || prev.SHA256 != e.SHA256 || prev.Target != target) {
		st = Added
	}
	return e, st, nil
}

// removal reads the hook file of the recorded entry e and returns it with the file bytes
// without e. A missing file holds no entry.
func removal(p *adapter.Profile, e Entry) (old file, out []byte, st Status, err error) {
	old, err = readFile(e.Target)
	if err != nil {
		return old, nil, "", err
	}
	if old.missing {
		return old, nil, NotFound, nil
	}
	out, st, err = remove(old.data, p, e)
	return old, out, st, err
}

// target returns the profile's install_target with "~/" as the home.
func (in *Installer) target(p *adapter.Profile) string {
	if rest, ok := strings.CutPrefix(p.InstallTarget, "~/"); ok {
		return filepath.Join(in.Home, rest)
	}
	return p.InstallTarget
}

// entry builds the frozen Brain entry of p: one matcher group with one command handler (§8.3).
// The Codex profile sets additional_context_limit 0, so Codex sends the whole brief inline;
// the field is in the entry and its hash, so Codex trust covers it (E8 epic).
func (in *Installer) entry(p *adapter.Profile, target string) (Entry, error) {
	type handler struct {
		Type                   string `json:"type"`
		Command                string `json:"command"`
		Timeout                int    `json:"timeout"`
		AdditionalContextLimit *int   `json:"additionalContextLimit,omitempty"`
	}
	command := quote(in.Binary) + " hook --profile " + p.Kind + " --event start"
	group := struct {
		Hooks []handler `json:"hooks"`
	}{[]handler{{"command", command, entryTimeout, p.AdditionalContextLimit}}}
	b, err := json.Marshal(group)
	if err != nil {
		return Entry{}, err
	}
	return Entry{Target: target, Command: command, Text: string(b), SHA256: hash(b)}, nil
}

// check reads the file again just before the write (§8.1). Any change since the first read,
// by you or another tool, stops the edit.
func (in *Installer) check(old file) error {
	if in.beforeCheck != nil {
		in.beforeCheck()
	}
	now, err := readFile(old.path)
	if err != nil {
		return err
	}
	if now.missing != old.missing || string(now.data) != string(old.data) {
		return fmt.Errorf("%w: %s", ErrChanged, old.path)
	}
	return nil
}

// insert appends entry e to the start event array of the hook file b. It returns Present with
// no change when the file already holds e, and Edited when it holds another Brain entry of p:
// one with the hash of the recorded entry prev, or a handler that runs `brain hook` for p.
func insert(b []byte, p *adapter.Profile, e, prev Entry) (out []byte, wrap string, st Status, err error) {
	event := p.Events.Start
	root, err := parse(b)
	if err != nil || root.kind != '{' {
		return nil, "", "", layoutError(e, "the file is not one JSON object")
	}
	hi := root.member("hooks")
	if hi < 0 {
		text := `"hooks":{` + jsonString(event) + ":[" + e.Text + "]}"
		return spliceAppend(b, root, text), wrapHooks, Added, nil
	}
	hooks := root.items[hi].val
	if hooks.kind != '{' {
		return nil, "", "", layoutError(e, `"hooks" is not an object`)
	}
	ei := hooks.member(event)
	if ei < 0 {
		return spliceAppend(b, hooks, jsonString(event)+":["+e.Text+"]"), wrapEvent, Added, nil
	}
	arr := hooks.items[ei].val
	if arr.kind != '[' {
		return nil, "", "", layoutError(e, fmt.Sprintf("%q is not an array", event))
	}
	for _, g := range arr.items {
		text := b[g.val.start:g.val.end]
		switch {
		case string(text) == e.Text:
			// With no record of the entry, uninstall may remove only the group.
			return nil, cmp.Or(prev.Wrap, wrapGroup), Present, nil
		case hash(text) == prev.SHA256 || isBrain(text, p):
			return nil, "", Edited, nil
		}
	}
	return spliceAppend(b, arr, e.Text), wrapGroup, Added, nil
}

// remove takes the recorded entry e out of the hook file b, with what install created around
// it (e.Wrap) when nothing else is left there.
func remove(b []byte, p *adapter.Profile, e Entry) ([]byte, Status, error) {
	event := p.Events.Start
	root, err := parse(b)
	if err != nil || root.kind != '{' {
		return nil, "", layoutError(e, "the file is not one JSON object")
	}
	hi := root.member("hooks")
	if hi < 0 || root.items[hi].val.kind != '{' {
		return nil, NotFound, nil
	}
	hooks := root.items[hi].val
	ei := hooks.member(event)
	if ei < 0 || hooks.items[ei].val.kind != '[' {
		return nil, NotFound, nil
	}
	arr := hooks.items[ei].val
	idx, st := -1, NotFound
	for i, g := range arr.items {
		text := b[g.val.start:g.val.end]
		if hash(text) == e.SHA256 {
			idx = i
			break
		}
		if isBrain(text, p) {
			st = Edited
		}
	}
	if idx < 0 {
		return nil, st, nil
	}
	from, to := arr.cut(idx)
	if e.Wrap != wrapGroup && len(arr.items) == 1 {
		from, to = hooks.cut(ei)
		if e.Wrap == wrapHooks && len(hooks.items) == 1 {
			from, to = root.cut(hi)
		}
	}
	return append(b[:from:from], b[to:]...), Removed, nil
}

// isBrain reports whether the matcher group text has a handler that runs `brain hook` for p.
// It matches the fixed arguments, not the whole command, so an entry whose command you edited
// still counts as the Brain entry: install adds no second one, and uninstall keeps it and its
// record.
func isBrain(text []byte, p *adapter.Profile) bool {
	var g struct {
		Hooks []struct {
			Command string `json:"command"`
		} `json:"hooks"`
	}
	// A group of another shape is not a Brain entry, so a decode error means "no".
	if json.Unmarshal(text, &g) != nil {
		return false
	}
	for _, h := range g.Hooks {
		if strings.Contains(h.Command, " hook --profile "+p.Kind+" ") {
			return true
		}
	}
	return false
}

func spliceAppend(b []byte, container node, text string) []byte {
	at, insert := container.appendAt(b, text)
	out := make([]byte, 0, len(b)+len(insert))
	out = append(out, b[:at]...)
	out = append(out, insert...)
	return append(out, b[at:]...)
}

func layoutError(e Entry, reason string) error {
	return fmt.Errorf("%w: %s: %s; add this entry by hand to the start event array:\n%s",
		ErrLayout, e.Target, reason, e.Text)
}

func jsonString(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		panic(err) // a Go string always encodes
	}
	return string(b)
}

func hash(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// quote returns path as one shell word: the CLIs run the command through a shell.
func quote(path string) string {
	unsafe := func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') &&
			!strings.ContainsRune("/._-+", r)
	}
	if strings.IndexFunc(path, unsafe) < 0 {
		return path
	}
	return "'" + strings.ReplaceAll(path, "'", `'\''`) + "'"
}

// file is one read of a hook file.
type file struct {
	path    string // the real file, after links: a rename must replace the file, not a link
	data    []byte
	mode    fs.FileMode
	missing bool
}

func readFile(path string) (file, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if errors.Is(err, fs.ErrNotExist) {
		// No file yet: install creates it, uninstall finds no entry.
		return file{path: path, mode: newFileMode, missing: true}, nil
	}
	if err != nil {
		return file{}, err
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return file{}, err
	}
	data, err := os.ReadFile(resolved)
	if err != nil {
		return file{}, err
	}
	return file{path: resolved, data: data, mode: info.Mode().Perm()}, nil
}

// writeFile replaces old.path with data: a temp file in the same folder, the old file's mode,
// then a rename.
func writeFile(old file, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(old.path), "."+filepath.Base(old.path)+"-*.tmp")
	if err != nil {
		return err
	}
	if err := writeAndRename(tmp, old.path, data, old.mode); err != nil {
		// The temp file is an orphan now; the error from its removal adds nothing.
		_ = os.Remove(tmp.Name())
		return err
	}
	return nil
}

func writeAndRename(tmp *os.File, path string, data []byte, mode fs.FileMode) error {
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close() // the chmod error is the one to report
		return err
	}
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

// lock takes the lock of the install record, <record>.lock. It serializes install and
// uninstall across Brain processes: each one reads the record, edits a hook file and writes
// the record, so two at the same time would lose one record update. Closing the file drops
// the lock.
func lock(record string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(record), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(lockFile(record), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close() // the lock error is the one to report
		return nil, fmt.Errorf("lock install record: %w", err)
	}
	return f, nil
}
