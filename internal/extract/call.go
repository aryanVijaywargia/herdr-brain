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
	"slices"
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
// exec.Cmd.Stdin), as E13-S1 needs for claude -p. Standard error stays apart and is dropped:
// only the exit status is reported.
type ExecRunner struct{}

// Run runs c.
func (ExecRunner) Run(ctx context.Context, c Command) ([]byte, error) {
	cmd := exec.CommandContext(ctx, c.Argv[0], c.Argv[1:]...)
	cmd.Dir, cmd.Env = c.Dir, c.Env
	cmd.WaitDelay = 5 * time.Second // [NEW] a child that keeps the pipes open does not hold the pass
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return out.Bytes(), fmt.Errorf("%s: %w", filepath.Base(c.Argv[0]), err)
	}
	return out.Bytes(), nil
}

// ErrInvalid says that the extractor answered, but not with a valid batch result (§7.3).
var ErrInvalid = errors.New("invalid extractor response")

// cleanEnv is the environment of an extractor call (E13-S1): only HOME, PATH, USER and LANG, so
// no HERDR_*, CLAUDE_CODE_* or ANTHROPIC_* variable reaches the CLI.
func cleanEnv() []string {
	return []string{"HOME=" + os.Getenv("HOME"), "PATH=" + os.Getenv("PATH"), "USER=" + os.Getenv("USER"),
		"LANG=en_US.UTF-8"}
}

// call runs the extractor CLI of s with prompt, outside any transaction, in a new scratch
// folder, and returns the result object that the schema describes. An error that wraps
// ErrInvalid is an invalid response; any other error is a failure.
func call(ctx context.Context, r Runner, s store.ExtractorSetting, prompt string) (json.RawMessage, error) {
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
		out, err := r.Run(ctx, Command{Dir: dir, Argv: claudeArgv(s.Model, prompt), Env: env})
		if err != nil {
			return nil, err
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
			return nil, err
		}
		return codexResult(out, filepath.Join(dir, "last.json"))
	}
	return nil, fmt.Errorf("no extractor CLI is chosen")
}

// claudeArgv is the E13-S1 argv of claude -p: no tools, no MCP, no settings, no hooks, no
// slash commands and no session file.
func claudeArgv(model, prompt string) []string {
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
		switch t := ev.Item.Type + ev.Item.ItemType; {
		case ev.Type == "turn.failed" || ev.Type == "error":
			return nil, errors.New("codex reported a failed turn")
		case ev.Type == "item.started" || ev.Type == "item.completed":
			if !slices.Contains(codexItems, t) {
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
