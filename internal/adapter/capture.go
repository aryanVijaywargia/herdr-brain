package adapter

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// The events of `brain hook --event` (E11-S5, §6.1). Start delivers the brief; the others
// store one hook record.
const (
	EventStart  = "start"
	EventPrompt = "prompt"
	EventTool   = "tool"
	EventStop   = "stop"
)

// Hook is one CLI hook event that runs `brain hook` for a Brain event.
type Hook struct {
	CLIEvent string // the event name in the CLI hook file, for example "PostToolUse"
	Event    string // the Brain event, for example EventTool
	Matcher  string // the tool matcher of the entry; "" for none
}

// Hooks returns the start hook and each hook record event that the profile turns on, in a
// fixed order.
func (p *Profile) Hooks() []Hook {
	hs := []Hook{{CLIEvent: p.Events.Start, Event: EventStart}}
	if p.Events.Prompt != "" {
		hs = append(hs, Hook{CLIEvent: p.Events.Prompt, Event: EventPrompt})
	}
	for _, e := range p.Events.Tool {
		hs = append(hs, Hook{CLIEvent: e, Event: EventTool, Matcher: p.Events.ToolMatcher})
	}
	if p.Events.Stop != "" {
		hs = append(hs, Hook{CLIEvent: p.Events.Stop, Event: EventStop})
	}
	return hs
}

// On reports whether the profile turns on Brain event event.
func (p *Profile) On(event string) bool {
	for _, h := range p.Hooks() {
		if h.Event == event {
			return true
		}
	}
	return false
}

// Record is what the base adapter reads from the input of one hook record event.
type Record struct {
	SessionID string
	CWD       string // "" when the input has none
	Text      string // the prompt or the reply; "" for a tool
	Command   string // the shell command of a tool
	// ExitStatus is the exit status of a tool; nil when the input has none (§6.7).
	ExitStatus *int
}

// ParseRecord reads the input of Brain event event (prompt, tool or stop) by the profile's
// JSON paths. It returns an error that wraps ErrRetest when the input is not a JSON object, or
// when the session id, the prompt or the command has no non-empty string. The cwd, the error
// and the reply are optional: a reply can be null. A prompt that answers a question of the CLI
// gives only the answers (answers).
func (p *Profile) ParseRecord(event string, raw []byte) (Record, error) {
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return Record{}, fmt.Errorf("%w: %w", ErrRetest, err)
	}
	var r Record
	var err error
	if r.SessionID, err = text(doc, p.Input.SessionID); err != nil {
		return Record{}, err
	}
	r.CWD = optional(doc, p.Input.CWD)
	switch event {
	case EventPrompt:
		if r.Text, err = text(doc, p.Input.Prompt); err == nil {
			r.Text, err = p.answers(r.Text)
		}
	case EventTool:
		r.Command, err = text(doc, p.Input.Command)
		if m := p.match(optional(doc, p.Input.Error)); m != "" {
			if n, perr := strconv.Atoi(m); perr == nil {
				r.ExitStatus = &n
			}
		}
	case EventStop:
		r.Text = optional(doc, p.Input.Reply)
	default:
		err = fmt.Errorf("unknown hook event %q", event)
	}
	if err != nil {
		return Record{}, err
	}
	return r, nil
}

// answers returns the text of prompt that you wrote. A prompt that answers a question of the
// CLI starts with the tag of input.question_reply and a JSON list, one object for each
// question: the CLI's question and your answer. Only the answers are yours, one on each line
// (#341). Codex-cli 0.160.1 sends "<send_user_message_question_reply> [{"answer": ...,
// "question": ..., "questionItemId": ...}]" [NEW: the M2 eval run 3 report gives the shape]. A
// list that is not JSON, or an answer that is not a string, is a new input shape: an error that
// wraps ErrRetest.
func (p *Profile) answers(prompt string) (string, error) {
	list, ok := strings.CutPrefix(prompt, p.Input.QuestionReply)
	if p.Input.QuestionReply == "" || !ok {
		return prompt, nil
	}
	var items []struct {
		Answer string `json:"answer"`
	}
	if err := json.Unmarshal([]byte(list), &items); err != nil {
		return "", fmt.Errorf("%w: question reply: %w", ErrRetest, err)
	}
	var as []string
	for _, it := range items {
		if it.Answer != "" {
			as = append(as, it.Answer)
		}
	}
	return strings.Join(as, "\n"), nil
}

// match returns the first group of the exit pattern in s, or "".
func (p *Profile) match(s string) string {
	if p.exit == nil || s == "" {
		return ""
	}
	if m := p.exit.FindStringSubmatch(s); m != nil {
		return m[1]
	}
	return ""
}

// optional returns the string at path in doc, or "" when path is empty or the value is
// missing, null or not a string.
func optional(doc map[string]any, path string) string {
	if path == "" {
		return ""
	}
	v, _ := lookup(doc, path)
	s, _ := v.(string)
	return s
}
