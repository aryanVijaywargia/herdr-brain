package brained

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aryanVijaywargia/herdr-brain/internal/bindings"
	"github.com/aryanVijaywargia/herdr-brain/internal/herdr"
	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

// wait is the longest a test waits for brained. Tests sync on channels; wait only ends a
// test that hangs.
const wait = 10 * time.Second

// fakeHerdr is a herdr API socket with the replies that brained needs.
type fakeHerdr struct {
	t        *testing.T
	path     string
	ln       net.Listener
	subs     chan *fakeSub
	mu       sync.Mutex
	panes    map[string]*fakePane
	moved    map[string]string // an old pane id → its new id: pane.get answers the old id
	reads    map[string][]string
	onRead   func(f *fakeHerdr) // runs after each pane.read, with mu held
	calls    []string           // the methods and read sources, in order
	plugins  string             // the JSON array of plugin.list
	failList bool               // pane.list gets an error
	gets     chan string        // the pane of each agent.get, when not nil
	conns    []net.Conn
	wg       sync.WaitGroup
}

// fakePane is one live pane.
type fakePane struct {
	ws      string
	agent   string // "" for no agent
	session string // the agent session id; "" for none
	// The agent.get state: the status ("" is working), the folder, the terminal and the seq.
	status, cwd, terminal string
	seq                   uint64
	// procs is the pane.process_info reply of the foreground processes; "" gives the agent
	// as its own process, or a shell when the pane has no agent.
	procs string
}

// agentJSON is the agent.get state of pane id.
func (p *fakePane) agentJSON(id string) string {
	status, terminal := p.status, p.terminal
	if status == "" {
		status = "working"
	}
	if terminal == "" {
		terminal = "term_1"
	}
	as := ""
	if p.session != "" {
		as = fmt.Sprintf(`,"agent_session":{"agent":%q,"kind":"id","source":"herdr:%s","value":%q}`, p.agent, p.agent, p.session)
	}
	return fmt.Sprintf(`{"pane_id":%q,"workspace_id":%q,"tab_id":"%s:t1","agent":%q,"agent_status":%q,"cwd":%q,"terminal_id":%q,"state_change_seq":%d,"focused":false,"revision":1%s}`,
		id, p.ws, p.ws, p.agent, status, p.cwd, terminal, p.seq, as)
}

// processes is the JSON list of the foreground processes of the pane.
func (p *fakePane) processes() string {
	switch {
	case p.procs != "":
		return p.procs
	case p.agent != "":
		return fmt.Sprintf(`[{"name":%q,"argv":[%q],"pid":2}]`, p.agent, p.agent)
	}
	return `[{"name":"zsh","argv":["-zsh"],"pid":1}]`
}

func (p *fakePane) json(id string) string {
	a, as := "null", ""
	if p.agent != "" {
		a = `"` + p.agent + `"`
	}
	if p.session != "" {
		as = fmt.Sprintf(`,"agent_session":{"agent":%q,"kind":"id","source":"herdr:%s","value":%q}`, p.agent, p.agent, p.session)
	}
	return fmt.Sprintf(`{"pane_id":%q,"workspace_id":%q,"tab_id":"%s:t1","agent":%s,"agent_status":"working"%s}`, id, p.ws, p.ws, a, as)
}

// fakeSub is one accepted events.subscribe connection. The test writes its events.
type fakeSub struct {
	subs []herdr.Subscription
	conn net.Conn
}

func newFakeHerdr(t *testing.T) *fakeHerdr {
	t.Helper()
	dir, err := os.MkdirTemp("", "bd") // short: a socket path has a limit of about 104 bytes
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	f := &fakeHerdr{t: t, path: filepath.Join(dir, "s"), subs: make(chan *fakeSub, 64), panes: map[string]*fakePane{},
		moved: map[string]string{}, reads: map[string][]string{},
		plugins: `[{"plugin_id":"brain","enabled":true}]`}
	f.ln, err = net.Listen("unix", f.path)
	if err != nil {
		t.Fatal(err)
	}
	f.wg.Add(1)
	go f.serve()
	t.Cleanup(func() {
		_ = f.ln.Close()
		f.mu.Lock()
		for _, c := range f.conns {
			_ = c.Close()
		}
		f.mu.Unlock()
		f.wg.Wait()
	})
	return f
}

// addPane adds a live pane; agent "" is a pane with no agent.
func (f *fakeHerdr) addPane(id, agent string) {
	ws, _, _ := strings.Cut(id, ":")
	f.mu.Lock()
	defer f.mu.Unlock()
	f.panes[id] = &fakePane{ws: ws, agent: agent}
}

func (f *fakeHerdr) setFailList(fail bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failList = fail
}

func (f *fakeHerdr) setPlugins(js string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.plugins = js
}

func (f *fakeHerdr) serve() {
	defer f.wg.Done()
	for {
		conn, err := f.ln.Accept()
		if err != nil {
			return
		}
		f.mu.Lock()
		f.conns = append(f.conns, conn)
		f.mu.Unlock()
		f.wg.Add(1)
		go func() {
			defer f.wg.Done()
			f.answer(conn)
		}()
	}
}

// answer answers the one request of conn. A subscription stays open for the test.
func (f *fakeHerdr) answer(conn net.Conn) {
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		return
	}
	var req struct {
		Method string `json:"method"`
		Params struct {
			Subscriptions []herdr.Subscription `json:"subscriptions"`
			PaneID        string               `json:"pane_id"`
			Target        string               `json:"target"`
			Source        string               `json:"source"`
			Lines         int                  `json:"lines"`
		} `json:"params"`
	}
	if err := json.Unmarshal(line, &req); err != nil {
		f.t.Errorf("request %s: %v", line, err)
		return
	}
	f.mu.Lock()
	pane := req.Params.PaneID
	if req.Method == "agent.get" {
		pane = req.Params.Target
	}
	reply := f.reply(req.Method, pane, req.Params.Source, req.Params.Lines, req.Params.Subscriptions)
	f.mu.Unlock()
	if reply == "" {
		_, _ = io.WriteString(conn, `{"id":"brain","error":{"code":"pane_not_found","message":"no pane"}}`+"\n")
		_ = conn.Close()
		return
	}
	if _, err := io.WriteString(conn, `{"id":"brain","result":`+reply+"}\n"); err != nil {
		return
	}
	if req.Method == "events.subscribe" {
		f.subs <- &fakeSub{subs: req.Params.Subscriptions, conn: conn}
		return
	}
	_ = conn.Close()
}

// reply returns the result JSON of one request, or "" for pane_not_found. f.mu is held.
func (f *fakeHerdr) reply(method, paneID, source string, lines int, subs []herdr.Subscription) string {
	f.calls = append(f.calls, strings.TrimSpace(method+" "+source))
	var reply string
	switch method {
	case "pane.list":
		if f.failList {
			return ""
		}
		var ps []string
		for _, id := range slices.Sorted(maps.Keys(f.panes)) {
			ps = append(ps, f.panes[id].json(id))
		}
		reply = `{"type":"pane_list","panes":[` + strings.Join(ps, ",") + `]}`
	case "pane.get":
		id := paneID
		if n, ok := f.moved[id]; ok {
			id = n
		}
		if p, ok := f.panes[id]; ok {
			reply = `{"type":"pane_info","pane":` + p.json(id) + `}`
		}
	case "pane.read":
		reply = f.read(paneID, source, lines)
	case "pane.process_info":
		if p, ok := f.panes[paneID]; ok {
			reply = fmt.Sprintf(`{"type":"pane_process_info","process_info":{"pane_id":%q,"shell_pid":1,"foreground_processes":%s}}`,
				paneID, p.processes())
		}
	case "agent.get":
		if f.gets != nil {
			f.gets <- paneID
		}
		if p, ok := f.panes[paneID]; ok && p.agent != "" {
			reply = `{"type":"agent_info","agent":` + p.agentJSON(paneID) + `}`
		}
	case "plugin.list":
		reply = `{"type":"plugin_list","plugins":` + f.plugins + `}`
	case "events.subscribe":
		reply = `{"type":"subscription_started"}`
		for _, s := range subs {
			if _, ok := f.panes[s.PaneID]; s.PaneID != "" && !ok {
				reply = ""
			}
		}
	}
	return reply
}

// read answers pane.read of pane id from its next recorded read; the last one repeats. It
// runs onRead after the read, as a change that comes while the read is pending.
func (f *fakeHerdr) read(id, source string, lines int) string {
	p, ok := f.panes[id]
	rs := f.reads[id]
	if !ok || len(rs) == 0 {
		return ""
	}
	text := rs[0]
	if len(rs) > 1 {
		f.reads[id] = rs[1:]
	}
	if lines > 0 {
		ls := strings.Split(strings.TrimRight(text, "\n"), "\n")
		if len(ls) > lines {
			text = strings.Join(ls[len(ls)-lines:], "\n") + "\n"
		}
	}
	js, err := json.Marshal(text)
	if err != nil {
		f.t.Error(err)
	}
	ws := p.ws
	if f.onRead != nil {
		f.onRead(f)
	}
	return fmt.Sprintf(`{"type":"pane_read","read":{"pane_id":%q,"workspace_id":%q,"tab_id":"%s:t1","source":%q,"format":"text","text":%s,"revision":1,"truncated":false}}`,
		id, ws, ws, source, js)
}

// sub returns the next accepted subscription.
func (f *fakeHerdr) sub(t *testing.T) *fakeSub {
	t.Helper()
	select {
	case s := <-f.subs:
		return s
	case <-time.After(wait):
		t.Fatal("no subscription")
		return nil
	}
}

// statusSubs returns the next n subscriptions, which must be pane status subscriptions, by pane.
func (f *fakeHerdr) statusSubs(t *testing.T, n int) map[string]*fakeSub {
	t.Helper()
	m := map[string]*fakeSub{}
	for range n {
		s := f.sub(t)
		if len(s.subs) != 1 || s.subs[0].Type != herdr.PaneStatusChanged {
			t.Fatalf("subscription %+v, want one pane status subscription", s.subs)
		}
		m[s.subs[0].PaneID] = s
	}
	return m
}

// send writes one event line; a write to a stream that brained closed may fail.
func (s *fakeSub) send(line string) {
	_, _ = io.WriteString(s.conn, line+"\n")
}

func statusLine(pane, status string) string {
	ws, _, _ := strings.Cut(pane, ":")
	return fmt.Sprintf(`{"event":"pane.agent_status_changed","data":{"agent":"claude","agent_status":%q,"pane_id":%q,"workspace_id":%q}}`, status, pane, ws)
}

func paneLine(kind, pane string) string {
	ws, _, _ := strings.Cut(pane, ":")
	return fmt.Sprintf(`{"event":%q,"data":{"agent":"claude","pane_id":%q,"workspace_id":%q}}`, kind, pane, ws)
}

// recorder is a Handler that passes each step to the test.
type recorder struct{ steps chan Step }

func (r recorder) Handle(_ context.Context, s Step) error {
	r.steps <- s
	return nil
}

// harness runs one brained against a fake herdr, with a bound store.
type harness struct {
	f     *fakeHerdr
	d     *daemon
	store string
	steps chan Step
	tick  chan time.Time
	done  chan error
}

// newHarness binds session s1 to a new store and returns a brained that is not started.
// kind is the binding kind.
func newHarness(t *testing.T, kind bindings.Kind) *harness {
	t.Helper()
	f := newFakeHerdr(t)
	h := &harness{f: f, store: t.TempDir(), steps: make(chan Step, 64), tick: make(chan time.Time), done: make(chan error, 1)}
	bf := filepath.Join(t.TempDir(), "bindings.json")
	b := bindings.Binding{Session: "s1", Socket: f.path, Store: h.store, Kind: kind, Created: time.Now()}
	if err := bindings.Write(bf, []bindings.Binding{b}); err != nil {
		t.Fatal(err)
	}
	h.d = &daemon{
		env:  env{session: "s1", socket: f.path, stateDir: t.TempDir(), pluginID: "brain"},
		sock: herdr.Socket{Path: f.path}, bindingsFile: bf, handler: recorder{h.steps}, log: io.Discard,
		tick: h.tick, tries: 1, queue: 64,
	}
	return h
}

// start runs brained and waits for its main subscription.
func (h *harness) start(t *testing.T) *fakeSub {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	go func() { h.done <- h.d.run(ctx) }()
	t.Cleanup(func() {
		cancel()
		<-h.done
	})
	main := h.f.sub(t)
	if !slices.Equal(main.subs, mainSubs()) {
		t.Fatalf("main subscription = %+v, want %+v", main.subs, mainSubs())
	}
	return main
}

func (h *harness) step(t *testing.T) Step {
	t.Helper()
	select {
	case s := <-h.steps:
		return s
	case err := <-h.done:
		h.done <- err
		t.Fatalf("brained ended: %v", err)
	case <-time.After(wait):
		t.Fatal("no step")
	}
	return Step{}
}

// check sends one tick.
func (h *harness) check(t *testing.T) {
	t.Helper()
	select {
	case h.tick <- time.Now():
	case <-time.After(wait):
		t.Fatal("brained takes no tick")
	}
}

func (h *harness) end(t *testing.T) error {
	t.Helper()
	select {
	case err := <-h.done:
		h.done <- err
		return err
	case <-time.After(wait):
		t.Fatal("brained did not end")
		return nil
	}
}

// setLearning records a learning switch in the store of h.
func (h *harness) setLearning(t *testing.T, s store.LearningSetting) {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, h.store, busyTimeout)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close() //nolint:errcheck // test
	if err := st.Write(ctx, func(tx *sql.Tx) error { return store.SetLearning(ctx, tx, s) }); err != nil {
		t.Fatal(err)
	}
}

func wantStep(t *testing.T, got Step, kind StepKind, pane string, gap Gap, reason Reason) {
	t.Helper()
	if got.Kind != kind || got.PaneID != pane || got.Gap != gap || got.Reason != reason {
		t.Errorf("step = %s %s gap %q reason %q, want %s %s gap %q reason %q",
			got.Kind, got.PaneID, got.Gap, got.Reason, kind, pane, gap, reason)
	}
}
