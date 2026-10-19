package extract

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

// callTimeout is the time limit of one extractor call (§7.3 lets E13-S2 set one). [NEW] 5
// minutes: the E13-S1 calls took well under a minute.
const callTimeout = 5 * time.Minute

// Command is one command that a Runner runs: argv in folder Dir, with environment Env and no
// standard input.
type Command struct {
	Dir  string
	Argv []string
	Env  []string
}

// Runner runs a command and returns its standard output. Brain runs the extractor CLI through
// it; tests use a fake that counts the calls.
type Runner interface {
	Run(ctx context.Context, c Command) ([]byte, error)
}

// ExecRunner runs a command as a child process. Standard input is /dev/null (a nil
// exec.Cmd.Stdin), as E13-S1 needs for claude -p. Standard error stays apart: a failure reports
// the exit status, and the end of standard error goes into its ExitError for the limit check
// (#377) and the cause of the failure (#428).
type ExecRunner struct{}

// Run runs c.
func (ExecRunner) Run(ctx context.Context, c Command) ([]byte, error) {
	cmd := exec.CommandContext(ctx, c.Argv[0], c.Argv[1:]...)
	cmd.Dir, cmd.Env = c.Dir, c.Env
	cmd.WaitDelay = 5 * time.Second // [NEW] a child that keeps the pipes open does not hold the pass
	var out bytes.Buffer
	var errOut tail
	cmd.Stdout, cmd.Stderr = &out, &errOut
	if err := cmd.Run(); err != nil {
		return out.Bytes(), fmt.Errorf("%s: %w", filepath.Base(c.Argv[0]), &ExitError{Err: err, Stderr: errOut.b})
	}
	return out.Bytes(), nil
}

// ExitError is the error of a command that failed: its error, and the end of its standard
// error, where the CLI writes why it failed (#377). Its text is the text of Err.
type ExitError struct {
	Err    error
	Stderr []byte
}

func (e *ExitError) Error() string { return e.Err.Error() }

func (e *ExitError) Unwrap() error { return e.Err }

// tailSize is the end of standard error that ExecRunner keeps. [NEW] 16 KiB: the CLI writes its
// error line last.
const tailSize = 16 << 10

// tail keeps the last tailSize bytes written to it.
type tail struct{ b []byte }

func (t *tail) Write(p []byte) (int, error) {
	t.b = append(t.b, p...)
	if len(t.b) > tailSize {
		t.b = t.b[len(t.b)-tailSize:]
	}
	return len(p), nil
}

// ErrInvalid says that the extractor answered, but not with a valid batch result (§7.3).
var ErrInvalid = errors.New("invalid extractor response")

// ErrLimit says that the extractor CLI refused the call at a usage or rate limit of its plan
// (#377): it ran no model, so the call does not count toward the daily call cap, and it uses
// no try of its batch or change request.
var ErrLimit = errors.New("at a usage or rate limit")

// limitText matches the error text of a CLI at a usage or rate limit [NEW, M2 eval run 6,
// 2026-10-10]: codex "exceeded retry limit, last status: 429 Too Many Requests" and "unexpected
// status 503 Service Unavailable: All managed Codex accounts are temporarily unavailable", and
// claude "You've hit your weekly limit · resets ...". The check reads only the error text of a
// failed call, never model text.
var limitText = regexp.MustCompile(`(?i)\b(?:429|503)\b|too many requests|service unavailable|usage limit|` +
	`rate limit|hit your .*limit`)

// ErrSignIn says that the extractor CLI could not sign in, as with an expired sign-in or a
// wrong API key (#428): it ran no model, so the call does not count toward the daily call cap
// and uses no try, as at a limit (ErrLimit). The batch gets its call again after retryAfter, so
// it commits once the sign-in works.
var ErrSignIn = errors.New("sign-in failed")

// signInText matches the error text of a CLI that could not sign in [NEW, #428]: claude "Failed
// to authenticate: OAuth session expired and could not be refreshed", "Invalid API key · Please
// run /login" and "Not logged in", and codex "unexpected status 401 Unauthorized", "Your access
// token could not be refreshed" and "Missing environment variable: `OPENAI_API_KEY`".
var signInText = regexp.MustCompile(`(?i)authenticat|unauthori[sz]ed|\b401\b|not logged in|log ?in\b|` +
	`sign(?:ed)?[ -]?in\b|api[ _-]?key|oauth|(?:access|refresh|bearer) token|credential`)

// maxCause is the longest cause of a failure in a stored reason, in runes [NEW, #428]: it fits
// one line of brain status.
const maxCause = 200

// event is one JSON line of the output of a CLI: a codex exec --json event, or the claude -p
// result.
type event struct {
	Type    string `json:"type"`
	Message string `json:"message"`
	Error   struct {
		Message string `json:"message"`
	} `json:"error"`
	IsError bool   `json:"is_error"`
	Result  string `json:"result"`
	Usage   struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

// events returns the JSON lines of out; a line that is not JSON is skipped.
func events(out []byte) []event {
	var evs []event
	for _, l := range bytes.Split(out, []byte("\n")) {
		var ev event
		if json.Unmarshal(l, &ev) == nil {
			evs = append(evs, ev)
		}
	}
	return evs
}

// ran reports whether ev shows that the model ran: a codex item, a completed turn or a used
// token.
func (ev event) ran() bool {
	return strings.HasPrefix(ev.Type, "item.") || ev.Type == "turn.completed" || ev.Usage.InputTokens+ev.Usage.OutputTokens > 0
}

// message is the error text of ev: the message of a codex error or failed turn event, or the
// result of a claude -p error result; "" for any other event.
func (ev event) message() string {
	switch {
	case ev.Type == "error":
		return ev.Message
	case ev.Type == "turn.failed":
		return ev.Error.Message
	case ev.IsError:
		return ev.Result
	}
	return ""
}

// limited reports whether a failed call says that the CLI is at a usage or rate limit: the
// message of a codex error or failed turn event in its output out, the result of a claude -p
// error result in out, or an "ERROR:" line of its standard error stderr (#377). A call whose
// output shows that the model ran, a codex item or a used token, cost tokens: it is no limit
// failure, also when the CLI hit the limit later.
func limited(out, stderr []byte) bool {
	evs := events(out)
	if slices.ContainsFunc(evs, event.ran) {
		return false
	}
	if slices.ContainsFunc(evs, func(ev event) bool { return limitText.MatchString(ev.message()) }) {
		return true
	}
	for _, l := range strings.Split(string(stderr), "\n") {
		if msg, ok := strings.CutPrefix(strings.TrimSpace(l), "ERROR:"); ok && limitText.MatchString(msg) {
			return true
		}
	}
	return false
}

// cause returns why a failed call says that it failed (#428): the last error text of its output
// out (event.message), else its last "ERROR:" line of stderr, else the last line of stderr. The
// text goes through hide with env, the environment of the call, and the secret filter (Redact),
// and is cut to maxCause runes. It is "" when the CLI said nothing.
func cause(out, stderr []byte, env []string) string {
	var why string
	for _, ev := range events(out) {
		why = cmp.Or(ev.message(), why)
	}
	if why == "" {
		why = lastLine(stderr)
	}
	why = strings.Join(strings.Fields(Redact(hide(why, env))), " ")
	if r := []rune(why); len(r) > maxCause {
		why = string(r[:maxCause]) + "..."
	}
	return why
}

// lastLine returns the text of the last "ERROR:" line of stderr, else its last line that is not
// blank.
func lastLine(stderr []byte) string {
	var last, lastErr string
	for _, l := range strings.Split(string(stderr), "\n") {
		l = strings.TrimSpace(l)
		if msg, ok := strings.CutPrefix(l, "ERROR:"); ok {
			lastErr = strings.TrimSpace(msg)
		}
		last = cmp.Or(l, last)
	}
	return cmp.Or(lastErr, last)
}

// runError is the error of the failed CLI run of a call of cli with environment env, with out
// its output and err the error of the run (failure).
func runError(cli store.ExtractorCLI, env []string, out []byte, err error) error {
	var exit *ExitError
	var stderr []byte
	if errors.As(err, &exit) {
		stderr = exit.Stderr
	}
	return failure(cli, env, out, stderr, err)
}

// failure is the error of a call of cli with environment env that failed with err, with out its
// output and stderr the end of its standard error: an ErrLimit error when the CLI is at a usage
// or rate limit (limited), an ErrSignIn error with its cause when it could not sign in and ran
// no model (#428), else err with the cause that the CLI gave, if any. So the stored reason says
// why, as "claude: Failed to authenticate: ...: sign-in failed".
func failure(cli store.ExtractorCLI, env []string, out, stderr []byte, err error) error {
	if limited(out, stderr) {
		return fmt.Errorf("%s is %w", cli, ErrLimit)
	}
	why := cause(out, stderr, env)
	switch {
	case why == "":
		return err
	case signInText.MatchString(why) && !slices.ContainsFunc(events(out), event.ran):
		return fmt.Errorf("%s: %s: %w", cli, why, ErrSignIn)
	}
	return fmt.Errorf("%w: %s", err, why)
}

// uncounted reports whether failed call error err ran no model, so its call leaves the daily
// call cap and uses no try: the CLI was at a limit (ErrLimit, #377) or could not sign in
// (ErrSignIn, #428).
func uncounted(err error) bool { return errors.Is(err, ErrLimit) || errors.Is(err, ErrSignIn) }

// call runs the extractor CLI of s with prompt and the JSON output schema schema, outside any
// transaction, in a new scratch folder, and returns the result object that schema describes:
// claimSchema for a batch or a change request, mergeSchema for the merge pass (§9.5). An error
// that wraps ErrInvalid is an invalid response; any other error is a failure.
func call(ctx context.Context, r Runner, s store.ExtractorSetting, schema, prompt string) (json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	dir, err := os.MkdirTemp("", "brain-extract-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir) //nolint:errcheck // a scratch folder in the temp folder
	env := cleanEnv(s.CLI, os.Getenv)
	switch s.CLI {
	case store.ExtractorClaude:
		out, err := r.Run(ctx, Command{Dir: dir, Argv: claudeArgv(s.Model, schema, prompt), Env: env})
		if err != nil {
			return nil, runError(s.CLI, env, out, err)
		}
		return claudeResult(out, env)
	case store.ExtractorCodex:
		out, err := r.Run(ctx, Command{Dir: dir, Argv: []string{"codex", "--disable", "plugins", "mcp", "list", "--json"}, Env: env})
		if err != nil {
			return nil, fmt.Errorf("list the MCP servers of codex: %w", err)
		}
		servers, err := enabledServers(out)
		if err != nil {
			return nil, err
		}
		if err := os.WriteFile(filepath.Join(dir, "schema.json"), []byte(schema), 0o600); err != nil {
			return nil, err
		}
		out, err = r.Run(ctx, Command{Dir: dir, Argv: codexArgv(s.Model, servers, prompt), Env: env})
		if err != nil {
			return nil, runError(s.CLI, env, out, err)
		}
		return codexResult(out, env, filepath.Join(dir, "last.json"))
	}
	return nil, fmt.Errorf("no extractor CLI is chosen")
}

// claudeArgv is the E13-S1 argv of claude -p with the output schema schema: no tools, no MCP,
// no settings, no hooks, no slash commands and no session file.
func claudeArgv(model, schema, prompt string) []string {
	argv := []string{"claude", "-p"}
	if model != "" {
		argv = append(argv, "--model", model)
	}
	return append(argv, "--tools", "", "--strict-mcp-config", "--setting-sources", "",
		"--settings", `{"disableAllHooks":true}`, "--disable-slash-commands", "--no-session-persistence",
		"--output-format", "json", "--json-schema", schema, prompt)
}

// claudeResult reads the structured output of a claude -p JSON result, from a call with
// environment env. With no structured output, the model refused the schema: the response is
// invalid (E13-S1). An error result is a failure.
func claudeResult(out []byte, env []string) (json.RawMessage, error) {
	var res struct {
		IsError          bool            `json:"is_error"`
		StructuredOutput json.RawMessage `json:"structured_output"`
	}
	switch err := json.Unmarshal(out, &res); {
	case err != nil:
		return nil, fmt.Errorf("claude output is not JSON: %w", ErrInvalid)
	case res.IsError:
		return nil, failure(store.ExtractorClaude, env, out, nil, errors.New("claude reported an error"))
	case len(res.StructuredOutput) == 0 || string(res.StructuredOutput) == "null":
		return nil, fmt.Errorf("claude gave no structured output: %w", ErrInvalid)
	}
	return res.StructuredOutput, nil
}

// codexDisabled are the codex features that the extractor call turns off (E13-S1).
var codexDisabled = []string{"hooks", "plugins", "apps", "shell_tool", "unified_exec", "multi_agent", "sleep_tool",
	"image_generation", "view_image", "browser_use", "computer_use", "tool_suggest", "skill_search"}

// codexArgv is the E13-S1 argv of codex exec, with each enabled MCP server of servers off.
// Codex keeps its code-mode exec, wait and request_user_input_async (§7.3).
func codexArgv(model string, servers []string, prompt string) []string {
	argv := []string{"codex", "exec"}
	if model != "" {
		argv = append(argv, "-m", model)
	}
	argv = append(argv, "--ephemeral", "--skip-git-repo-check", "--ignore-rules", "-s", "read-only")
	for _, f := range codexDisabled {
		argv = append(argv, "--disable", f)
	}
	argv = append(argv, "-c", "agents.enabled=false", "-c", "tools.experimental_request_user_input.enabled=false")
	for _, s := range servers {
		argv = append(argv, "-c", "mcp_servers."+s+".enabled=false")
	}
	return append(argv, "-c", `web_search="disabled"`, "--output-schema", "schema.json", "-o", "last.json",
		"--json", prompt)
}

// enabledServers returns the names of the enabled servers in the output of codex mcp list
// --json. A server with no enabled field is enabled.
func enabledServers(out []byte) ([]string, error) {
	var list []struct {
		Name    string `json:"name"`
		Enabled *bool  `json:"enabled"`
	}
	if err := json.Unmarshal(out, &list); err != nil {
		return nil, fmt.Errorf("read the MCP servers of codex: %w", err)
	}
	var names []string
	for _, s := range list {
		if s.Enabled == nil || *s.Enabled {
			names = append(names, s.Name)
		}
	}
	return names, nil
}

// codexItems are the item types of a codex exec reply with no tool use [NEW]. Any other item
// is a tool item: Brain rejects the reply (E13-S1).
var codexItems = []string{"agent_message", "reasoning"}

// codexResult reads the -o file of codex exec, after it checks the --json events out of a call
// with environment env. A failed turn or no file is a failure; a tool item is an invalid
// response (E13-S1).
func codexResult(out []byte, env []string, last string) (json.RawMessage, error) {
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(nil, 16<<20)
	for sc.Scan() {
		var ev struct {
			Type string `json:"type"`
			Item struct {
				Type     string `json:"type"`
				ItemType string `json:"item_type"`
			} `json:"item"`
		}
		if json.Unmarshal(sc.Bytes(), &ev) != nil {
			continue
		}
		switch ev.Type {
		case "turn.failed", "error":
			return nil, failure(store.ExtractorCodex, env, out, nil, errors.New("codex reported a failed turn"))
		case "item.started", "item.completed":
			if t := ev.Item.Type + ev.Item.ItemType; !slices.Contains(codexItems, t) {
				return nil, fmt.Errorf("codex used a tool (%s): %w", t, ErrInvalid)
			}
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read codex events: %w", err)
	}
	res, err := os.ReadFile(last)
	if err != nil {
		return nil, errors.New("codex wrote no last message")
	}
	return res, nil
}
