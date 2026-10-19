package extract

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

// The failures of the extractor CLI at a usage or rate limit in the M2 eval run 6 (2026-10-10,
// codex-cli 0.160.1, claude 2.1.296): the ERROR lines of codex exec, the error events of codex
// exec --json, and the error result of claude -p [the codex events and the claude result are
// made from the run 6 texts: the run kept the plain codex output only].
const (
	codex429 = "ERROR: exceeded retry limit, last status: 429 Too Many Requests\n"
	codex503 = "ERROR: unexpected status 503 Service Unavailable: All managed Codex accounts are temporarily " +
		"unavailable for this runtime request., url: http://127.0.0.1:64305/responses\n"
	codexEvents429 = `{"type":"thread.started","thread_id":"t1"}` + "\n" + `{"type":"turn.started"}` + "\n" +
		`{"type":"error","message":"exceeded retry limit, last status: 429 Too Many Requests"}` + "\n" +
		`{"type":"turn.failed","error":{"message":"exceeded retry limit, last status: 429 Too Many Requests"}}` + "\n"
	claudeWeekly = `{"type":"result","subtype":"success","is_error":true,` +
		`"result":"You've hit your weekly limit · resets Oct 12 at 9:30am (UTC)"}`
)

// exit is the error of a CLI run that exited with status 1 and wrote stderr.
func exit(cli, stderr string) error {
	return fmt.Errorf("%s: %w", cli, &ExitError{Err: errors.New("exit status 1"), Stderr: []byte(stderr)})
}

// AC #377: Brain tells a failure at a usage or rate limit from other failures by the error text
// of the CLI: an ERROR line of codex, a codex error or failed turn event, or the error result of
// claude -p. Model text in the output, such as an agent message about a 429 response, and other
// errors are no limit.
func TestLimited(t *testing.T) {
	for _, c := range []struct {
		name        string
		out, stderr string
		want        bool
	}{
		{"codex 429 line", "", codex429, true},
		{"codex 503 line", "", codex503, true},
		{"codex 429 events", codexEvents429, "", true},
		{"claude weekly limit", claudeWeekly, "", true},
		{"claude rate limit", `{"is_error":true,"result":"API Error: Rate limit reached"}`, "", true},
		{"no text", "", "", false},
		{"other codex error", "", "ERROR: stream disconnected before completion\n", false},
		{"other claude error", `{"is_error":true,"result":"API Error: Request timed out"}`, "", false},
		{"agent message", `{"type":"item.completed","item":{"type":"agent_message","text":"return 429 Too Many Requests"}}` +
			"\n" + `{"type":"turn.failed","error":{"message":"stream disconnected"}}`, "", false},
		{"claude reply text", `{"is_error":false,"result":"add a rate limit of 429"}`, "", false},
		{"codex limit after model output", `{"type":"item.completed","item":{"type":"reasoning","text":"..."}}` + "\n" +
			`{"type":"turn.failed","error":{"message":"exceeded retry limit, last status: 429 Too Many Requests"}}`, codex429, false},
		{"claude limit with used tokens", `{"is_error":true,"result":"API Error: Rate limit reached",` +
			`"usage":{"input_tokens":5120,"output_tokens":0}}`, "", false},
		{"claude limit with no tokens", `{"is_error":true,"result":"API Error: Rate limit reached",` +
			`"usage":{"input_tokens":0,"output_tokens":0}}`, "", true},
		{"stderr log line", "", "WARN retrying after 429 Too Many Requests\n", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := limited([]byte(c.out), []byte(c.stderr)); got != c.want {
				t.Errorf("limited() = %v, want %v", got, c.want)
			}
		})
	}
}

// codexAt is the answer of codex while codex exec gives out and stderr and exits with status 1:
// codex mcp list still works, as it calls no model.
func codexAt(out, stderr string) func(context.Context, string) ([]byte, error) {
	return func(_ context.Context, prompt string) ([]byte, error) {
		if prompt == "--json" {
			return []byte("[]"), nil
		}
		return []byte(out), exit("codex", stderr)
	}
}

// day returns the use of the daily caps at the time of the fixture.
func (f *fixture) day() Day {
	f.t.Helper()
	var d Day
	f.write(func(ctx context.Context, tx *sql.Tx) (err error) {
		d, err = f.pass().Day(ctx, tx)
		return err
	})
	return d
}

// AC #377 (M2 eval run 6): while the extractor CLI is at a usage or rate limit, each batch call
// fails and is tried again each retryAfter, but none of those calls counts toward the daily call
// cap, and none uses a try of the batch: after a long outage the batch is not lost, and brain
// status names the limit. When the CLI is back, the batch commits with one counted call. A
// failure for another reason still counts and uses a try: the CLI ran the model, so the call
// cost tokens.
func TestLimitUsesNoCall(t *testing.T) {
	for _, c := range []struct {
		name  string
		cli   store.ExtractorCLI
		reply func(context.Context, string) ([]byte, error)
	}{
		{"codex 429", store.ExtractorCodex, codexAt("", codex429)},
		{"codex 503", store.ExtractorCodex, codexAt("", codex503)},
		{"codex 429 events", store.ExtractorCodex, codexAt(codexEvents429, "")},
		{"claude weekly limit", store.ExtractorClaude, func(context.Context, string) ([]byte, error) {
			return []byte(claudeWeekly), exit("claude", "")
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			f.write(func(ctx context.Context, tx *sql.Tx) error { return store.SetExtractorCLI(ctx, tx, c.cli, "", t0) })
			f.session("s1", true, line{"ws-A", pnpm, t0})
			f.cli.reply = c.reply
			for i := range 2 * maxTries {
				if i > 0 {
					f.now = f.now.Add(retryAfter)
				}
				f.mustRun()
			}
			if n := f.cli.calls(); n < 2*maxTries {
				t.Errorf("CLI runs = %d, want at least %d: a try each retryAfter", n, 2*maxTries)
			}
			b := f.batches()[0]
			if b.Status != store.BatchFailed || b.ModelCalls != 0 || spent(b) {
				t.Fatalf("batch after the outage = %+v, want failed with no counted call and its tries", b)
			}
			if d := f.day(); d.Calls != 0 {
				t.Errorf("calls of the day = %d, want 0", d.Calls)
			}
			ws := f.waiting()
			if len(ws) != 1 || !strings.Contains(ws[0].Failed, string(c.cli)+" is at a usage or rate limit") {
				t.Errorf("waiting = %+v, want the limit as the reason of the failed call", ws)
			}
			f.now = f.now.Add(retryAfter)
			f.cli.reply = claimOf("use pnpm")
			if c.cli == store.ExtractorCodex {
				f.cli.reply = f.codexClaim("use pnpm")
			}
			f.mustRun()
			if b := f.batches()[0]; b.Status != store.BatchCommitted || b.ModelCalls != 1 {
				t.Errorf("batch when the CLI is back = %+v, want committed after 1 counted call", b)
			}
			if d := f.day(); d.Calls != 1 {
				t.Errorf("calls of the day = %d, want 1", d.Calls)
			}
		})
	}

	t.Run("other failure", func(t *testing.T) {
		f := newFixture(t)
		f.session("s1", true, line{"ws-A", pnpm, t0})
		f.cli.reply = func(context.Context, string) ([]byte, error) {
			return []byte(`{"is_error":true,"result":"API Error: Request timed out"}`), exit("claude", "")
		}
		f.mustRun()
		if b := f.batches()[0]; b.Status != store.BatchFailed || b.ModelCalls != 1 ||
			b.Reason != "claude: exit status 1: API Error: Request timed out" {
			t.Errorf("batch = %+v, want failed after 1 counted call", b)
		}
		if d := f.day(); d.Calls != 1 {
			t.Errorf("calls of the day = %d, want 1", d.Calls)
		}
	})
}

// codexClaim is the answer of codex with one claim of rule, as claimOf for claude: codex exec
// writes the result to its -o file in the folder of the call.
func (f *fixture) codexClaim(rule string) func(context.Context, string) ([]byte, error) {
	return func(ctx context.Context, prompt string) ([]byte, error) {
		if prompt == "--json" {
			return []byte("[]"), nil
		}
		out, err := claimOf(rule)(ctx, prompt)
		if err != nil {
			return nil, err
		}
		var res struct {
			StructuredOutput json.RawMessage `json:"structured_output"`
		}
		if err := json.Unmarshal(out, &res); err != nil {
			return nil, err
		}
		f.cli.mu.Lock()
		dir := f.cli.cmds[len(f.cli.cmds)-1].Dir
		f.cli.mu.Unlock()
		return []byte(`{"type":"turn.completed"}`), os.WriteFile(filepath.Join(dir, "last.json"), res.StructuredOutput, 0o600)
	}
}

// AC #377: a change request whose call fails at a usage or rate limit uses no try and no call of
// the cap, also after a long outage; then it gets its draft.
func TestLimitChangeRequest(t *testing.T) {
	f := newFixture(t)
	p := f.proposed()
	cr := f.ask(p, "split it")
	calls := f.day().Calls
	f.cli.reply = redraft(func(context.Context, string) ([]byte, error) { return []byte(claudeWeekly), exit("claude", "") })
	for range 2 * maxTries {
		f.mustRun()
		f.now = f.now.Add(retryAfter)
	}
	got := f.request(cr.ID)
	if got.Status != store.ChangeFailed || got.ModelCalls != 0 || ChangeSpent(got) {
		t.Fatalf("request after the outage = %+v, want failed with no counted call", got)
	}
	if d := f.day(); d.Calls != calls {
		t.Errorf("calls of the day = %d, want %d as before the request", d.Calls, calls)
	}
	f.cli.reply = redraft(claimOf("use pnpm 9"))
	f.worked("b0")
	if got := f.request(cr.ID); got.Status != store.ChangeCommitted || got.ModelCalls != 1 {
		t.Errorf("request when the CLI is back = %+v, want committed after 1 counted call", got)
	}
}

// AC #377: ExecRunner keeps the end of the standard error of a failed command in its ExitError,
// so the limit check reads the codex ERROR line; the error text stays the exit status.
func TestExecRunnerStderr(t *testing.T) {
	_, err := ExecRunner{}.Run(context.Background(), Command{Dir: t.TempDir(),
		Argv: []string{"/bin/sh", "-c", "printf '%s' \"$0\" >&2; exit 1", codex429}})
	var e *ExitError
	if !errors.As(err, &e) || err.Error() != "sh: exit status 1" || string(e.Stderr) != codex429 {
		t.Fatalf("Run() error = %v (%+v), want sh: exit status 1 with the codex line", err, e)
	}
	if !limited(nil, e.Stderr) {
		t.Errorf("limited() = false for the stderr of the run")
	}
}

// limitWhen answers a prompt that holds word as the CLI at its weekly limit, and any other with
// reply.
func limitWhen(word string, reply func(context.Context, string) ([]byte, error)) func(context.Context, string) ([]byte, error) {
	return func(ctx context.Context, prompt string) ([]byte, error) {
		if strings.Contains(prompt, word) {
			return []byte(claudeWeekly), exit("claude", "")
		}
		return reply(ctx, prompt)
	}
}

// AC #377: a failed batch and a failed change request whose last try, the one after maxTries
// that a working call unlocks, the CLI refuses at its limit keep that try: it comes again
// retryAfter later, with no other call that works, and then commits.
func TestLimitAtLastTry(t *testing.T) {
	t.Run("batch", func(t *testing.T) {
		f := newFixture(t)
		f.session("s1", true, line{"ws-A", "no, use yarn instead of npm here", t0})
		f.cli.reply = down
		for range 5 {
			f.mustRun()
			f.now = f.now.Add(retryAfter)
		}
		f.cli.reply = limitWhen("yarn", claimOf("use pnpm"))
		f.worked("b0")
		f.worked("b1")
		b := f.batches()[0]
		if b.Status != store.BatchFailed || b.ModelCalls != maxTries || b.Refusal != store.RefusalLimit {
			t.Fatalf("batch = %+v, want failed at the limit with %d counted calls", b, maxTries)
		}
		f.now = f.now.Add(retryAfter)
		f.cli.reply = claimOf("use yarn")
		f.mustRun()
		if b := f.batches()[0]; b.Status != store.BatchCommitted || b.ModelCalls != maxTries+1 {
			t.Errorf("batch retryAfter later = %+v, want committed after its last try", b)
		}
	})
	t.Run("change request", func(t *testing.T) {
		f := newFixture(t)
		cr := f.ask(f.proposed(), "split it")
		f.cli.reply = redraft(down)
		for range 5 {
			f.mustRun()
			f.now = f.now.Add(retryAfter)
		}
		f.cli.reply = redraft(func(context.Context, string) ([]byte, error) { return []byte(claudeWeekly), exit("claude", "") })
		f.worked("b0")
		f.worked("b1")
		if got := f.request(cr.ID); got.Status != store.ChangeFailed || got.ModelCalls != maxTries || got.Refusal != store.RefusalLimit {
			t.Fatalf("request = %+v, want failed at the limit with %d counted calls", got, maxTries)
		}
		f.now = f.now.Add(retryAfter)
		f.cli.reply = redraft(claimOf("use pnpm 9"))
		f.mustRun()
		if got := f.request(cr.ID); got.Status != store.ChangeCommitted || got.ModelCalls != maxTries+1 {
			t.Errorf("request retryAfter later = %+v, want committed after its last try", got)
		}
	})
}
