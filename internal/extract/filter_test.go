package extract

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"testing"

	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

// AC #428 (Sol round 2): the token filter of a stored reason hides every encoded form of a
// secret that the exact filter does not know, such as a lower-case percent escape, hex, a JSON
// slash escape and base64, and a URL. The common sign-in and limit messages of claude and codex
// stay readable.
func TestTokenFilter(t *testing.T) {
	const token = "proxy/token+alpha=42"
	secrets := secretsOf(map[string]string{"ANTHROPIC_AUTH_TOKEN": token})
	forms := []string{"proxy%2ftoken%2balpha%3d42", hex.EncodeToString([]byte(token)), `proxy\/token+alpha=42`,
		base64.StdEncoding.EncodeToString([]byte(token + "\n")), "https://gw.example/v1/messages"}
	err := runError(store.ExtractorClaude, secrets, nil, exit("claude", "error: 401 Unauthorized for "+strings.Join(forms, ", ")+"\n"))
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
		if got := reasonText(msg, secrets); got != msg {
			t.Errorf("reasonText(%q) = %q, want it readable as it is", msg, got)
		}
	}
}

// AC #428 (Sol round 2): a failed call whose model ran counts, also when its text ends with
// "401 Unauthorized: sign-in failed": Brain reads the stored refusal, never the reason text. The
// batch stops at maxTries calls while no call works.
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

// textHolding returns the TEXT columns of the store, as table.column, that hold s in a row.
func (f *fixture) textHolding(s string) []string {
	f.t.Helper()
	var found []string
	f.write(func(ctx context.Context, tx *sql.Tx) error {
		cols, err := textColumns(ctx, tx)
		if err != nil {
			return err
		}
		for _, c := range cols {
			var n int
			if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM "`+c[0]+`" WHERE instr("`+c[1]+`", ?) > 0`, s).
				Scan(&n); err != nil {
				return err
			}
			if n > 0 {
				found = append(found, c[0]+"."+c[1])
			}
		}
		return nil
	})
	return found
}

// textColumns returns each TEXT column of the store as its table and name.
func textColumns(ctx context.Context, tx *sql.Tx) (cols [][2]string, err error) {
	rows, err := tx.QueryContext(ctx, `SELECT m.name, p.name FROM sqlite_schema m, pragma_table_info(m.name) p
		WHERE m.type = 'table' AND p.type = 'TEXT'`)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, rows.Close()) }()
	for rows.Next() {
		var c [2]string
		if err := rows.Scan(&c[0], &c[1]); err != nil {
			return nil, err
		}
		cols = append(cols, c)
	}
	return cols, rows.Err()
}

// AC #428 (Sol round 2): a reply that echoes a sign-in value in its claim text, as in situation,
// rule and expected_benefit, makes a proposal with the value hidden: no column of the store
// holds it.
func TestResultEchoHidden(t *testing.T) {
	const tok = "echo-tok-424242"
	t.Setenv("ANTHROPIC_AUTH_TOKEN", tok)
	f := newFixture(t)
	f.session("s1", true, line{"ws-A", pnpm, t0})
	f.cli.reply = func(_ context.Context, prompt string) ([]byte, error) {
		refs := []string{}
		for _, m := range refID.FindAllStringSubmatch(prompt, -1) {
			refs = append(refs, m[1])
		}
		return claudeReply(map[string]any{"claims": []any{map[string]any{"kind": "correction",
			"situation": "installing with " + tok, "rule": "use pnpm, not the key " + tok, "scope_guess": "workspace",
			"evidence_refs": refs, "same_as": nil, "conflicts_with": nil, "touches_permission": false,
			"expected_benefit": "no lockfile " + tok}}})
	}
	f.mustRun()
	if b := f.batches()[0]; b.Status != store.BatchCommitted {
		t.Fatalf("batch = %+v, want committed", b)
	}
	if got := f.textHolding("[secret]"); len(got) == 0 {
		t.Errorf("no column holds the hidden claim text")
	}
	if got := f.textHolding(tok); len(got) > 0 {
		t.Errorf("columns %v hold the sign-in value", got)
	}
}

// overlapSecrets are 256 header values of growing runs of "a", the case of Sol round 2 that
// allocated 322 MiB.
func overlapSecrets() []string {
	var h strings.Builder
	for i := range 256 {
		fmt.Fprintf(&h, "X-H%d: %s\n", i, strings.Repeat("a", i+1))
	}
	return secretsOf(map[string]string{"ANTHROPIC_CUSTOM_HEADERS": h.String()})
}

// AC #428 (Sol round 2): hide keeps its memory in proportion to the text, also when many secrets
// overlap all over a 16 KiB error text.
func TestHideMemory(t *testing.T) {
	secrets, text := overlapSecrets(), strings.Repeat("a", 16<<10)
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	got := reasonText(text, secrets)
	runtime.ReadMemStats(&after)
	if n := after.TotalAlloc - before.TotalAlloc; n > 4<<20 {
		t.Errorf("reasonText allocated %d bytes, want 4 MiB or less", n)
	}
	if got != "[secret]" {
		t.Errorf("reasonText() = %q, want one [secret]", got)
	}
}

func BenchmarkHideOverlap(b *testing.B) {
	secrets, text := overlapSecrets(), strings.Repeat("a", 16<<10)
	b.ReportAllocs()
	for range b.N {
		hide(text, secrets)
	}
}
