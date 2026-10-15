package bindings

import (
	"encoding/json"
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
		binding("brain-eval", filepath.Join(dir, "eval.sock"), mkdir(t, filepath.Join(dir, "eval-store")), KindTest),
	}
	if err := Write(path, want); err != nil {
		t.Fatal(err)
	}
	got, _, err := Read(path)
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
	if got, _, err = Read(path); err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("Read after second Write = %+v, %v, want %+v", got, err, want)
	}
}

func TestReadMissingFileHasNoBindings(t *testing.T) {
	got, dropped, err := Read(filepath.Join(t.TempDir(), "bindings.json"))
	if err != nil || got != nil || dropped != nil {
		t.Errorf("Read(missing) = %v, %v, want nil, nil", got, err)
	}
}

func TestWriteIsAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bindings.json")
	main := t.TempDir()
	good := []Binding{binding("default", "/tmp/default.sock", main, KindMain)}
	if err := Write(path, good); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	// A refused write leaves the old file as it was.
	bad := []Binding{good[0], binding("default", "/tmp/other.sock", main, KindMain)}
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
	store := t.TempDir()
	valid := `{"session":"default","socket":"/tmp/d.sock","store":"` + store + `","kind":"main","created":"2026-09-28T10:00:00Z"}`
	tests := []struct {
		name, content, wantErr string
	}{
		{"not json", "bindings = []", "invalid character"},
		{"unknown field", `{"bindings":[],"extra":1}`, `unknown field "extra"`},
		{"unknown binding field", `{"bindings":[` + strings.Replace(valid, `{`, `{"color":"red",`, 1) + `]}`, `unknown field "color"`},
		{"object after object", `{"bindings":[]} {}`, "data after the bindings object"},
		{"brace after object", `{"bindings":[]} }`, "data after the bindings object"},
		{"duplicate session", `{"bindings":[` + valid + `,` + strings.Replace(valid, "d.sock", "e.sock", 1) + `]}`, "already bound"},
		{"bad kind", `{"bindings":[` + strings.Replace(valid, `"main"`, `"prod"`, 1) + `]}`, `kind "prod"`},
		{"main binding with a bad time", `{"bindings":[` + strings.Replace(valid, `"2026-09-28T10:00:00Z"`, `"not-a-time"`, 1) + `]}`, "binding 0"},
		{"main binding with a wrong type", `{"bindings":[` + strings.Replace(valid, `"/tmp/d.sock"`, `7`, 1) + `]}`, "binding 0"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "bindings.json")
			if err := os.WriteFile(path, []byte(tt.content), 0o600); err != nil {
				t.Fatal(err)
			}
			if got, _, err := Read(path); err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("Read(%s) = %+v, %v, want an error with %q", tt.content, got, err, tt.wantErr)
			}
		})
	}
}

// Each case is a valid first binding plus a second one that must be refused.
func TestWriteRefuses(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "data", "bindings.json")
	main := mkdir(t, filepath.Join(dir, "stores", "main"))
	evalStore := mkdir(t, filepath.Join(dir, "eval"))
	mkdir(t, filepath.Join(main, "eval"))
	mkdir(t, filepath.Join(evalStore, "2"))
	sock := filepath.Join(dir, "default.sock")
	if err := os.WriteFile(sock, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	sockLink := symlink(t, sock, filepath.Join(dir, "sock-link"))
	mainLink := symlink(t, main, filepath.Join(dir, "main-link"))
	evalLink := symlink(t, evalStore, filepath.Join(dir, "eval-link"))
	subLink := symlink(t, mkdir(t, filepath.Join(dir, "stores", "sub")), filepath.Join(dir, "sub-link"))
	dangling := symlink(t, filepath.Join(dir, "not-yet"), filepath.Join(dir, "dangling"))
	otherSock := filepath.Join(dir, "other.sock")

	first := binding("default", sock, main, KindMain)
	firstTest := binding("brain-eval", filepath.Join(dir, "eval.sock"), evalStore, KindTest)
	tests := []struct {
		name          string
		first, second Binding
		wantErr       string
	}{
		{"same session name", first, binding("default", otherSock, main, KindMain), "already bound"},
		{"same socket", first, binding("work", sock, main, KindMain), "already bound to session"},
		{"same socket through a link", first, binding("work", sockLink, main, KindMain), "already bound to session"},
		// Raw strings: filepath.Join would clean the "..".
		{"socket with dot-dot", first, binding("work", dir+"/x/../default.sock", main, KindMain), "not a clean path"},
		{"test store through a link and dot-dot", first, binding("brain-eval", otherSock, subLink+"/../main", KindTest), "not a clean path"},
		{"S76: test store is the main store through a link", first, binding("brain-eval", otherSock, mainLink, KindTest), "is the main store"},
		{"test store is the main store", first, binding("brain-eval", otherSock, main, KindTest), "is the main store"},
		{"test store holds the main store", first, binding("brain-eval", otherSock, filepath.Join(dir, "stores"), KindTest), "inside one another"},
		{"test store inside the main store", first, binding("brain-eval", otherSock, filepath.Join(mainLink, "eval"), KindTest), "inside one another"},
		{"test store inside another test store", firstTest, binding("eval-2", otherSock, filepath.Join(evalLink, "2"), KindTest), "inside one another"},
		{"test store is the data home", first, binding("brain-eval", otherSock, filepath.Join(dir, "data"), KindTest), "holds the bindings folder"},
		{"test store is another test store through a link", firstTest, binding("eval-2", otherSock, evalLink, KindTest), "also the test store"},
		{"real session uses a test store", firstTest, binding("work", otherSock, evalStore, KindMain), "cannot use the test store"},
		{"second main store", first, binding("work", otherSock, evalStore, KindMain), "not the main store"},
		{"store is a dangling link", first, binding("brain-eval", otherSock, dangling, KindTest), "missing target"},
		{"store does not exist", first, binding("brain-eval", otherSock, filepath.Join(dir, "none"), KindTest), "does not exist"},
		{"store is a file", first, binding("brain-eval", otherSock, sock, KindTest), "not a folder"},
		{"relative socket", first, binding("work", "work.sock", main, KindMain), "not absolute"},
		{"relative store", first, binding("work", otherSock, "main", KindMain), "not absolute"},
		{"no session name", first, binding("", otherSock, main, KindMain), "no session name"},
		{"no created time", first, Binding{Session: "work", Socket: otherSock, Store: main, Kind: KindMain}, "no created time"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := Write(path, []Binding{tt.first}); err != nil {
				t.Fatalf("Write(first) = %v, want nil", err)
			}
			if err := Write(path, []Binding{tt.first, tt.second}); err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("Write(%+v) = %v, want an error with %q", tt.second, err, tt.wantErr)
			}
		})
	}
}

// §8.3: the bindings file stays outside every store, the main store too.
func TestWriteRefusesStoreThatHoldsBindings(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "data", "bindings.json")
	mkdir(t, filepath.Join(dir, "data"))
	for _, store := range []string{filepath.Join(dir, "data"), dir} {
		t.Run(store, func(t *testing.T) {
			b := binding("default", filepath.Join(dir, "d.sock"), store, KindMain)
			if err := Write(path, []Binding{b}); err == nil || !strings.Contains(err.Error(), "holds the bindings folder") {
				t.Errorf("Write(store %s) = %v, want an error with %q", store, err, "holds the bindings folder")
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

// caseInsensitive reports whether dir sits on a disk that ignores letter case.
func caseInsensitive(t *testing.T, dir string) bool {
	t.Helper()
	probe := mkdir(t, filepath.Join(dir, "Probe"))
	_, err := os.Stat(filepath.Join(dir, "probe"))
	if err := os.Remove(probe); err != nil {
		t.Fatal(err)
	}
	return err == nil
}

// A test store that holds the main store in another
// letter case. A reset of the test store would delete the main store.
func TestWriteRefusesCaseAliasParent(t *testing.T) {
	dir := t.TempDir()
	if !caseInsensitive(t, dir) {
		t.Skip("case-sensitive disk: no case alias")
	}
	main := mkdir(t, filepath.Join(dir, "Stores", "main"))
	bs := []Binding{
		binding("default", filepath.Join(dir, "d.sock"), main, KindMain),
		binding("brain-eval", filepath.Join(dir, "e.sock"), filepath.Join(dir, "stores"), KindTest),
	}
	if err := Write(filepath.Join(dir, "bindings.json"), bs); err == nil || !strings.Contains(err.Error(), "inside one another") {
		t.Errorf("Write = %v, want an error with %q", err, "inside one another")
	}
}

// Two stores that differ only in letter case. Before the
// folders exist, nothing can tell whether they are one folder, so Write refuses a missing
// store. Once created, they are one folder.
func TestWriteRefusesCaseAliasOfMissingStore(t *testing.T) {
	dir := t.TempDir()
	if !caseInsensitive(t, dir) {
		t.Skip("case-sensitive disk: no case alias")
	}
	path := filepath.Join(dir, "bindings.json")
	bs := []Binding{
		binding("default", filepath.Join(dir, "d.sock"), filepath.Join(dir, "Future"), KindMain),
		binding("brain-eval", filepath.Join(dir, "e.sock"), filepath.Join(dir, "future"), KindTest),
	}
	if err := Write(path, bs); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("Write before the folders exist = %v, want an error with %q", err, "does not exist")
	}
	mkdir(t, filepath.Join(dir, "Future"))
	if err := Write(path, bs); err == nil || !strings.Contains(err.Error(), "is the main store") {
		t.Errorf("Write after mkdir = %v, want an error with %q", err, "is the main store")
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

	bs, _, err := Read(path)
	if err != nil {
		t.Fatal(err)
	}
	bs = append(bs, binding("default", filepath.Join(dir, "default.sock"), main, KindMain))
	if err := Write(path, bs); err != nil {
		t.Fatal(err)
	}
	got, _, err := Read(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != work {
		t.Errorf("Read = %+v, want the gone session %+v first", got, work)
	}
}

func TestBySocket(t *testing.T) {
	dir := t.TempDir()
	main := mkdir(t, filepath.Join(dir, "main"))
	sockets := mkdir(t, filepath.Join(dir, "sockets"))
	sock := filepath.Join(sockets, "herdr.sock")
	if err := os.WriteFile(sock, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	link := symlink(t, sockets, filepath.Join(dir, "link"))
	bs := []Binding{
		binding("default", sock, main, KindMain),
		binding("brain-eval", filepath.Join(dir, "eval.sock"), filepath.Join(dir, "eval-store"), KindTest),
	}
	tests := []struct {
		name   string
		socket string
		want   string // session; empty: no binding
	}{
		{name: "stored path", socket: sock, want: "default"},
		{name: "path through a link", socket: filepath.Join(link, "herdr.sock"), want: "default"},
		{name: "socket that does not exist yet", socket: filepath.Join(dir, "eval.sock"), want: "brain-eval"},
		{name: "unbound socket", socket: filepath.Join(dir, "other.sock")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b, ok, err := BySocket(bs, tt.socket)
			if err != nil || ok != (tt.want != "") || b.Session != tt.want {
				t.Errorf("BySocket(%s) = %q, %v, %v, want %q", tt.socket, b.Session, ok, err, tt.want)
			}
		})
	}

	broken := symlink(t, filepath.Join(dir, "gone"), filepath.Join(dir, "broken.sock"))
	if _, ok, err := BySocket(bs, broken); err == nil || ok {
		t.Errorf("BySocket(link to a missing target) = %v, %v, want an error", ok, err)
	}
	// The kernel resolves link/.. to the parent of the
	// link target, but Clean would make this the stored socket.
	other := mkdir(t, filepath.Join(dir, "other", "nested"))
	dotdot := symlink(t, other, filepath.Join(sockets, "up")) + "/../herdr.sock"
	if b, ok, err := BySocket(bs, dotdot); err == nil || ok {
		t.Errorf("BySocket(%s) = %q, %v, %v, want an error", dotdot, b.Session, ok, err)
	}
}

// writeFile writes bs as a bindings file without the checks of Write.
func writeFile(t *testing.T, path string, bs []Binding) {
	t.Helper()
	data, err := json.Marshal(file{Bindings: bs})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// A bad test binding drops only itself; the other sessions keep their bindings.
func TestReadDropsBadTestBinding(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bindings.json")
	main := mkdir(t, filepath.Join(dir, "main"))
	dangling := symlink(t, filepath.Join(dir, "gone"), filepath.Join(dir, "dangling"))
	def := binding("default", filepath.Join(dir, "default.sock"), main, KindMain)
	work := binding("work", filepath.Join(dir, "work.sock"), main, KindMain)
	eval := binding("eval", filepath.Join(dir, "eval.sock"), mkdir(t, filepath.Join(dir, "eval-store")), KindTest)
	t1Store := mkdir(t, filepath.Join(dir, "t1-store"))
	shared := mkdir(t, filepath.Join(dir, "shared"))
	test := func(session, socket, store string) Binding {
		return binding(session, filepath.Join(dir, socket), store, KindTest)
	}
	tests := []struct {
		name        string
		bs          []Binding
		wantKept    []Binding
		wantDropped []string // sessions
		wantErr     string
	}{
		{
			name:        "dangling link as test store",
			bs:          []Binding{def, test("t1", "t1.sock", filepath.Join(dangling, "store")), eval},
			wantKept:    []Binding{def, eval},
			wantDropped: []string{"t1"},
			wantErr:     "link to a missing target",
		},
		{
			name:        "dangling link as test socket",
			bs:          []Binding{def, binding("t1", dangling, t1Store, KindTest)},
			wantKept:    []Binding{def},
			wantDropped: []string{"t1"},
			wantErr:     "link to a missing target",
		},
		{
			name:        "relative test store",
			bs:          []Binding{def, test("t1", "t1.sock", "store")},
			wantKept:    []Binding{def},
			wantDropped: []string{"t1"},
			wantErr:     "not absolute",
		},
		{
			name:        "test store holds the bindings folder",
			bs:          []Binding{def, test("t1", "t1.sock", dir)},
			wantKept:    []Binding{def},
			wantDropped: []string{"t1"},
			wantErr:     "holds the bindings folder",
		},
		{
			name:        "test store is the main store",
			bs:          []Binding{def, test("t1", "t1.sock", main), work},
			wantKept:    []Binding{def, work},
			wantDropped: []string{"t1"},
			wantErr:     "is the main store",
		},
		{
			name:        "test binding before the main binding it conflicts with",
			bs:          []Binding{test("default", "t1.sock", t1Store), def},
			wantKept:    []Binding{def},
			wantDropped: []string{"default"},
			wantErr:     "already bound",
		},
		{
			name:        "two test bindings share a store",
			bs:          []Binding{def, test("t1", "t1.sock", shared), test("t2", "t2.sock", shared), eval},
			wantKept:    []Binding{def, eval},
			wantDropped: []string{"t1", "t2"},
			wantErr:     "also the test store",
		},
		// The third one must not survive.
		{
			name:        "three test bindings share a store",
			bs:          []Binding{def, test("t1", "t1.sock", shared), test("t2", "t2.sock", shared), test("t3", "t3.sock", shared)},
			wantKept:    []Binding{def},
			wantDropped: []string{"t1", "t2", "t3"},
			wantErr:     "also the test store",
		},
		{
			name:        "three test bindings share a store, main binding last",
			bs:          []Binding{test("t3", "t3.sock", shared), test("t1", "t1.sock", shared), test("t2", "t2.sock", shared), def},
			wantKept:    []Binding{def},
			wantDropped: []string{"t3", "t1", "t2"},
			wantErr:     "also the test store",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			writeFile(t, path, tt.bs)
			kept, dropped, err := Read(path)
			if err != nil {
				t.Fatalf("Read: %v", err)
			}
			if !reflect.DeepEqual(kept, tt.wantKept) {
				t.Errorf("kept = %+v, want %+v", kept, tt.wantKept)
			}
			var sessions []string
			for _, d := range dropped {
				sessions = append(sessions, d.Binding.Session)
				if d.Err == nil || !strings.Contains(d.Err.Error(), tt.wantErr) {
					t.Errorf("dropped %q: err = %v, want one with %q", d.Binding.Session, d.Err, tt.wantErr)
				}
			}
			if !reflect.DeepEqual(sessions, tt.wantDropped) {
				t.Errorf("dropped sessions = %v, want %v", sessions, tt.wantDropped)
			}
		})
	}
}

// A bad main binding, or main bindings that disagree, stop every session.
func TestReadFailsOnBadMainBinding(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bindings.json")
	main := mkdir(t, filepath.Join(dir, "main"))
	dangling := symlink(t, filepath.Join(dir, "gone"), filepath.Join(dir, "dangling"))
	def := binding("default", filepath.Join(dir, "default.sock"), main, KindMain)
	eval := binding("eval", filepath.Join(dir, "eval.sock"), mkdir(t, filepath.Join(dir, "eval-store")), KindTest)
	tests := []struct {
		name    string
		bs      []Binding
		wantErr string
	}{
		{"relative main store", []Binding{eval, binding("work", filepath.Join(dir, "w.sock"), "main", KindMain)}, "not absolute"},
		{"dangling link as main socket", []Binding{eval, binding("work", dangling, main, KindMain)}, "link to a missing target"},
		{"main store holds the bindings folder", []Binding{eval, binding("work", filepath.Join(dir, "w.sock"), dir, KindMain)}, "holds the bindings folder"},
		{"main bindings disagree", []Binding{def, eval, binding("work", filepath.Join(dir, "w.sock"), mkdir(t, filepath.Join(dir, "other")), KindMain)}, "is not the main store"},
		{"unknown kind", []Binding{def, binding("x", filepath.Join(dir, "x.sock"), filepath.Join(dir, "x"), "prod")}, `kind "prod"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			writeFile(t, path, tt.bs)
			kept, dropped, err := Read(path)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("Read = %+v, %+v, %v, want an error with %q", kept, dropped, err, tt.wantErr)
			}
		})
	}
}

// Write stays strict: setup runs with the user present, so it refuses a bad test binding.
func TestWriteRefusesBadTestBinding(t *testing.T) {
	dir := t.TempDir()
	main := mkdir(t, filepath.Join(dir, "main"))
	bs := []Binding{
		binding("default", filepath.Join(dir, "default.sock"), main, KindMain),
		binding("eval", filepath.Join(dir, "eval.sock"), main, KindTest),
	}
	if err := Write(filepath.Join(dir, "bindings.json"), bs); err == nil || !strings.Contains(err.Error(), "is the main store") {
		t.Errorf("Write = %v, want an error with %q", err, "is the main store")
	}
}

// A test entry that does not decode drops only itself.
func TestReadDropsMalformedTestEntry(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bindings.json")
	def := binding("default", filepath.Join(dir, "default.sock"), mkdir(t, filepath.Join(dir, "main")), KindMain)
	main, err := json.Marshal(def)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name, entry, wantErr string
	}{
		{"bad time", `{"session":"t1","socket":"/t1.sock","store":"/t1","kind":"test","created":"not-a-time"}`, "not-a-time"},
		{"wrong type", `{"session":"t1","socket":7,"store":"/t1","kind":"test","created":"2026-09-28T10:00:00Z"}`, "cannot unmarshal"},
		{"unknown field", `{"session":"t1","socket":"/t1.sock","store":"/t1","kind":"test","color":"red"}`, `unknown field "color"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			content := `{"bindings":[` + tt.entry + `,` + string(main) + `]}`
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			kept, dropped, err := Read(path)
			if err != nil {
				t.Fatalf("Read: %v", err)
			}
			if !reflect.DeepEqual(kept, []Binding{def}) {
				t.Errorf("kept = %+v, want %+v", kept, []Binding{def})
			}
			if len(dropped) != 1 || dropped[0].Binding.Session != "t1" || dropped[0].Binding.Kind != KindTest ||
				!strings.Contains(dropped[0].Err.Error(), tt.wantErr) {
				t.Errorf("dropped = %+v, want t1 with an error with %q", dropped, tt.wantErr)
			}
		})
	}
}
