package extract

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

// AC #428: the environment of a call is the environment of brained, with no
// variable of the denylist: HERDR_*, BRAIN_* and the session markers of Claude Code and codex.
// Sign-in, provider, proxy and any other variables stay, and LANG stays as it is.
func TestCleanEnv(t *testing.T) {
	kept := []string{"HOME=/h", "PATH=/bin", "USER=u", "LANG=fr_FR.UTF-8", "ANTHROPIC_API_KEY=k", "ANTHROPIC_AUTH_TOKEN=t",
		"ANTHROPIC_BASE_URL=http://127.0.0.1:8317", "CLAUDE_CODE_OAUTH_TOKEN=o", "CLAUDE_CODE_USE_BEDROCK=1",
		"CLAUDE_CODE_USE_VERTEX=1", "CLAUDE_CONFIG_DIR=/h/.claude-work", "AWS_REGION=us-east-1", "OPENAI_API_KEY=k",
		"CODEX_HOME=/h/.codex", "CLIPROXY_KEY=c", "HTTPS_PROXY=http://proxy:3128", "no_proxy=localhost",
		"SSL_CERT_FILE=/ca.pem", "RANDOM_VAR=x", "EMPTY="}
	dropped := []string{"HERDR_SESSION=default", "HERDR_PANE_ID=w1:p1", "HERDR_SOCKET_PATH=/s", "BRAIN_CALL_ORIGIN=x",
		"BRAIN_PROPOSAL_ID=1", "CLAUDECODE=1", "CLAUDE_CODE_ENTRYPOINT=cli", "CLAUDE_CODE_SSE_PORT=1",
		"CLAUDE_CODE_SESSION_ID=s", "CLAUDE_CODE_MESSAGING_SOCKET=/m", "CLAUDE_CODE_MESSAGING_TOKEN=m", "CLAUDE_PID=1",
		"CODEX_SANDBOX=seatbelt", "CODEX_SANDBOX_NETWORK_DISABLED=1", "CODEX_THREAD_ID=t", "CODEX_TOOL_CALL_ID=c"}
	if got := cleanEnv(append(slices.Clone(kept), dropped...)); !slices.Equal(got, kept) {
		t.Errorf("cleanEnv() = %q, want %q", got, kept)
	}
	if got := cleanEnv([]string{"HOME=/h"}); !slices.Equal(got, []string{"HOME=/h", "LANG=en_US.UTF-8"}) {
		t.Errorf("cleanEnv(no LANG) = %q, want LANG=en_US.UTF-8 added", got)
	}
}

// AC #428: the denylist names no sign-in or provider variable of a CLI.
func TestDenyList(t *testing.T) {
	for _, name := range []string{"CLAUDE_CODE_OAUTH_TOKEN", "CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX",
		"CLAUDE_CONFIG_DIR", "CODEX_HOME", "CODEX_API_KEY", "CODEX_ACCESS_TOKEN", "CODEX_CA_CERTIFICATE"} {
		if slices.Contains(denyNames, name) {
			t.Errorf("the denylist has %s", name)
		}
	}
}

// AC #428: the error of a failed call says why the CLI failed: the error result of claude, the
// error event of codex, else the end of its standard error. The text goes through the secret
// filter and the token filter, so it has no token, and is cut short. A sign-in failure
// that ran no model is an ErrSignIn error, with a stable reason form; a limit stays a limit.
func TestFailureReason(t *testing.T) {
	long := strings.Repeat("word ", 100)
	for _, c := range []struct {
		name        string
		cli         store.ExtractorCLI
		out, stderr string
		want        string
		signIn      bool
	}{
		{"claude oauth expired", store.ExtractorClaude,
			`{"type":"result","is_error":true,"result":"Failed to authenticate: OAuth session expired and could not be refreshed"}`,
			"", "claude: Failed to authenticate: OAuth session expired and could not be refreshed: sign-in failed", true},
		{"claude invalid key", store.ExtractorClaude, `{"is_error":true,"result":"Invalid API key · Please run /login"}`, "",
			"claude: Invalid API key · Please run /login: sign-in failed", true},
		{"codex 401 events", store.ExtractorCodex, `{"type":"turn.started"}` + "\n" +
			`{"type":"error","message":"unexpected status 401 Unauthorized: Missing bearer authentication"}` + "\n" +
			`{"type":"turn.failed","error":{"message":"unexpected status 401 Unauthorized: Missing bearer authentication"}}`, "",
			"codex: unexpected status 401 Unauthorized: Missing bearer authentication: sign-in failed", true},
		{"codex stderr", store.ExtractorCodex, "", "WARNING: x\nERROR: Your access token could not be refreshed. Please sign in again.\n" +
			"note: see logs\n", "codex: Your access token could not be refreshed. Please sign in again.: sign-in failed", true},
		{"token in stderr", store.ExtractorClaude, "", "debug: sent Bearer proxy-tok-0123456789\nerror: 401 for proxy-tok-0123456789\n",
			"claude: error: 401 for [hidden]: sign-in failed", true},
		{"header values", store.ExtractorClaude, `{"is_error":true,"result":"Unauthorized: bad header hdr-secret-4242 for blue-team-77"}`,
			"", "claude: Unauthorized: bad header [hidden] for [hidden]: sign-in failed", true},
		{"proxy password", store.ExtractorCodex, "", "ERROR: proxy http://bob:pw-5150@proxy:3128 refused\n",
			"sh: exit status 1: proxy [hidden] refused", false},
		{"pattern secret", store.ExtractorClaude, `{"is_error":true,"result":"Invalid API key sk-ant-abcdefghijklmnopqrstuvwxyz"}`,
			"", "claude: Invalid API key [secret]: sign-in failed", true},
		{"other failure", store.ExtractorClaude, `{"is_error":true,"result":"API Error: Request timed out"}`, "",
			"sh: exit status 1: API Error: Request timed out", false},
		{"no text", store.ExtractorClaude, "", "", "sh: exit status 1: the CLI gave no error text", false},
		{"long text", store.ExtractorClaude, "", long, "sh: exit status 1: " + strings.TrimSpace(long)[:maxCause] + "...", false},
		{"model ran", store.ExtractorCodex, `{"type":"item.completed","item":{"type":"reasoning"}}` + "\n" +
			`{"type":"turn.failed","error":{"message":"stream error: 401 Unauthorized"}}`, "",
			"sh: exit status 1: stream error: 401 Unauthorized", false},
		{"limit", store.ExtractorClaude, claudeWeekly, "", "claude is at a usage or rate limit", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := runError(c.cli, []byte(c.out), exit("sh", c.stderr))
			if err.Error() != c.want || errors.Is(err, ErrSignIn) != c.signIn {
				t.Errorf("runError() = %q (sign-in %v), want %q (sign-in %v)", err, errors.Is(err, ErrSignIn), c.want, c.signIn)
			}
			for _, v := range []string{"proxy-tok", "hdr-secret", "blue-team", "pw-5150", "plainkey", "sk-ant-"} {
				if strings.Contains(err.Error(), v) {
					t.Errorf("runError() = %q, holds %q", err, v)
				}
			}
		})
	}
}

// AC #428: an error result of claude with exit status 0, and a failed codex turn, also say why.
func TestResultFailure(t *testing.T) {
	_, err := claudeResult([]byte(`{"is_error":true,"result":"Not logged in · Please run /login"}`))
	if want := "claude: Not logged in · Please run /login: sign-in failed"; err == nil || err.Error() != want ||
		!errors.Is(err, ErrSignIn) {
		t.Errorf("claudeResult() = %v, want %q", err, want)
	}
	_, err = codexResult([]byte(`{"type":"turn.failed","error":{"message":"model gpt-x not found"}}`), "")
	if want := "codex reported a failed turn: model gpt-x not found"; err == nil || err.Error() != want {
		t.Errorf("codexResult() = %v, want %q", err, want)
	}
}

// AC #428: status and doctor give a short hint for a sign-in refusal, for the chosen CLI.
func TestSignInHint(t *testing.T) {
	for cli, want := range map[store.ExtractorCLI]string{
		store.ExtractorClaude: "sign in again: run claude, then /login, or set ANTHROPIC_API_KEY in the herdr server env",
		store.ExtractorCodex:  "sign in again: run codex login, or set OPENAI_API_KEY in the herdr server env",
	} {
		if got := SignInHint(cli); got != want {
			t.Errorf("SignInHint(%s) = %q, want %q", cli, got, want)
		}
	}
}

// AC #428: while the sign-in of the CLI is broken, each call of a batch fails and the batch is
// tried again each retryAfter, but no call counts toward the daily cap or uses a try. When the
// sign-in works again, the batch commits with one counted call.
func TestSignInRetry(t *testing.T) {
	expired := func(context.Context, string) ([]byte, error) {
		return []byte(`{"is_error":true,"result":"Failed to authenticate: OAuth session expired and could not be refreshed"}`),
			exit("claude", "")
	}
	f := newFixture(t)
	f.session("s1", true, line{"ws-A", pnpm, t0})
	f.cli.reply = expired
	for i := range 2 * maxTries {
		if i > 0 {
			f.now = f.now.Add(retryAfter)
		}
		f.mustRun()
		f.mustRun() // within retryAfter: no call
	}
	if n := f.cli.calls(); n != 2*maxTries {
		t.Errorf("CLI runs = %d, want %d: one try each retryAfter", n, 2*maxTries)
	}
	b := f.batches()[0]
	if b.Status != store.BatchFailed || b.ModelCalls != 0 || spent(b) || b.Refusal != store.RefusalSignIn {
		t.Fatalf("batch while signed out = %+v, want failed with no counted call, its tries and a sign-in reason", b)
	}
	if d := f.day(); d.Calls != 0 {
		t.Errorf("calls of the day = %d, want 0", d.Calls)
	}
	f.now = f.now.Add(retryAfter)
	f.cli.reply = claimOf("use pnpm")
	f.mustRun()
	if b := f.batches()[0]; b.Status != store.BatchCommitted || b.ModelCalls != 1 {
		t.Errorf("batch after the sign-in = %+v, want committed after 1 counted call", b)
	}
}
