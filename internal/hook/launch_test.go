package hook

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aryanVijaywargia/herdr-brain/internal/adapter"
	"github.com/aryanVijaywargia/herdr-brain/internal/bindings"
	"github.com/aryanVijaywargia/herdr-brain/internal/brained"
	"github.com/aryanVijaywargia/herdr-brain/internal/herdr"
	"github.com/aryanVijaywargia/herdr-brain/internal/launch"
	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

// herdrSocket is a herdr API socket that answers pane.get, as launch.Check asks it.
type herdrSocket struct {
	path   string
	mu     sync.Mutex
	agents map[string]string // live pane id → its agent; "" for none
}

func newHerdrSocket(t *testing.T) *herdrSocket {
	t.Helper()
	dir, err := os.MkdirTemp("", "hl") // short: a socket path has a limit of about 104 bytes
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	s := &herdrSocket{path: filepath.Join(dir, "s"), agents: map[string]string{}}
	ln, err := net.Listen("unix", s.path)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			s.answer(conn)
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		wg.Wait()
	})
	return s
}

func (s *herdrSocket) set(pane, agent string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.agents[pane] = agent
}

func (s *herdrSocket) remove(pane string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.agents, pane)
}

// answer answers the one pane.get request of conn.
func (s *herdrSocket) answer(conn net.Conn) {
	defer conn.Close() //nolint:errcheck // test
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		return
	}
	var req struct {
		Params struct {
			PaneID string `json:"pane_id"`
		} `json:"params"`
	}
	if err := json.Unmarshal(line, &req); err != nil {
		return
	}
	s.mu.Lock()
	agent, ok := s.agents[req.Params.PaneID]
	s.mu.Unlock()
	if !ok {
		_, _ = io.WriteString(conn, `{"id":"brain","error":{"code":"pane_not_found","message":"no pane"}}`+"\n")
		return
	}
	a := "null"
	if agent != "" {
		a = `"` + agent + `"`
	}
	ws, _, _ := strings.Cut(req.Params.PaneID, ":")
	_, _ = fmt.Fprintf(conn, `{"id":"brain","result":{"type":"pane_info","pane":{"pane_id":%q,"workspace_id":%q,"tab_id":"%s:t1","agent":%s}}}`+"\n",
		req.Params.PaneID, ws, ws, a)
}

// launchFixture is one store bound to the herdr session default of a herdr socket, with a
// launch record for each launched pane.
type launchFixture struct {
	t        *testing.T
	sock     *herdrSocket
	b        bindings.Binding
	bindings string
	retest   string
	clock    time.Time
}

func newLaunchFixture(t *testing.T) *launchFixture {
	t.Helper()
	sock := newHerdrSocket(t)
	b := bindings.Binding{Session: "default", Socket: sock.path, Store: t.TempDir(), Kind: bindings.KindMain, Created: testNow}
	bf := filepath.Join(t.TempDir(), "bindings.json")
	if err := bindings.Write(bf, []bindings.Binding{b}); err != nil {
		t.Fatal(err)
	}
	return &launchFixture{t: t, sock: sock, b: b, bindings: bf, retest: filepath.Join(t.TempDir(), "retest"), clock: testNow}
}

// launch makes the launch record of cli in pane, live in herdr, and returns its folder.
func (f *launchFixture) launch(pane, cli string) string {
	f.t.Helper()
	f.sock.set(pane, cli)
	ws, _, _ := strings.Cut(pane, ":")
	dir, err := launch.Create(context.Background(), f.b, ws, pane, cli, "", os.Getpid(), testNow)
	if err != nil {
		f.t.Fatal(err)
	}
	return dir
}

func (f *launchFixture) write(fn func(ctx context.Context, tx *sql.Tx) error) {
	f.t.Helper()
	s := openStore(f.t, f.b.Store)
	defer closeStore(f.t, s)
	write(f.t, s, func(tx *sql.Tx) error { return fn(context.Background(), tx) })
}

// capture opens a capture session of harness in pane with key k, and its segment, as brained
// keeps it. It ends the open session of the pane first, as a new agent session does.
func (f *launchFixture) capture(harness, pane string, kind store.KeyKind, id string) {
	f.t.Helper()
	f.write(func(ctx context.Context, tx *sql.Tx) error {
		ss, err := store.Sessions(ctx, tx)
		if err != nil {
			return err
		}
		for _, s := range ss {
			if s.PaneID == pane && s.Ended.IsZero() {
				if err := store.EndSession(ctx, tx, s.Key, testNow); err != nil {
					return err
				}
			}
		}
		key := store.SessionKey{HerdrSession: "default", Harness: harness, Kind: kind, ID: id}
		n, err := store.OpenSession(ctx, tx, key, pane, "", testNow)
		if err != nil {
			return err
		}
		_, err = store.StartSegment(ctx, tx, n, pane, "", "visible", testNow)
		return err
	})
}

// hook is the hook run of event of profile p with environment environ: launched with the
// launch folder dir, or from the environment when dir is "". Its pane function answers the
// panes of the herdr socket, as `herdr pane get` does.
func (f *launchFixture) hook(p *adapter.Profile, event, dir string, environ []string) Hook {
	f.clock = f.clock.Add(time.Second)
	at := f.clock
	return Hook{Profile: p, Event: event, Launch: dir, Environ: environ, Bindings: f.bindings, Retest: f.retest,
		Pane: func(_ context.Context, _ herdr.Target, id string) (herdr.Pane, error) {
			f.sock.mu.Lock()
			defer f.sock.mu.Unlock()
			if _, ok := f.sock.agents[id]; !ok {
				return herdr.Pane{}, herdr.ErrPaneNotFound
			}
			ws, _, _ := strings.Cut(id, ":")
			return herdr.Pane{ID: id, WorkspaceID: ws}, nil
		},
		// A late start: the hook cap and the lookup deadline do not cut a run on a busy test machine.
		Start: time.Now().Add(time.Hour), Now: func() time.Time { return at }}
}

// run runs h with input raw through Run, as `brain hook` does, and checks that it printed
// nothing.
func (f *launchFixture) run(h Hook, raw string) error {
	f.t.Helper()
	var out bytes.Buffer
	err := h.Run(context.Background(), strings.NewReader(raw), &out)
	if out.Len() != 0 {
		f.t.Errorf("hook --event %s printed %q, want nothing", h.Event, out.String())
	}
	return err
}

// rows returns the observations with the pane of their segment, and the command records.
func (f *launchFixture) rows() ([]store.Observation, []string, []store.CommandRecord) {
	f.t.Helper()
	var obs []store.Observation
	var panes []string
	var cmds []store.CommandRecord
	f.write(func(ctx context.Context, tx *sql.Tx) (err error) {
		if obs, err = store.Observations(ctx, tx); err != nil {
			return err
		}
		segs, err := store.Segments(ctx, tx)
		if err != nil {
			return err
		}
		for _, o := range obs {
			i := slices.IndexFunc(segs, func(s store.Segment) bool { return s.ID == o.SegmentID })
			panes = append(panes, segs[i].PaneID)
		}
		cmds, err = store.CommandRecords(ctx, tx)
		return err
	})
	return obs, panes, cmds
}

// launchRecord reads the launch record of the launch folder dir and its sessions.
func (f *launchFixture) launchRecord(dir string) (store.Launch, []store.LaunchSession) {
	f.t.Helper()
	var l store.Launch
	var ss []store.LaunchSession
	f.write(func(ctx context.Context, tx *sql.Tx) error {
		ls, err := store.RunningLaunches(ctx, tx, f.b.Socket)
		if err != nil {
			return err
		}
		i := slices.IndexFunc(ls, func(l store.Launch) bool { return filepath.Base(dir) == fmt.Sprint(l.ID) })
		if i < 0 {
			return errors.New("no running launch of " + dir)
		}
		l = ls[i]
		ss, err = store.LaunchSessions(ctx, tx, l.ID)
		return err
	})
	return l, ss
}

// noHerdrEnv is the environment of a hook with no herdr variables, as a shared Codex app
// server gives it (herdr#4649).
var noHerdrEnv = []string{"HOME=/home/me"}

// claudeRecords are the Claude hook record inputs of TestRecord.
var claudeRecords = []struct{ event, raw string }{
	{adapter.EventTool, `{"session_id":"s1","cwd":"/work","tool_input":{"command":"ls nosuchfile-zq"},
		"error":"Exit code 1\nls: nosuchfile-zq: No such file or directory"}`},
	{adapter.EventTool, `{"session_id":"s1","cwd":"/work","tool_input":{"command":"curl -H 'x-api-key: ` +
		anthropicKey + `' https://api.example.com"}}`},
	{adapter.EventTool, `{"session_id":"s1","tool_input":{"command":"echo ` + strings.Repeat("é", 3000) + `"}}`},
	{adapter.EventPrompt, `{"session_id":"s1","prompt":"Reply with only the word ready."}`},
	{adapter.EventStop, `{"session_id":"s1","last_assistant_message":"ready"}`},
}

// AC E24-S5: a launched Claude session, whose hooks get the launch folder and no herdr
// environment, gives the same capture records as the current global hooks in a herdr pane.
// The launch record shows the session with the capture source hooks.
func TestLaunchRecordSameAsGlobal(t *testing.T) {
	p := claude(t)
	global := newLaunchFixture(t)
	global.sock.set("w14:p1", "claude")
	global.capture("claude", "w14:p1", store.KeyCapture, "cap-1")
	launched := newLaunchFixture(t)
	dir := launched.launch("w14:p1", "claude")
	launched.capture("claude", "w14:p1", store.KeyCapture, "cap-1")
	for _, r := range claudeRecords {
		if err := global.run(global.hook(p, r.event, "", environ(global.b.Socket, "", "w14:p1", "w14")), r.raw); err != nil {
			t.Fatalf("global hook --event %s: %v", r.event, err)
		}
		if err := launched.run(launched.hook(p, r.event, dir, noHerdrEnv), r.raw); err != nil {
			t.Fatalf("launched hook --event %s: %v", r.event, err)
		}
	}
	gObs, _, gCmds := global.rows()
	lObs, lPanes, lCmds := launched.rows()
	if len(gObs) != len(claudeRecords) || len(gCmds) != 3 {
		t.Fatalf("global hooks stored %d observations and %d command records, want %d and 3", len(gObs), len(gCmds), len(claudeRecords))
	}
	same := func(o store.Observation) store.Observation {
		o.ID, o.CaptureIdentity, o.EnrichedBy = 0, "", 0
		return o
	}
	if !slices.Equal(slices.Collect(mapSeq(gObs, same)), slices.Collect(mapSeq(lObs, same))) {
		t.Errorf("launched observations = %+v, want those of the global hooks %+v", lObs, gObs)
	}
	sameCmd := func(c store.CommandRecord) string {
		exit := "none"
		if c.ExitStatus != nil {
			exit = fmt.Sprint(*c.ExitStatus)
		}
		c.ObservationID, c.ExitStatus = 0, nil
		return fmt.Sprintf("%+v exit %s", c, exit)
	}
	if !slices.Equal(slices.Collect(mapSeq(gCmds, sameCmd)), slices.Collect(mapSeq(lCmds, sameCmd))) {
		t.Errorf("launched command records = %+v, want those of the global hooks %+v", lCmds, gCmds)
	}
	for i, pane := range lPanes {
		if pane != "w14:p1" {
			t.Errorf("launched observation %d is in pane %s, want w14:p1", i, pane)
		}
	}
	l, ss := launched.launchRecord(dir)
	want := []store.LaunchSession{{ID: "s1", Source: store.CaptureHooks, FirstSeen: testNow.Add(time.Second)}}
	if l.NativeSessionID != "s1" || !slices.Equal(ss, want) {
		t.Errorf("launch record = session %q, sessions %+v; want s1 and %+v", l.NativeSessionID, ss, want)
	}
}

// mapSeq yields fn of each element of s.
func mapSeq[T, U any](s []T, fn func(T) U) func(yield func(U) bool) {
	return func(yield func(U) bool) {
		for _, v := range s {
			if !yield(fn(v)) {
				return
			}
		}
	}
}

// AC E24-S5: a refused or broken launch captures nothing and records no session, and the hook
// prints nothing: `brain hook` exits 0 on the error.
func TestLaunchRefused(t *testing.T) {
	stop := `{"session_id":"s1","last_assistant_message":"ready"}`
	for _, c := range []struct {
		name    string
		profile string
		environ []string
		prep    func(f *launchFixture, dir string) string // returns the launch folder of the hook
		want    launch.Reason                             // "" for an error that is no refusal
	}{
		{name: "ended", prep: func(f *launchFixture, dir string) string {
			if err := launch.End(context.Background(), f.b.Store, f.b.Socket, func(store.Launch) bool { return true },
				store.EndAgentExit, testNow); err != nil {
				f.t.Fatal(err)
			}
			return dir
		}, want: launch.Unknown},
		{name: "pane gone", prep: func(f *launchFixture, dir string) string {
			f.sock.remove("w1:p1")
			return dir
		}, want: launch.PaneGone},
		{name: "another agent", prep: func(f *launchFixture, dir string) string {
			f.sock.set("w1:p1", "codex")
			return dir
		}, want: launch.OtherAgent},
		{name: "call from another pane", environ: []string{"HERDR_ENV=1", "HERDR_PANE_ID=w1:p2"}, want: launch.OtherPane},
		{name: "unknown ID", prep: func(f *launchFixture, dir string) string {
			if err := os.WriteFile(filepath.Join(dir, "id"), []byte(strings.Repeat("0", 32)), 0o600); err != nil {
				f.t.Fatal(err)
			}
			return dir
		}, want: launch.Unknown},
		{name: "not a launch folder", prep: func(f *launchFixture, _ string) string { return f.b.Store }},
		{name: "hook of another CLI", profile: "codex"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newLaunchFixture(t)
			dir := f.launch("w1:p1", "claude")
			f.capture("claude", "w1:p1", store.KeyCapture, "cap-1")
			f.capture("codex", "w1:p1", store.KeyCapture, "cap-2")
			hookDir := dir
			if c.prep != nil {
				hookDir = c.prep(f, dir)
			}
			p := claude(t)
			if c.profile != "" {
				var err error
				if p, err = adapter.Load(c.profile); err != nil {
					t.Fatal(err)
				}
			}
			err := f.run(f.hook(p, adapter.EventStop, hookDir, c.environ), stop)
			var r *launch.RefusedError
			if c.want != "" && (!errors.As(err, &r) || r.Reason != c.want) || c.want == "" && err == nil {
				t.Errorf("hook = %v, want refused: %q (or an error for \"\")", err, c.want)
			}
			if obs, _, _ := f.rows(); len(obs) != 0 {
				t.Errorf("observations = %+v, want none", obs)
			}
			f.write(func(ctx context.Context, tx *sql.Tx) error {
				var n int
				if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM launch_sessions`).Scan(&n); err != nil || n != 0 {
					t.Errorf("launch sessions = %d, %v, want none", n, err)
				}
				return nil
			})
		})
	}
}

// AC E24-S5: two Codex panes in one folder (one workspace), each launched, with one hook
// environment and no herdr pane id in it. Each hook record goes to the pane of its own launch.
func TestLaunchTwoCodexPanes(t *testing.T) {
	p, err := adapter.Load("codex")
	if err != nil {
		t.Fatal(err)
	}
	f := newLaunchFixture(t)
	dirs := map[string]string{"w1:p1": f.launch("w1:p1", "codex"), "w1:p2": f.launch("w1:p2", "codex")}
	f.capture("codex", "w1:p1", store.KeyAgent, "thread-1")
	f.capture("codex", "w1:p2", store.KeyAgent, "thread-2")
	env := []string{"HOME=/home/me", "HERDR_ENV=1"}
	for _, r := range []struct{ pane, thread, text string }{
		{"w1:p1", "thread-1", "alpha prompt of pane one"},
		{"w1:p2", "thread-2", "beta prompt of pane two"},
		{"w1:p1", "thread-1", "gamma prompt of pane one"},
	} {
		raw := fmt.Sprintf(`{"session_id":%q,"turn_id":"t","cwd":"/work","hook_event_name":"UserPromptSubmit","prompt":%q}`, r.thread, r.text)
		if err := f.run(f.hook(p, adapter.EventPrompt, dirs[r.pane], env), raw); err != nil {
			t.Fatalf("hook of %s: %v", r.pane, err)
		}
	}
	obs, panes, _ := f.rows()
	got := map[string]string{}
	for i, o := range obs {
		got[o.Excerpt] = panes[i]
	}
	want := map[string]string{"alpha prompt of pane one": "w1:p1", "beta prompt of pane two": "w1:p2", "gamma prompt of pane one": "w1:p1"}
	if !mapsEqual(got, want) {
		t.Errorf("pane of each prompt = %v, want %v", got, want)
	}
	for pane, thread := range map[string]string{"w1:p1": "thread-1", "w1:p2": "thread-2"} {
		if _, ss := f.launchRecord(dirs[pane]); len(ss) != 1 || ss[0].ID != thread || ss[0].Source != store.CaptureHooks {
			t.Errorf("sessions of the launch of %s = %+v, want %s from hooks", pane, ss, thread)
		}
	}
}

func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || w != v {
			return false
		}
	}
	return true
}

// E24-S5: a launched prompt record that matches a ready proposal starts its suggestion process
// with the herdr variables of the pane of the launch record, as its own environment has none.
func TestLaunchSuggestionEnv(t *testing.T) {
	const prompt = `{"session_id":"s1","prompt":"run make Foo"}`
	p := claude(t)
	f := newLaunchFixture(t)
	dir := f.launch("w14:p1", "claude")
	f.capture("claude", "w14:p1", store.KeyCapture, "cap-1")
	if err := f.run(f.hook(p, adapter.EventPrompt, dir, noHerdrEnv), prompt); err != nil {
		t.Fatal(err)
	}
	main := &fixture{mainStore: f.b.Store}
	proposeFrom(t, main, promptEntries(t, main)[0].ObservationID)
	var got [][]string
	h := f.hook(p, adapter.EventPrompt, dir, noHerdrEnv)
	h.Dispatch = func(_ context.Context, environ []string, _ brained.Dispatch) error {
		got = append(got, environ)
		return nil
	}
	if err := f.run(h, prompt); err != nil {
		t.Fatal(err)
	}
	want := []string{"HOME=/home/me", "HERDR_ENV=1", "HERDR_SESSION=default", "HERDR_SOCKET_PATH=" + f.b.Socket,
		"HERDR_PANE_ID=w14:p1", "HERDR_WORKSPACE_ID=w14"}
	if len(got) != 1 || !slices.Equal(got[0], want) {
		t.Errorf("suggestion environments = %q, want one %q", got, want)
	}
}

// E24-S5 review: a record before brained opens the capture session of the pane, or of the new
// session after /clear, is not stored: the same as with the global hooks. The launch then shows
// no session with the source hooks for it, so brained can still read its rollout.
func TestLaunchDroppedRecordNoSource(t *testing.T) {
	p := claude(t)
	for _, c := range []struct {
		name string
		prep func(f *launchFixture)
	}{
		{name: "no capture session yet"},
		{name: "clear before brained sees it", prep: func(f *launchFixture) {
			f.capture("claude", "w1:p1", store.KeyAgent, "s1")
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			const prompt = `{"session_id":"s2","prompt":"first prompt after clear"}`
			global := newLaunchFixture(t)
			global.sock.set("w1:p1", "claude")
			launched := newLaunchFixture(t)
			dir := launched.launch("w1:p1", "claude")
			if c.prep != nil {
				c.prep(global)
				c.prep(launched)
			}
			if err := global.run(global.hook(p, adapter.EventPrompt, "", environ(global.b.Socket, "", "w1:p1", "w1")), prompt); err != nil {
				t.Fatal(err)
			}
			if err := launched.run(launched.hook(p, adapter.EventPrompt, dir, noHerdrEnv), prompt); err != nil {
				t.Fatal(err)
			}
			for name, f := range map[string]*launchFixture{"global": global, "launched": launched} {
				if obs, _, _ := f.rows(); len(obs) != 0 {
					t.Errorf("%s hook stored %+v, want nothing", name, obs)
				}
			}
			if l, ss := launched.launchRecord(dir); l.NativeSessionID != "" || len(ss) != 0 {
				t.Errorf("launch record = current %q, sessions %+v; want none", l.NativeSessionID, ss)
			}
		})
	}
}

// E24-S5 review: a launch that ends while its hook reads the input takes no record and no
// session: the write checks the launch again.
func TestLaunchEndDuringHookInput(t *testing.T) {
	f := newLaunchFixture(t)
	dir := f.launch("w1:p1", "claude")
	f.capture("claude", "w1:p1", store.KeyAgent, "s1")
	l, _ := f.launchRecord(dir)
	h := f.hook(claude(t), adapter.EventPrompt, dir, noHerdrEnv)
	h.noted = func() {
		f.write(func(ctx context.Context, tx *sql.Tx) error {
			return store.EndLaunch(ctx, tx, l.ID, store.EndAgentExit, testNow)
		})
	}
	err := f.run(h, `{"session_id":"s1","prompt":"late prompt"}`)
	var r *launch.RefusedError
	if !errors.As(err, &r) || r.Reason != launch.Ended {
		t.Errorf("hook = %v, want refused: %s", err, launch.Ended)
	}
	if obs, _, _ := f.rows(); len(obs) != 0 {
		t.Errorf("observations = %+v, want none", obs)
	}
	f.write(func(ctx context.Context, tx *sql.Tx) error {
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM launch_sessions`).Scan(&n); err != nil || n != 0 {
			t.Errorf("launch sessions = %d, %v, want none", n, err)
		}
		return nil
	})
}

// AC E24-S5: /clear starts a new native session in the same launch. The launch accepts it:
// it becomes the current session, both sessions show in the record, and the new session is
// still captured, in the capture session that brained opened for it.
func TestLaunchClear(t *testing.T) {
	p := claude(t)
	f := newLaunchFixture(t)
	dir := f.launch("w1:p1", "claude")
	f.capture("claude", "w1:p1", store.KeyAgent, "s1")
	prompt := func(session, text string) string {
		return fmt.Sprintf(`{"session_id":%q,"prompt":%q}`, session, text)
	}
	if err := f.run(f.hook(p, adapter.EventPrompt, dir, noHerdrEnv), prompt("s1", "first prompt before clear")); err != nil {
		t.Fatal(err)
	}
	// /clear: herdr names the new session, and brained opens its capture session.
	f.capture("claude", "w1:p1", store.KeyAgent, "s2")
	if err := f.run(f.hook(p, adapter.EventPrompt, dir, noHerdrEnv), prompt("s2", "second prompt after clear")); err != nil {
		t.Fatal(err)
	}
	obs, _, _ := f.rows()
	var texts []string
	for _, o := range obs {
		texts = append(texts, o.Excerpt)
	}
	if want := []string{"first prompt before clear", "second prompt after clear"}; !slices.Equal(texts, want) {
		t.Errorf("observations = %q, want %q", texts, want)
	}
	l, ss := f.launchRecord(dir)
	want := []store.LaunchSession{
		{ID: "s1", Source: store.CaptureHooks, FirstSeen: testNow.Add(time.Second)},
		{ID: "s2", Source: store.CaptureHooks, FirstSeen: testNow.Add(2 * time.Second)},
	}
	if l.NativeSessionID != "s2" || !slices.Equal(ss, want) {
		t.Errorf("launch record = current %q, sessions %+v; want s2 and %+v", l.NativeSessionID, ss, want)
	}
}
