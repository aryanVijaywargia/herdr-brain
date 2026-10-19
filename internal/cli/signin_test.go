package cli

import (
	"context"
	"database/sql"
	"encoding/base64"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/aryanVijaywargia/herdr-brain/internal/extract"
	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

// fakeCLI is a fake extractor CLI that answers --version and lists no MCP server. For a model
// call it writes its environment to the file env, and then runs call. It uses only shell
// builtins: PATH holds only the bin folder of the fixture.
func fakeCLI(version, env, call string) string {
	return "#!/bin/sh\ncase \"$*\" in\n--version) echo '" + version + "'; exit 0;;\n*'mcp list'*) echo '[]'; exit 0;;\nesac\n" +
		"export -p > '" + env + "'\n" + call
}

// signedOut is the model call of a CLI with a broken sign-in: it prints out and stderr and exits
// with status 1.
func signedOut(out, stderr string) string {
	return "printf '%s\\n' '" + out + "'\nprintf '%s\\n' '" + stderr + "' >&2\nexit 1\n"
}

// signedIn is the model call of a CLI that works and finds no claim: claude prints its result,
// and codex writes its -o file in the folder of the call.
var signedIn = map[store.ExtractorCLI]string{
	store.ExtractorClaude: `printf '%s\n' '{"type":"result","is_error":false,"structured_output":{"claims":[]}}'` + "\n",
	store.ExtractorCodex:  `printf '%s\n' '{"claims":[]}' > last.json` + "\n" + `printf '%s\n' '{"type":"turn.completed"}'` + "\n",
}

// AC #428 end to end: a fake claude and a fake codex on PATH print their
// environment and fail with a sign-in error. The call inherits the env of brained: sign-in,
// proxy and any other variables pass, and HERDR_*, BRAIN_* and the session markers do not. brain
// status and brain doctor show why the call failed, with the sign-in hint of that CLI, and with
// the tokens that the CLI printed hidden, raw or encoded. The call does not count toward the
// daily cap. When the sign-in works, the batch commits at its retry and the hint goes.
func TestExtractorSignInEndToEnd(t *testing.T) {
	const token, cpKey = "proxy/token+alpha=42", "cp-key-31337"
	b64 := func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
	for _, c := range []struct {
		cli          store.ExtractorCLI
		version      string
		out, stderr  string
		reason, hint string
	}{
		{store.ExtractorClaude, "2.1.283 (Claude Code)",
			`{"type":"result","is_error":true,"result":"Failed to authenticate: token ` + url.QueryEscape(token) + " " + b64(token) +
				` bearer bear-tok-77 at http://bu:bp-secret@127.0.0.1:9"}`,
			"debug: Authorization: Bearer bear-tok-77",
			"claude: Failed to authenticate: token [hidden] [hidden] bearer [hidden] at [hidden]: sign-in failed",
			"sign in again: run claude, then /login, or set ANTHROPIC_API_KEY in the herdr server env"},
		{store.ExtractorCodex, "codex-cli 0.154.0",
			`{"type":"turn.failed","error":{"message":"unexpected status 401 Unauthorized: key ` + cpKey + " " + b64(cpKey) + `"}}`,
			"ERROR: unexpected status 401 Unauthorized: key plainkey-0123456789",
			"codex: unexpected status 401 Unauthorized: key [hidden] [hidden]: sign-in failed",
			"sign in again: run codex login, or set OPENAI_API_KEY in the herdr server env"},
	} {
		t.Run(string(c.cli), func(t *testing.T) {
			f := newDoctorFixture(t)
			passed := map[string]string{
				"ANTHROPIC_AUTH_TOKEN": token, "ANTHROPIC_BASE_URL": "http://bu:bp-secret@127.0.0.1:9",
				"ANTHROPIC_CUSTOM_HEADERS": "Authorization: Bearer bear-tok-77", "CLAUDE_CODE_OAUTH_TOKEN": "oauth-tok-5",
				"CLIPROXY_KEY": cpKey, "OPENAI_API_KEY": "plainkey-0123456789", "CODEX_HOME": f.codexHome,
				"HTTPS_PROXY": "http://proxy.invalid:3128", "RANDOM_VAR": "x",
			}
			dropped := map[string]string{"HERDR_SESSION": "default", "HERDR_PANE_ID": "w14:p1", "BRAIN_CALL_ORIGIN": "x",
				"CLAUDECODE": "1", "CLAUDE_CODE_ENTRYPOINT": "cli", "CODEX_SANDBOX": "seatbelt"}
			for _, m := range []map[string]string{passed, dropped} {
				for k, v := range m {
					t.Setenv(k, v)
				}
			}
			bin := filepath.Join(filepath.Dir(f.store), "bin", string(c.cli))
			env := filepath.Join(t.TempDir(), "env")
			writeCLI(t, bin, fakeCLI(c.version, env, signedOut(c.out, c.stderr)))
			f.storeWrite(t, func(ctx context.Context, tx *sql.Tx) error {
				return store.SetExtractorCLI(ctx, tx, c.cli, "", testNow)
			})
			f.signalSession(t, "ws_a14", "a1")
			f.pass(t, testNow.Add(150*time.Minute))
			dump, err := os.ReadFile(env)
			if err != nil {
				t.Fatalf("the fake %s made no model call: %v", c.cli, err)
			}
			// The messages name a variable, never its value.
			has := func(name string) bool { return regexp.MustCompile(`(?m)\s` + name + `=`).Match(dump) }
			for name := range passed {
				if !has(name) {
					t.Errorf("the env of %s has no %s", c.cli, name)
				}
			}
			for name := range dropped {
				if has(name) {
					t.Errorf("the env of %s has %s", c.cli, name)
				}
			}
			status := f.mustStatus(t)
			for _, want := range []string{"  last 24h: 0 of 40 extractor calls", "workspace ws_a14: failed, " + c.reason + ", 30m ago\n",
				"    fix: " + c.hint + "; Brain tries again later\n"} {
				if !strings.Contains(status, want) {
					t.Errorf("brain status:\n%s\nwant %q", status, want)
				}
			}
			out := f.failed(t, "", "extraction: the last extractor call for workspace ws_a14 failed ("+c.reason+
				"), so its signals wait; fix: "+c.hint+"; Brain tries again later")
			for i, secret := range []string{token, url.QueryEscape(token), b64(token), "bear-tok", "bp-secret", cpKey, b64(cpKey),
				"plainkey"} {
				if strings.Contains(status, secret) || strings.Contains(out, secret) {
					t.Errorf("status or doctor shows secret form %d", i)
				}
			}

			writeCLI(t, bin, fakeCLI(c.version, env, signedIn[c.cli]))
			f.pass(t, testNow.Add(210*time.Minute))
			status, err = f.statusAt(t, f.sock, testNow.Add(4*time.Hour))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(status, "  failed batches: none\n") || strings.Contains(status, "sign in again") {
				t.Errorf("brain status after the sign-in:\n%s\nwant no failed batch and no hint", status)
			}
		})
	}
}

// writeCLI writes the fake CLI script to path.
func writeCLI(t *testing.T, path, script string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil { //nolint:gosec // a test CLI must be executable
		t.Fatal(err)
	}
}

// pass runs one extraction pass of the fixture at now, with the real ExecRunner.
func (f *doctorFixture) pass(t *testing.T, now time.Time) {
	t.Helper()
	s := openStore(t, f.store)
	defer closeStore(t, s)
	p := extract.Pass{Store: s, Runner: extract.ExecRunner{}, HerdrSession: "default", DefaultOn: true,
		Now: func() time.Time { return now }}
	if err := p.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// AC #428 (Sol round 3): a sign-in refusal of a failed change request alone gives the sign-in
// hint in brain status, and brain doctor fails the check with it.
func TestChangeRequestSignInHint(t *testing.T) {
	f := newDoctorFixture(t)
	f.setClaude(t)
	reason := "claude: Not logged in · Please run /login: sign-in failed"
	f.storeWrite(t, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO change_requests (source, source_id, revision, workspace_key, status,
			model_calls, reason, refusal, created, at) VALUES ('proposal', 99, 1, 'ws_a14', 'failed', 1, ?, 'sign-in', ?, ?)`,
			reason, testNow.Format(time.RFC3339Nano), testNow.Format(time.RFC3339Nano))
		return err
	})
	hint := "sign in again: run claude, then /login, or set ANTHROPIC_API_KEY in the herdr server env"
	status := f.mustStatus(t)
	for _, want := range []string{"  failed batches: none\n", "  failed change requests: 1\n",
		"    request 1 of proposal 99, workspace ws_a14: failed, " + reason, "    fix: " + hint + "; Brain tries again later\n"} {
		if !strings.Contains(status, want) {
			t.Errorf("brain status:\n%s\nwant %q", status, want)
		}
	}
	f.failed(t, "", "extraction: the last extractor call for change request 1 failed ("+reason+"), so its draft waits; fix: "+
		hint+"; Brain tries again later")
}
