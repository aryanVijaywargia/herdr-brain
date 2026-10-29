// Package transcript reads past Claude Code and Codex session files for the history scan at
// setup. It parses only: it stores nothing and calls no secret filter. The formats come
// from the CLI docs and source code; no real transcript was read to write it.
package transcript

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// ErrUnknownFormat says that a line of a file has a shape the reader does not know. The reader
// fails closed: the scan skips the whole file and counts it.
var ErrUnknownFormat = errors.New("unknown format")

// Session is one past session of one file.
type Session struct {
	ID  string // the agent session id
	CWD string // the cwd of the first record that has one
	// Interactive says the session is one that you ran: a Codex session with the source cli or
	// vscode. Claude files have no known mark of a claude -p run, so it is always true for
	// Claude.
	Interactive bool
	Fork        bool // a Codex fork (forked_from_id): it copies its parent's records and tokens
	SideChain   bool // a Codex sub-agent session (parent_thread_id)
	First, Last time.Time
	Turns       []Turn
}

// Turn is a prompt and everything up to the next prompt.
type Turn struct {
	Prompt   Text
	Commands []Command
	Reply    Text // the last assistant text of the turn; zero when the turn has none
	// Tokens is the token count of the turn. TokensKnown is false when no usage field of the
	// turn was found: the count is "unknown", never 0.
	Tokens      int64
	TokensKnown bool
}

// Text is the text of one record. ID is the record id of the capture identity.
type Text struct {
	ID   string
	At   time.Time
	Text string
}

// Command is one shell command of the agent.
type Command struct {
	ID      string
	At      time.Time
	Command string
	Exit    *int  // nil when the file has no exit status for it
	Failure *Text // the output of a failed command; nil when it did not fail or no result came
}

// session builds a Session from the records of a file, in file order.
type session struct {
	s Session
}

// at records the time of a record, and the cwd when it is the first one.
func (b *session) at(t time.Time, cwd string) {
	if !t.IsZero() {
		if b.s.First.IsZero() {
			b.s.First = t
		}
		b.s.Last = t
	}
	if b.s.CWD == "" {
		b.s.CWD = cwd
	}
}

// turn returns the open turn, or nil before the first prompt.
func (b *session) turn() *Turn {
	if len(b.s.Turns) == 0 {
		return nil
	}
	return &b.s.Turns[len(b.s.Turns)-1]
}

// prompt starts a new turn.
func (b *session) prompt(t Text) {
	b.s.Turns = append(b.s.Turns, Turn{Prompt: t})
}

// reply sets the last assistant text of the open turn.
func (b *session) reply(t Text) {
	if tn := b.turn(); tn != nil {
		tn.Reply = t
	}
}

// tokens adds n tokens to the open turn.
func (b *session) tokens(n int64) {
	if tn := b.turn(); tn != nil {
		tn.Tokens += n
		tn.TokensKnown = true
	}
}

// command adds c to the open turn. It returns where c is, for its result, and false before the
// first prompt.
func (b *session) command(c Command) (ref, bool) {
	return b.commandIn(len(b.s.Turns)-1, c)
}

// commandIn adds c to the turn at index i. It returns where c is, for its result, and false
// when i is before the first prompt.
func (b *session) commandIn(i int, c Command) (ref, bool) {
	if i < 0 {
		return ref{}, false
	}
	tn := &b.s.Turns[i]
	tn.Commands = append(tn.Commands, c)
	return ref{i, len(tn.Commands) - 1}, true
}

// result sets the exit status and the failure output of the command at r.
func (b *session) result(r ref, exit *int, failure *Text) {
	c := &b.s.Turns[r.turn].Commands[r.cmd]
	c.Exit, c.Failure = exit, failure
}

// ref is the place of a command in a session.
type ref struct{ turn, cmd int }

// lines calls fn with each whole line of the first limit bytes of the file at path and the
// byte offset of the line. A last line with no newline is not yet whole: a session that still
// runs or a cut at the limit leaves it, so it is skipped.
func lines(path string, limit int64, fn func(line []byte, offset int64) error) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close() //nolint:errcheck // read-only: Close cannot lose data
	r := bufio.NewReader(io.LimitReader(f, limit))
	var offset int64
	for n := 1; ; n++ {
		line, err := r.ReadBytes('\n')
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read %s: %w", path, err)
		}
		if err := fn(line, offset); err != nil {
			return fmt.Errorf("%s line %d: %w", path, n, err)
		}
		offset += int64(len(line))
	}
}

// parseTime parses the time of a record; "" gives the zero time.
func parseTime(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("%w: time: %w", ErrUnknownFormat, err)
	}
	return t, nil
}

// injected lists the text blocks that Brain or the CLI injected, by start and end mark. A
// block with no end runs to the end of the text. Claude saves the additionalContext of
// start hooks in a system reminder, so this drops other start hooks' output too.
var injected = [][2]string{
	{"<!-- brain:begin", "<!-- brain:end -->"},
	{"<system-reminder>", "</system-reminder>"},
	{"<environment_context>", "</environment_context>"},
	{"<user_instructions>", "</user_instructions>"},
	{"<skills_instructions>", "</skills_instructions>"},
}

// scaffold lists marks of a whole text that the CLI wrote, not you: a slash command, its local
// output, an interrupt note and Codex AGENTS.md text. Claude also writes a task notification,
// a local command caveat and a ! shell command with its output; Codex writes a ! shell command
// too.
var scaffold = []string{"<command-name>", "<local-command-stdout>", "<task-notification>", "<local-command-caveat>",
	"<bash-input>", "<bash-stdout>", "<user_shell_command>"}

// scaffoldStart lists marks that start a whole text that the CLI wrote.
var scaffoldStart = []string{"[Request interrupted", "# AGENTS.md instructions"}

// clean returns text without its injected blocks, or "" when the CLI wrote all of it.
func clean(text string) string {
	for _, b := range injected {
		for {
			i := strings.Index(text, b[0])
			if i < 0 {
				break
			}
			j := strings.Index(text[i:], b[1])
			if j < 0 {
				text = text[:i]
				break
			}
			text = text[:i] + text[i+j+len(b[1]):]
		}
	}
	text = strings.TrimSpace(text)
	if slices.ContainsFunc(scaffold, func(m string) bool { return strings.Contains(text, m) }) ||
		slices.ContainsFunc(scaffoldStart, func(m string) bool { return strings.HasPrefix(text, m) }) {
		return ""
	}
	return text
}

// Harness names, as the profiles name them.
const (
	Claude = "claude"
	Codex  = "codex"
)

// File is one candidate session file.
type File struct {
	Harness string
	Path    string
	Changed time.Time
}

// Listing is the candidate files of a home, newest first, and the number of compressed Codex
// files that the scan does not read.
type Listing struct {
	Files      []File
	Compressed int
}

// List lists the candidate session files under the Claude home claudeHome and the Codex home
// codexHome: Claude <claude home>/projects/<folder>/<id>.jsonl and Codex
// <codex home>/sessions/YYYY/MM/DD/rollout-*.jsonl, by change time, newest first. The fixed
// depths leave out the Claude subagents/ and tool-results/ folders and Codex
// archived_sessions/. It skips Claude set-aside copies and counts Codex *.jsonl.zst files.
func List(claudeHome, codexHome string) (Listing, error) {
	var l Listing
	claude, err := atDepth(filepath.Join(claudeHome, "projects"), 2)
	if err != nil {
		return Listing{}, err
	}
	for _, p := range claude {
		name := filepath.Base(p)
		if strings.HasSuffix(name, ".jsonl") && !strings.Contains(name, ".orphaned-") &&
			!strings.Contains(name, ".superseded-") {
			if l.Files, err = appendFile(l.Files, Claude, p); err != nil {
				return Listing{}, err
			}
		}
	}
	codex, err := atDepth(filepath.Join(codexHome, "sessions"), 4)
	if err != nil {
		return Listing{}, err
	}
	for _, p := range codex {
		name := filepath.Base(p)
		switch {
		case !strings.HasPrefix(name, "rollout-"):
		case strings.HasSuffix(name, ".jsonl.zst"):
			l.Compressed++
		case strings.HasSuffix(name, ".jsonl"):
			if l.Files, err = appendFile(l.Files, Codex, p); err != nil {
				return Listing{}, err
			}
		}
	}
	slices.SortStableFunc(l.Files, func(a, b File) int { return b.Changed.Compare(a.Changed) })
	return l, nil
}

// appendFile appends the file at path with its change time.
func appendFile(files []File, harness, path string) ([]File, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	return append(files, File{Harness: harness, Path: path, Changed: info.ModTime()}), nil
}

// atDepth returns the regular files exactly depth levels under root.
func atDepth(root string, depth int) ([]string, error) {
	var ps []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			// A home with no Claude or no Codex has no files of it.
			if p == root && errors.Is(err, fs.ErrNotExist) {
				return fs.SkipAll
			}
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		n := len(strings.Split(rel, string(filepath.Separator)))
		switch {
		case d.IsDir() && rel != "." && n >= depth:
			return fs.SkipDir
		case d.Type().IsRegular() && n == depth:
			ps = append(ps, p)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}
	return ps, nil
}
