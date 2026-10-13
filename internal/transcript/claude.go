package transcript

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// claudeShell is the name of the Claude shell tool, as the profile's tool matcher names it.
const claudeShell = "Bash"

// claudeExit is the exit line of a failed Claude shell result, the rule of the profile's
// input.exit_pattern (adapters/claude.toml).
var claudeExit = regexp.MustCompile(`^Exit code (\d+)`)

// claudeLine is one line of a Claude session file (§17 item 34).
type claudeLine struct {
	Type             string         `json:"type"`
	Subtype          string         `json:"subtype"`
	UUID             string         `json:"uuid"`
	Timestamp        string         `json:"timestamp"`
	CWD              string         `json:"cwd"`
	IsSidechain      bool           `json:"isSidechain"`
	IsMeta           bool           `json:"isMeta"`
	IsCompactSummary bool           `json:"isCompactSummary"`
	Message          *claudeMessage `json:"message"`
}

type claudeMessage struct {
	ID      string          `json:"id"`
	Model   string          `json:"model"`
	Content json.RawMessage `json:"content"`
	Usage   *claudeUsage    `json:"usage"`
}

type claudeUsage struct {
	InputTokens              int64 `json:"input_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
}

// claudeBlock is one block of message.content.
type claudeBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`
}

// claude reads one Claude session file.
type claude struct {
	session
	usage   map[string]bool // the assistant message ids whose usage is counted
	pending map[string]ref  // the shell tool_use ids that wait for their result
	// replyID is the message id of the reply of the open turn: Claude can write the text
	// blocks of one message on several lines, and the reply is the whole text of the message.
	replyID string
}

// ReadClaude reads the Claude session file at path up to limit bytes (§6.8). The session id is
// the file name: Claude names each file <session id>.jsonl (§17 item 32).
func ReadClaude(path string, limit int64) (Session, error) {
	c := claude{usage: map[string]bool{}, pending: map[string]ref{}}
	c.s.ID = strings.TrimSuffix(filepath.Base(path), ".jsonl")
	c.s.Interactive = true
	if err := lines(path, limit, c.line); err != nil {
		return Session{}, err
	}
	return c.s, nil
}

// line reads one line. A line of a type other than user or assistant gives only its time and
// cwd: system, summary and the other types hold no prompt, command or reply (§17 item 34).
func (c *claude) line(raw []byte, _ int64) error {
	var l claudeLine
	if err := json.Unmarshal(raw, &l); err != nil {
		return fmt.Errorf("%w: %w", ErrUnknownFormat, err)
	}
	if l.Type == "" {
		return fmt.Errorf("%w: no type", ErrUnknownFormat)
	}
	at, err := parseTime(l.Timestamp)
	if err != nil {
		return err
	}
	c.at(at, l.CWD)
	if l.Type != "user" && l.Type != "assistant" {
		return nil
	}
	if l.UUID == "" || l.Message == nil {
		return fmt.Errorf("%w: %s record with no uuid or message", ErrUnknownFormat, l.Type)
	}
	blocks, err := claudeBlocks(l.Message.Content)
	if err != nil {
		return err
	}
	// Side chains, meta records, compaction summaries and synthetic replies are dropped (§6.8).
	if l.IsSidechain || l.IsMeta || l.IsCompactSummary || l.Message.Model == "<synthetic>" {
		return nil
	}
	if l.Type == "user" {
		return c.user(l, at, blocks)
	}
	return c.assistant(l, at, blocks)
}

// claudeBlocks returns the blocks of content: a string is one text block.
func claudeBlocks(content json.RawMessage) ([]claudeBlock, error) {
	var s string
	if err := json.Unmarshal(content, &s); err == nil {
		return []claudeBlock{{Type: "text", Text: s}}, nil
	}
	var bs []claudeBlock
	if err := json.Unmarshal(content, &bs); err != nil {
		return nil, fmt.Errorf("%w: message content: %w", ErrUnknownFormat, err)
	}
	for _, b := range bs {
		if b.Type == "" {
			return nil, fmt.Errorf("%w: content block with no type", ErrUnknownFormat)
		}
	}
	return bs, nil
}

// user reads a user record: a prompt from its text blocks, and the results of shell commands.
// A record that holds only tool results is a tool reply, not a prompt. Images and other blocks
// are dropped.
func (c *claude) user(l claudeLine, at time.Time, blocks []claudeBlock) error {
	var texts []string
	for i, b := range blocks {
		switch b.Type {
		case "text":
			if t := clean(b.Text); t != "" {
				texts = append(texts, t)
			}
		case "tool_result":
			if err := c.toolResult(b, Text{ID: blockID(l.UUID, i), At: at}); err != nil {
				return err
			}
		}
	}
	if len(texts) > 0 {
		c.prompt(Text{ID: l.UUID, At: at, Text: strings.Join(texts, "\n")})
	}
	return nil
}

// toolResult reads the result of a shell command: its exit status from the exit line, when it
// has one. Only the output of a failed command is kept (S88); is_error marks the failure.
func (c *claude) toolResult(b claudeBlock, t Text) error {
	r, ok := c.pending[b.ToolUseID]
	if !ok {
		return nil
	}
	delete(c.pending, b.ToolUseID)
	out, err := claudeResultText(b.Content)
	if err != nil {
		return err
	}
	var exit *int
	if m := claudeExit.FindStringSubmatch(out); m != nil {
		if n, err := strconv.Atoi(m[1]); err == nil {
			exit = &n
		}
	}
	var failure *Text
	if b.IsError {
		t.Text = out
		failure = &t
	}
	c.result(r, exit, failure)
	return nil
}

// claudeResultText returns the text of a tool_result content: a string or a list of blocks.
func claudeResultText(content json.RawMessage) (string, error) {
	if len(bytes.TrimSpace(content)) == 0 {
		return "", nil
	}
	var s string
	if err := json.Unmarshal(content, &s); err == nil {
		return s, nil
	}
	var bs []claudeBlock
	if err := json.Unmarshal(content, &bs); err != nil {
		return "", fmt.Errorf("%w: tool result content: %w", ErrUnknownFormat, err)
	}
	var texts []string
	for _, b := range bs {
		if b.Type == "text" {
			texts = append(texts, b.Text)
		}
	}
	return strings.Join(texts, "\n"), nil
}

// assistant reads an assistant record: its usage once for each message id, its text and its
// shell commands.
func (c *claude) assistant(l claudeLine, at time.Time, blocks []claudeBlock) error {
	if u := l.Message.Usage; u != nil && !c.usage[l.Message.ID] {
		c.usage[l.Message.ID] = true
		c.tokens(u.InputTokens + u.CacheCreationInputTokens + u.CacheReadInputTokens + u.OutputTokens)
	}
	for i, b := range blocks {
		switch {
		case b.Type == "text":
			if t := clean(b.Text); t != "" {
				c.text(l, at, t)
			}
		case b.Type == "tool_use" && b.Name == claudeShell:
			var in struct {
				Command string `json:"command"`
			}
			if err := json.Unmarshal(b.Input, &in); err != nil || in.Command == "" {
				return fmt.Errorf("%w: %s input with no command", ErrUnknownFormat, claudeShell)
			}
			if r, ok := c.command(Command{ID: blockID(l.UUID, i), At: at, Command: in.Command}); ok {
				c.pending[b.ID] = r
			}
		}
	}
	return nil
}

// text adds text t of record l to the reply of the open turn. The text of a later message
// replaces the reply; more text of the same message joins it, and the reply keeps the id of
// its first record.
func (c *claude) text(l claudeLine, at time.Time, t string) {
	tn := c.turn()
	if tn == nil {
		return
	}
	if c.replyID == l.Message.ID && tn.Reply.ID != "" {
		tn.Reply.Text += "\n" + t
		tn.Reply.At = at
		return
	}
	c.replyID = l.Message.ID
	c.reply(Text{ID: l.UUID, At: at, Text: t})
}

// blockID is the record id of block i of the record uuid: one record can hold several commands
// (§6.8, S89).
func blockID(uuid string, i int) string {
	return uuid + "#" + strconv.Itoa(i)
}
