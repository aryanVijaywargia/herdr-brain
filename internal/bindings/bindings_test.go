package bindings

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

var created = time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC)

func binding(session, socket, store string, kind Kind) Binding {
	return Binding{Session: session, Socket: socket, Store: store, Kind: kind, Created: created}
}

func mkdir(t *testing.T, p string) string {
	t.Helper()
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func symlink(t *testing.T, target, link string) string {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	return link
}

func TestRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "data", "bindings.json")
	main := mkdir(t, filepath.Join(dir, "main"))
	want := []Binding{
		binding("default", filepath.Join(dir, "default.sock"), main, KindMain),
		binding("work", filepath.Join(dir, "work.sock"), main, KindMain),
		binding("brain-eval", filepath.Join(dir, "eval.sock"), filepath.Join(dir, "eval-store"), KindTest),
	}
	if err := Write(path, want); err != nil {
		t.Fatal(err)
	}
	got, err := Read(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Read after Write = %+v, want %+v", got, want)
	}

	// A second write replaces the file.
	want = want[:1]
	if err := Write(path, want); err != nil {
		t.Fatal(err)
	}
	if got, err = Read(path); err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("Read after second Write = %+v, %v, want %+v", got, err, want)
	}
}

func TestReadMissingFileHasNoBindings(t *testing.T) {
	got, err := Read(filepath.Join(t.TempDir(), "bindings.json"))
	if err != nil || got != nil {
		t.Errorf("Read(missing) = %v, %v, want nil, nil", got, err)
	}
}

func TestWriteIsAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bindings.json")
	good := []Binding{binding("default", "/tmp/default.sock", "/tmp/main", KindMain)}
	if err := Write(path, good); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	// A refused write leaves the old file as it was.
	bad := []Binding{good[0], binding("default", "/tmp/other.sock", "/tmp/main", KindMain)}
	if err := Write(path, bad); err == nil {
		t.Fatal("Write with a duplicate session: want an error")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Errorf("file after refused Write = %s, want %s", after, before)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("folder has %d entries, want only bindings.json (no temp file left)", len(entries))
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if mode := fi.Mode().Perm(); mode != 0o600 {
		t.Errorf("file mode = %o, want 600", mode)
	}
}

func TestReadRejectsBadFile(t *testing.T) {
	valid := `{"session":"default","socket":"/tmp/d.sock","store":"/tmp/main","kind":"main","created":"2026-09-28T10:00:00Z"}`
	tests := []struct {
		name, content string
	}{
		{"not json", "bindings = []"},
		{"unknown field", `{"bindings":[],"extra":1}`},
		{"unknown binding field", `{"bindings":[{"session":"a","color":"red"}]}`},
		{"data after object", `{"bindings":[]} {}`},
		{"duplicate session", `{"bindings":[` + valid + `,` + strings.Replace(valid, "d.sock", "e.sock", 1) + `]}`},
		{"bad kind", `{"bindings":[` + strings.Replace(valid, `"main"`, `"prod"`, 1) + `]}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "bindings.json")
			if err := os.WriteFile(path, []byte(tt.content), 0o600); err != nil {
				t.Fatal(err)
			}
			if got, err := Read(path); err == nil {
				t.Errorf("Read(%s) = %+v, nil, want an error", tt.content, got)
			}
		})
	}
}

// Each case is a valid first binding plus a second one that must be refused.
func TestWriteRefuses(t *testing.T) {
	dir := t.TempDir()
	main := mkdir(t, filepath.Join(dir, "main"))
	evalStore := mkdir(t, filepath.Join(dir, "eval"))
	sock := filepath.Join(dir, "default.sock")
	if err := os.WriteFile(sock, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	sockLink := symlink(t, sock, filepath.Join(dir, "sock-link"))
	mainLink := symlink(t, main, filepath.Join(dir, "main-link"))
	evalLink := symlink(t, evalStore, filepath.Join(dir, "eval-link"))
	dangling := symlink(t, filepath.Join(dir, "not-yet"), filepath.Join(dir, "dangling"))
	otherSock := filepath.Join(dir, "other.sock")

	first := binding("default", sock, main, KindMain)
	firstTest := binding("brain-eval", filepath.Join(dir, "eval.sock"), evalStore, KindTest)
	tests := []struct {
		name          string
		first, second Binding
	}{
		{"same session name", first, binding("default", otherSock, main, KindMain)},
		{"same socket", first, binding("work", sock, main, KindMain)},
		{"same socket through a link", first, binding("work", sockLink, main, KindMain)},
		{"same socket through dot-dot", first, binding("work", filepath.Join(dir, "x", "..", "default.sock"), main, KindMain)},
		{"S76: test store is the main store through a link", first, binding("brain-eval", otherSock, mainLink, KindTest)},
		{"test store is the main store", first, binding("brain-eval", otherSock, main, KindTest)},
		{"test store is another test store through a link", firstTest, binding("eval-2", otherSock, evalLink, KindTest)},
		{"real session uses a test store", firstTest, binding("work", otherSock, evalStore, KindMain)},
		{"second main store", first, binding("work", otherSock, evalStore, KindMain)},
		{"store is a dangling link", first, binding("brain-eval", otherSock, dangling, KindTest)},
		{"relative socket", first, binding("work", "work.sock", main, KindMain)},
		{"relative store", first, binding("work", otherSock, "main", KindMain)},
		{"no session name", first, binding("", otherSock, main, KindMain)},
		{"no created time", first, Binding{Session: "work", Socket: otherSock, Store: main, Kind: KindMain}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "bindings.json")
			if err := Write(path, []Binding{tt.first}); err != nil {
				t.Fatalf("Write(first) = %v, want nil", err)
			}
			if err := Write(path, []Binding{tt.first, tt.second}); err == nil {
				t.Errorf("Write(%+v) = nil, want an error", tt.second)
			}
		})
	}
}

// On a case-insensitive disk (the macOS default), another letter case is the same folder.
func TestWriteRefusesCaseAlias(t *testing.T) {
	dir := t.TempDir()
	main := mkdir(t, filepath.Join(dir, "Main"))
	alias := filepath.Join(dir, "main")
	if _, err := os.Stat(alias); err != nil {
		t.Skip("case-sensitive disk: no case alias")
	}
	bs := []Binding{
		binding("default", filepath.Join(dir, "d.sock"), main, KindMain),
		binding("brain-eval", filepath.Join(dir, "e.sock"), alias, KindTest),
	}
	if err := Write(filepath.Join(dir, "bindings.json"), bs); err == nil {
		t.Error("Write with a test store that is the main store in another case: want an error")
	}
}

// §8.3: when a bound session disappears from herdr, its binding stays in the file.
func TestBindingOfGoneSessionStays(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bindings.json")
	main := mkdir(t, filepath.Join(dir, "main"))
	sock := filepath.Join(dir, "work.sock")
	if err := os.WriteFile(sock, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	work := binding("work", sock, main, KindMain)
	if err := Write(path, []Binding{work}); err != nil {
		t.Fatal(err)
	}
	// The herdr session ends: its socket goes away.
	if err := os.Remove(sock); err != nil {
		t.Fatal(err)
	}

	bs, err := Read(path)
	if err != nil {
		t.Fatal(err)
	}
	bs = append(bs, binding("default", filepath.Join(dir, "default.sock"), main, KindMain))
	if err := Write(path, bs); err != nil {
		t.Fatal(err)
	}
	got, err := Read(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != work {
		t.Errorf("Read = %+v, want the gone session %+v first", got, work)
	}
}
