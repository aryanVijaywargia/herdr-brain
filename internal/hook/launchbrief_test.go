package hook

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aryanVijaywargia/herdr-brain/internal/adapter"
	"github.com/aryanVijaywargia/herdr-brain/internal/launch"
	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

// itemOf is the text of the workspace item of herdr workspace ws in mapBriefs.
func itemOf(ws string) string { return "the workspace item of " + ws }

// mapBriefs maps the herdr workspaces w1 and w2 of the fixture, each with its own item, adds a
// global item, and builds the brief.
func (f *launchFixture) mapBriefs() {
	f.t.Helper()
	root := canonical(f.t, f.t.TempDir())
	roots := map[string]string{}
	var entries []map[string]string
	for _, ws := range []string{"w1", "w2"} {
		roots[ws] = filepath.Join(root, ws)
		if err := os.Mkdir(roots[ws], 0o700); err != nil {
			f.t.Fatal(err)
		}
		entries = append(entries, entry("ws_"+ws, "default", ws, roots[ws]))
	}
	writeJSON(f.t, filepath.Join(filepath.Dir(f.b.Socket), "session.json"), sessionFile(roots))
	writeJSON(f.t, filepath.Join(f.b.Store, "workspaces.json"), mapFile(entries...))
	s := openStore(f.t, f.b.Store)
	defer closeStore(f.t, s)
	addItem(f.t, s, "", mainGlobal)
	for _, ws := range []string{"w1", "w2"} {
		addWorkspace(f.t, s, "ws_"+ws, "default", ws)
		addItem(f.t, s, "ws_"+ws, itemOf(ws))
	}
	build(f.t, s, f.b.Store)
}

// receiptFields is the part of receipt d that says which brief went to which session.
func receiptFields(d store.Delivery) string {
	return fmt.Sprint(d.Profile, *d.HerdrSession, d.PaneID, *d.AgentSessionID, *d.WorkspaceKey, *d.BriefHash,
		*d.OutputGeneration, d.ItemRevisions, d.Result)
}

// AC #444: for the same workspace, a launched session gets the same brief text as the current
// start hook gives: a Claude launch compiles it for its --append-system-prompt-file flag, and
// the start hook of a Codex launch prints it. Two panes in two workspaces each get the brief of
// their own workspace. The start of each launch writes one receipt: the receipt of the current
// start hook, with the launch record ID. The start hook of a Claude launch prints nothing.
func TestLaunchStartBrief(t *testing.T) {
	f := newLaunchFixture(t)
	f.mapBriefs()
	ctx := context.Background()
	for _, kind := range []string{"claude", "codex"} {
		p, err := adapter.Load(kind)
		if err != nil {
			t.Fatal(err)
		}
		for _, pane := range []string{"w1:p1", "w2:p1"} {
			ws, _, _ := strings.Cut(pane, ":")
			other := map[string]string{"w1": "w2", "w2": "w1"}[ws]
			f.sock.set(pane, "")
			var out bytes.Buffer
			if err := f.hook(p, adapter.EventStart, "", environ(f.sock.path, "default", pane, ws)).Run(ctx, strings.NewReader(input), &out); err != nil {
				t.Fatal(err)
			}
			want := briefOf(t, out.String())
			if !strings.Contains(want, itemOf(ws)) || strings.Contains(want, itemOf(other)) {
				t.Fatalf("start hook brief of %s = %q, want the item of %s only", pane, want, ws)
			}
			before := deliveries(t, f.b.Store)
			dir := f.launch(pane, kind)
			if kind == "claude" {
				got, err := LaunchBrief(ctx, f.b, ws, pane, "", dir, func() time.Time { return testNow })
				if err != nil || got != want {
					t.Errorf("LaunchBrief(%s) = %q, %v, want the brief of the start hook %q", pane, got, err, want)
				}
				if n := len(deliveries(t, f.b.Store)); n != len(before) {
					t.Errorf("LaunchBrief wrote %d receipts, want none: the start hook writes it", n-len(before))
				}
			}
			out.Reset()
			if err := f.hook(p, adapter.EventStart, dir, noHerdrEnv).Run(ctx, strings.NewReader(input), &out); err != nil {
				t.Fatalf("%s start hook of the launch in %s: %v", kind, pane, err)
			}
			switch got := out.String(); {
			case kind == "claude" && got != "":
				t.Errorf("claude start hook of the launch printed %q, want nothing", got)
			case kind == "codex" && briefOf(t, got) != want:
				t.Errorf("codex start hook of the launch in %s printed %q, want the brief of the start hook %q", pane, briefOf(t, got), want)
			}
			after := deliveries(t, f.b.Store)
			added := after[len(before):]
			id := filepath.Base(dir)
			if len(added) != 1 || added[0].LaunchID == nil || fmt.Sprint(*added[0].LaunchID) != id {
				t.Fatalf("%s launch in %s wrote receipts %+v, want one with launch %s", kind, pane, added, id)
			}
			if got, want := receiptFields(added[0]), receiptFields(before[len(before)-1]); got != want {
				t.Errorf("%s launch receipt = %s, want the start hook receipt %s", kind, got, want)
			}
		}
	}
}

// A refused launch, and a Claude launch with no kept brief receipt, print nothing and write no
// receipt: the start never fails.
func TestLaunchStartRefused(t *testing.T) {
	f := newLaunchFixture(t)
	f.mapBriefs()
	ctx := context.Background()
	for _, tt := range []struct {
		name, kind  string
		gone, brief bool     // the pane is gone; a Claude launch kept its brief receipt
		env         []string // the environment of the hook
	}{
		{name: "claude pane gone", kind: "claude", gone: true, brief: true, env: noHerdrEnv},
		{name: "codex pane gone", kind: "codex", gone: true, env: noHerdrEnv},
		{name: "claude call from another pane", kind: "claude", brief: true, env: append(slices.Clone(noHerdrEnv), "HERDR_PANE_ID=w1:p9")},
		{name: "claude no brief receipt", kind: "claude", env: noHerdrEnv},
	} {
		t.Run(tt.name, func(t *testing.T) {
			p, err := adapter.Load(tt.kind)
			if err != nil {
				t.Fatal(err)
			}
			dir := f.launch("w1:p1", tt.kind)
			if tt.brief {
				if _, err := LaunchBrief(ctx, f.b, "w1", "w1:p1", "", dir, time.Now); err != nil {
					t.Fatal(err)
				}
			}
			if tt.gone {
				f.sock.remove("w1:p1")
			}
			before := len(deliveries(t, f.b.Store))
			if err := f.run(f.hook(p, adapter.EventStart, dir, tt.env), input); err == nil {
				t.Error("start hook of the launch = nil error, want one")
			}
			if n := len(deliveries(t, f.b.Store)); n != before {
				t.Errorf("start hook of the launch wrote %d receipts, want none", n-before)
			}
		})
	}
}

// A store with no built brief yet: a Claude launch gets no brief and keeps no receipt, as the
// start hook prints nothing and writes no receipt then.
func TestLaunchBriefNoGeneration(t *testing.T) {
	f := newLaunchFixture(t)
	dir := f.launch("w1:p1", "claude")
	text, err := LaunchBrief(context.Background(), f.b, "w1", "w1:p1", "", dir, time.Now)
	if err != nil || text != "" {
		t.Errorf("LaunchBrief() = %q, %v, want no brief and no error", text, err)
	}
	if _, err := os.Stat(filepath.Join(dir, launch.StartFile)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("%s: %v, want none", launch.StartFile, err)
	}
}

// Review of #444: a launch that resumes a session by its ID (brain claude --resume <id>) gets
// the brief that the current start hook gives that session: the last session line leaves the
// resumed session out. A launch that names no session (--continue, or --resume with no ID)
// does not know the session before its start, so its brief can name the resumed session as the
// last session: a known limit.
func TestLaunchBriefResume(t *testing.T) {
	f := newLaunchFixture(t)
	f.mapBriefs()
	ctx := context.Background()
	p := claude(t)
	raw := startInput("resume-me", "/tmp/resume-me.jsonl")
	first := f.launch("w1:p1", "claude")
	if _, err := LaunchBrief(ctx, f.b, "w1", "w1:p1", "", first, time.Now); err != nil {
		t.Fatal(err)
	}
	if err := f.run(f.hook(p, adapter.EventStart, first, noHerdrEnv), raw); err != nil {
		t.Fatal(err)
	}
	var normal bytes.Buffer
	env := environ(f.sock.path, "default", "w1:p1", "w1")
	if err := f.hook(p, adapter.EventStart, "", env).Run(ctx, strings.NewReader(raw), &normal); err != nil {
		t.Fatal(err)
	}
	want := briefOf(t, normal.String())
	for _, tt := range []struct {
		session string
		same    bool
	}{{"resume-me", true}, {"", false}} {
		dir := f.launch("w1:p1", "claude")
		got, err := LaunchBrief(ctx, f.b, "w1", "w1:p1", tt.session, dir, time.Now)
		if err != nil {
			t.Fatal(err)
		}
		if (got == want) != tt.same || tt.same == strings.Contains(got, "/tmp/resume-me.jsonl") {
			t.Errorf("LaunchBrief(session %q) = %q, want the start hook brief %q: %t", tt.session, got, want, tt.same)
		}
	}
}
