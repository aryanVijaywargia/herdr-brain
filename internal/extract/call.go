package extract

import (
	"bufio"
	"bytes"
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
// (#377).
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

// limited reports whether a failed call says that the CLI is at a usage or rate limit: the
// message of a codex error or failed turn event in its output out, the result of a claude -p
// error result in out, or an "ERROR:" line of its standard error stderr (#377). A call whose
// output shows that the model ran, a codex item or a used token, cost tokens: it is no limit
// failure, also when the CLI hit the limit later.
func limited(out, stderr []byte) bool {
	limit := false
	for _, l := range bytes.Split(out, []byte("\n")) {
		var ev struct {
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
		if json.Unmarshal(l, &ev) != nil {
			continue
		}
		switch {
		case strings.HasPrefix(ev.Type, "item."), ev.Type == "turn.completed", ev.Usage.InputTokens+ev.Usage.OutputTokens > 0:
			return false
		case ev.Type == "error" && limitText.MatchString(ev.Message),
			ev.Type == "turn.failed" && limitText.MatchString(ev.Error.Message),
			ev.IsError && limitText.MatchString(ev.Result):
			limit = true
		}
	}
	for _, l := range strings.Split(string(stderr), "\n") {
		if msg, ok := strings.CutPrefix(strings.TrimSpace(l), "ERROR:"); ok && limitText.MatchString(msg) {
			limit = true
		}
	}
	return limit
}

// runError is the error of the failed CLI run of call, with out its output: an ErrLimit error
// when the CLI is at a usage or rate limit (limited).
func runError(cli store.ExtractorCLI, out []byte, err error) error {
	var exit *ExitError
	var stderr []byte
	if errors.As(err, &exit) {
		stderr = exit.Stderr
	}
	if limited(out, stderr) {
		return fmt.Errorf("%s is %w", cli, ErrLimit)
	}
	return err
}

// cleanEnv is the environment of an extractor call (E13-S1): only HOME, PATH, USER and LANG, so
// no HERDR_*, CLAUDE_CODE_* or ANTHROPIC_* variable reaches the CLI.
func cleanEnv() []string {
	return []string{"HOME=" + os.Getenv("HOME"), "PATH=" + os.Getenv("PATH"), "USER=" + os.Getenv("USER"),
		"LANG=en_US.UTF-8"}
}

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
	env := cleanEnv()
	switch s.CLI {
	case store.ExtractorClaude:
		out, err := r.Run(ctx, Command{Dir: dir, Argv: claudeArgv(s.Model, schema, prompt), Env: env})
		if err != nil {
			return nil, runError(s.CLI, out, err)
		}
		return claudeResult(out)
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
			return nil, runError(s.CLI, out, err)
		}
		return codexResult(out, filepath.Join(dir, "last.json"))
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

// claudeResult reads the structured output of a claude -p JSON result. With no structured
// output, the model refused the schema: the response is invalid (E13-S1).
func claudeResult(out []byte) (json.RawMessage, error) {
	var res struct {
		IsError          bool            `json:"is_error"`
		StructuredOutput json.RawMessage `json:"structured_output"`
	}
	switch err := json.Unmarshal(out, &res); {
	case err != nil:
		return nil, fmt.Errorf("claude output is not JSON: %w", ErrInvalid)
	case res.IsError && limited(out, nil):
		return nil, fmt.Errorf("claude is %w", ErrLimit)
	case res.IsError:
		return nil, errors.New("claude reported an error")
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

// codexResult reads the -o file of codex exec, after it checks the --json events out. A
// failed turn or no file is a failure; a tool item is an invalid response (E13-S1).
func codexResult(events []byte, last string) (json.RawMessage, error) {
	sc := bufio.NewScanner(bytes.NewReader(events))
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
			if limited(events, nil) {
				return nil, fmt.Errorf("codex is %w", ErrLimit)
			}
			return nil, errors.New("codex reported a failed turn")
		case "item.started", "item.completed":
			if t := ev.Item.Type + ev.Item.ItemType; !slices.Contains(codexItems, t) {
				return nil, fmt.Errorf("codex used a tool (%s): %w", t, ErrInvalid)
			}
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read codex events: %w", err)
	}
	out, err := os.ReadFile(last)
	if err != nil {
		return nil, errors.New("codex wrote no last message")
	}
	return out, nil
}
