package transcript

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// codexShell lists the Codex function tools that run a shell command [TX]. A local_shell_call
// is a shell command too.
var codexShell = map[string]bool{"shell": true, "shell_command": true, "exec_command": true, "container.exec": true}

// codexExit is a known exit line of Codex shell output [NEW, TX, §17 item 34: the exact line in
// the installed version is open]: "Exit code: N" of the shell tools and "Process exited with
// code N" of exec_command. Codex saves no exit event, so this line is the only exit status.
var codexExit = regexp.MustCompile(`(?m)^(?:Exit code: |Process exited with code )(-?\d+)\r?$`)

// codexInteractive lists the sources of sessions that you ran (§6.6).
var codexInteractive = map[string]bool{"cli": true, "vscode": true}

// codexLine is one line of a Codex session file (§17 item 34).
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
	Type string `json:"type"`
	Info *struct {
		TotalTokenUsage struct {
			TotalTokens *int64 `json:"total_tokens"`
		} `json:"total_token_usage"`
	} `json:"info"`
}

// codex reads one Codex session file.
type codex struct {
	session
	meta    bool
	total   int64          // the highest total_tokens so far
	pending map[string]ref // the shell call ids that wait for their output
}

// ReadCodex reads the Codex session file at path up to limit bytes (§6.8). The first line is
// the session_meta (§17 item 32).
func ReadCodex(path string, limit int64) (Session, error) {
	c := codex{pending: map[string]ref{}}
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
	switch l.Type {
	case "session_meta":
		err = c.sessionMeta(l.Payload)
		c.at(at, "")
	case "response_item":
		c.at(at, "")
		err = c.item(l.Payload, Text{ID: c.s.ID + "@" + strconv.FormatInt(off, 10), At: at})
	case "event_msg":
		c.at(at, "")
		err = c.event(l.Payload)
	case "turn_context", "compacted":
		// A turn context holds no record of the turn; a compaction summary is dropped (§6.8).
		c.at(at, "")
	default:
		err = fmt.Errorf("%w: line type %q", ErrUnknownFormat, l.Type)
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
// event_msg copy (§6.8). Other item types, as reasoning, hold no prompt, command or reply: Codex
// adds item types over time, so only an item with no type is an unknown shape.
func (c *codex) item(payload json.RawMessage, t Text) error {
	var it codexItem
	if err := json.Unmarshal(payload, &it); err != nil {
		return fmt.Errorf("%w: response_item: %w", ErrUnknownFormat, err)
	}
	if it.Type == "" {
		return fmt.Errorf("%w: response_item with no type", ErrUnknownFormat)
	}
	switch it.Type {
	case "message":
		t.Text = codexText(it)
		switch {
		case t.Text == "":
		case it.Role == "user":
			c.prompt(t)
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
		c.call(it.CallID, Command{ID: t.ID, At: t.At, Command: cmd})
	case "local_shell_call":
		if it.Action == nil || len(it.Action.Command) == 0 {
			return fmt.Errorf("%w: local_shell_call with no command", ErrUnknownFormat)
		}
		c.call(it.CallID, Command{ID: t.ID, At: t.At, Command: argv(it.Action.Command)})
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

// call adds a shell command to the open turn.
func (c *codex) call(id string, cmd Command) {
	if r, ok := c.command(cmd); ok && id != "" {
		c.pending[id] = r
	}
}

// output reads the output of a shell call. The exit status comes only from a known exit line;
// only the output of a failed command is kept (§17 item 34, S88). An output that is not a
// string has no exit line.
func (c *codex) output(it codexItem, t Text) {
	r, ok := c.pending[it.CallID]
	if !ok {
		return
	}
	delete(c.pending, it.CallID)
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
	c.result(r, &n, failure)
}

// event reads an event_msg. Only token_count is read: the turn takes the growth of
// total_tokens, so a repeated line adds nothing (§6.8). A token_count with no info carries no
// usage; an info with no total is an unknown shape, not a total of 0 (S91).
func (c *codex) event(payload json.RawMessage) error {
	var e codexEvent
	if err := json.Unmarshal(payload, &e); err != nil {
		return fmt.Errorf("%w: event_msg: %w", ErrUnknownFormat, err)
	}
	switch {
	case e.Type == "":
		return fmt.Errorf("%w: event_msg with no type", ErrUnknownFormat)
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
