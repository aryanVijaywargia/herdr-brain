package transcript

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// codexShell lists the Codex function tools that run a shell command. A local_shell_call is
// a shell command too.
var codexShell = map[string]bool{"shell": true, "shell_command": true, "exec_command": true, "container.exec": true}

// codexExit is a known exit line of Codex shell output (the exact line in the installed
// version is not checked yet): "Exit code: N" of the shell tools and "Process exited with
// code N" of exec_command. A legacy file saves no exit event, so there this line is the only
// exit status; a paginated file also has the exit_code of an item_completed.
var codexExit = regexp.MustCompile(`(?m)^(?:Exit code: |Process exited with code )(-?\d+)\r?$`)

// codexItems lists the response_item types that the reader reads. The other types are skipped
// before their fields are decoded: a tool_search_call, for one, has object arguments.
var codexItems = map[string]bool{"message": true, "function_call": true, "local_shell_call": true,
	"function_call_output": true}

// codexInteractive lists the sources of sessions that you ran.
var codexInteractive = map[string]bool{"cli": true, "vscode": true}

// codexLine is one line of a Codex session file.
type codexLine struct {
	Timestamp string          `json:"timestamp"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload"`
}

type codexMeta struct {
	ID             string          `json:"id"`
	CWD            string          `json:"cwd"`
	Source         json.RawMessage `json:"source"`
	ForkedFromID   string          `json:"forked_from_id"`
	ParentThreadID string          `json:"parent_thread_id"`
}

// codexItem is the payload of a response_item.
type codexItem struct {
	Type    string `json:"type"`
	Role    string `json:"role"`
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	Name      string          `json:"name"`
	Arguments string          `json:"arguments"`
	CallID    string          `json:"call_id"`
	Output    json.RawMessage `json:"output"`
	Action    *struct {
		Command []string `json:"command"`
	} `json:"action"`
}

// codexEvent is the payload of an event_msg.
type codexEvent struct {
	Type   string `json:"type"`
	TurnID string `json:"turn_id"`
	Info   *struct {
		TotalTokenUsage struct {
			TotalTokens *int64 `json:"total_tokens"`
		} `json:"total_token_usage"`
	} `json:"info"`
	Item *codexTurnItem `json:"item"`
}

// codexTurnItem is the item of an item_completed event_msg. Only the fields of a
// CommandExecution item are read.
type codexTurnItem struct {
	Type             string   `json:"type"`
	ID               string   `json:"id"`
	Command          []string `json:"command"`
	Source           string   `json:"source"`
	Status           string   `json:"status"`
	AggregatedOutput string   `json:"aggregated_output"`
	ExitCode         *int     `json:"exit_code"`
}

// codex reads one Codex session file.
type codex struct {
	session
	meta   bool
	total  int64          // the highest total_tokens so far
	calls  map[string]ref // the shell commands by call id, for their result
	turnID string         // the Codex turn id of the last task_started
	turns  map[string]int // a Codex turn id → the index of its last prompt in Turns
}

// ReadCodex reads the Codex session file at path up to limit bytes. The first line is the
// session_meta.
func ReadCodex(path string, limit int64) (Session, error) {
	c := codex{calls: map[string]ref{}, turns: map[string]int{}}
	if err := lines(path, limit, c.line); err != nil {
		return Session{}, err
	}
	return c.s, nil
}

// line reads one line at byte offset off.
func (c *codex) line(raw []byte, off int64) error {
	var l codexLine
	if err := json.Unmarshal(raw, &l); err != nil {
		return fmt.Errorf("%w: %w", ErrUnknownFormat, err)
	}
	at, err := parseTime(l.Timestamp)
	if err != nil {
		return err
	}
	if !c.meta && l.Type != "session_meta" {
		return fmt.Errorf("%w: first line is %q, not session_meta", ErrUnknownFormat, l.Type)
	}
	t := Text{ID: c.s.ID + "@" + strconv.FormatInt(off, 10), At: at}
	switch l.Type {
	case "session_meta":
		err = c.sessionMeta(l.Payload)
		c.at(at, "")
	case "response_item":
		c.at(at, "")
		err = c.item(l.Payload, t)
	case "event_msg":
		c.at(at, "")
		err = c.event(l.Payload, t)
	case "":
		err = fmt.Errorf("%w: line with no type", ErrUnknownFormat)
	default:
		// The other lines hold no prompt, reply or command: turn_context, compacted (its
		// summary is dropped), world_state and token_usage_record (the tokens come from
		// token_count), and in codex-cli 0.165.0 retained_context, security_risk_score,
		// realtime_item and the inter_agent_communication lines. Codex adds line types over
		// time, so a new one is skipped too: it must not fail the whole file.
		c.at(at, "")
	}
	return err
}

// sessionMeta reads the session_meta line. A later one, as a resume writes, adds nothing.
func (c *codex) sessionMeta(payload json.RawMessage) error {
	if c.meta {
		return nil
	}
	var m codexMeta
	if err := json.Unmarshal(payload, &m); err != nil {
		return fmt.Errorf("%w: session_meta: %w", ErrUnknownFormat, err)
	}
	if m.ID == "" {
		return fmt.Errorf("%w: session_meta with no id", ErrUnknownFormat)
	}
	// The source is a string for a plain session; a sub-agent source is an object.
	var source string
	_ = json.Unmarshal(m.Source, &source) // not a string: not cli or vscode
	c.meta = true
	c.s.ID, c.s.CWD = m.ID, m.CWD
	c.s.Interactive = codexInteractive[source]
	c.s.Fork, c.s.SideChain = m.ForkedFromID != "", m.ParentThreadID != ""
	return nil
}

// item reads a response_item. The prompt comes only from a user message here, never from the
// event_msg copy. Other item types, as reasoning, hold no prompt, command or reply: Codex
// adds item types over time, so only an item with no type is an unknown shape. A
// custom_tool_call exec runs a JavaScript cell, not a shell command: the shell commands of the
// cell come from the item_completed command executions of a paginated file.
func (c *codex) item(payload json.RawMessage, t Text) error {
	var head struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(payload, &head); err != nil {
		return fmt.Errorf("%w: response_item: %w", ErrUnknownFormat, err)
	}
	if head.Type == "" {
		return fmt.Errorf("%w: response_item with no type", ErrUnknownFormat)
	}
	if !codexItems[head.Type] {
		return nil
	}
	var it codexItem
	if err := json.Unmarshal(payload, &it); err != nil {
		return fmt.Errorf("%w: response_item: %w", ErrUnknownFormat, err)
	}
	switch it.Type {
	case "message":
		t.Text = codexText(it)
		switch {
		case t.Text == "":
		case it.Role == "user":
			c.prompt(t)
			if c.turnID != "" {
				c.turns[c.turnID] = len(c.s.Turns) - 1
			}
		case it.Role == "assistant":
			c.reply(t)
		}
	case "function_call":
		if !codexShell[it.Name] {
			return nil
		}
		cmd, err := codexArguments(it.Arguments)
		if err != nil {
			return err
		}
		c.call(it.CallID, len(c.s.Turns)-1, Command{ID: t.ID, At: t.At, Command: cmd})
	case "local_shell_call":
		if it.Action == nil || len(it.Action.Command) == 0 {
			return fmt.Errorf("%w: local_shell_call with no command", ErrUnknownFormat)
		}
		c.call(it.CallID, len(c.s.Turns)-1, Command{ID: t.ID, At: t.At, Command: argv(it.Action.Command)})
	case "function_call_output":
		c.output(it, t)
	}
	return nil
}

// codexText returns the text of a message, without injected text and images.
func codexText(it codexItem) string {
	var texts []string
	for _, p := range it.Content {
		if p.Type != "input_text" && p.Type != "output_text" {
			continue
		}
		if t := clean(p.Text); t != "" {
			texts = append(texts, t)
		}
	}
	return strings.Join(texts, "\n")
}

// codexArguments returns the command of the JSON arguments of a shell function call: a
// command list or string, or the cmd string of exec_command.
func codexArguments(arguments string) (string, error) {
	var a struct {
		Command json.RawMessage `json:"command"`
		Cmd     string          `json:"cmd"`
	}
	if err := json.Unmarshal([]byte(arguments), &a); err != nil {
		return "", fmt.Errorf("%w: shell arguments: %w", ErrUnknownFormat, err)
	}
	if a.Cmd != "" {
		return a.Cmd, nil
	}
	var s string
	if err := json.Unmarshal(a.Command, &s); err == nil && s != "" {
		return s, nil
	}
	var list []string
	if err := json.Unmarshal(a.Command, &list); err == nil && len(list) > 0 {
		return argv(list), nil
	}
	return "", fmt.Errorf("%w: shell arguments with no command", ErrUnknownFormat)
}

// argv returns a command list as one command: the script of a shell -c or -lc call, else the
// words joined by spaces.
func argv(list []string) string {
	if len(list) == 3 && (list[1] == "-c" || list[1] == "-lc") {
		return list[2]
	}
	return strings.Join(list, " ")
}

// call adds a shell command to the turn at index turn, once per call id: a paginated file has
// both the function_call and the item_completed of one exec_command.
func (c *codex) call(id string, turn int, cmd Command) (ref, bool) {
	if r, ok := c.calls[id]; ok {
		return r, true
	}
	r, ok := c.commandIn(turn, cmd)
	if ok && id != "" {
		c.calls[id] = r
	}
	return r, ok
}

// exit sets the exit status and the failure output of the command at r. The first one wins:
// the item_completed and the function_call_output of one call give the same status.
func (c *codex) exit(r ref, n int, failure *Text) {
	if c.s.Turns[r.turn].Commands[r.cmd].Exit == nil {
		c.result(r, &n, failure)
	}
}

// output reads the output of a shell call. The exit status comes only from a known exit line;
// only the output of a failed command is kept. An output that is not a string has no exit
// line.
func (c *codex) output(it codexItem, t Text) {
	r, ok := c.calls[it.CallID]
	if !ok {
		return
	}
	var out string
	if err := json.Unmarshal(it.Output, &out); err != nil {
		return
	}
	m := codexExit.FindStringSubmatch(out)
	if m == nil {
		return
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return
	}
	var failure *Text
	if n != 0 {
		t.Text = out
		failure = &t
	}
	c.exit(r, n, failure)
}

// event reads an event_msg: task_started, item_completed and token_count. The turn takes the
// growth of total_tokens, so a repeated line adds nothing. A token_count with no info carries
// no usage; an info with no total is an unknown shape, not a total of 0.
func (c *codex) event(payload json.RawMessage, t Text) error {
	var e codexEvent
	if err := json.Unmarshal(payload, &e); err != nil {
		return fmt.Errorf("%w: event_msg: %w", ErrUnknownFormat, err)
	}
	switch {
	case e.Type == "":
		return fmt.Errorf("%w: event_msg with no type", ErrUnknownFormat)
	case e.Type == "task_started":
		c.turnID = e.TurnID
		return nil
	case e.Type == "item_completed":
		return c.completed(e.Item, e.TurnID, t)
	case e.Type != "token_count" || e.Info == nil:
		return nil
	case e.Info.TotalTokenUsage.TotalTokens == nil:
		return fmt.Errorf("%w: token_count with no total_tokens", ErrUnknownFormat)
	}
	total := *e.Info.TotalTokenUsage.TotalTokens
	c.tokens(max(total-c.total, 0))
	c.total = max(total, c.total)
	return nil
}

// completed reads the item of an item_completed. A paginated file (history_mode paginated,
// codex-cli 0.165.0) writes one for each turn item; a legacy file writes none of the kinds read
// here. A UserMessage or AgentMessage is a copy of a response_item message, the one source of
// the prompt and the reply. A CommandExecution is a shell command with its exit_code: for the
// commands of a custom_tool_call exec cell it is the only record. A command that you ran with
// ! (source user_shell) is not the agent's, and a declined one did not run.
//
// A command that runs on in the background can end after the next turn started, so a new
// command goes to the last prompt of its Codex turn (turnID), when that turn is known. Its
// failure output gets its own record id, after the id of the line, as the command can have
// the id of the same line.
func (c *codex) completed(it *codexTurnItem, turnID string, t Text) error {
	if it == nil || it.Type == "" {
		return fmt.Errorf("%w: item_completed with no item type", ErrUnknownFormat)
	}
	if it.Type != "CommandExecution" || it.Source == "user_shell" {
		return nil
	}
	if len(it.Command) == 0 {
		return fmt.Errorf("%w: CommandExecution with no command", ErrUnknownFormat)
	}
	turn, ok := c.turns[turnID]
	if !ok {
		turn = len(c.s.Turns) - 1
	}
	r, ok := c.call(it.ID, turn, Command{ID: t.ID, At: t.At, Command: argv(it.Command)})
	if !ok || it.ExitCode == nil || it.Status == "declined" {
		return nil
	}
	var failure *Text
	if *it.ExitCode != 0 {
		failure = &Text{ID: t.ID + "#output", At: t.At, Text: it.AggregatedOutput}
	}
	c.exit(r, *it.ExitCode, failure)
	return nil
}
