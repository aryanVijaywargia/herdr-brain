package extract

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

// AC #428: the environment of a call is HOME, PATH, USER and LANG, and each sign-in and routing
// variable of the chosen CLI that is set: the claude variables go only to claude and the codex
// variables only to codex. An empty variable, HERDR_*, other CLAUDE_CODE_* variables and any
// other variable stay out.
func TestCleanEnv(t *testing.T) {
	vars := map[string]string{
		"HOME": "/h", "PATH": "/bin", "USER": "u", "LANG": "fr_FR.UTF-8",
		"ANTHROPIC_AUTH_TOKEN": "a-tok", "ANTHROPIC_BASE_URL": "http://127.0.0.1:8317", "CLAUDE_CODE_OAUTH_TOKEN": "o-tok",
		"CLAUDE_CODE_USE_BEDROCK": "1", "AWS_REGION": "us-east-1",
		"OPENAI_API_KEY": "k", "CODEX_HOME": "/h/.codex",
		"HTTPS_PROXY": "http://proxy:3128", "no_proxy": "localhost", "SSL_CERT_FILE": "/ca.pem", "TMPDIR": "/t",
		"ANTHROPIC_API_KEY": "", "HERDR_SESSION": "default", "HERDR_ENV": "1", "CLAUDE_CODE_ENTRYPOINT": "cli", "CLAUDECODE": "1",
		"AWS_DEFAULT_REGION": "eu-west-1", "SECRET_THING": "s", "GOOGLE_API_KEY": "g",
	}
	getenv := func(name string) string { return vars[name] }
	base := []string{"HOME=/h", "PATH=/bin", "USER=u", "LANG=en_US.UTF-8", "TMPDIR=/t", "HTTPS_PROXY=http://proxy:3128",
		"no_proxy=localhost", "SSL_CERT_FILE=/ca.pem", "AWS_REGION=us-east-1"}
	for _, c := range []struct {
		cli  store.ExtractorCLI
		want []string
	}{
		{store.ExtractorClaude, append(slices.Clone(base), "ANTHROPIC_AUTH_TOKEN=a-tok", "CLAUDE_CODE_OAUTH_TOKEN=o-tok",
			"ANTHROPIC_BASE_URL=http://127.0.0.1:8317", "CLAUDE_CODE_USE_BEDROCK=1")},
		{store.ExtractorCodex, append(slices.Clone(base), "OPENAI_API_KEY=k", "CODEX_HOME=/h/.codex")},
	} {
		t.Run(string(c.cli), func(t *testing.T) {
			if got := cleanEnv(c.cli, getenv); !slices.Equal(got, c.want) {
				t.Errorf("cleanEnv(%s) = %q, want %q", c.cli, got, c.want)
			}
		})
	}
}

// AC #428: every variable of the allowlist has a name with no "=", is listed once, and no
// HERDR_* variable is on it. Of the CLAUDE_CODE_* variables, only the sign-in token and the
// provider switches are.
func TestCallEnvList(t *testing.T) {
	keep := []string{"CLAUDE_CODE_OAUTH_TOKEN", "CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX"}
	var seen []string
	for _, v := range callEnv {
		switch {
		case v.name == "" || strings.Contains(v.name, "="):
			t.Errorf("bad name %q", v.name)
		case slices.Contains(seen, v.name):
			t.Errorf("%s is listed twice", v.name)
		case strings.HasPrefix(v.name, "HERDR_"),
			strings.HasPrefix(v.name, "CLAUDE_CODE_") && !slices.Contains(keep, v.name):
			t.Errorf("%s is on the allowlist", v.name)
		}
		seen = append(seen, v.name)
	}
}

// signInEnv is the environment of a call with a sign-in token that no secret pattern matches,
// as a token of a local proxy can be, and custom headers.
var signInEnv = []string{"HOME=/h", "PATH=/bin", "ANTHROPIC_AUTH_TOKEN=proxy-tok-0123456789",
	"ANTHROPIC_CUSTOM_HEADERS=X-Team: blue-team-77\nX-Key: hdr-secret-4242", "OPENAI_API_KEY=plainkey123",
	"HTTPS_PROXY=http://bob:pw-5150@proxy:3128"}

// AC #428: the error of a failed call says why the CLI failed: the error result of claude, the
// error event of codex, else the end of its standard error. The text goes through the secret
// filter and has no value of a secret variable of the call, and is cut short. A sign-in failure
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
			"claude: error: 401 for [secret]: sign-in failed", true},
		{"header values", store.ExtractorClaude, `{"is_error":true,"result":"Unauthorized: bad header hdr-secret-4242 for blue-team-77"}`,
			"", "claude: Unauthorized: bad header [secret] for [secret]: sign-in failed", true},
		{"proxy password", store.ExtractorCodex, "", "ERROR: proxy http://bob:pw-5150@proxy:3128 refused\n",
			"sh: exit status 1: proxy [secret] refused", false},
		{"pattern secret", store.ExtractorClaude, `{"is_error":true,"result":"Invalid API key sk-ant-abcdefghijklmnopqrstuvwxyz"}`,
			"", "claude: Invalid API key [secret]: sign-in failed", true},
		{"other failure", store.ExtractorClaude, `{"is_error":true,"result":"API Error: Request timed out"}`, "",
			"sh: exit status 1: API Error: Request timed out", false},
		{"no text", store.ExtractorClaude, "", "", "sh: exit status 1", false},
		{"long text", store.ExtractorClaude, "", long, "sh: exit status 1: " + strings.TrimSpace(long)[:maxCause] + "...", false},
		{"model ran", store.ExtractorCodex, `{"type":"item.completed","item":{"type":"reasoning"}}` + "\n" +
			`{"type":"turn.failed","error":{"message":"stream error: 401 Unauthorized"}}`, "",
			"sh: exit status 1: stream error: 401 Unauthorized", false},
		{"limit", store.ExtractorClaude, claudeWeekly, "", "claude is at a usage or rate limit", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := runError(c.cli, signInEnv, []byte(c.out), exit("sh", c.stderr))
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
	_, err := claudeResult([]byte(`{"is_error":true,"result":"Not logged in · Please run /login"}`), nil)
	if want := "claude: Not logged in · Please run /login: sign-in failed"; err == nil || err.Error() != want ||
		!errors.Is(err, ErrSignIn) {
		t.Errorf("claudeResult() = %v, want %q", err, want)
	}
	_, err = codexResult([]byte(`{"type":"turn.failed","error":{"message":"model gpt-x not found"}}`), nil, "")
	if want := "codex reported a failed turn: model gpt-x not found"; err == nil || err.Error() != want {
		t.Errorf("codexResult() = %v, want %q", err, want)
	}
}

// AC #428: status and doctor give a short hint for a sign-in failure, for the CLI that failed,
// and none for any other reason.
func TestSignInHint(t *testing.T) {
	for _, c := range []struct{ reason, want string }{
		{"claude: Failed to authenticate: OAuth session expired: sign-in failed",
			"sign in again: run claude, then /login, or set ANTHROPIC_API_KEY in the herdr server env"},
		{"codex: unexpected status 401 Unauthorized: sign-in failed",
			"sign in again: run codex login, or set OPENAI_API_KEY in the herdr server env"},
		{"claude: exit status 1", ""},
		{"claude is at a usage or rate limit", ""},
		{"", ""},
	} {
		if got := SignInHint(c.reason); got != c.want {
			t.Errorf("SignInHint(%q) = %q, want %q", c.reason, got, c.want)
		}
	}
}

// AC #428: while the sign-in of the CLI is broken, each call of a batch fails and the batch is
// tried again each retryAfter, but no call counts toward the daily cap or uses a try. When the
// sign-in works again, the batch commits with one counted call. A batch that failed before
// #428 with a counted call gets its retry too: TestFailedBatchTries.
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
	if b.Status != store.BatchFailed || b.ModelCalls != 0 || spent(b) || SignInHint(b.Reason) == "" {
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
