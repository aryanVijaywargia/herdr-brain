package brained

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/aryanVijaywargia/herdr-brain/internal/bindings"
	"github.com/aryanVijaywargia/herdr-brain/internal/store"
	"github.com/aryanVijaywargia/herdr-brain/internal/transcript"
)

// The history scan tests use hand-made session files in a scratch home;
// no line comes from a real session. They use no real home, no model and no real herdr.

// scanClaim is the claim time of the test scans.
var scanClaim = time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)

// scanFix is a store with a scan row, a scratch home and a history scan with fake herdr calls.
type scanFix struct {
	t     *testing.T
	home  string
	g     Gate
	s     *historyScan
	notes []string // the notification bodies
	syncs int      // the refreshes of the workspace map
}

func newScanFix(t *testing.T, kind bindings.Kind) *scanFix {
	t.Helper()
	f := &scanFix{t: t, home: t.TempDir()}
	f.g = NewGate(bindings.Binding{Session: "s1", Socket: "/tmp/none", Store: t.TempDir(), Kind: kind, Created: scanClaim})
	f.s = newHistoryScan([]string{"HOME=" + f.home, "PATH=/usr/bin:/bin"}, filepath.Join(t.TempDir(), "config.toml"),
		func(r store.ScanReport, extractor bool) string {
			return fmt.Sprintf("%d repeats, extractor %v", len(r.Repeats), extractor)
		})
	f.s.now = func() time.Time { return scanClaim }
	f.s.sync = func(context.Context, Gate, *store.Store) error {
		f.syncs++
		return nil
	}
	f.s.notify = func(_ context.Context, _ Gate, body string) error {
		f.notes = append(f.notes, body)
		return nil
	}
	f.write(func(ctx context.Context, tx *sql.Tx) error {
		return store.RequestScan(ctx, tx, scanClaim.Add(-time.Hour))
	})
	return f
}

// write runs fn in one store transaction.
func (f *scanFix) write(fn func(ctx context.Context, tx *sql.Tx) error) {
	f.t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, f.g.Binding.Store, busyTimeout)
	if err != nil {
		f.t.Fatal(err)
	}
	defer st.Close() //nolint:errcheck // test
	if err := st.Write(ctx, func(tx *sql.Tx) error { return fn(ctx, tx) }); err != nil {
		f.t.Fatal(err)
	}
}

// run runs the scan once and fails on an error.
func (f *scanFix) run() bool {
	f.t.Helper()
	done, err := f.s.run(context.Background(), f.g)
	if err != nil {
		f.t.Fatalf("scan: %v", err)
	}
	return done
}

// row returns the scan row.
func (f *scanFix) row() store.Scan {
	f.t.Helper()
	var sc store.Scan
	f.write(func(ctx context.Context, tx *sql.Tx) (err error) {
		sc, _, err = store.HistoryScan(ctx, tx)
		return err
	})
	return sc
}

// report returns the report of the scan with the default scan gates.
func (f *scanFix) report() store.ScanReport {
	f.t.Helper()
	var r store.ScanReport
	f.write(func(ctx context.Context, tx *sql.Tx) (err error) {
		r, err = store.HistoryScanReport(ctx, tx, 1, store.ScanGates(store.DefaultGates()))
		return err
	})
	return r
}

// observations returns every observation and command record of the store.
func (f *scanFix) observations() ([]store.Observation, []store.CommandRecord) {
	f.t.Helper()
	var os []store.Observation
	var cs []store.CommandRecord
	f.write(func(ctx context.Context, tx *sql.Tx) (err error) {
		if os, err = store.Observations(ctx, tx); err != nil {
			return err
		}
		cs, err = store.CommandRecords(ctx, tx)
		return err
	})
	return os, cs
}

// workspace adds the live workspace key of herdr session binding with a new project folder,
// and returns the folder.
func (f *scanFix) workspace(key, binding string) string {
	f.t.Helper()
	dir, err := filepath.EvalSymlinks(f.t.TempDir())
	if err != nil {
		f.t.Fatal(err)
	}
	f.workspaceAt(key, binding, dir)
	return dir
}

func (f *scanFix) workspaceAt(key, binding, dir string) {
	f.t.Helper()
	f.write(func(ctx context.Context, tx *sql.Tx) error {
		return store.InsertWorkspace(ctx, tx, store.Workspace{Key: key, HerdrSession: binding, HerdrWorkspaceID: "w" + key,
			Label: key, RootFolder: &dir, Status: store.WorkspaceLive, LastSeen: scanClaim})
	})
}

// file writes a session file at rel under the scratch home, changed age before the claim time.
func (f *scanFix) file(rel string, age time.Duration, lines ...string) string {
	f.t.Helper()
	p := filepath.Join(f.home, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		f.t.Fatal(err)
	}
	at := scanClaim.Add(-age)
	if err := os.Chtimes(p, at, at); err != nil {
		f.t.Fatal(err)
	}
	return p
}

// claude writes the Claude session file of session id.
func (f *scanFix) claude(id string, age time.Duration, lines ...string) string {
	f.t.Helper()
	return f.file(filepath.Join(".claude", "projects", "-p", id+".jsonl"), age, lines...)
}

// codex writes the Codex session file of session id.
func (f *scanFix) codex(id string, age time.Duration, lines ...string) {
	f.t.Helper()
	f.file(filepath.Join(".codex", "sessions", "2026", "10", "12", "rollout-"+id+".jsonl"), age, lines...)
}

type obj = map[string]any

func jsonLine(v obj) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// at is a record time in the window: n minutes after the start of 2026-10-12.
func at(n int) string {
	return time.Date(2026, 10, 12, 0, n, 0, 0, time.UTC).Format(time.RFC3339Nano)
}

// cUser is a Claude user record with content, a string or blocks.
func cUser(uuid, cwd string, n int, content any, extra ...obj) string {
	l := obj{"type": "user", "uuid": uuid, "cwd": cwd, "timestamp": at(n), "message": obj{"role": "user", "content": content}}
	for _, e := range extra {
		for k, v := range e {
			l[k] = v
		}
	}
	return jsonLine(l)
}

// cAssistant is a Claude assistant record of message msg with blocks; tokens < 0 gives no usage.
func cAssistant(uuid, msg string, n int, tokens int64, blocks ...obj) string {
	m := obj{"id": msg, "role": "assistant", "model": "claude-test", "content": blocks}
	if tokens >= 0 {
		m["usage"] = obj{"input_tokens": tokens, "cache_creation_input_tokens": 0, "cache_read_input_tokens": 0, "output_tokens": 0}
	}
	return jsonLine(obj{"type": "assistant", "uuid": uuid, "timestamp": at(n), "message": m})
}

func cText(t string) obj { return obj{"type": "text", "text": t} }

func cBash(id, cmd string) obj {
	return obj{"type": "tool_use", "id": id, "name": "Bash", "input": obj{"command": cmd}}
}

func cResult(id, out string, isErr bool) obj {
	return obj{"type": "tool_result", "tool_use_id": id, "content": out, "is_error": isErr}
}

// xMeta is the session_meta line of Codex session id.
func xMeta(id, cwd, source string, extra obj) string {
	p := obj{"id": id, "cwd": cwd, "source": source}
	for k, v := range extra {
		p[k] = v
	}
	return jsonLine(obj{"timestamp": at(0), "type": "session_meta", "payload": p})
}

func xItem(n int, payload obj) string {
	return jsonLine(obj{"timestamp": at(n), "type": "response_item", "payload": payload})
}

func xMessage(n int, role, kind, text string) string {
	return xItem(n, obj{"type": "message", "role": role, "content": []obj{{"type": kind, "text": text}}})
}

func xUser(n int, text string) string      { return xMessage(n, "user", "input_text", text) }
func xAssistant(n int, text string) string { return xMessage(n, "assistant", "output_text", text) }

func xCall(n int, call, cmd string) string {
	return xItem(n, obj{"type": "function_call", "name": "exec_command", "arguments": jsonLine(obj{"cmd": cmd}), "call_id": call})
}

func xOutput(n int, call, out string) string {
	return xItem(n, obj{"type": "function_call_output", "call_id": call, "output": out})
}

func xTokens(n int, total int64) string {
	return jsonLine(obj{"timestamp": at(n), "type": "event_msg", "payload": obj{"type": "token_count",
		"info": obj{"total_token_usage": obj{"total_tokens": total}}}})
}

// AC 5: the scan imports each session as hook records would give it: the prompts
// (sender unconfirmed), each shell command with its command record and the failure excerpt of a
// failed one, and the last assistant text of each turn (sender assistant). It drops what the CLI
// or Brain injected, side chains, compaction summaries, AGENTS.md text, file text that the
// agent read and the secrets, and stores no transcript path.
func TestScanImport(t *testing.T) {
	f := newScanFix(t, bindings.KindMain)
	secret := "AKIAABCDEFGHIJKLMNOP"
	long := strings.Repeat("word ", 150) // 750 characters
	big := "echo " + strings.Repeat("x", 5000)
	cpath := f.claude("c1", 2*time.Hour,
		jsonLine(obj{"type": "summary", "summary": "summary record text", "leafUuid": "u0"}),
		cUser("u1", "/nowhere/app", 1, "<system-reminder>hook context text</system-reminder>use pnpm with key "+secret),
		cAssistant("a1", "m1", 2, 100, cText("I run both."), cBash("t1", "pnpm test"), cBash("t2", "pnpm lint"),
			obj{"type": "tool_use", "id": "t3", "name": "Read", "input": obj{"file_path": "/nowhere/app/a.txt"}}),
		cUser("u2", "/nowhere/app", 3, []obj{cResult("t1", "Exit code 1\nFAIL a.test", true),
			cResult("t2", "file text that lint printed", false), cResult("t3", "file text that the agent read", false)}),
		cAssistant("a2", "m2", 4, 50, cText("The test fails.")),
		cUser("u3", "/nowhere/app", 5, "side chain text", obj{"isSidechain": true}),
		cUser("u4", "/nowhere/app", 6, "meta text", obj{"isMeta": true}),
		cUser("u5", "/nowhere/app", 7, "compaction summary text", obj{"isCompactSummary": true}),
		cUser("u6", "/nowhere/app", 8, "<!-- brain:begin -->brain brief text<!-- brain:end -->"),
		cUser("u7", "/nowhere/app", 9, long),
		cAssistant("a3", "m3", 10, -1, cBash("t4", big)),
	)
	f.codex("x1", 3*time.Hour,
		xMeta("x1", "/nowhere/api", "cli", nil),
		xUser(1, "# AGENTS.md instructions for /nowhere/api\n\nagents file text"),
		xUser(2, "run the api tests"),
		xCall(3, "c1", "make test"),
		xOutput(4, "c1", "Exit code: 2\nOutput:\nFAIL api"),
		xTokens(5, 1000),
		xAssistant(6, "make test fails."),
	)
	if !f.run() {
		t.Fatal("run = not done, want done")
	}
	obs, cmds := f.observations()
	for _, o := range obs {
		for _, gone := range []string{"hook context", "side chain", "meta text", "compaction summary", "summary record",
			"brain brief", "agents file", "AGENTS.md", "file text", secret, f.home, cpath} {
			if strings.Contains(o.Excerpt, gone) {
				t.Errorf("observation %s holds %q: %q", o.CaptureIdentity, gone, o.Excerpt)
			}
		}
		if o.Source != store.SourceTranscript {
			t.Errorf("observation %s source = %s, want transcript", o.CaptureIdentity, o.Source)
		}
		if n := utf8.RuneCountInString(o.Excerpt); n > excerptLimit {
			t.Errorf("observation %s excerpt = %d characters, want at most %d", o.CaptureIdentity, n, excerptLimit)
		}
	}
	got := map[string]string{}
	for _, o := range obs {
		got[o.CaptureIdentity] = fmt.Sprintf("%s %s %v %q", o.Event, o.Sender, o.Truncated, cut(o.Excerpt, 30))
	}
	want := map[string]string{
		"claude:u1":                     `prompt unconfirmed false "use pnpm with key [secret]"`,
		"claude:a1#1":                   `tool tool false "pnpm test"`,
		"claude:a1#2":                   `tool tool false "pnpm lint"`,
		"claude:u2#0":                   `tool tool false "Exit code 1\nFAIL a.test"`,
		"claude:a2":                     `stop assistant false "The test fails."`,
		"claude:u7":                     fmt.Sprintf("prompt unconfirmed true %q", cut(long, 30)),
		"claude:a3#0":                   fmt.Sprintf("tool tool true %q", cut(big, 30)),
		"codex:x1@" + offsetOf(t, f, 2): `prompt unconfirmed false "run the api tests"`,
		"codex:x1@" + offsetOf(t, f, 3): `tool tool false "make test"`,
		"codex:x1@" + offsetOf(t, f, 4): `tool tool false "Exit code: 2\nOutput:\nFAIL api"`,
		"codex:x1@" + offsetOf(t, f, 6): `stop assistant false "make test fails."`,
	}
	diffMaps(t, got, want)
	// One record with two shell commands gives two command records; a long command is cut
	// at 4 KB; a failed command keeps its exit status.
	var argv []string
	for _, c := range cmds {
		exit := "none"
		if c.ExitStatus != nil {
			exit = fmt.Sprint(*c.ExitStatus)
		}
		argv = append(argv, fmt.Sprintf("%s exit %s truncated %v %s", cut(c.Argv, 12), exit, c.Truncated, c.CWD))
		if len(c.Argv) > argvLimit {
			t.Errorf("command record = %d bytes, want at most %d", len(c.Argv), argvLimit)
		}
	}
	wantArgv := []string{"pnpm test exit 1 truncated false /nowhere/app", "pnpm lint exit none truncated false /nowhere/app",
		"echo xxxxxxx exit none truncated true /nowhere/app", "make test exit 2 truncated false /nowhere/api"}
	if !slices.Equal(argv, wantArgv) {
		t.Errorf("command records =\n%q\nwant\n%q", argv, wantArgv)
	}
	f.write(func(ctx context.Context, tx *sql.Tx) error {
		ss, err := store.Sessions(ctx, tx)
		if err != nil {
			return err
		}
		var rows []string
		for _, s := range ss {
			rows = append(rows, fmt.Sprintf("%s %s %s %s %q %q", s.Key.HerdrSession, s.Key.Harness, s.Key.Kind, s.Key.ID,
				s.AgentSessionID, s.WorkspaceKey))
			wm, err := store.Watermarks(ctx, tx, s.ID)
			if err != nil {
				return err
			}
			var last int64
			for _, o := range obs {
				if strings.HasPrefix(o.CaptureIdentity, s.Key.Harness+":") && o.ID > last {
					last = o.ID
				}
			}
			if wm[""] != last {
				t.Errorf("session %s watermark = %v, want %d: the normal pass makes no batch of it", s.Key.ID, wm, last)
			}
		}
		want := []string{`s1 claude agent c1 "c1" ""`, `s1 codex agent x1 "x1" ""`}
		if !slices.Equal(rows, want) {
			t.Errorf("sessions = %q, want %q", rows, want)
		}
		segs, err := store.Segments(ctx, tx)
		if len(segs) != 2 || segs[0].PaneID != "" {
			t.Errorf("segments = %+v, want one with no pane for each session", segs)
		}
		return err
	})
	if sc := f.row(); sc.Status != store.ScanExtracting || sc.Counts["claude"].Read != 1 || sc.Counts["codex"].Read != 1 {
		t.Errorf("scan row = %+v, want extracting with 1 Claude and 1 Codex session read", sc)
	}
}

// offsetOf returns the byte offset of line i of the Codex file of session x1, the record id of
// that line.
func offsetOf(t *testing.T, f *scanFix, i int) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(f.home, ".codex", "sessions", "2026", "10", "12", "rollout-x1.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	off := 0
	for _, l := range strings.SplitAfter(string(b), "\n")[:i] {
		off += len(l)
	}
	return fmt.Sprint(off)
}

func cut(s string, n int) string {
	r := []rune(s)
	return string(r[:min(len(r), n)])
}

func diffMaps(t *testing.T, got, want map[string]string) {
	t.Helper()
	for k, w := range want {
		if g, ok := got[k]; !ok {
			t.Errorf("%s: missing, want %s", k, w)
		} else if g != w {
			t.Errorf("%s = %s, want %s", k, g, w)
		}
	}
	for k, g := range got {
		if _, ok := want[k]; !ok {
			t.Errorf("%s = %s, want none", k, g)
		}
	}
}

// AC 2: the scan picks the newest 300 files that changed in the 30 days before the claim time,
// Claude and Codex together, under the Claude and Codex homes of the environment.
func TestScanFiles(t *testing.T) {
	var files []transcript.File
	for i := range 305 {
		files = append(files, transcript.File{Harness: transcript.Claude, Path: fmt.Sprint(i),
			Changed: scanClaim.Add(-time.Duration(i) * time.Hour)})
	}
	files = append([]transcript.File{
		{Harness: transcript.Codex, Path: "after", Changed: scanClaim.Add(time.Minute)},
	}, files...)
	files = append(files, transcript.File{Harness: transcript.Codex, Path: "old", Changed: scanClaim.Add(-31 * 24 * time.Hour)})
	var homes []string
	s := &historyScan{environ: []string{"HOME=/h", "CODEX_HOME=/x"}, list: func(claude, codex string) (transcript.Listing, error) {
		homes = []string{claude, codex}
		return transcript.Listing{Files: files, Compressed: 4}, nil
	}}
	got, compressed, err := s.files(scanClaim)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != scanFiles || got[0].Path != "0" || got[len(got)-1].Path != "299" || compressed != 4 {
		t.Errorf("files = %d from %s to %s, %d compressed; want 300 from 0 to 299, 4 compressed", len(got), got[0].Path,
			got[len(got)-1].Path, compressed)
	}
	if !slices.Equal(homes, []string{"/h/.claude", "/x"}) {
		t.Errorf("homes = %q, want /h/.claude and /x", homes)
	}
	for _, c := range []struct {
		environ       []string
		claude, codex string
		wantErr       bool
	}{
		{[]string{"HOME=/h"}, "/h/.claude", "/h/.codex", false},
		{[]string{"HOME=/h", "CLAUDE_CONFIG_DIR=/c"}, "/c", "/h/.codex", false},
		{[]string{"CLAUDE_CONFIG_DIR=/c", "CODEX_HOME=/x"}, "/c", "/x", false},
		{nil, "", "", true},
	} {
		claude, codex, err := cliHomes(c.environ)
		if claude != c.claude || codex != c.codex || (err != nil) != c.wantErr {
			t.Errorf("cliHomes(%q) = %q, %q, %v; want %q, %q, error %v", c.environ, claude, codex, err, c.claude, c.codex, c.wantErr)
		}
	}
}

// AC 3, AC 4: the scan skips and counts, by reason, a codex exec session, a fork, a
// sub-agent session, a file with an unknown line, the compressed files, a session that capture
// knows, a shared checkout and a workspace with learning off. A session in one workspace goes to
// it, with the binding of that workspace's herdr session; one in no workspace goes to the global
// pool with the runner's binding. The scan refreshes its workspace map first.
func TestScanSkipsAndScope(t *testing.T) {
	f := newScanFix(t, bindings.KindMain)
	app := f.workspace("k1", "s2")
	shared := f.workspace("k2", "s1")
	f.workspaceAt("k3", "s2", shared)
	off := f.workspace("k4", "s1")
	f.write(func(ctx context.Context, tx *sql.Tx) error {
		if err := store.SetLearning(ctx, tx, store.LearningSetting{Scope: store.LearningWorkspace, Target: "k4", Changed: scanClaim}); err != nil {
			return err
		}
		_, err := store.OpenSession(ctx, tx, store.SessionKey{HerdrSession: "s1", Harness: "claude", Kind: store.KeyAgent,
			ID: "known"}, "w1:p1", "", scanClaim)
		return err
	})
	prompt := func(id, cwd string) []string {
		return []string{cUser(id+"-u", cwd, 1, "a prompt"), cAssistant(id+"-a", id+"-m", 2, 10, cText("a reply"))}
	}
	f.claude("inapp", time.Hour, prompt("inapp", filepath.Join(app, "sub"))...)
	f.claude("global", time.Hour, prompt("global", "/nowhere")...)
	f.claude("known", time.Hour, prompt("known", "/nowhere")...)
	f.claude("shared", time.Hour, prompt("shared", shared)...)
	f.claude("off", time.Hour, prompt("off", off)...)
	f.codex("exec", time.Hour, xMeta("exec", "/nowhere", "exec", nil), xUser(1, "a prompt"))
	f.codex("fork", time.Hour, xMeta("fork", "/nowhere", "cli", obj{"forked_from_id": "parent"}), xUser(1, "a prompt"))
	f.codex("sub", time.Hour, xMeta("sub", "/nowhere", "cli", obj{"parent_thread_id": "parent"}), xUser(1, "a prompt"))
	f.codex("odd", time.Hour, xMeta("odd", "/nowhere", "cli", nil), jsonLine(obj{"timestamp": at(1), "type": "event_msg", "payload": obj{}}))
	f.codex("read", time.Hour, xMeta("read", "/nowhere", "vscode", nil), xUser(1, "a prompt"))
	f.file(filepath.Join(".codex", "sessions", "2026", "10", "12", "rollout-z.jsonl.zst"), time.Hour)
	f.run()
	sc := f.row()
	want := map[string]store.ScanCounts{
		"claude": {Found: 5, Read: 2, Skipped: map[store.SkipReason]int{store.SkipKnown: 1, store.SkipSharedCheckout: 1,
			store.SkipLearningOff: 1}},
		"codex": {Found: 5, Read: 1, Skipped: map[store.SkipReason]int{store.SkipNotInteractive: 2, store.SkipFork: 1,
			store.SkipUnknownFormat: 1, store.SkipCompressed: 1}},
	}
	if fmt.Sprint(sc.Counts) != fmt.Sprint(want) {
		t.Errorf("counts = %v,\nwant %v", sc.Counts, want)
	}
	if f.syncs != 1 {
		t.Errorf("workspace map refreshes = %d, want 1", f.syncs)
	}
	f.write(func(ctx context.Context, tx *sql.Tx) error {
		ss, err := store.Sessions(ctx, tx)
		var rows []string
		for _, s := range ss {
			rows = append(rows, fmt.Sprintf("%s %s %s %q", s.Key.HerdrSession, s.Key.Harness, s.Key.ID, s.WorkspaceKey))
		}
		slices.Sort(rows)
		want := []string{`s1 claude global ""`, `s1 claude known ""`, `s1 codex read ""`, `s2 claude inapp "k1"`}
		if !slices.Equal(rows, want) {
			t.Errorf("sessions = %q,\nwant %q", rows, want)
		}
		return err
	})
}

// A Claude session with the entrypoint sdk-cli (claude -p, the Agent SDK) is not interactive
// and the scan skips it; one with cli is read.
func TestScanClaudeEntrypoint(t *testing.T) {
	f := newScanFix(t, bindings.KindMain)
	f.claude("sdk", time.Hour, cUser("sdk-u", "/nowhere", 1, "a prompt", obj{"entrypoint": "sdk-cli"}))
	f.claude("term", time.Hour, cUser("term-u", "/nowhere", 1, "a prompt", obj{"entrypoint": "cli"}))
	f.run()
	want := map[string]store.ScanCounts{
		"claude": {Found: 2, Read: 1, Skipped: map[store.SkipReason]int{store.SkipNotInteractive: 1}},
	}
	if got := f.row().Counts; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("counts = %v,\nwant %v", got, want)
	}
}

// AC 1: a stop between two files and a file that grew after the first read: the row in
// reading of the binding goes back to the start at the next run, with its first claim time.
// The sessions that the scan imported are read again and add nothing, and each occurrence and
// turn counts once. A session that the store knows from capture or hook records is skipped.
func TestScanRestart(t *testing.T) {
	f := newScanFix(t, bindings.KindMain)
	first := []string{cUser("u1", "/nowhere", 1, "use pnpm"), cAssistant("a1", "m1", 2, 100, cBash("t1", "pnpm i"), cBash("t2", "pnpm test"))}
	more := []string{cUser("u2", "/nowhere", 3, "use pnpm"), cAssistant("a2", "m2", 4, 200, cText("ok"))}
	p := f.claude("c1", time.Hour, first...)
	f.codex("x1", 2*time.Hour, xMeta("x1", "/nowhere", "cli", nil), xUser(1, "run the tests"), xTokens(2, 50))
	read := f.s.read
	reads := 0
	f.s.read = func(file transcript.File, limit int64) (transcript.Session, error) {
		reads++
		if reads == 2 {
			return transcript.Session{}, errors.New("brained stopped")
		}
		return read(file, limit)
	}
	if done, err := f.s.run(context.Background(), f.g); done || err == nil {
		t.Fatalf("run = %v, %v; want not done with the stop", done, err)
	}
	if sc := f.row(); sc.Status != store.ScanReading || sc.Binding != "s1" || !sc.Claimed.Equal(scanClaim) {
		t.Fatalf("row after the stop = %+v, want reading by s1", sc)
	}
	// The session grew; a new brained of the same binding claims the row at its start.
	grow(t, p, more...)
	f.s.now = func() time.Time { return scanClaim.Add(time.Hour) } // the claim time stays the first
	if !f.run() {
		t.Fatal("second run = not done, want done")
	}
	obs, cmds := f.observations()
	var ids []string
	for _, o := range obs {
		ids = append(ids, o.CaptureIdentity)
	}
	slices.Sort(ids)
	wantIDs := []string{"claude:a1#0", "claude:a1#1", "claude:a2", "claude:u1", "claude:u2", "codex:x1@" + offsetOf(t, f, 1)}
	if !slices.Equal(ids, wantIDs) || len(cmds) != 2 {
		t.Errorf("observations = %q, %d command records; want %q, 2", ids, len(cmds), wantIDs)
	}
	r := f.report()
	if r.All != (store.TurnTokens{Tokens: 350, Known: 3}) {
		t.Errorf("turn tokens = %+v, want 350 over 3 turns, each once", r.All)
	}
	if sc := f.row(); sc.Counts["claude"].Read != 1 || sc.Counts["codex"].Read != 1 {
		t.Errorf("counts = %v, want 1 session read of each CLI", sc.Counts)
	}
	// The row is done for this brained: another run reads nothing.
	reads = 0
	if !f.run() || reads != 0 {
		t.Errorf("third run read %d files, want none", reads)
	}
}

// grow adds lines to the session file at p, changed a minute before the claim time.
func grow(t *testing.T, p string, lines ...string) {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, append(b, []byte(strings.Join(lines, "\n")+"\n")...), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, scanClaim.Add(-time.Minute), scanClaim.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
}

// AC 6: a finding with 4 occurrences in 3 sessions, a turn with no usage field, and two
// signals in the same turn. The finding shows 3 sessions and the tokens of its distinct turns
// once each, with the turn with no usage apart; the repeats together count each turn once. A
// correction marker with no repeat stays below its gate. The row keeps ids only.
func TestScanFindings(t *testing.T) {
	f := newScanFix(t, bindings.KindMain)
	repeat := "use pnpm for installs please"
	fail := func(uuid, tool string, n int) []string {
		return []string{cAssistant(uuid, uuid+"-m", n, -1, cBash(tool, "pnpm install")),
			cUser(uuid+"-r", "/nowhere", n+1, []obj{cResult(tool, "Exit code 1\nERR_PNPM", true)})}
	}
	s1 := append([]string{cUser("s1-u1", "/nowhere", 1, repeat), cAssistant("s1-a1", "s1-m1", 2, 1000, cText("ok"))},
		fail("s1-f", "t1", 3)...)
	s1 = append(s1, cUser("s1-u2", "/nowhere", 10, repeat), cAssistant("s1-a2", "s1-m2", 11, -1, cText("ok")))
	s2 := append([]string{cUser("s2-u1", "/nowhere", 1, repeat), cAssistant("s2-a1", "s2-m1", 2, 2000, cText("ok"))},
		fail("s2-f", "t1", 3)...)
	s3 := []string{cUser("s3-u1", "/nowhere", 1, repeat), cAssistant("s3-a1", "s3-m1", 2, 500, cText("ok")),
		cUser("s3-u2", "/nowhere", 5, "don't touch the lock file"), cAssistant("s3-a2", "s3-m2", 6, 7, cText("ok"))}
	f.claude("s1", time.Hour, s1...)
	f.claude("s2", 2*time.Hour, s2...)
	f.claude("s3", 3*time.Hour, s3...)
	f.run()
	r := f.report()
	var got []string
	for _, rep := range r.Repeats {
		got = append(got, fmt.Sprintf("%d %s %d obs %d sessions %+v", rep.Finding.ID, rep.Finding.Kind, len(rep.Finding.Obs),
			rep.Sessions, rep.Turns))
	}
	want := []string{
		"1 prompt 4 obs 3 sessions {Tokens:3500 Known:3 Unknown:1}",
		"2 failure 2 obs 2 sessions {Tokens:3000 Known:2 Unknown:0}",
	}
	if !slices.Equal(got, want) {
		t.Errorf("repeats =\n%q\nwant\n%q", got, want)
	}
	if r.InRepeats != (store.TurnTokens{Tokens: 3500, Known: 3, Unknown: 1}) {
		t.Errorf("tokens in repeats = %+v, want each turn once: 3500 over 3 turns, 1 with no usage", r.InRepeats)
	}
	if len(f.notes) != 1 || f.notes[0] != "2 repeats, extractor false" {
		t.Errorf("notifications = %q, want one with the report", f.notes)
	}
}

// A repeated prompt needs at least 3 words. A 2-word prompt in 3 sessions is no finding; a
// 3-word prompt in 2 sessions is one. The match uses word sets, so "run tests" joins "run run
// tests": each prompt counts with its own words, in either order.
func TestScanPromptWords(t *testing.T) {
	for name, c := range map[string]struct {
		prompts [][]string // the prompts of each session, newest session first
		want    []string
	}{
		"floor": {[][]string{{"pls continue", "run the tests"}, {"pls continue", "run the tests"}, {"pls continue"}},
			[]string{`prompt "run the tests" 2 sessions`}},
		"short first": {[][]string{{"run run tests"}, {"run run tests"}, {"run tests"}},
			[]string{`prompt "run run tests" 2 sessions`}},
		"short last": {[][]string{{"run tests"}, {"run run tests"}, {"run run tests"}},
			[]string{`prompt "run run tests" 2 sessions`}},
		"one long": {[][]string{{"run run tests"}, {"run tests"}}, nil},
	} {
		t.Run(name, func(t *testing.T) {
			f := newScanFix(t, bindings.KindMain)
			for i, ps := range c.prompts {
				id := fmt.Sprintf("s%d", i+1)
				var lines []string
				for j, p := range ps {
					u := fmt.Sprintf("%s-%d", id, j)
					lines = append(lines, cUser(u+"u", "/nowhere", 2*j+1, p), cAssistant(u+"a", u+"m", 2*j+2, 10, cText("ok")))
				}
				f.claude(id, time.Duration(i+1)*time.Hour, lines...)
			}
			f.run()
			var got []string
			for _, rep := range f.report().Repeats {
				got = append(got, fmt.Sprintf("%s %q %d sessions", rep.Finding.Kind, rep.Excerpt, rep.Sessions))
			}
			if !slices.Equal(got, c.want) {
				t.Errorf("repeats = %q, want %q", got, c.want)
			}
		})
	}
}

// AC 5: a session that brain forget --session removed stays out: its capture identities
// are in the forget tombstones, so a second read stores nothing for it. A forget takes its
// occurrences out of the report, and a finding below its gate leaves it.
func TestScanForget(t *testing.T) {
	f := newScanFix(t, bindings.KindMain)
	f.claude("c1", time.Hour, cUser("u1", "/nowhere", 1, "run the full suite"), cAssistant("a1", "m1", 2, 10, cText("ok")))
	f.claude("c2", 2*time.Hour, cUser("u2", "/nowhere", 1, "run the full suite"), cAssistant("a2", "m2", 2, 20, cText("ok")))
	f.run()
	if n := len(f.report().Repeats); n != 1 {
		t.Fatalf("repeats = %d, want 1", n)
	}
	f.write(func(ctx context.Context, tx *sql.Tx) error {
		ss, err := store.SessionsByID(ctx, tx, "s1", "c1")
		if err != nil || len(ss) != 1 {
			return fmt.Errorf("sessions of c1 = %v, %w", ss, err)
		}
		return store.ForgetSession(ctx, tx, ss[0].ID)
	})
	if n := len(f.report().Repeats); n != 0 {
		t.Errorf("repeats after the forget = %d, want 0: the finding is below its gate", n)
	}
	// A new scan row reads the files again: the forgotten session stores nothing.
	f.write(func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE history_scans SET status = 'pending', binding = NULL`)
		return err
	})
	f.run()
	obs, _ := f.observations()
	for _, o := range obs {
		if strings.HasSuffix(o.CaptureIdentity, ":u1") || strings.HasSuffix(o.CaptureIdentity, ":a1") {
			t.Errorf("observation %s of the forgotten session is back", o.CaptureIdentity)
		}
	}
}

// AC 10: a store with learning off waits, and a test binding never scans. With nothing to
// claim, the row waits for setup.
func TestScanWaits(t *testing.T) {
	f := newScanFix(t, bindings.KindMain)
	f.write(func(ctx context.Context, tx *sql.Tx) error {
		return store.SetLearning(ctx, tx, store.LearningSetting{Scope: store.LearningStore, Changed: scanClaim})
	})
	if f.run() || f.row().Status != store.ScanPending {
		t.Errorf("learning off: run done or row %s, want not done and pending", f.row().Status)
	}
	tf := newScanFix(t, bindings.KindTest)
	tf.claude("c1", time.Hour, cUser("u1", "/nowhere", 1, "a prompt"))
	if !tf.run() || tf.row().Status != store.ScanPending || tf.syncs != 0 {
		t.Errorf("test binding: row %s after %d refreshes, want pending, never read", tf.row().Status, tf.syncs)
	}
	nf := newScanFix(t, bindings.KindMain)
	nf.write(func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `DELETE FROM history_scans`)
		return err
	})
	if nf.run() {
		t.Error("no row: run done, want not done: setup can still write the row")
	}
}

// AC 1, AC 8: the brained of the main store claims the row at its tick and runs the scan
// beside the loop: events still reach the handler while a slow reader reads. The read ends
// with one notification, opens no pane and makes no model call.
func TestScanInBackground(t *testing.T) {
	h := newHarness(t, bindings.KindMain)
	f := newScanFix(t, bindings.KindMain)
	f.g = NewGate(bindings.Binding{Session: "s1", Store: h.store, Kind: bindings.KindMain})
	f.write(func(ctx context.Context, tx *sql.Tx) error { return store.RequestScan(ctx, tx, scanClaim) })
	f.claude("c1", time.Hour, cUser("u1", "/nowhere", 1, "a prompt"))
	reading, release := make(chan struct{}), make(chan struct{})
	read := f.s.read
	f.s.read = func(file transcript.File, limit int64) (transcript.Session, error) {
		close(reading)
		<-release
		return read(file, limit)
	}
	ends := make(chan bool, 4)
	h.d.scan = func(ctx context.Context, g Gate) (bool, error) {
		done, err := f.s.run(ctx, g)
		ends <- done
		return done, err
	}
	h.f.addPane("w1:p1", "claude")
	main := h.start(t)
	st := h.f.statusSubs(t, 1)["w1:p1"]
	wantStep(t, h.step(t), Baseline, "w1:p1", GapStart, "")
	h.check(t)
	select {
	case <-reading:
	case <-time.After(wait):
		t.Fatal("the scan did not start at the tick")
	}
	st.send(statusLine("w1:p1", "idle"))
	wantStep(t, h.step(t), Change, "w1:p1", "", "")
	main.send(paneLine("pane_closed", "w1:p1"))
	wantStep(t, h.step(t), End, "w1:p1", "", ReasonGone)
	close(release)
	select {
	case done := <-ends:
		if !done {
			t.Error("scan = not done, want done")
		}
	case <-time.After(wait):
		t.Fatal("the scan did not end")
	}
	h.check(t)
	h.check(t) // a done scan does not start again
	select {
	case <-ends:
		t.Error("the scan ran again")
	default:
	}
	f.noPaneNoCall(t, h)
}

// noPaneNoCall fails unless the scan showed one notification, asked herdr for no pane and made
// no extractor batch, so no model call.
func (f *scanFix) noPaneNoCall(t *testing.T, h *harness) {
	t.Helper()
	if len(f.notes) != 1 {
		t.Errorf("notifications = %q, want one", f.notes)
	}
	h.f.mu.Lock()
	calls := slices.Clone(h.f.calls)
	h.f.mu.Unlock()
	for _, c := range calls {
		if strings.HasPrefix(c, "plugin.pane") {
			t.Errorf("herdr call %s: the scan opens no pane", c)
		}
	}
	f.write(func(ctx context.Context, tx *sql.Tx) error {
		var batches int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM extraction_batches`).Scan(&batches); err != nil {
			return err
		}
		if batches != 0 {
			t.Errorf("extraction batches = %d, want 0: the scan makes no model call", batches)
		}
		return nil
	})
}

// AC 6: the kind of each finding: a repeated text with a correction marker is a correction, a
// repeated lesson in plain words is one lesson, other repeated text is a repeated prompt, a
// lesson counts from its first telling, a procedure joins the procedures that it is like, and
// a command chain and a failure join by their signature. A procedure below its gate
// and a chain in fewer than 3 sessions are no findings.
func TestScanKinds(t *testing.T) {
	f := newScanFix(t, bindings.KindMain)
	procs := []string{
		"First pull the main branch, then run the unit tests, then build the docker image, then push it to the staging registry and tell me.",
		"First pull the main branch, then run the unit tests, then build the release image, then push it to the staging registry and report.",
		"First pull the main branch, then run all unit tests, then build the docker image, then deploy it to the staging cluster and tell me.",
	}
	for i := range 3 {
		id := fmt.Sprintf("c%d", i)
		u := func(n int, text string) string { return cUser(fmt.Sprintf("%s-u%d", id, n), "/nowhere", n, text) }
		lines := []string{
			u(1, "don't use npm in this repo"),
			cAssistant(id+"-a1", id+"-m1", 2, 10, cBash(id+"-t1", "go vet ./..."), cBash(id+"-t2", "go test ./...")),
			u(3, "remember to rebase on main first"),
			cAssistant(id+"-a3", id+"-m3", 4, 10, cText("ok")),
			u(5, procs[i]),
			cAssistant(id+"-a5", id+"-m5", 6, 10, cText("ok")),
		}
		if i == 0 {
			lines = append(lines, u(7, "from now on write tests first"), cAssistant(id+"-a7", id+"-m7", 8, 10, cText("ok")))
		}
		f.claude(id, time.Duration(i+1)*time.Hour, lines...)
	}
	f.run()
	var got []string
	for _, rep := range f.report().Repeats {
		got = append(got, fmt.Sprintf("%s %d", rep.Finding.Kind, len(rep.Finding.Obs)))
	}
	slices.Sort(got)
	want := []string{"command_chain 6", "correction 3", "lesson 1", "lesson 3", "procedure 3"}
	if !slices.Equal(got, want) {
		t.Errorf("findings = %q,\nwant %q", got, want)
	}
}

// When no Codex file parses, the scan reads no Codex session, and Claude continues.
func TestScanNoCodexParses(t *testing.T) {
	f := newScanFix(t, bindings.KindMain)
	f.codex("x1", time.Hour, xMeta("x1", "/nowhere", "cli", nil), jsonLine(obj{"timestamp": at(1), "type": "event_msg", "payload": obj{}}))
	f.codex("x2", time.Hour, jsonLine(obj{"timestamp": at(1), "type": "response_item", "payload": obj{}}))
	f.claude("c1", time.Hour, cUser("u1", "/nowhere", 1, "a prompt"))
	f.run()
	want := map[string]store.ScanCounts{
		"claude": {Found: 1, Read: 1},
		"codex":  {Found: 2, Skipped: map[store.SkipReason]int{store.SkipUnknownFormat: 2}},
	}
	if sc := f.row(); fmt.Sprint(sc.Counts) != fmt.Sprint(want) || sc.Status != store.ScanExtracting {
		t.Errorf("row = %s %v, want extracting %v", sc.Status, sc.Counts, want)
	}
}

// Learning off for the store during the read stops the read; the row stays reading,
// with no session skipped for it, and the read starts again when learning is on (AC 10).
func TestScanPausesWhenLearningOff(t *testing.T) {
	f := newScanFix(t, bindings.KindMain)
	f.claude("c1", time.Hour, cUser("u1", "/nowhere", 1, "a prompt"))
	f.claude("c2", 2*time.Hour, cUser("u2", "/nowhere", 1, "a prompt"))
	read := f.s.read
	f.s.read = func(file transcript.File, limit int64) (transcript.Session, error) {
		f.write(func(ctx context.Context, tx *sql.Tx) error {
			return store.SetLearning(ctx, tx, store.LearningSetting{Scope: store.LearningStore, Changed: time.Now()})
		})
		return read(file, limit)
	}
	if f.run() {
		t.Fatal("run with learning off = done, want a wait")
	}
	if sc := f.row(); sc.Status != store.ScanReading || len(sc.Counts["claude"].Skipped) != 0 {
		t.Errorf("row = %s %v, want reading with no skip", sc.Status, sc.Counts)
	}
	f.s.read = read
	f.write(func(ctx context.Context, tx *sql.Tx) error {
		return store.SetLearning(ctx, tx, store.LearningSetting{Scope: store.LearningStore, On: true, Changed: time.Now()})
	})
	if !f.run() || f.row().Counts["claude"].Read != 2 {
		t.Errorf("run with learning on: row %+v, want 2 sessions read", f.row())
	}
}

// A failed refresh of the workspace map stops the scan before it reads, so no session
// goes to a scope from an old map (AC 4). A workspace that appears during the read applies.
func TestScanWorkspaceMap(t *testing.T) {
	f := newScanFix(t, bindings.KindMain)
	f.claude("c1", time.Hour, cUser("u1", "/nowhere", 1, "a prompt"))
	f.s.sync = func(context.Context, Gate, *store.Store) error { return errors.New("herdr is down") }
	if done, err := f.s.run(context.Background(), f.g); done || err == nil {
		t.Fatalf("run = %v, %v; want not done with the refresh error", done, err)
	}
	if obs, _ := f.observations(); len(obs) != 0 {
		t.Errorf("observations = %d, want none before the refresh works", len(obs))
	}
	f.s.sync = func(context.Context, Gate, *store.Store) error { return nil }
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f.claude("c1", time.Hour, cUser("u1", dir, 1, "a prompt"))
	read := f.s.read
	f.s.read = func(file transcript.File, limit int64) (transcript.Session, error) {
		f.workspaceAt("k1", "s1", dir) // mapped after the scan started
		return read(file, limit)
	}
	f.run()
	f.write(func(ctx context.Context, tx *sql.Tx) error {
		ss, err := store.Sessions(ctx, tx)
		if len(ss) != 1 || ss[0].WorkspaceKey != "k1" {
			t.Errorf("sessions = %+v, want one in workspace k1", ss)
		}
		return err
	})
}

// #406: the scan's real refresh of the workspace map runs `herdr workspace list` with a
// deadline. The herdr client refuses a call with none, and brained runs the scan with a context
// that has none.
func TestScanWorkspaceMapDeadline(t *testing.T) {
	f := newScanFix(t, bindings.KindMain)
	f.claude("c1", time.Hour, cUser("u1", "/nowhere", 1, "a prompt"))
	dir := t.TempDir()
	bin, called := filepath.Join(dir, "herdr"), filepath.Join(dir, "called")
	script := "#!/bin/sh\ntouch " + called + "\n" +
		`echo '{"id":"cli:workspace:list","result":{"type":"workspace_list","workspaces":[]}}'` + "\n"
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	f.s.sync = newHistoryScan([]string{"HOME=" + f.home, "PATH=/usr/bin:/bin", "HERDR_BIN_PATH=" + bin}, "", nil).sync
	if !f.run() {
		t.Error("scan = not done, want done")
	}
	if _, err := os.Stat(called); err != nil {
		t.Errorf("fake herdr: %v, want one workspace list call", err)
	}
	if obs, _ := f.observations(); len(obs) != 1 {
		t.Errorf("observations = %d, want 1", len(obs))
	}
}

// An imported session whose file changed after the claim time is out of the restart's
// pick, and its records still count in the findings; live records of a scanned session take the
// live path and are no finding input (AC 6). The end of brained stops the read between two
// files.
func TestScanRestartKeepsImported(t *testing.T) {
	f := newScanFix(t, bindings.KindMain)
	p := f.claude("c1", time.Hour, cUser("u1", "/nowhere", 1, "run the full suite"), cAssistant("a1", "m1", 2, 10, cText("ok")))
	f.claude("c2", 2*time.Hour, cUser("u2", "/nowhere", 1, "run the full suite"), cAssistant("a2", "m2", 2, 20, cText("ok")))
	f.claude("c3", 3*time.Hour, cUser("u3", "/nowhere", 1, "a prompt"))
	ctx, cancel := context.WithCancel(context.Background())
	read, n := f.s.read, 0
	f.s.read = func(file transcript.File, limit int64) (transcript.Session, error) {
		if n++; n == 3 {
			cancel() // brained stops after the first two files
		}
		return read(file, limit)
	}
	if done, err := f.s.run(ctx, f.g); done || !errors.Is(err, context.Canceled) {
		t.Fatalf("run = %v, %v; want a stop", done, err)
	}
	f.s.read = read
	after := scanClaim.Add(time.Hour)
	if err := os.Chtimes(p, after, after); err != nil {
		t.Fatal(err)
	}
	f.write(func(ctx context.Context, tx *sql.Tx) error {
		ss, err := store.SessionsByID(ctx, tx, "s1", "c2")
		if err != nil || len(ss) != 1 {
			return fmt.Errorf("sessions of c2 = %v, %w", ss, err)
		}
		segs, err := store.Segments(ctx, tx)
		for _, g := range segs {
			if g.SessionID == ss[0].ID {
				_, err = store.AddObservation(ctx, tx, store.Observation{CaptureIdentity: "live/1", Source: store.SourceHerdr,
					SegmentID: g.ID, Event: "idle", Excerpt: "run the full suite", Status: store.ObservationComplete,
					Sender: store.SenderUnconfirmed, Counts: true, At: scanClaim})
			}
		}
		return err
	})
	f.run()
	reps := f.report().Repeats
	if len(reps) != 1 || len(reps[0].Finding.Obs) != 2 || reps[0].Turns != (store.TurnTokens{Tokens: 30, Known: 2}) {
		t.Fatalf("repeats = %+v, want one of the 2 scanned prompts, 30 tokens", reps)
	}
	for _, o := range reps[0].Finding.Obs {
		if !strings.HasPrefix(o.Identity, "claude:") || o.Turn == 0 {
			t.Errorf("finding observation %+v, want a scanned record with its turn", o)
		}
	}
}

// A Claude file that repeats a prompt record gives one turn, and the usage of each
// assistant message counts once in it.
func TestScanRepeatedRecord(t *testing.T) {
	f := newScanFix(t, bindings.KindMain)
	u := cUser("u1", "/nowhere", 1, "a prompt")
	f.claude("c1", time.Hour, u, cAssistant("a1", "m1", 2, 100, cText("ok")), u, cAssistant("a2", "m2", 3, 50, cText("done")))
	f.run()
	if r := f.report(); r.All != (store.TurnTokens{Tokens: 150, Known: 1}) {
		t.Errorf("turn tokens = %+v, want one turn of 150 tokens", r.All)
	}
}

// The done mark of the scan belongs to the binding: a binding that changes (here a
// test binding that becomes main) gets a scan run again.
func TestScanDoneFollowsBinding(t *testing.T) {
	h := newHarness(t, bindings.KindTest)
	runs := make(chan Gate, 4)
	h.d.scan = func(_ context.Context, g Gate) (bool, error) {
		runs <- g
		return true, nil
	}
	h.start(t)
	h.check(t)
	wantRun := func(kind bindings.Kind) {
		t.Helper()
		select {
		case g := <-runs:
			if g.Binding.Kind != kind {
				t.Errorf("scan run of a %s binding, want %s", g.Binding.Kind, kind)
			}
		case <-time.After(wait):
			t.Fatalf("no scan run of the %s binding", kind)
		}
	}
	wantRun(bindings.KindTest)
	h.check(t)
	h.check(t)
	b := bindings.Binding{Session: "s1", Socket: h.f.path, Store: h.store, Kind: bindings.KindMain, Created: time.Now()}
	if err := bindings.Write(h.d.bindingsFile, []bindings.Binding{b}); err != nil {
		t.Fatal(err)
	}
	h.check(t)
	wantRun(bindings.KindMain)
	h.check(t)
	select {
	case <-runs:
		t.Error("a done scan ran again for the same binding")
	default:
	}
}
