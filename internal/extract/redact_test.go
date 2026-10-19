package extract

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"net/url"
	"strings"
	"testing"

	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

// AC #428 (Sol round 1): no form of a secret of the call survives hide: the whole value, a URL
// password and user info, the token after "Authorization: Bearer", a header value, a query
// value, and a short token, each raw, URL-encoded and base64-encoded. The user info of any URL
// in the text is hidden too.
func TestHideForms(t *testing.T) {
	secrets := secretsOf(map[string]string{
		"ANTHROPIC_AUTH_TOKEN":     "proxy/token+alpha=42",
		"ANTHROPIC_CUSTOM_HEADERS": "Authorization: Bearer bear-tok-77\nX-Key: hdr-9",
		"ANTHROPIC_BASE_URL":       "https://bu:bp-secret@gw.example/v1?key=qk-55",
		"HTTPS_PROXY":              "pu:pp-secret@proxy:3128",
		"OPENAI_API_KEY":           "123456",
	})
	for _, raw := range []string{"proxy/token+alpha=42", "bear-tok-77", "hdr-9", "bp-secret", "bu:bp-secret", "qk-55",
		"pp-secret", "pu:pp-secret", "123456"} {
		b := []byte(raw)
		for _, form := range []string{raw, url.QueryEscape(raw), url.PathEscape(raw), base64.StdEncoding.EncodeToString(b),
			base64.URLEncoding.EncodeToString(b), base64.RawStdEncoding.EncodeToString(b), base64.RawURLEncoding.EncodeToString(b)} {
			if got := hide("before "+form+" after", secrets); strings.Contains(got, form) || !strings.Contains(got, "[secret]") {
				t.Errorf("hide(a form of %q) = %q, want it hidden", raw, got)
			}
		}
	}
	if got, want := hide("proxy http://other:pw@host/x refused", nil), "proxy http://[secret]@host/x refused"; got != want {
		t.Errorf("hide(user info) = %q, want %q", got, want)
	}
}

// AC #428 (Sol round 1): the sign-in check reads the full error text of the CLI. A secret of the
// call inside "401", and auth text after the cut of the reason, still make a sign-in failure.
func TestSignInBeforeHide(t *testing.T) {
	for _, c := range []struct{ name, stderr, want string }{
		{"short token", "error: HTTP 401\n", "claude: error: HTTP 40[secret]: sign-in failed"},
		{"auth text after the cut", strings.Repeat("x", 300) + " 401 Unauthorized\n",
			"claude: " + strings.Repeat("x", maxCause) + "...: sign-in failed"},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := runError(store.ExtractorClaude, secretsOf(map[string]string{"ANTHROPIC_AUTH_TOKEN": "1"}), nil,
				exit("claude", c.stderr))
			if !errors.Is(err, ErrSignIn) || err.Error() != c.want {
				t.Errorf("runError() = %q, want %q as a sign-in failure", err, c.want)
			}
		})
	}
}

// legacyBatch gives batch id the bare reason of a Brain before #428 and calls counted calls.
func (f *fixture) legacyBatch(id int64, calls int) {
	f.t.Helper()
	f.write(func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE extraction_batches SET reason = 'claude: exit status 1', model_calls = ? WHERE id = ?`,
			calls, id)
		return err
	})
}

// AC #428 (Sol round 1), the owner's case: 4 batches failed before #428 with the bare reason
// "claude: exit status 1", 2 of them with 3 counted calls and 1 with no try left. Within
// retryAfter they wait. Then each gets its call with no other call that works, and all commit
// once the sign-in works.
func TestLegacyFailuresRetry(t *testing.T) {
	f := newFixture(t)
	for _, s := range []string{"s1", "s2", "s3", "s4"} {
		f.session(s, true, line{"ws-" + s, pnpm, t0})
	}
	f.cli.reply = down
	f.mustRun()
	bs := f.batches()
	if len(bs) != 4 {
		t.Fatalf("batches = %d, want 4", len(bs))
	}
	for i, calls := range []int{1, 3, 3, 4} {
		f.legacyBatch(bs[i].ID, calls)
	}
	runs := f.cli.calls()
	f.now = f.now.Add(retryAfter / 2)
	f.mustRun()
	if n := f.cli.calls() - runs; n != 0 {
		t.Errorf("calls within retryAfter = %d, want 0", n)
	}
	f.now = f.now.Add(retryAfter)
	f.cli.reply = claimOf("use pnpm")
	f.mustRun()
	for _, b := range f.batches() {
		if b.Status != store.BatchCommitted {
			t.Errorf("batch %d = %s (%s), want committed after the sign-in works", b.ID, b.Status, b.Reason)
		}
	}
}

// AC #428 (Sol round 1): the recovery of a legacy reason is once per batch. When its call fails
// for another reason, the new reason counts as usual, and the batch with no try left gets no
// call each retryAfter.
func TestLegacyRetryOnce(t *testing.T) {
	f := newFixture(t)
	f.session("s1", true, line{"ws-A", pnpm, t0})
	f.cli.reply = down
	f.mustRun()
	f.legacyBatch(f.batches()[0].ID, maxTries)
	f.cli.reply = func(context.Context, string) ([]byte, error) {
		return []byte(`{"is_error":true,"result":"API Error: Request timed out"}`), exit("claude", "")
	}
	runs := f.cli.calls()
	for range 5 {
		f.now = f.now.Add(retryAfter)
		f.mustRun()
	}
	b := f.batches()[0]
	if n := f.cli.calls() - runs; n != 1 || b.ModelCalls != maxTries+1 || !spent(b) || refused(b.Reason) {
		t.Errorf("calls = %d, batch = %+v, want 1 call, then no try left", n, b)
	}
}
