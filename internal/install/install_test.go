package install

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/aryanVijaywargia/herdr-brain/internal/adapter"
)

const fakeBinary = "/home/test/.config/herdr/plugins/github/brain-000000000000/bin/brain"

// newInstaller returns an installer whose home and data home are in a temp folder. HOME points
// there too, so no test can reach the real ~/.claude or ~/.codex.
func newInstaller(t *testing.T) *Installer {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", "")
	return &Installer{
		Home:   home,
		Record: filepath.Join(home, ".local", "share", "brain", "install.json"),
		Binary: fakeBinary,
	}
}

func profile(t *testing.T, harness string) *adapter.Profile {
	t.Helper()
	p, err := adapter.Load(harness)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// put writes data to the profile's hook file in the temp home and returns its path.
func put(t *testing.T, in *Installer, p *adapter.Profile, data []byte) string {
	t.Helper()
	path := in.target(p)
	if !strings.HasPrefix(path, in.Home+string(filepath.Separator)) {
		t.Fatalf("target %q is outside the temp home %q", path, in.Home)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o640); err != nil {
		t.Fatal(err)
	}
	return path
}

func read(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func fixture(t *testing.T, harness string) []byte {
	t.Helper()
	name := map[string]string{"claude": "claude-settings.json", "codex": "codex-hooks.json"}[harness]
	return read(t, filepath.Join("testdata", name))
}

// expect returns a check of the result of Install or Uninstall: expect(t, "Install", Added)(in.Install(p)).
func expect(t *testing.T, name string, want Status) func(Status, error) {
	t.Helper()
	return func(got Status, err error) {
		t.Helper()
		if err != nil || got != want {
			t.Fatalf("%s = %q, %v, want %q", name, got, err, want)
		}
	}
}

// startGroups decodes the start event array of a hook file.
func startGroups(t *testing.T, b []byte) []json.RawMessage {
	t.Helper()
	var doc struct {
		Hooks map[string][]json.RawMessage `json:"hooks"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatalf("hook file is not valid JSON: %v\n%s", err, b)
	}
	return doc.Hooks["SessionStart"]
}

// TestRoundTrip: the fixtures hold the herdr integration hooks and other user hooks. After
// install and uninstall, each file equals the original, byte for byte.
func TestRoundTrip(t *testing.T) {
	tests := []struct {
		name, harness string
		data          string
		wrap          string
	}{
		{"claude settings", "claude", "", wrapGroup},
		{"codex hooks", "codex", "", wrapGroup},
		{"empty start array", "claude", "{\n  \"hooks\": {\n    \"SessionStart\": []\n  }\n}\n", wrapGroup},
		{"no start event", "codex", `{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"x"}]}]}}`, wrapEvent},
		{"empty hooks", "claude", "{\"hooks\": {}}\n", wrapEvent},
		{"no hooks", "claude", "{\n\t\"model\": \"fake\"\n}\n", wrapHooks},
		{"empty object", "codex", "{}", wrapHooks},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := newInstaller(t)
			p := profile(t, tt.harness)
			orig := []byte(tt.data)
			if tt.data == "" {
				orig = fixture(t, tt.harness)
			}
			path := put(t, in, p, orig)

			expect(t, "Install", Added)(in.Install(p))
			installed := read(t, path)
			groups := startGroups(t, installed)
			rec, err := Read(in.Record)
			if err != nil {
				t.Fatal(err)
			}
			e := rec.Entries[p.Kind]
			if last := string(groups[len(groups)-1]); last != e.Text {
				t.Errorf("last start group = %s, want the Brain entry %s", last, e.Text)
			}
			if e.Target != path || e.Wrap != tt.wrap || e.SHA256 != hash([]byte(e.Text)) {
				t.Errorf("record entry = %+v, want target %s, wrap %s and the hash of the entry", e, path, tt.wrap)
			}
			if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o640 {
				t.Errorf("mode after install = %v, %v, want 0640", info.Mode().Perm(), err)
			}

			expect(t, "Uninstall", Removed)(in.Uninstall(p))
			if got := read(t, path); string(got) != string(orig) {
				t.Errorf("after uninstall:\n%s\nwant the original:\n%s", got, orig)
			}
		})
	}
}

func TestEntry(t *testing.T) {
	tests := []struct {
		harness, want string
	}{
		{"claude", `{"hooks":[{"type":"command","command":"` + fakeBinary + ` hook --profile claude --event start","timeout":5}]}`},
		{"codex", `{"hooks":[{"type":"command","command":"` + fakeBinary + ` hook --profile codex --event start","timeout":5,"additionalContextLimit":0}]}`},
	}
	for _, tt := range tests {
		in := newInstaller(t)
		p := profile(t, tt.harness)
		put(t, in, p, fixture(t, tt.harness))
		expect(t, "Install", Added)(in.Install(p))
		rec, err := Read(in.Record)
		if err != nil {
			t.Fatal(err)
		}
		if got := rec.Entries[p.Kind].Text; got != tt.want {
			t.Errorf("%s entry = %s, want %s", tt.harness, got, tt.want)
		}
	}
}

func TestQuote(t *testing.T) {
	tests := []struct{ in, want string }{
		{"/opt/brain/bin/brain", "/opt/brain/bin/brain"},
		{"/Users/a b/brain", "'/Users/a b/brain'"},
		{"/tmp/it's/brain", `'/tmp/it'\''s/brain'`},
	}
	for _, tt := range tests {
		if got := quote(tt.in); got != tt.want {
			t.Errorf("quote(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestInstallTwiceAddsOneEntry(t *testing.T) {
	for _, harness := range []string{"claude", "codex"} {
		in := newInstaller(t)
		p := profile(t, harness)
		path := put(t, in, p, fixture(t, harness))
		expect(t, "Install", Added)(in.Install(p))
		once := read(t, path)
		expect(t, "second Install", Present)(in.Install(p))
		if got := read(t, path); string(got) != string(once) {
			t.Errorf("%s: second install changed the file:\n%s", harness, got)
		}
		if n := strings.Count(string(once), "hook --profile "+harness); n != 1 {
			t.Errorf("%s: file holds %d Brain entries, want 1", harness, n)
		}
	}
}

// A lost install record does not give a second entry: install finds the same entry.
func TestInstallWithLostRecord(t *testing.T) {
	in := newInstaller(t)
	p := profile(t, "claude")
	path := put(t, in, p, fixture(t, "claude"))
	expect(t, "Install", Added)(in.Install(p))
	once := read(t, path)
	if err := os.Remove(in.Record); err != nil {
		t.Fatal(err)
	}
	expect(t, "Install", Present)(in.Install(p))
	if got := read(t, path); string(got) != string(once) {
		t.Errorf("install after a lost record changed the file:\n%s", got)
	}
	expect(t, "Uninstall", Removed)(in.Uninstall(p))
	if got := read(t, path); string(got) != string(fixture(t, "claude")) {
		t.Errorf("after uninstall:\n%s\nwant the original", got)
	}
}

func TestHandEditedEntryStays(t *testing.T) {
	tests := []struct{ name, old, new string }{
		{"timeout", `"timeout":5,`, `"timeout":7,`},
		{"command", `--event start"`, `--event start --verbose"`},
		{"binary path", fakeBinary + " hook", "/home/test/.local/bin/brain hook"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := newInstaller(t)
			p := profile(t, "codex")
			path := put(t, in, p, fixture(t, "codex"))
			expect(t, "Install", Added)(in.Install(p))
			edited := strings.Replace(string(read(t, path)), tt.old, tt.new, 1)
			put(t, in, p, []byte(edited))

			expect(t, "Uninstall", Edited)(in.Uninstall(p))
			if got := read(t, path); string(got) != edited {
				t.Errorf("uninstall changed a hand-edited file:\n%s", got)
			}
			rec, err := Read(in.Record)
			if err != nil {
				t.Fatal(err)
			}
			if _, ok := rec.Entries["codex"]; !ok {
				t.Error("record lost the entry of a hand-edited file, want it kept")
			}
			expect(t, "Install", Edited)(in.Install(p))
			if got := read(t, path); string(got) != edited {
				t.Errorf("install changed a hand-edited file:\n%s", got)
			}
		})
	}
}

// Two installs at the same time, from two panes: the record lock keeps both record updates.
func TestOverlappingInstalls(t *testing.T) {
	in := newInstaller(t)
	claude, codex := profile(t, "claude"), profile(t, "codex")
	claudeFile := put(t, in, claude, fixture(t, "claude"))
	codexFile := put(t, in, codex, fixture(t, "codex"))
	other := &Installer{Home: in.Home, Record: in.Record, Binary: in.Binary}

	done := make(chan error, 1)
	in.beforeCheck = func() {
		f, err := os.OpenFile(in.Record+".lock", os.O_RDWR, 0)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close() //nolint:errcheck // read-only probe of the lock
		if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); !errors.Is(err, syscall.EWOULDBLOCK) {
			t.Errorf("record lock during install: flock = %v, want EWOULDBLOCK", err)
		}
		go func() {
			_, err := other.Install(codex)
			done <- err
		}()
	}
	expect(t, "Install claude", Added)(in.Install(claude))
	if err := <-done; err != nil {
		t.Fatalf("Install codex = %v", err)
	}

	rec, err := Read(in.Record)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{claudeFile, codexFile} {
		if _, ok := rec.Snapshots[path]; !ok {
			t.Errorf("record has no snapshot of %s", path)
		}
	}
	in.beforeCheck = nil
	expect(t, "Uninstall claude", Removed)(in.Uninstall(claude))
	expect(t, "Uninstall codex", Removed)(in.Uninstall(codex))
	for path, want := range map[string][]byte{claudeFile: fixture(t, "claude"), codexFile: fixture(t, "codex")} {
		if got := read(t, path); string(got) != string(want) {
			t.Errorf("%s after uninstall:\n%s\nwant the original", path, got)
		}
	}
}

// TestS45: text you change around the Brain entry stays; only the unchanged entry goes.
func TestS45(t *testing.T) {
	in := newInstaller(t)
	p := profile(t, "claude")
	orig := string(fixture(t, "claude"))
	path := put(t, in, p, []byte(orig))
	expect(t, "Install", Added)(in.Install(p))
	rec, err := Read(in.Record)
	if err != nil {
		t.Fatal(err)
	}
	entry := rec.Entries["claude"].Text
	user := `{"hooks":[{"type":"command","command":"/home/test/bin/mine"}]}`

	// Your edits after the install: a new group after the Brain entry, another timeout, another
	// model.
	edit := func(s, afterHerdrGroup string) string {
		s = strings.Replace(s, afterHerdrGroup, afterHerdrGroup+",\n      "+user, 1)
		s = strings.Replace(s, `"timeout": 10`, `"timeout": 20`, 1)
		return strings.Replace(s, `"model": "fake-model"`, `"model": "other-model"`, 1)
	}
	changed := edit(string(read(t, path)), entry)
	put(t, in, p, []byte(changed))
	want := edit(orig, "        ]\n      }")

	expect(t, "Uninstall", Removed)(in.Uninstall(p))
	if got := read(t, path); string(got) != want {
		t.Errorf("after uninstall:\n%s\nwant:\n%s", got, want)
	}
}

// A file that changes between the read and the write is detected, and nothing is written.
func TestChangedBeforeWrite(t *testing.T) {
	for _, op := range []string{"install", "uninstall"} {
		t.Run(op, func(t *testing.T) {
			in := newInstaller(t)
			p := profile(t, "claude")
			path := put(t, in, p, fixture(t, "claude"))
			run := in.Install
			if op == "uninstall" {
				expect(t, "Install", Added)(in.Install(p))
				run = in.Uninstall
			}
			before, err := os.ReadFile(in.Record)
			if err != nil && !errors.Is(err, fs.ErrNotExist) {
				t.Fatal(err)
			}
			other := strings.Replace(string(read(t, path)), "fake-model", "changed-model", 1)
			in.beforeCheck = func() { put(t, in, p, []byte(other)) }

			if st, err := run(p); !errors.Is(err, ErrChanged) {
				t.Fatalf("%s = %q, %v, want ErrChanged", op, st, err)
			}
			if got := read(t, path); string(got) != other {
				t.Errorf("hook file = \n%s\nwant the other tool's bytes", got)
			}
			after, err := os.ReadFile(in.Record)
			if err != nil && !errors.Is(err, fs.ErrNotExist) {
				t.Fatal(err)
			}
			if string(after) != string(before) {
				t.Errorf("install record changed:\n%s\nwant:\n%s", after, before)
			}
		})
	}
}

// A layout the splice cannot handle is refused: nothing is written, and the error holds the
// entry for a manual edit.
func TestLayoutRefused(t *testing.T) {
	tests := []struct{ name, data string }{
		{"not json", `{"hooks": {`},
		{"root array", `[]`},
		{"hooks not object", `{"hooks": []}`},
		{"start not array", `{"hooks": {"SessionStart": {}}}`},
		{"comments", "// my settings\n{}"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := newInstaller(t)
			p := profile(t, "claude")
			path := put(t, in, p, []byte(tt.data))
			st, err := in.Install(p)
			if !errors.Is(err, ErrLayout) || !strings.Contains(err.Error(), "hook --profile claude --event start") {
				t.Fatalf("Install = %q, %v, want ErrLayout with the entry", st, err)
			}
			if got := read(t, path); string(got) != tt.data {
				t.Errorf("file = %s, want it unchanged", got)
			}
			if _, err := os.Stat(in.Record); !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("install record: %v, want no file", err)
			}
		})
	}
}

// The snapshot holds the whole file before the first edit, and stays after uninstall.
func TestSnapshot(t *testing.T) {
	in := newInstaller(t)
	p := profile(t, "codex")
	orig := fixture(t, "codex")
	path := put(t, in, p, orig)
	expect(t, "Install", Added)(in.Install(p))
	expect(t, "Uninstall", Removed)(in.Uninstall(p))
	put(t, in, p, []byte(`{"hooks":{}}`))
	expect(t, "Install", Added)(in.Install(p))

	rec, err := Read(in.Record)
	if err != nil {
		t.Fatal(err)
	}
	if s := rec.Snapshots[path]; s.Missing || string(s.Data) != string(orig) {
		t.Errorf("snapshot = %+v, want the bytes before the first edit", s)
	}
	if info, err := os.Stat(in.Record); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("install record mode = %v, %v, want 0600", info.Mode().Perm(), err)
	}
}

func TestMissingFile(t *testing.T) {
	in := newInstaller(t)
	p := profile(t, "codex")
	path := in.target(p)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	expect(t, "Install", Added)(in.Install(p))
	if groups := startGroups(t, read(t, path)); len(groups) != 1 {
		t.Errorf("new file has %d start groups, want 1", len(groups))
	}
	rec, err := Read(in.Record)
	if err != nil {
		t.Fatal(err)
	}
	if !rec.Snapshots[path].Missing {
		t.Errorf("snapshot = %+v, want missing", rec.Snapshots[path])
	}
	expect(t, "Uninstall", Removed)(in.Uninstall(p))
	if got := read(t, path); string(got) != "{}\n" {
		t.Errorf("after uninstall = %q, want %q", got, "{}\n")
	}
}

// A hook file that is a link (a dotfiles repo) stays a link; Brain edits the linked file.
func TestLinkedFile(t *testing.T) {
	in := newInstaller(t)
	p := profile(t, "claude")
	dotfile := filepath.Join(in.Home, "dotfiles", "settings.json")
	if err := os.MkdirAll(filepath.Dir(dotfile), 0o700); err != nil {
		t.Fatal(err)
	}
	orig := fixture(t, "claude")
	if err := os.WriteFile(dotfile, orig, 0o644); err != nil {
		t.Fatal(err)
	}
	link := in.target(p)
	if err := os.MkdirAll(filepath.Dir(link), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(dotfile, link); err != nil {
		t.Fatal(err)
	}
	expect(t, "Install", Added)(in.Install(p))
	if info, err := os.Lstat(link); err != nil || info.Mode()&fs.ModeSymlink == 0 {
		t.Fatalf("hook file after install: %v, %v, want a link", info, err)
	}
	if len(startGroups(t, read(t, dotfile))) != 2 {
		t.Error("linked file has no Brain entry")
	}
	expect(t, "Uninstall", Removed)(in.Uninstall(p))
	if got := read(t, dotfile); string(got) != string(orig) {
		t.Errorf("linked file after uninstall:\n%s\nwant the original", got)
	}
}

// Uninstall of an entry that you removed by hand drops it from the record.
func TestUninstallNotFound(t *testing.T) {
	in := newInstaller(t)
	p := profile(t, "claude")
	expect(t, "Uninstall", NotInstalled)(in.Uninstall(p))
	path := put(t, in, p, fixture(t, "claude"))
	expect(t, "Install", Added)(in.Install(p))
	put(t, in, p, fixture(t, "claude"))
	expect(t, "Uninstall", NotFound)(in.Uninstall(p))
	if got := read(t, path); string(got) != string(fixture(t, "claude")) {
		t.Errorf("file = %s, want it unchanged", got)
	}
	expect(t, "Uninstall", NotInstalled)(in.Uninstall(p))
}

// Install once, reset a test store, then inspect and uninstall from the main binding: the
// install record is still there, and nothing moves between the stores.
func TestRecordOutsideStores(t *testing.T) {
	in := newInstaller(t)
	dataHome := filepath.Dir(in.Record)
	mainStore := filepath.Join(dataHome, "main")
	testStore := filepath.Join(in.Home, "eval-store")
	for _, dir := range []string{mainStore, testStore} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "brain.db"), []byte(dir), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	p := profile(t, "claude")
	put(t, in, p, fixture(t, "claude"))
	expect(t, "Install", Added)(in.Install(p)) // from the test binding

	if err := os.RemoveAll(testStore); err != nil { // the eval runner resets the test store
		t.Fatal(err)
	}
	if err := os.MkdirAll(testStore, 0o700); err != nil {
		t.Fatal(err)
	}

	rec, err := Read(in.Record)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := rec.Entries["claude"]; !ok {
		t.Fatal("install record lost the entry after a test store reset")
	}
	expect(t, "Uninstall", Removed)(in.Uninstall(p)) // from the main binding

	if got := list(t, mainStore); strings.Join(got, ",") != "brain.db" {
		t.Errorf("main store holds %v, want only brain.db", got)
	}
	if got := read(t, filepath.Join(mainStore, "brain.db")); string(got) != mainStore {
		t.Errorf("main store file changed: %q", got)
	}
	if got := list(t, testStore); len(got) != 0 {
		t.Errorf("reset test store holds %v, want nothing", got)
	}
}

func list(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// WouldUninstall gives the status of Uninstall and writes nothing: no hook file, no record and
// no lock file.
func TestWouldUninstall(t *testing.T) {
	in := newInstaller(t)
	p := profile(t, "claude")
	expect(t, "WouldUninstall", NotInstalled)(in.WouldUninstall(p))
	if _, err := os.Stat(filepath.Dir(in.Record)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("data home after WouldUninstall with no record: %v, want none", err)
	}

	path := put(t, in, p, fixture(t, "claude"))
	expect(t, "Install", Added)(in.Install(p))
	installed, rec := read(t, path), read(t, in.Record)
	if err := os.Remove(lockFile(in.Record)); err != nil {
		t.Fatal(err)
	}
	expect(t, "WouldUninstall", Removed)(in.WouldUninstall(p))
	if string(read(t, path)) != string(installed) || string(read(t, in.Record)) != string(rec) {
		t.Error("WouldUninstall changed the hook file or the record")
	}
	if _, err := os.Stat(lockFile(in.Record)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("lock file after WouldUninstall: %v, want none", err)
	}

	put(t, in, p, []byte(strings.Replace(string(installed), `"timeout":5`, `"timeout":6`, 1)))
	expect(t, "WouldUninstall of an edited entry", Edited)(in.WouldUninstall(p))
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	expect(t, "WouldUninstall with no hook file", NotFound)(in.WouldUninstall(p))
}
