package herdr

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

// TestMain lets the test binary act as a fake herdr when BRAIN_FAKE_HERDR names a folder.
func TestMain(m *testing.M) {
	if dir := os.Getenv("BRAIN_FAKE_HERDR"); dir != "" {
		os.Exit(fakeHerdr(dir))
	}
	os.Exit(m.Run())
}

// reply is what the fake herdr prints for one command.
type reply struct {
	Stdout string
	Stderr string
	Code   int
	Hang   bool
}

// call is one run of the fake herdr: its arguments and the target env it got.
type call struct {
	Args    []string
	Session string
	Socket  string
}

// fakeHerdr answers from dir/replies.json, keyed by its arguments, and appends each call to
// dir/calls.jsonl.
func fakeHerdr(dir string) int {
	args := os.Args[1:]
	f, err := os.OpenFile(filepath.Join(dir, "calls.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return 97
	}
	err = json.NewEncoder(f).Encode(call{args, os.Getenv("HERDR_SESSION"), os.Getenv("HERDR_SOCKET_PATH")})
	if err = errors.Join(err, f.Close()); err != nil {
		return 97
	}
	data, err := os.ReadFile(filepath.Join(dir, "replies.json"))
	if err != nil {
		return 98
	}
	var replies map[string]reply
	if err := json.Unmarshal(data, &replies); err != nil {
		return 98
	}
	r, ok := replies[strings.Join(args, " ")]
	if !ok {
		return 99
	}
	if r.Hang {
		time.Sleep(time.Hour) // a herdr that does not answer; the caller's deadline kills it
	}
	if _, err := os.Stdout.WriteString(r.Stdout); err != nil {
		return 96
	}
	if _, err := os.Stderr.WriteString(r.Stderr); err != nil {
		return 96
	}
	return r.Code
}

const (
	testSession = "brain-test"
	testSocket  = "/tmp/brain-test/herdr.sock"
)

// fakeClient returns a client whose herdr is the fake, and the fake's folder. Extra environ
// entries go into the parent environment.
func fakeClient(t *testing.T, replies map[string]reply, environ ...string) (*Client, string) {
	t.Helper()
	env, dir := fakeEnv(t, replies, environ...)
	c, err := New(env, Target{Session: testSession, Socket: testSocket})
	if err != nil {
		t.Fatal(err)
	}
	return c, dir
}

// fakeEnv writes the replies of the fake herdr and returns a parent environment whose herdr
// is the fake, and the fake's folder.
func fakeEnv(t *testing.T, replies map[string]reply, environ ...string) ([]string, string) {
	t.Helper()
	dir := t.TempDir()
	data, err := json.Marshal(replies)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "replies.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	return append([]string{"BRAIN_FAKE_HERDR=" + dir, "HERDR_BIN_PATH=" + os.Args[0]}, environ...), dir
}

func calls(t *testing.T, dir string) []call {
	t.Helper()
	f, err := os.Open(filepath.Join(dir, "calls.jsonl"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close() //nolint:errcheck // read-only: Close cannot lose data
	var cs []call
	s := bufio.NewScanner(f)
	for s.Scan() {
		var c call
		if err := json.Unmarshal(s.Bytes(), &c); err != nil {
			t.Fatal(err)
		}
		cs = append(cs, c)
	}
	if err := s.Err(); err != nil {
		t.Fatal(err)
	}
	return cs
}

func fixture(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// deadline returns a context with a deadline that no passing test comes near.
func deadline(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func ptr(s string) *string { return &s }

// wantShape checks that err is a *ShapeError for field.
func wantShape(t *testing.T, err error, field string) {
	t.Helper()
	var se *ShapeError
	if !errors.As(err, &se) {
		t.Fatalf("err = %v, want a *ShapeError", err)
	}
	if se.Field != field {
		t.Errorf("ShapeError.Field = %q, want %q (err: %v)", se.Field, field, err)
	}
}

func TestPane(t *testing.T) {
	noAgent := fixture(t, "pane-no-agent.json")
	tests := []struct {
		name  string
		reply reply
		want  Pane
		field string // want a ShapeError for this field
		err   string // want an error that contains this text
	}{
		{
			name:  "no agent and no session",
			reply: reply{Stdout: noAgent},
			want:  Pane{ID: "w1:p1", WorkspaceID: "w1", TabID: "w1:t1", Cwd: ptr("/private/tmp")},
		},
		{
			name:  "agent and session",
			reply: reply{Stdout: fixture(t, "pane-agent.json")},
			want: Pane{ID: "w14:p2F", WorkspaceID: "w14", TabID: "w14:t1", Agent: ptr("claude"), Cwd: ptr("/private/tmp"),
				AgentSession: &AgentSession{Agent: "claude", Source: "herdr:claude", Kind: "id", Value: "6a345ec1-3050-4e61-b736-d0e9d2edab82"}},
		},
		{
			name:  "optional fields null",
			reply: reply{Stdout: fixture(t, "pane-null-optional.json")},
			want:  Pane{ID: "w1:p1", WorkspaceID: "w1", TabID: "w1:t1"},
		},
		{name: "malformed pane id", reply: reply{Stdout: fixture(t, "pane-bad-id.json")}, field: "pane.pane_id"},
		{
			name:  "missing workspace id",
			reply: reply{Stdout: strings.Replace(noAgent, `"workspace_id":"w1"`, `"x":"w1"`, 1)},
			field: "pane.workspace_id",
		},
		{
			name:  "null tab id",
			reply: reply{Stdout: strings.Replace(noAgent, `"tab_id":"w1:t1"`, `"tab_id":null`, 1)},
			field: "pane.tab_id",
		},
		{
			name:  "tab in another workspace",
			reply: reply{Stdout: strings.Replace(noAgent, `"tab_id":"w1:t1"`, `"tab_id":"w2:t1"`, 1)},
			field: "pane.tab_id",
		},
		{
			name:  "pane id of wrong type",
			reply: reply{Stdout: strings.Replace(noAgent, `"pane_id":"w1:p1"`, `"pane_id":1`, 1)},
			field: "",
		},
		{
			name:  "agent session without value",
			reply: reply{Stdout: strings.Replace(fixture(t, "pane-agent.json"), `,"value":"6a345ec1-3050-4e61-b736-d0e9d2edab82"`, "", 1)},
			field: "pane.agent_session.value",
		},
		{name: "no pane", reply: reply{Stdout: `{"result":{"type":"pane_info"}}`}, field: "pane"},
		{name: "other result type", reply: reply{Stdout: fixture(t, "workspace-list.json")}, field: "result.type"},
		{name: "no result", reply: reply{Stdout: `{"id":"cli:pane:get"}`}, field: "result.type"},
		{name: "not JSON", reply: reply{Stdout: "pane w1:p1\n"}, field: ""},
		{
			name:  "moved to another workspace",
			reply: reply{Stdout: fixture(t, "pane-moved.json")},
			want:  Pane{ID: "w2:p2", WorkspaceID: "w2", TabID: "w2:t1", Cwd: ptr("/private/tmp/brain-mv")},
		},
		{
			name:  "pane not found",
			reply: reply{Stderr: fixture(t, "error-pane-not-found.json"), Code: 1},
			err:   "herdr pane get: pane bogus not found: pane not found",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, dir := fakeClient(t, map[string]reply{"pane get w1:p1": tt.reply})
			got, err := c.Pane(deadline(t), "w1:p1")
			switch {
			case tt.err != "":
				if err == nil || err.Error() != tt.err || !errors.Is(err, ErrPaneNotFound) {
					t.Fatalf("Pane() err = %v, want %q wrapping ErrPaneNotFound", err, tt.err)
				}
			case tt.want == Pane{}:
				wantShape(t, err, tt.field)
			case err != nil:
				t.Fatalf("Pane() err = %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("Pane() = %+v, want %+v", got, tt.want)
			}
			wantArgs := []string{"pane", "get", "w1:p1"}
			if cs := calls(t, dir); len(cs) != 1 || !slices.Equal(cs[0].Args, wantArgs) {
				t.Errorf("herdr calls = %+v, want one call with %q", cs, wantArgs)
			}
		})
	}
}

// AC (bug #247): past workspace 9, herdr 0.9.3 gives ids with letters (wZ, w1A:p1, w1N:p1,
// w1N:t1). The pane check and the reply parser accept them.
func TestLetterIDs(t *testing.T) {
	paneIn := func(ws string) string {
		return strings.NewReplacer(`"w1:p1"`, `"`+ws+`:p1"`, `"w1:t1"`, `"`+ws+`:t1"`, `"w1"`, `"`+ws+`"`).Replace(fixture(t, "pane-no-agent.json"))
	}
	for _, ws := range []string{"w1A", "w1N"} {
		c, dir := fakeClient(t, map[string]reply{"pane get " + ws + ":p1": {Stdout: paneIn(ws)}})
		got, err := c.Pane(deadline(t), ws+":p1")
		if want := (Pane{ID: ws + ":p1", WorkspaceID: ws, TabID: ws + ":t1", Cwd: ptr("/private/tmp")}); err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("Pane(%s:p1) = %+v, %v, want %+v", ws, got, err, want)
		}
		if cs := calls(t, dir); len(cs) != 1 {
			t.Errorf("herdr calls = %+v, want one", cs)
		}
	}
	pane := paneIn("w1N")

	list := `{"result":{"type":"workspace_list","workspaces":[` +
		`{"label":"z","number":10,"workspace_id":"wZ"},{"label":"a","number":11,"workspace_id":"w1A"}]}}`
	c, _ := fakeClient(t, map[string]reply{"workspace list": {Stdout: list}})
	ws, err := c.Workspaces(deadline(t))
	if want := []Workspace{{ID: "wZ", Label: "z", Number: 10}, {ID: "w1A", Label: "a", Number: 11}}; err != nil || !reflect.DeepEqual(ws, want) {
		t.Errorf("Workspaces() = %+v, %v, want %+v", ws, err, want)
	}

	// The workspace part must still match: a w1N pane in w1A is refused.
	moved := strings.Replace(pane, `"workspace_id":"w1N"`, `"workspace_id":"w1A"`, 1)
	c, _ = fakeClient(t, map[string]reply{"pane get w1N:p1": {Stdout: moved}})
	_, err = c.Pane(deadline(t), "w1N:p1")
	wantShape(t, err, "pane.pane_id")
}

func TestServerVersion(t *testing.T) {
	snap := fixture(t, "snapshot-0.9.1.json")
	tests := []struct {
		name  string
		out   string
		want  string
		field string
	}{
		{name: "0.9.1", out: snap, want: "0.9.1"},
		{name: "0.8.2", out: fixture(t, "snapshot-0.8.2.json"), want: "0.8.2"},
		{name: "no version", out: strings.Replace(snap, `"version":"0.9.1",`, "", 1), field: "snapshot.version"},
		{name: "no protocol", out: strings.Replace(snap, `"protocol":22,`, "", 1), field: "snapshot.protocol"},
		{name: "no snapshot", out: `{"result":{"type":"session_snapshot"}}`, field: "snapshot"},
		{name: "version of wrong type", out: strings.Replace(snap, `"version":"0.9.1"`, `"version":91`, 1), field: ""},
		{name: "malformed version", out: strings.Replace(snap, `"version":"0.9.1"`, `"version":"nine"`, 1), field: "snapshot.version"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, _ := fakeClient(t, map[string]reply{"api snapshot": {Stdout: tt.out}})
			got, err := c.ServerVersion(deadline(t))
			if tt.want == "" {
				wantShape(t, err, tt.field)
			} else if err != nil {
				t.Fatalf("ServerVersion() err = %v", err)
			}
			if got != tt.want {
				t.Errorf("ServerVersion() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestClientVersion(t *testing.T) {
	c, _ := fakeClient(t, map[string]reply{"--version": {Stdout: fixture(t, "version.txt")}})
	if got, err := c.ClientVersion(deadline(t)); err != nil || got != "0.9.1" {
		t.Errorf("ClientVersion() = %q, %v, want \"0.9.1\", nil", got, err)
	}
	c, _ = fakeClient(t, map[string]reply{"--version": {Stdout: "0.9.1\n"}})
	got, err := c.ClientVersion(deadline(t))
	wantShape(t, err, "")
	if got != "" {
		t.Errorf("ClientVersion() = %q, want \"\"", got)
	}
}

func TestWorkspaces(t *testing.T) {
	list := fixture(t, "workspace-list.json")
	tests := []struct {
		name  string
		out   string
		want  []Workspace
		field string
	}{
		{name: "recorded", out: list, want: []Workspace{{ID: "w1", Label: "brain-test", Number: 1}}},
		{name: "empty", out: `{"result":{"type":"workspace_list","workspaces":[]}}`, want: []Workspace{}},
		{name: "no label", out: strings.Replace(list, `"label":"brain-test",`, "", 1), field: "workspaces[0].label"},
		{name: "no number", out: strings.Replace(list, `"number":1,`, "", 1), field: "workspaces[0].number"},
		{name: "malformed id", out: strings.Replace(list, `"workspace_id":"w1"`, `"workspace_id":"1"`, 1), field: "workspaces[0].workspace_id"},
		{name: "no list", out: `{"result":{"type":"workspace_list"}}`, field: "workspaces"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, _ := fakeClient(t, map[string]reply{"workspace list": {Stdout: tt.out}})
			got, err := c.Workspaces(deadline(t))
			if tt.want == nil {
				wantShape(t, err, tt.field)
			} else if err != nil {
				t.Fatalf("Workspaces() err = %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("Workspaces() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestSessions(t *testing.T) {
	list := fixture(t, "session-list.json")
	want := []Session{
		{Name: "default", Default: true, Dir: "/tmp/brain-t0005-hF23/home/.config/herdr",
			Socket: "/tmp/brain-t0005-hF23/home/.config/herdr/herdr.sock"},
		{Name: "brain-t0005", Running: true, Dir: "/tmp/brain-t0005-hF23/home/.config/herdr/sessions/brain-t0005",
			Socket: "/tmp/brain-t0005-hF23/home/.config/herdr/sessions/brain-t0005/herdr.sock"},
	}
	env, dir := fakeEnv(t, map[string]reply{"session list --json": {Stdout: list}}, "HERDR_SESSION=main")
	got, err := Sessions(deadline(t), env)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("Sessions() = %+v, %v, want %+v, nil", got, err, want)
	}
	if cs := calls(t, dir); len(cs) != 1 || cs[0].Session != "main" {
		t.Errorf("herdr calls = %+v, want one call with the parent environment", cs)
	}

	for field, out := range map[string]string{
		"sessions[1].running":     strings.Replace(list, `"running":true,`, "", 1),
		"sessions[1].name":        strings.Replace(list, `"name":"brain-t0005"`, `"name":""`, 1),
		"sessions[0].session_dir": strings.Replace(list, `"session_dir":"/tmp/brain-t0005-hF23/home/.config/herdr"`, `"session_dir":""`, 1),
		"sessions[0].socket_path": strings.Replace(list, `"socket_path":"/tmp/brain-t0005-hF23/home/.config/herdr/herdr.sock"`, `"socket_path":"herdr.sock"`, 1),
	} {
		env, _ := fakeEnv(t, map[string]reply{"session list --json": {Stdout: out}})
		got, err := Sessions(deadline(t), env)
		wantShape(t, err, field)
		if got != nil {
			t.Errorf("Sessions() = %+v, want nil", got)
		}
	}
}

func TestOpenPluginPane(t *testing.T) {
	cmd := "plugin pane open --plugin brainrec --entrypoint review --placement split --target-pane w1:p1 --no-focus --env BRAIN_REVIEW=1"
	spec := PluginPaneSpec{Plugin: "brainrec", Entrypoint: "review", Placement: "split", TargetPane: "w1:p1", Env: []string{"BRAIN_REVIEW=1"}}
	opened := fixture(t, "plugin-pane-opened.json")

	c, _ := fakeClient(t, map[string]reply{cmd: {Stdout: opened}})
	got, err := c.OpenPluginPane(deadline(t), spec)
	want := PluginPane{PluginID: "brainrec", Entrypoint: "review", Pane: Pane{
		ID: "w1:p3", WorkspaceID: "w1", TabID: "w1:t1", Cwd: ptr("/private/tmp/brain-t0005-hF23/plug")}}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("OpenPluginPane() = %+v, %v, want %+v, nil", got, err, want)
	}

	for field, out := range map[string]string{
		"plugin_pane.entrypoint":   strings.Replace(opened, `"entrypoint":"review",`, "", 1),
		"plugin_pane.pane.pane_id": strings.Replace(opened, `"pane_id":"w1:p3"`, `"pane_id":"w1:x3"`, 1),
		"plugin_pane":              `{"result":{"type":"plugin_pane_opened"}}`,
	} {
		c, _ := fakeClient(t, map[string]reply{cmd: {Stdout: out}})
		got, err := c.OpenPluginPane(deadline(t), spec)
		wantShape(t, err, field)
		if !reflect.DeepEqual(got, PluginPane{}) {
			t.Errorf("OpenPluginPane() = %+v, want the zero value", got)
		}
	}
}

func TestClosePluginPane(t *testing.T) {
	closed := fixture(t, "plugin-pane-closed.json")
	c, _ := fakeClient(t, map[string]reply{"plugin pane close w1:p3": {Stdout: closed}})
	if err := c.ClosePluginPane(deadline(t), "w1:p3"); err != nil {
		t.Errorf("ClosePluginPane(w1:p3) = %v, want nil", err)
	}

	c, _ = fakeClient(t, map[string]reply{"plugin pane close w1:p4": {Stdout: closed}})
	wantShape(t, c.ClosePluginPane(deadline(t), "w1:p4"), "pane_id")

	notFound := `{"error":{"code":"plugin_pane_not_found","message":"plugin pane not found"},"id":"cli:plugin"}`
	c, _ = fakeClient(t, map[string]reply{"plugin pane close w1:p1": {Stderr: notFound, Code: 1}})
	err := c.ClosePluginPane(deadline(t), "w1:p1")
	if want := "herdr plugin pane close: plugin pane not found (plugin_pane_not_found)"; err == nil || err.Error() != want {
		t.Errorf("ClosePluginPane(w1:p1) = %v, want %q", err, want)
	}
}

// E14-S3: the notification of a suggestion. herdr answers shown false with no client attached.
func TestShowNotification(t *testing.T) {
	shown := fixture(t, "notification-show.json")
	c, dir := fakeClient(t, map[string]reply{"notification show Brain has 1 suggestion --body proposal 3": {Stdout: shown}})
	if err := c.ShowNotification(deadline(t), "Brain has 1 suggestion", "proposal 3"); err != nil {
		t.Errorf("ShowNotification() = %v, want nil", err)
	}
	want := call{Args: []string{"notification", "show", "Brain has 1 suggestion", "--body", "proposal 3"}, Session: testSession, Socket: testSocket}
	if cs := calls(t, dir); len(cs) != 1 || !reflect.DeepEqual(cs[0], want) {
		t.Errorf("herdr calls = %+v, want [%+v]", cs, want)
	}
	c, _ = fakeClient(t, map[string]reply{"notification show t --body b": {Stdout: `{"result":{"type":"notification_show"}}`}})
	wantShape(t, c.ShowNotification(deadline(t), "t", "b"), "shown")
}

// TestTarget shows that the client's target wins over inherited HERDR_SESSION and
// HERDR_SOCKET_PATH of another session: the child gets no --session flag and both variables
// set to the one target, so they never disagree. TestLiveTarget shows herdr's own order.
func TestTarget(t *testing.T) {
	c, dir := fakeClient(t, map[string]reply{"workspace list": {Stdout: fixture(t, "workspace-list.json")}},
		"HERDR_SESSION=main", "HERDR_SOCKET_PATH=/home/me/.config/herdr/herdr.sock")
	if _, err := c.Workspaces(deadline(t)); err != nil {
		t.Fatal(err)
	}
	want := call{Args: []string{"workspace", "list"}, Session: testSession, Socket: testSocket}
	if cs := calls(t, dir); len(cs) != 1 || !reflect.DeepEqual(cs[0], want) {
		t.Errorf("herdr calls = %+v, want [%+v]", cs, want)
	}
}

func TestChildEnv(t *testing.T) {
	parent := []string{"HOME=/home/me", "HERDR_SESSION=main", "HERDR_PANE_ID=w1:p1", "HERDR_SOCKET_PATH=/main.sock", "HERDR_SESSION=other"}
	got := childEnv(parent, Target{Session: testSession, Socket: testSocket})
	want := []string{"HOME=/home/me", "HERDR_PANE_ID=w1:p1", "HERDR_SESSION=" + testSession, "HERDR_SOCKET_PATH=" + testSocket}
	if !slices.Equal(got, want) {
		t.Errorf("childEnv() = %q, want %q", got, want)
	}
	if parent[1] != "HERDR_SESSION=main" {
		t.Errorf("childEnv changed its input: %q", parent)
	}
}

func TestNew(t *testing.T) {
	bin := []string{"HERDR_BIN_PATH=/usr/local/bin/herdr"}
	empty, onPath := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(onPath, "herdr"), nil, 0o700); err != nil {
		t.Fatal(err)
	}
	relative, err := filepath.Rel(mustWd(t), onPath)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name    string
		environ []string
		target  Target
		wantBin string // empty: want an error
	}{
		{name: "HERDR_BIN_PATH", environ: bin, target: Target{Session: "default", Socket: testSocket}, wantBin: "/usr/local/bin/herdr"},
		{name: "no session", environ: bin, target: Target{Socket: testSocket}},
		{name: "relative socket", environ: bin, target: Target{Session: testSession, Socket: "herdr.sock"}},
		{name: "no herdr", environ: []string{"PATH=" + empty}, target: Target{Session: testSession, Socket: testSocket}},
		{name: "herdr on PATH", environ: []string{"PATH=relative:" + empty + ":" + onPath}, target: Target{Session: testSession, Socket: testSocket}, wantBin: filepath.Join(onPath, "herdr")},
		{name: "herdr only in a relative PATH entry", environ: []string{"PATH=" + relative}, target: Target{Session: testSession, Socket: testSocket}},
	}
	// The process PATH has herdr; New must look only at environ.
	t.Setenv("PATH", onPath)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := New(tt.environ, tt.target)
			if tt.wantBin == "" {
				if err == nil {
					t.Fatalf("New() = %+v, want an error", c)
				}
				return
			}
			if err != nil || c.bin != tt.wantBin {
				t.Fatalf("New() = %+v, %v, want bin %q", c, err, tt.wantBin)
			}
		})
	}
}

// TestTargetArgs shows that no caller string can change the herdr target: herdr 0.9.1 takes
// the last --session anywhere in the command line.
func TestTargetArgs(t *testing.T) {
	open := func(s PluginPaneSpec) func(*Client) error {
		return func(c *Client) error {
			_, err := c.OpenPluginPane(deadline(t), s)
			return err
		}
	}
	spec := PluginPaneSpec{Plugin: "brain", Entrypoint: "review", Placement: "split", TargetPane: "w1:p1"}
	with := func(f func(*PluginPaneSpec)) PluginPaneSpec {
		s := spec
		f(&s)
		return s
	}
	tests := []struct {
		name string
		call func(*Client) error
		want string // part of the error
	}{
		{"pane id flag", func(c *Client) error { _, err := c.Pane(deadline(t), "--session"); return err }, `malformed pane id "--session"`},
		{"pane id flag=value", func(c *Client) error { _, err := c.Pane(deadline(t), "--session=default"); return err }, `malformed pane id "--session=default"`},
		{"close pane id flag=value", func(c *Client) error { return c.ClosePluginPane(deadline(t), "--session=default") }, "malformed pane id"},
		{"plugin flag", open(with(func(s *PluginPaneSpec) { s.Plugin = "--session" })), `argument "--session" would change the herdr target`},
		{"entrypoint flag=value", open(with(func(s *PluginPaneSpec) { s.Entrypoint = "--session=default" })), `argument "--session=default"`},
		{"placement flag", open(with(func(s *PluginPaneSpec) { s.Placement = "--session" })), `argument "--session"`},
		{"target pane flag=value", open(with(func(s *PluginPaneSpec) { s.TargetPane = "--session=default" })), `argument "--session=default"`},
		{"env flag=value", open(with(func(s *PluginPaneSpec) { s.Env = []string{"--session=default"} })), `argument "--session=default"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, dir := fakeClient(t, map[string]reply{})
			if err := tt.call(c); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v, want it to contain %q", err, tt.want)
			}
			if cs := calls(t, dir); len(cs) != 0 {
				t.Errorf("herdr ran: %+v", cs)
			}
		})
	}
}

func mustWd(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return wd
}

func TestDeadline(t *testing.T) {
	c, dir := fakeClient(t, map[string]reply{"workspace list": {Hang: true}})
	if _, err := c.Workspaces(context.Background()); err == nil {
		t.Error("Workspaces() with no deadline: err = nil, want an error")
	}
	if cs := calls(t, dir); len(cs) != 0 {
		t.Errorf("herdr ran with no deadline: %+v", cs)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if _, err := c.Workspaces(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Workspaces() on a hung herdr: err = %v, want context.DeadlineExceeded", err)
	}
}

func TestPanes(t *testing.T) {
	list := fixture(t, "pane-list.json")
	tests := []struct {
		name  string
		out   string
		want  []Pane
		field string // want a ShapeError for this field
	}{
		{
			name: "two panes",
			out:  list,
			want: []Pane{
				{ID: "w1:p1", WorkspaceID: "w1", TabID: "w1:t1", Cwd: ptr("/private/tmp")},
				{ID: "w14:p2F", WorkspaceID: "w14", TabID: "w14:t1", Agent: ptr("claude"), Cwd: ptr("/private/tmp"),
					AgentSession: &AgentSession{Agent: "claude", Source: "herdr:claude", Kind: "id", Value: "6a345ec1-3050-4e61-b736-d0e9d2edab82"}},
			},
		},
		{name: "no panes", out: `{"result":{"type":"pane_list","panes":[]}}`, want: []Pane{}},
		{name: "panes missing", out: `{"result":{"type":"pane_list"}}`, field: "panes"},
		{
			name:  "bad pane",
			out:   strings.Replace(list, `"pane_id":"w14:p2F"`, `"pane_id":"p2F"`, 1),
			field: "panes[1].pane_id",
		},
		{name: "other result type", out: fixture(t, "workspace-list.json"), field: "result.type"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, dir := fakeClient(t, map[string]reply{"pane list": {Stdout: tt.out}})
			got, err := c.Panes(deadline(t))
			if tt.field != "" {
				wantShape(t, err, tt.field)
			} else if err != nil {
				t.Fatalf("Panes() err = %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("Panes() = %+v, want %+v", got, tt.want)
			}
			wantArgs := []string{"pane", "list"}
			if cs := calls(t, dir); len(cs) != 1 || !slices.Equal(cs[0].Args, wantArgs) {
				t.Errorf("herdr calls = %+v, want one call with %q", cs, wantArgs)
			}
		})
	}
}

func TestPaneProcesses(t *testing.T) {
	cmd := "pane process-info --pane w1:p2"
	info := fixture(t, "pane-process-info.json")
	c, _ := fakeClient(t, map[string]reply{cmd: {Stdout: info}})
	got, err := c.PaneProcesses(deadline(t), "w1:p2")
	want := []Process{{Name: "brain", Argv: []string{"/tmp/brain-t0048/plug/bin/brain", "pane", "review", "--proposal", "7"}, PID: 77750}}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("PaneProcesses() = %+v, %v, want %+v, nil", got, err, want)
	}

	for field, out := range map[string]string{
		"process_info.pane_id":                      strings.Replace(info, `"pane_id":"w1:p2"`, `"pane_id":"w1:p3"`, 1),
		"process_info.foreground_processes[0].name": strings.Replace(info, `"name":"brain",`, "", 1),
		"process_info":                              `{"result":{"type":"pane_process_info"}}`,
	} {
		c, _ := fakeClient(t, map[string]reply{cmd: {Stdout: out}})
		got, err := c.PaneProcesses(deadline(t), "w1:p2")
		wantShape(t, err, field)
		if got != nil {
			t.Errorf("PaneProcesses() = %+v, want nil", got)
		}
	}

	c, _ = fakeClient(t, map[string]reply{cmd: {Stderr: fixture(t, "error-pane-not-found.json"), Code: 1}})
	if _, err := c.PaneProcesses(deadline(t), "w1:p2"); !errors.Is(err, ErrPaneNotFound) {
		t.Errorf("PaneProcesses(closed pane) = %v, want ErrPaneNotFound", err)
	}
}
