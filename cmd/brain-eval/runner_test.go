package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/aryanVijaywargia/herdr-brain/internal/adapter"
	"github.com/aryanVijaywargia/herdr-brain/internal/bindings"
	"github.com/aryanVijaywargia/herdr-brain/internal/herdr"
	"github.com/aryanVijaywargia/herdr-brain/internal/install"
	"github.com/aryanVijaywargia/herdr-brain/internal/paths"
)

const mainSocket = "/tmp/brain-main-fake/herdr.sock"

// fakePane is a pane of the fake herdr.
type fakePane struct {
	pane    herdr.Pane
	cwd     string
	lines   []string // the screen
	shell   int
	fg      []herdr.Process
	closed  bool
	ran     []string // the commands typed into the pane
	prompts []string // the prompts sent to its agent
	args    []string // the CLI arguments of its agent
}

// fakeHerdr is the brain-eval session in memory. RunInPane runs the command with sh in the
// pane's folder, so setup steps write real files; ReadAgent shows the prompt, the test store
// and the scratch folder, so a test sees what a probe could see.
type fakeHerdr struct {
	store   string
	panes   map[string]*fakePane
	order   []string
	agents  map[string]string // agent name to pane id
	alive   map[int]bool
	nextPID int
	nextWS  int
	labels  map[string]string // workspace id to label
	stuck   bool              // a closed pane's processes keep running
	// answer, when set, is the reply of an agent to prompt: the screen then shows the prompt
	// and the reply only.
	answer   func(p *fakePane, prompt string) string
	onStart  func(pane, kind string) // runs when an agent starts: its hook
	onRun    func()                  // runs when a pane runs a command: a Brain call of a step
	notReady map[string]bool         // agent kinds that do not get ready
	notIdle  bool                    // a read finds the agent still working
	scrolled bool                    // the ask scrolled off the screen of a read
	badAsk   string                  // the agent kind whose prompt fails
	visible  int                     // the count of visible reads
	// session is herdr's session.json, with the root of each saved workspace. herdr saves a
	// new workspace late: after saveAfter calls of Workspaces, or never with noSave.
	session   string
	roots     map[string]string // workspace id to its root folder
	saveIn    map[string]int    // workspace id to the Workspaces calls before its save
	saveAfter int
	noSave    bool
}

func newFake(store, session string) *fakeHerdr {
	return &fakeHerdr{store: store, panes: map[string]*fakePane{}, agents: map[string]string{}, labels: map[string]string{},
		alive: map[int]bool{}, nextPID: 1000, session: session, roots: map[string]string{}, saveIn: map[string]int{}}
}

// save writes session.json with each workspace whose save is due.
func (f *fakeHerdr) save() error {
	type saved struct {
		ID          string `json:"id"`
		IdentityCwd string `json:"identity_cwd"`
	}
	ws := []saved{}
	for _, id := range slices.Sorted(maps.Keys(f.roots)) {
		if !f.noSave && f.saveIn[id] == 0 {
			ws = append(ws, saved{id, f.roots[id]})
		}
	}
	data, err := json.Marshal(map[string]any{"version": 3, "workspaces": ws})
	if err != nil {
		return err
	}
	return os.WriteFile(f.session, data, 0o600)
}

func (f *fakeHerdr) pid() int {
	f.nextPID++
	f.alive[f.nextPID] = true
	return f.nextPID
}

func (f *fakeHerdr) add(id, cwd string) *fakePane {
	ws, _, _ := strings.Cut(id, ":")
	p := &fakePane{pane: herdr.Pane{ID: id, WorkspaceID: ws, TabID: ws + ":t1"}, cwd: cwd, shell: f.pid()}
	f.panes[id] = p
	f.order = append(f.order, id)
	return p
}

func (f *fakeHerdr) get(id string) (*fakePane, error) {
	p, ok := f.panes[id]
	if !ok || p.closed {
		return nil, fmt.Errorf("pane %s: %w", id, herdr.ErrPaneNotFound)
	}
	return p, nil
}

func (f *fakeHerdr) Panes(context.Context) ([]herdr.Pane, error) {
	var ps []herdr.Pane
	for _, id := range f.order {
		if p := f.panes[id]; !p.closed {
			ps = append(ps, p.pane)
		}
	}
	return ps, nil
}

// Workspaces lists the workspaces with an open pane: herdr closes a workspace with its last
// pane. Each call brings the late save of a new workspace one call nearer.
func (f *fakeHerdr) Workspaces(context.Context) ([]herdr.Workspace, error) {
	for id, n := range f.saveIn {
		f.saveIn[id] = max(n-1, 0)
	}
	if err := f.save(); err != nil {
		return nil, err
	}
	var ws []herdr.Workspace
	for _, id := range f.order {
		p := f.panes[id]
		if !p.closed && !slices.ContainsFunc(ws, func(w herdr.Workspace) bool { return w.ID == p.pane.WorkspaceID }) {
			ws = append(ws, herdr.Workspace{ID: p.pane.WorkspaceID, Label: f.labels[p.pane.WorkspaceID]})
		}
	}
	return ws, nil
}

func (f *fakeHerdr) PaneProcessInfo(_ context.Context, id string) (herdr.ProcessInfo, error) {
	p, err := f.get(id)
	if err != nil {
		return herdr.ProcessInfo{}, err
	}
	return herdr.ProcessInfo{ShellPID: p.shell, Processes: slices.Clone(p.fg)}, nil
}

func (f *fakeHerdr) CreateWorkspace(_ context.Context, cwd, label string) (herdr.Pane, error) {
	f.nextWS++
	id := fmt.Sprintf("w%d", f.nextWS)
	f.labels[id], f.roots[id], f.saveIn[id] = label, cwd, f.saveAfter
	if err := f.save(); err != nil {
		return herdr.Pane{}, err
	}
	return f.add(id+":p1", cwd).pane, nil
}

func (f *fakeHerdr) SplitPane(_ context.Context, id, cwd string) (herdr.Pane, error) {
	p, err := f.get(id)
	if err != nil {
		return herdr.Pane{}, err
	}
	return f.add(fmt.Sprintf("%s:p%d", p.pane.WorkspaceID, len(f.order)+1), cwd).pane, nil
}

func (f *fakeHerdr) ClosePane(_ context.Context, id string) error {
	p, err := f.get(id)
	if err != nil {
		return err
	}
	p.closed = true
	if !f.stuck {
		f.alive[p.shell] = false
		for _, fg := range p.fg {
			f.alive[fg.PID] = false
		}
	}
	return nil
}

func (f *fakeHerdr) RunInPane(_ context.Context, id, command string) error {
	p, err := f.get(id)
	if err != nil {
		return err
	}
	p.ran = append(p.ran, command)
	if f.onRun != nil {
		f.onRun()
	}
	cmd := exec.Command("sh", "-c", command)
	cmd.Dir = p.cwd
	out, err := cmd.CombinedOutput()
	if err != nil {
		return err
	}
	p.lines = append(p.lines, strings.Split(strings.TrimRight(string(out), "\n"), "\n")...)
	return nil
}

func (f *fakeHerdr) WaitOutput(_ context.Context, id, re string, _ time.Duration) (string, error) {
	p, err := f.get(id)
	if err != nil {
		return "", err
	}
	rx := regexp.MustCompile(re)
	for _, l := range p.lines {
		if rx.MatchString(l) {
			return l, nil
		}
	}
	return "", errors.New("wait-output timed out")
}

func (f *fakeHerdr) StartAgent(_ context.Context, name, kind, id string, args []string) error {
	p, err := f.get(id)
	if err != nil {
		return err
	}
	if f.notReady[kind] {
		return errors.New("agent start: the agent did not get ready (agent_not_ready)")
	}
	if f.onStart != nil {
		f.onStart(id, kind)
	}
	p.pane.Agent = &kind
	p.args = args
	p.fg = append(p.fg, herdr.Process{Name: kind, PID: f.pid()})
	f.agents[name] = id
	return nil
}

func (f *fakeHerdr) PromptAgent(_ context.Context, target, text string, _ time.Duration) error {
	p, err := f.get(f.agents[target])
	if err != nil {
		return err
	}
	if p.pane.Agent != nil && *p.pane.Agent == f.badAsk {
		return errors.New("agent prompt: the pane is gone (pane_not_found)")
	}
	p.prompts = append(p.prompts, text)
	return nil
}

func (f *fakeHerdr) ReadAgent(_ context.Context, target string, _ int) (string, error) {
	p, err := f.get(f.agents[target])
	if err != nil {
		return "", err
	}
	if f.notIdle {
		return "", errors.New("agent read: the agent is working (agent_not_idle)")
	}
	if f.answer != nil {
		last := p.prompts[len(p.prompts)-1]
		if f.scrolled {
			return f.answer(p, last) + "\n", nil
		}
		return fmt.Sprintf("> %s\n%s\n", last, f.answer(p, last)), nil
	}
	return fmt.Sprintf("prompt: %s\nstore: %s\nscratch: %s\n", strings.Join(p.prompts, " | "), dump(f.store), dump(p.cwd)), nil
}

func (f *fakeHerdr) ReadAgentVisible(ctx context.Context, target string) (string, error) {
	f.visible++
	return f.ReadAgent(ctx, target, 0)
}

// dump lists the files under dir with their text, except .git.
func dump(dir string) string {
	var b strings.Builder
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && d.Name() == ".git" {
			return filepath.SkipDir
		}
		rel, _ := filepath.Rel(dir, path)
		data, _ := os.ReadFile(path)
		fmt.Fprintf(&b, "[%s %s]", rel, strings.TrimSpace(string(data)))
		return nil
	})
	return b.String()
}

// fakeServer is a server that the runner started.
type fakeServer struct{ stopped int }

func (s *fakeServer) Stop(context.Context) error { s.stopped++; return nil }

// env is the scratch world of one test: a HOME with a real-looking setup, a test store
// outside it, and the fake herdr.
type env struct {
	home, data, mainStore, testStore string
	socket                           string // the socket of the brain-eval session
	fake                             *fakeHerdr
	targets                          []herdr.Target
	running                          bool // brain-eval runs before the run
	server                           *fakeServer
	sessionCalls                     int
	r                                *Runner
}

func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{home: t.TempDir(), testStore: filepath.Join(t.TempDir(), "store"), socket: filepath.Join(t.TempDir(), "herdr.sock"),
		running: true, server: &fakeServer{}}
	e.data = filepath.Join(e.home, ".local", "share", "brain")
	e.mainStore = filepath.Join(e.data, "main")
	write(t, filepath.Join(e.mainStore, "brain.db"), "real rows")
	write(t, filepath.Join(e.home, ".claude", "settings.json"), `{"theme":"dark"}`)
	write(t, filepath.Join(e.home, ".codex", "config.toml"), `model = "x"`)
	e.install(t, "claude", "codex")
	write(t, filepath.Join(e.testStore, "brain.db"), "earlier test rows")
	e.bind(t, bindings.KindTest)
	e.fake = newFake(e.testStore, filepath.Join(filepath.Dir(e.socket), "session.json"))
	e.r = &Runner{
		Bindings:  filepath.Join(e.data, "bindings.json"),
		Install:   filepath.Join(e.data, "install.json"),
		MainStore: e.mainStore,
		Lock:      filepath.Join(e.data, "eval.lock"),
		// Conformance is only written for a delivery stage.
		Conformance: filepath.Join(e.data, "conformance.json"),
		Scratch:     t.TempDir(),
		Environ:     []string{"PATH=" + os.Getenv("PATH"), "HOME=" + e.home},
		Herdr: func(tg herdr.Target) (Herdr, error) {
			e.targets = append(e.targets, tg)
			return e.fake, nil
		},
		Sessions: func(context.Context) ([]herdr.Session, error) {
			e.sessionCalls++
			return []herdr.Session{
				{Name: "default", Default: true, Running: true, Socket: mainSocket},
				{Name: session, Running: e.running, Socket: e.socket},
			}, nil
		},
		StartServer: func(context.Context, string) (Server, error) {
			e.running = true
			return e.server, nil
		},
		Alive: func(pid int) bool { return e.fake.alive[pid] },
		Timeouts: Timeouts{Call: 5 * time.Second, Start: 5 * time.Second, Prompt: 5 * time.Second, Step: 5 * time.Second,
			Map: 5 * time.Second, Exit: 50 * time.Millisecond, Poll: time.Millisecond},
		Now:     func() time.Time { return time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC) },
		Version: func(context.Context, string) (string, error) { return "2.1.283", nil },
		Stdout:  io.Discard,
	}
	return e
}

// install adds the Brain hook of each harness to its hook file in the scratch HOME, as
// `brain setup` does.
func (e *env) install(t *testing.T, harnesses ...string) {
	t.Helper()
	in := &install.Installer{Home: e.home, Record: filepath.Join(e.data, "install.json"), Binary: "/opt/brain/bin/brain"}
	for _, h := range harnesses {
		p, err := adapter.Load(h)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := in.Install(p); err != nil {
			t.Fatal(err)
		}
	}
}

// bind writes the bindings file: the default session on the main store, and brain-eval on
// the test store with kind k.
func (e *env) bind(t *testing.T, k bindings.Kind) {
	t.Helper()
	now := time.Date(2026, 9, 29, 0, 0, 0, 0, time.UTC)
	store := e.testStore
	if k == bindings.KindMain {
		store = e.mainStore
	}
	bs := []bindings.Binding{{Session: session, Socket: e.socket, Store: store, Kind: k, Created: now}}
	if k == bindings.KindTest {
		bs = append(bs, bindings.Binding{Session: "default", Socket: mainSocket, Store: e.mainStore, Kind: bindings.KindMain, Created: now})
	}
	if err := bindings.Write(filepath.Join(e.data, "bindings.json"), bs); err != nil {
		t.Fatal(err)
	}
}

func write(t *testing.T, path, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}

// tree returns each file and folder under root with its text.
func tree(t *testing.T, root string) map[string]string {
	t.Helper()
	m := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			m[path] = "dir"
			return nil
		}
		data, err := os.ReadFile(path)
		m[path] = string(data)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// diff lists the paths whose entry differs between a and b.
func diff(a, b map[string]string) []string {
	var d []string
	for k, v := range a {
		if w, ok := b[k]; !ok || w != v {
			d = append(d, k)
		}
	}
	for k := range b {
		if _, ok := a[k]; !ok {
			d = append(d, k)
		}
	}
	sort.Strings(d)
	return d
}

// S74: after a run, the real store has no new row and no real pane got a test marker. The
// run leaves the CLI config files, install.json and the bindings file unchanged: only the
// eval lock is new in HOME.
func TestS74RealStoreAndPanesUntouched(t *testing.T) {
	e := newEnv(t)
	other := e.fake.add("w9:p1", t.TempDir()) // a plain shell pane that the run did not open
	before := tree(t, e.home)
	out, err := e.r.Run(context.Background(), []Case{{
		ID:     "s74",
		Setup:  []string{"echo {{marker}} > note.txt"},
		Probes: []Probe{{Harness: "claude", Prompt: "remember {{marker}}"}, {Harness: "codex", Prompt: "recall {{marker}}"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	after := tree(t, e.home)
	if d := diff(before, after); !slices.Equal(d, []string{filepath.Join(e.data, "eval.lock")}) {
		t.Errorf("HOME changed at %v, want only the eval lock", d)
	}
	for _, tg := range e.targets {
		if tg != (herdr.Target{Session: session, Socket: e.socket}) {
			t.Errorf("herdr target %+v, want only the brain-eval session", tg)
		}
	}
	if len(other.ran) != 0 || len(other.prompts) != 0 || other.closed {
		t.Errorf("the other pane got %v and %v (closed %v), want nothing", other.ran, other.prompts, other.closed)
	}
	if len(out) != 1 || len(out[0].Replies) != 2 || !strings.Contains(out[0].Replies[0].Screen, "remember "+out[0].Marker) {
		t.Fatalf("outcome %+v, want two replies to the marked prompt", out)
	}
	if !strings.Contains(out[0].Replies[0].Text, "[note.txt "+out[0].Marker+"]") {
		t.Errorf("reply %q, want the setup step's file in the scratch folder", out[0].Replies[0].Text)
	}
	for id, p := range e.fake.panes {
		if id != other.pane.ID && !p.closed {
			t.Errorf("pane %s that the run opened is still open", id)
		}
	}
	if e.server.stopped != 0 {
		t.Errorf("the run stopped a session that it did not start")
	}
}

// S78: two cases in a row; nothing from the first reaches the second: not its store rows,
// not its scratch files, not its marker.
func TestS78NothingCrossesCases(t *testing.T) {
	e := newEnv(t)
	seed := fmt.Sprintf("echo {{marker}} > seed.txt; echo {{marker}} > %s/rows.txt; mkdir -p %s/out/gen-1", e.testStore, e.testStore)
	cases := []Case{
		{ID: "one", Setup: []string{seed}, Probes: []Probe{{Harness: "claude", Prompt: "one {{marker}}"}}},
		{ID: "two", Probes: []Probe{{Harness: "claude", Prompt: "two {{marker}}"}}},
	}
	out, err := e.r.Run(context.Background(), cases)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 {
		t.Fatalf("got %d outcomes, want 2", len(out))
	}
	first, second := out[0], out[1]
	if first.Marker == second.Marker {
		t.Errorf("both cases have marker %s, want a fresh one", first.Marker)
	}
	if !strings.Contains(first.Replies[0].Text, "[rows.txt "+first.Marker+"]") {
		t.Errorf("case one reply %q, want its own store row", first.Replies[0].Text)
	}
	two := second.Replies[0].Text
	if strings.Contains(two, first.Marker) || strings.Contains(two, "seed.txt") || strings.Contains(two, "gen-1") ||
		strings.Contains(two, "earlier test rows") {
		t.Errorf("case two reply %q holds data of case one or of an earlier run", two)
	}
	if left, _ := os.ReadDir(e.r.Scratch); len(left) != 0 {
		t.Errorf("scratch folders %v remain, want none", left)
	}
}

// A missing install or test binding stops the run before any write: HOME and the test store
// stay as they were, and the runner never reaches herdr. The error names the setup command.
func TestSetupMissing(t *testing.T) {
	tests := []struct {
		name   string
		change func(t *testing.T, e *env)
		cases  []Case
		want   string
	}{
		{"no install record", func(t *testing.T, e *env) { rm(t, filepath.Join(e.data, "install.json")) }, nil, "run `brain setup` first"},
		{"empty install record", func(t *testing.T, e *env) { write(t, filepath.Join(e.data, "install.json"), `{"entries":{}}`) }, nil, "run `brain setup` first"},
		{"no bindings file", func(t *testing.T, e *env) { rm(t, filepath.Join(e.data, "bindings.json")) }, nil, "brain setup --session brain-eval --test <folder>"},
		{"main binding", func(t *testing.T, e *env) { e.bind(t, bindings.KindMain) }, nil, "not a test store"},
		{"store folder gone", func(t *testing.T, e *env) { rm(t, e.testStore) }, nil, "is refused"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			tt.change(t, e)
			home, store := tree(t, e.home), tree(t, filepath.Dir(e.testStore))
			_, err := e.r.Run(context.Background(), tt.cases)
			if !errors.Is(err, errSetup) || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("Run() = %v, want errSetup with %q", err, tt.want)
			}
			if d := diff(home, tree(t, e.home)); len(d) != 0 {
				t.Errorf("HOME changed at %v, want no write", d)
			}
			if d := diff(store, tree(t, filepath.Dir(e.testStore))); len(d) != 0 {
				t.Errorf("test store changed at %v, want no write", d)
			}
			if e.sessionCalls != 0 || len(e.targets) != 0 {
				t.Errorf("the run called herdr %d times, want none", e.sessionCalls+len(e.targets))
			}
		})
	}
}

func rm(t *testing.T, path string) {
	t.Helper()
	if err := os.RemoveAll(path); err != nil {
		t.Fatal(err)
	}
}

// A second runner while one runs stops at the lock, before it reaches herdr or resets the
// first run's data.
func TestSecondRunnerStops(t *testing.T) {
	e := newEnv(t)
	unlock, err := lockFile(e.r.Lock) // the first run
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	store := tree(t, e.testStore)
	_, err = e.r.Run(context.Background(), []Case{{ID: "c", Setup: []string{"true"}}})
	if err == nil || !strings.Contains(err.Error(), "another eval run holds") {
		t.Errorf("Run() = %v, want the lock error", err)
	}
	if d := diff(store, tree(t, e.testStore)); len(d) != 0 || e.sessionCalls != 0 {
		t.Errorf("test store changed at %v, herdr calls %d; want none", d, e.sessionCalls)
	}
}

// After an interrupted run, a writer of that run can still be alive in the test session:
// an agent pane, a pane whose foreground process is a CLI or brain, or a case workspace
// whose shell may run a writer in the background. The next run does not
// reset the store, and leaves that pane alone.
func TestLiveWriterBlocksReset(t *testing.T) {
	claude := "claude"
	tests := []struct {
		name string
		set  func(p *fakePane)
	}{
		{"agent pane", func(p *fakePane) { p.pane.Agent = &claude }},
		{"brain process", func(p *fakePane) { p.fg = []herdr.Process{{Name: "brain", PID: 7}} }},
		{"codex process", func(p *fakePane) { p.fg = []herdr.Process{{Name: "codex", PID: 8}} }},
		{"case workspace", func(*fakePane) {}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			left := e.fake.add("w9:p1", t.TempDir())
			if tt.name == "case workspace" {
				e.fake.labels["w9"] = caseLabel + "old"
			}
			tt.set(left)
			store := tree(t, e.testStore)
			_, err := e.r.Run(context.Background(), []Case{{ID: "c", Setup: []string{"true"}}})
			if err == nil || !strings.Contains(err.Error(), "no reset") {
				t.Errorf("Run() = %v, want a refused reset", err)
			}
			after := tree(t, e.testStore)
			delete(after, paths.EvalResults(e.testStore)) // the results of the run, which did not run the case
			if d := diff(store, after); len(d) != 0 {
				t.Errorf("test store changed at %v, want no reset", d)
			}
			if left.closed || len(e.fake.panes) != 1 {
				t.Errorf("the run closed the pane or opened one: %d panes", len(e.fake.panes))
			}
		})
	}
}

// When the runner cannot confirm that the last case's processes exited, it stops the run
// without the next reset.
func TestUnconfirmedExitStopsRun(t *testing.T) {
	e := newEnv(t)
	e.fake.stuck = true
	cases := []Case{
		{ID: "one", Setup: []string{fmt.Sprintf("echo one > %s/rows.txt", e.testStore)}, Probes: []Probe{{Harness: "claude", Prompt: "p"}}},
		{ID: "two", Setup: []string{"echo two"}},
	}
	_, err := e.r.Run(context.Background(), cases)
	if !errors.Is(err, errExitUnconfirmed) {
		t.Fatalf("Run() = %v, want errExitUnconfirmed", err)
	}
	data, rerr := os.ReadFile(filepath.Join(e.testStore, "rows.txt"))
	if rerr != nil || string(data) != "one\n" {
		t.Errorf("test store rows.txt = %q, %v; want case one's data, not a reset", data, rerr)
	}
	if e.fake.nextWS != 1 {
		t.Errorf("the run opened %d workspaces, want only case one's", e.fake.nextWS)
	}
}

// The runner starts the brain-eval session when it does not run, and then stops and deletes
// it at the end, also after a failed case. It never stops a session that it reused.
func TestSessionStartAndStop(t *testing.T) {
	tests := []struct {
		name    string
		running bool
		setup   string
		stopped int
	}{
		{"reused", true, "true", 0},
		{"started", false, "true", 1},
		{"started, case fails", false, "false", 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			e.running = tt.running
			_, err := e.r.Run(context.Background(), []Case{{ID: "c", Setup: []string{tt.setup}}})
			if (err != nil) != (tt.setup == "false") {
				t.Errorf("Run() = %v", err)
			}
			if e.server.stopped != tt.stopped {
				t.Errorf("session stopped %d times, want %d", e.server.stopped, tt.stopped)
			}
		})
	}
}

// A session whose socket is not the one of the test binding is not the bound session.
func TestSessionSocketMismatch(t *testing.T) {
	e := newEnv(t)
	e.running = false
	sessions := e.r.Sessions
	e.r.Sessions = func(ctx context.Context) ([]herdr.Session, error) {
		ss, err := sessions(ctx)
		for i := range ss {
			if ss[i].Name == session {
				ss[i].Socket = "/tmp/other/herdr.sock"
			}
		}
		return ss, err
	}
	store := tree(t, e.testStore)
	_, err := e.r.Run(context.Background(), []Case{{ID: "c"}})
	if err == nil || !strings.Contains(err.Error(), "but its binding has") {
		t.Errorf("Run() = %v, want the socket error", err)
	}
	if e.server.stopped != 1 || len(e.targets) != 0 {
		t.Errorf("stopped %d, targets %v; want the started session stopped and no herdr client", e.server.stopped, e.targets)
	}
	if d := diff(store, tree(t, e.testStore)); len(d) != 0 {
		t.Errorf("test store changed at %v, want no reset", d)
	}
}

func TestMarker(t *testing.T) {
	re := regexp.MustCompile(`^[a-z]{4}-[0-9]{4}$`)
	seen := map[string]bool{}
	for range 20 {
		m, err := newMarker()
		if err != nil || !re.MatchString(m) {
			t.Fatalf("newMarker() = %q, %v", m, err)
		}
		seen[m] = true
	}
	if len(seen) < 19 { // [NEW] 26^4 * 10^4 shapes: a repeat in 20 is next to impossible
		t.Errorf("newMarker() gave %d different markers in 20", len(seen))
	}
}
