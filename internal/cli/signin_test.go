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

// fakeSignedOut is a fake extractor CLI that answers --version, lists no MCP server, and for a
// model call writes its environment to the file env, prints out and stderr as a CLI with a
// broken sign-in does, and exits with status 1. It uses only shell builtins: PATH holds only
// the bin folder of the fixture.
func fakeSignedOut(version, env, out, stderr string) string {
	return "#!/bin/sh\ncase \"$*\" in\n--version) echo '" + version + "'; exit 0;;\n*'mcp list'*) echo '[]'; exit 0;;\nesac\n" +
		"export -p > '" + env + "'\nprintf '%s\\n' '" + out + "'\nprintf '%s\\n' '" + stderr + "' >&2\nexit 1\n"
}

// AC #428 end to end: a fake claude and a fake codex on PATH print their environment and fail
// with a sign-in error. The call passes the sign-in and routing variables of the chosen CLI,
// and no HERDR_*, other CLAUDE_CODE_* or other variable. brain status and brain doctor show
// why the call failed, with the sign-in hint of that CLI, and no token. The call does not
// count toward the daily cap. The CLI prints its tokens raw, URL-encoded and base64-encoded,
// a bearer token of its headers and the password of its base URL; none reaches the stored
// reason. codex also gets the API key variable of its provider in config.toml (Sol round 1).
func TestExtractorSignInEndToEnd(t *testing.T) {
	const token, cpKey = "proxy/token+alpha=42", "cp-key-31337"
	b64 := func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
	for _, c := range []struct {
		cli             store.ExtractorCLI
		version         string
		out, stderr     string
		passed, dropped []string
		reason, hint    string
	}{
		{store.ExtractorClaude, "2.1.283 (Claude Code)",
			`{"type":"result","is_error":true,"result":"Failed to authenticate: token ` + url.QueryEscape(token) + " " + b64(token) +
				` bearer bear-tok-77 at http://bu:bp-secret@127.0.0.1:9"}`,
			"debug: Authorization: Bearer bear-tok-77",
			[]string{"ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_BASE_URL", "ANTHROPIC_CUSTOM_HEADERS", "HTTPS_PROXY", "HOME", "PATH"},
			[]string{"OPENAI_API_KEY", "CODEX_HOME", "CLIPROXY_KEY"},
			"claude: Failed to authenticate: token [secret] [secret] bearer [secret] at [secret]: sign-in failed",
			"sign in again: run claude, then /login, or set ANTHROPIC_API_KEY in the herdr server env"},
		{store.ExtractorCodex, "codex-cli 0.154.0",
			`{"type":"turn.failed","error":{"message":"unexpected status 401 Unauthorized: key ` + cpKey + " " + b64(cpKey) + `"}}`,
			"ERROR: unexpected status 401 Unauthorized: key plainkey-0123456789",
			[]string{"OPENAI_API_KEY", "CODEX_HOME", "CLIPROXY_KEY", "HTTPS_PROXY", "HOME", "PATH"},
			[]string{"ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_BASE_URL", "ANTHROPIC_CUSTOM_HEADERS"},
			"codex: unexpected status 401 Unauthorized: key [secret] [secret]: sign-in failed",
			"sign in again: run codex login, or set OPENAI_API_KEY in the herdr server env"},
	} {
		t.Run(string(c.cli), func(t *testing.T) {
			f := newDoctorFixture(t)
			for k, v := range map[string]string{
				"ANTHROPIC_AUTH_TOKEN": token, "ANTHROPIC_BASE_URL": "http://bu:bp-secret@127.0.0.1:9",
				"ANTHROPIC_CUSTOM_HEADERS": "Authorization: Bearer bear-tok-77", "CLIPROXY_KEY": cpKey,
				"OPENAI_API_KEY": "plainkey-0123456789", "CODEX_HOME": f.codexHome, "HTTPS_PROXY": "http://proxy.invalid:3128",
				"HERDR_SESSION": "default", "HERDR_PANE_ID": "w14:p1", "CLAUDE_CODE_ENTRYPOINT": "cli", "CLAUDECODE": "1",
				"RANDOM_VAR": "x",
			} {
				t.Setenv(k, v)
			}
			config := filepath.Join(f.codexHome, "config.toml")
			trusted, err := os.ReadFile(config)
			if err != nil {
				t.Fatal(err)
			}
			provider := "model_provider = \"cliproxy\"\n" + string(trusted) + "\n[model_providers.cliproxy]\nenv_key = \"CLIPROXY_KEY\"\n"
			if err := os.WriteFile(config, []byte(provider), 0o600); err != nil {
				t.Fatal(err)
			}
			bin := filepath.Join(filepath.Dir(f.store), "bin")
			env := filepath.Join(t.TempDir(), "env")
			script := fakeSignedOut(c.version, env, c.out, c.stderr)
			if err := os.WriteFile(filepath.Join(bin, string(c.cli)), []byte(script), 0o700); err != nil { //nolint:gosec // a test CLI must be executable
				t.Fatal(err)
			}
			f.storeWrite(t, func(ctx context.Context, tx *sql.Tx) error {
				return store.SetExtractorCLI(ctx, tx, c.cli, "", testNow)
			})
			f.signalSession(t, "ws_a14", "a1")
			s := openStore(t, f.store)
			p := extract.Pass{Store: s, Runner: extract.ExecRunner{}, HerdrSession: "default", DefaultOn: true,
				Now: func() time.Time { return testNow.Add(150 * time.Minute) }}
			if err := p.Run(context.Background()); err != nil {
				t.Fatal(err)
			}
			closeStore(t, s)
			dump, err := os.ReadFile(env)
			if err != nil {
				t.Fatalf("the fake %s made no model call: %v", c.cli, err)
			}
			// The messages name a variable, never its value.
			has := func(name string) bool { return regexp.MustCompile(`(?m)\s` + name + `=`).Match(dump) }
			for _, name := range c.passed {
				if !has(name) {
					t.Errorf("the env of %s has no %s", c.cli, name)
				}
			}
			for _, name := range append(c.dropped, "HERDR_SESSION", "HERDR_PANE_ID", "CLAUDE_CODE_ENTRYPOINT", "CLAUDECODE", "RANDOM_VAR") {
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
		})
	}
}
