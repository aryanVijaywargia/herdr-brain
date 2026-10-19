package extract

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"
	"testing"

	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

// AC #428: the token filter of a stored reason hides a token in every form, such as raw, a
// lower-case percent escape, hex, a JSON slash escape and base64, and a URL. The common sign-in
// and limit messages of claude and codex stay readable.
func TestTokenFilter(t *testing.T) {
	const token = "proxy/token+alpha=42"
	forms := []string{token, "proxy%2ftoken%2balpha%3d42", hex.EncodeToString([]byte(token)), `proxy\/token+alpha=42`,
		base64.StdEncoding.EncodeToString([]byte(token + "\n")), "https://gw.example/v1/messages"}
	err := runError(store.ExtractorClaude, nil, exit("claude", "error: 401 Unauthorized for "+strings.Join(forms, ", ")+"\n"))
	if !errors.Is(err, ErrSignIn) {
		t.Errorf("runError() = %q, want a sign-in failure", err)
	}
	for i, form := range forms {
		if strings.Contains(err.Error(), form) {
			t.Errorf("runError() = %q, holds form %d", err, i)
		}
	}
	for _, msg := range []string{
		"Failed to authenticate: OAuth session expired and could not be refreshed",
		"Invalid API key · Please run /login",
		"Not logged in · Please run /login",
		"unexpected status 401 Unauthorized: Missing bearer or basic authentication in header",
		"Your access token could not be refreshed because your refresh token has expired. Please log out and sign in again.",
		"exceeded retry limit, last status: 429 Too Many Requests",
		"You've hit your weekly limit · resets Oct 12 at 9:30am (UTC)",
		"API Error: Rate limit reached",
		"API Error: Request timed out",
	} {
		if got := reasonText(msg); got != msg {
			t.Errorf("reasonText(%q) = %q, want it readable as it is", msg, got)
		}
	}
}

// AC #428: a failed call whose model ran counts, also when its text ends with "401 Unauthorized:
// sign-in failed": Brain reads the stored refusal, never the reason text. The batch stops at
// maxTries calls while no call works.
func TestReasonTextIsNoRefusal(t *testing.T) {
	f := newFixture(t)
	f.session("s1", true, line{"ws-A", pnpm, t0})
	f.cli.reply = func(context.Context, string) ([]byte, error) {
		return []byte(`{"is_error":true,"result":"stream error: 401 Unauthorized: sign-in failed",` +
			`"usage":{"input_tokens":5,"output_tokens":1}}`), exit("claude", "")
	}
	for range 10 {
		f.mustRun()
		f.now = f.now.Add(retryAfter)
	}
	b := f.batches()[0]
	if n := f.cli.calls(); n != maxTries || b.ModelCalls != maxTries || b.Refusal != store.RefusalNone ||
		!strings.HasSuffix(b.Reason, "sign-in failed") {
		t.Errorf("calls = %d, batch = %+v, want %d counted calls and no refusal", n, b, maxTries)
	}
}
