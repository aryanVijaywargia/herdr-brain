package hook

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/aryanVijaywargia/herdr-brain/internal/adapter"
	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

// startInput is a start hook input of agent session id with transcript path path; path ""
// leaves the field out.
func startInput(id, path string) string {
	if path == "" {
		return fmt.Sprintf(`{"session_id":%q,"cwd":"/tmp/x"}`, id)
	}
	return fmt.Sprintf(`{"session_id":%q,"cwd":"/tmp/x","transcript_path":%q}`, id, path)
}

// start runs the hook with profile p in env on input and returns the printed brief, or "".
func start(t *testing.T, f *fixture, p *adapter.Profile, env []string, input string) string {
	t.Helper()
	var out bytes.Buffer
	if err := f.hook(p, env).Run(context.Background(), strings.NewReader(input), &out); err != nil {
		t.Fatalf("Run() = %v, want nil", err)
	}
	return briefOf(t, out.String())
}

func codex(t *testing.T) *adapter.Profile {
	t.Helper()
	p, err := adapter.Load("codex")
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// lastLine is the last session line of a claude session at testNow with transcript path.
func lastLine(path string) string {
	return "Last session in this workspace: claude, 2026-09-28 10:30, transcript " + path + "\n"
}

// AC E15-S4: Claude start, then Codex start in the same workspace: the Codex brief ends with
// the line, with the Claude path, and each receipt stores its own path. The brief hash covers
// the printed text, the line included; the item revisions do not change.
func TestLastSessionLine(t *testing.T) {
	f := newFixture(t)
	env := environ(f.mainSock, "", "w14:p1", "w14")
	first := start(t, f, claude(t), env, startInput("claude-1", "/t/claude-1.jsonl"))
	if strings.Contains(first, "Last session") {
		t.Errorf("first brief of the workspace has a last session line:\n%s", first)
	}
	got := start(t, f, codex(t), env, startInput("codex-1", "/t/codex-1.jsonl"))
	if want := lastLine("/t/claude-1.jsonl") + "<!-- brain:end -->"; !strings.HasSuffix(got, want) {
		t.Errorf("codex brief =\n%s\nwant it to end with\n%s", got, want)
	}
	if strings.Count(got, "Last session") != 1 || got != strings.TrimSuffix(first, "<!-- brain:end -->")+lastLine("/t/claude-1.jsonl")+"<!-- brain:end -->" {
		t.Errorf("codex brief =\n%s\nwant the first brief with one line added at the end of the workspace part:\n%s", got, first)
	}
	ds := deliveries(t, f.mainStore)
	if len(ds) != 2 {
		t.Fatalf("receipts = %d, want 2", len(ds))
	}
	for i, want := range []struct{ profile, path, brief string }{
		{"claude", "/t/claude-1.jsonl", first}, {"codex", "/t/codex-1.jsonl", got},
	} {
		sum := sha256.Sum256([]byte(want.brief))
		d := ds[i]
		if d.Profile != want.profile || d.TranscriptPath == nil || *d.TranscriptPath != want.path ||
			*d.BriefHash != hex.EncodeToString(sum[:]) {
			t.Errorf("receipt %d = %s, want %s with path %s and the hash of its printed brief", i+1, show(d), want.profile, want.path)
		}
	}
	if !reflect.DeepEqual(ds[0].ItemRevisions, ds[1].ItemRevisions) {
		t.Errorf("item revisions = %v and %v, want the same: the line is no item", ds[0].ItemRevisions, ds[1].ItemRevisions)
	}
	// The built output and its manifest hold no line.
	for _, name := range []string{"workspaces/ws_a14.md", "manifest.json"} {
		b, err := os.ReadFile(filepath.Join(f.mainStore, outDir, fmt.Sprintf("gen-%d", f.gen), name))
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(b, []byte("transcript")) {
			t.Errorf("%s holds the last session line:\n%s", name, b)
		}
	}
}

// AC E15-S4: no line for the first session, an unmapped workspace, a withheld workspace part,
// a withheld brief, a resume, a session of another workspace, a start with no transcript path
// and a forgotten session. A workspace with no items gets the line under its label.
func TestNoLastSessionLine(t *testing.T) {
	claudeA14 := func(t *testing.T, f *fixture) {
		t.Helper()
		start(t, f, claude(t), environ(f.mainSock, "", "w14:p1", "w14"), startInput("claude-1", "/t/claude-1.jsonl"))
	}
	// claudeA16 stores a receipt of ws_a16, whose part the hook withholds: "withheld: identity".
	claudeA16 := func(t *testing.T, f *fixture) {
		t.Helper()
		start(t, f, claude(t), environ(f.mainSock, "", "w16:p1", "w16"), startInput("claude-1", "/t/claude-1.jsonl"))
	}
	update := func(q string) func(t *testing.T, f *fixture) {
		return func(t *testing.T, f *fixture) {
			t.Helper()
			s := openStore(t, f.mainStore)
			defer closeStore(t, s)
			write(t, s, func(tx *sql.Tx) error {
				_, err := tx.Exec(q)
				return err
			})
		}
	}
	tests := []struct {
		name   string
		prep   []func(t *testing.T, f *fixture)
		env    func(f *fixture) []string
		input  string
		want   string // the line; "" for none
		silent bool   // the hook prints nothing
	}{
		{name: "first session of the workspace", env: paneA14, input: startInput("codex-1", "/t/codex-1.jsonl")},
		{name: "unmapped workspace", prep: []func(*testing.T, *fixture){claudeA14},
			env: func(f *fixture) []string { return environ(f.mainSock, "", "w15:p1", "w15") }},
		{name: "workspace part withheld", prep: []func(*testing.T, *fixture){claudeA16},
			env: func(f *fixture) []string { return environ(f.mainSock, "", "w16:p1", "w16") }},
		{name: "forget pending", prep: []func(*testing.T, *fixture){claudeA14, update(`UPDATE outputs SET holds_forgotten_text = 1`)},
			env: paneA14, silent: true},
		{name: "inconsistent output", prep: []func(*testing.T, *fixture){claudeA14, func(t *testing.T, f *fixture) {
			t.Helper()
			name := filepath.Join(f.mainStore, outDir, fmt.Sprintf("gen-%d", f.gen), "workspaces", "ws_a14.md")
			if err := os.WriteFile(name, []byte("edited by hand\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}}, env: paneA14, silent: true},
		{name: "resume of the only earlier session", prep: []func(*testing.T, *fixture){claudeA14}, env: paneA14,
			input: startInput("claude-1", "/t/claude-1.jsonl")},
		// B's w14 is another workspace than A's w14.
		{name: "session of another workspace", prep: []func(*testing.T, *fixture){claudeA14},
			env: func(f *fixture) []string { return environ(f.workSock, "work", "w14:p1", "w14") }},
		{name: "earlier start with no transcript path", prep: []func(*testing.T, *fixture){func(t *testing.T, f *fixture) {
			t.Helper()
			start(t, f, claude(t), environ(f.mainSock, "", "w14:p1", "w14"), startInput("claude-1", ""))
		}}, env: paneA14},
		// brain forget --session clears the path (internal/cli TestForgetSessionReceiptOnly).
		{name: "forgotten session", prep: []func(*testing.T, *fixture){claudeA14, func(t *testing.T, f *fixture) {
			t.Helper()
			s := openStore(t, f.mainStore)
			defer closeStore(t, s)
			write(t, s, func(tx *sql.Tx) error {
				return store.ClearTranscriptPaths(context.Background(), tx, "default", "claude-1")
			})
		}}, env: paneA14},
		{name: "workspace with no items gets the line under its label",
			prep: []func(*testing.T, *fixture){func(t *testing.T, f *fixture) {
				t.Helper()
				start(t, f, claude(t), environ(f.workSock, "work", "w14:p1", "w14"), startInput("claude-1", "/t/claude-1.jsonl"))
			}},
			env:  func(f *fixture) []string { return environ(f.workSock, "work", "w14:p1", "w14") },
			want: "## This workspace\n" + lastLine("/t/claude-1.jsonl")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			for _, prep := range tt.prep {
				prep(t, f)
			}
			input := tt.input
			if input == "" {
				input = startInput("codex-1", "/t/codex-1.jsonl")
			}
			got := start(t, f, codex(t), tt.env(f), input)
			if tt.silent != (got == "") {
				t.Fatalf("brief =\n%s\nwant printed %v", got, !tt.silent)
			}
			if tt.want == "" && strings.Contains(got, "Last session") {
				t.Errorf("brief =\n%s\nwant no last session line", got)
			}
			if tt.want != "" && !strings.HasSuffix(got, tt.want+"<!-- brain:end -->") {
				t.Errorf("brief =\n%s\nwant it to end with\n%s", got, tt.want)
			}
		})
	}
}

func paneA14(f *fixture) []string { return environ(f.mainSock, "", "w14:p1", "w14") }

// AC E15-S4: a start input with no transcript path gives a receipt with NULL.
func TestNoTranscriptPath(t *testing.T) {
	f := newFixture(t)
	start(t, f, claude(t), paneA14(f), startInput("claude-1", ""))
	start(t, f, claude(t), paneA14(f), startInput("claude-2", "/t/claude-2.jsonl"))
	ds := deliveries(t, f.mainStore)
	if len(ds) != 2 || ds[0].TranscriptPath != nil || ds[1].TranscriptPath == nil {
		t.Errorf("receipts = %v, want a NULL transcript path, then /t/claude-2.jsonl", ds)
	}
}

// AC E15-S5: a start that gives "withheld: identity" never counts as the last session. Start A
// in w16 is withheld; after w16's root moves back into its project folder, start B prints the
// workspace part with no line for A, or with the line of an earlier emitted start.
func TestLastSessionSkipsIdentity(t *testing.T) {
	pane := func(f *fixture) []string { return environ(f.mainSock, "", "w16:p1", "w16") }
	// root writes the saved root of w16 into the session file of the main socket.
	root := func(t *testing.T, f *fixture, dir string) {
		t.Helper()
		writeJSON(t, filepath.Join(filepath.Dir(f.mainSock), "session.json"), sessionFile(map[string]string{
			"w14": canonical(t, filepath.Join(f.dir, "proj", "sub")), "w15": canonical(t, filepath.Join(f.dir, "proj")),
			"w16": canonical(t, filepath.Join(f.dir, dir))}))
	}
	for _, tt := range []struct {
		name    string
		earlier bool   // an emitted start in w16 comes before A
		want    string // the line; "" for none
	}{
		{name: "no earlier session"},
		{name: "an earlier emitted session", earlier: true, want: lastLine("/t/claude-0.jsonl")},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			if tt.earlier {
				root(t, f, "proj")
				start(t, f, claude(t), pane(f), startInput("claude-0", "/t/claude-0.jsonl"))
				root(t, f, "other")
			}
			start(t, f, claude(t), pane(f), startInput("claude-1", "/t/claude-1.jsonl"))
			ds := deliveries(t, f.mainStore)
			if a := ds[len(ds)-1]; a.Result != store.ResultWithheld || a.WithheldReason == nil || *a.WithheldReason != reasonIdentity ||
				a.TranscriptPath == nil || *a.TranscriptPath != "/t/claude-1.jsonl" {
				t.Fatalf("receipt of A = %s, want withheld: identity with path /t/claude-1.jsonl", show(a))
			}
			root(t, f, "proj")
			got := start(t, f, codex(t), pane(f), startInput("codex-1", "/t/codex-1.jsonl"))
			if !strings.Contains(got, "## This workspace") {
				t.Fatalf("brief of B =\n%s\nwant the workspace part printed", got)
			}
			if tt.want == "" && strings.Contains(got, "Last session") {
				t.Errorf("brief of B =\n%s\nwant no last session line", got)
			}
			if tt.want != "" && !strings.HasSuffix(got, tt.want+"<!-- brain:end -->") {
				t.Errorf("brief of B =\n%s\nwant it to end with\n%s", got, tt.want)
			}
		})
	}
}
