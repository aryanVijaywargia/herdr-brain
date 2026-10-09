package brained

import (
	"path"
	"regexp"
	"slices"
	"strings"

	"github.com/aryanVijaywargia/herdr-brain/internal/secret"
	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

// block is injected text between a start and an end marker (§6.3).
type block struct{ start, end string }

var (
	// brainBlock is a Brain block or Brain hook output: it carries the brief markers
	// (internal/brief).
	brainBlock = block{"brain:begin", "brain:end"}
	// injected is the text that Brain or the CLI injected. Capture drops it before storage
	// (§6.3). A block with no end line in the read runs to the end of the read.
	injected = []block{brainBlock, {"<environment_context>", "</environment_context>"},
		{"<skills_instructions>", "</skills_instructions>"}}
	// compaction lists text that starts a compaction summary. Capture drops that line and the
	// rest of the read, and the read is the new base: capture starts again from the new screen
	// (§6.3). [NEW, not recorded] Claude Code: the first words of its summary and its screen
	// line; Codex: its screen line.
	compaction = []string{"This session is being continued from a previous conversation", "Conversation compacted",
		"Context compacted"}
)

// mark is a line start that gives the sender of a block of lines (§6.5).
type mark struct {
	prefix string
	sender store.Sender
}

// marks lists the sender marks of each harness [NEW, the recorded reads in testdata]: the
// prompt, the reply and the tool output. A line at column 0 starts a block: its mark gives the
// sender, and with no mark the sender is not confirmed. A line that starts with a space
// continues the block above it, unless it starts with a mark. The prompt mark gives no owner
// mark: a script or another agent can submit text too (§6.3).
var marks = map[string][]mark{
	"claude": {{"❯ ", store.SenderUnconfirmed}, {"⏺ ", store.SenderAssistant}, {"  ⎿", store.SenderTool}},
	"codex":  {{"› ", store.SenderUnconfirmed}, {"• ", store.SenderAssistant}, {"  └", store.SenderTool}},
}

// inMarks lists the harnesses that draw each prompt that you send with the prompt mark, and
// each other line of a prompt with an indent [NEW, #338, M2 eval run 3]. A line at column 0
// with no mark there is no prompt: it is the rest of a line that the terminal wrapped, or text
// that the CLI did not indent, as a line of tool output whose └ is above the read. So it
// continues the block above it, as an indented line does. The head of a read holds no prompt
// text there: the screen rules drop the indented lines at the top of a Codex read.
var inMarks = map[string]bool{"codex": true}

// quoteMark is the start of a Codex prompt line that quotes text: Codex 0.160.1 shows your
// answer to its question as "> {question}", a blank line and the answer, after the prompt mark
// (tui/src/async_question_reply.rs, #341).
const quoteMark = "› > "

// approval matches the question of a Codex approval dialog, and detail the line under it [the
// recorded reads in testdata, and the M2 eval run]: Codex asks it for the agent, so it starts a
// block of the assistant, also under a prompt line, and the dialog is no user text (#281). The
// Claude dialog is in the block of its tool call.
var (
	approval = regexp.MustCompile(`^\s+Would you like to .+\?$`)
	detail   = regexp.MustCompile(`^\s+(?:Environment: |Reason: |\$ )`)
)

// senders returns the sender of each line of a read of harness.
func senders(harness string, lines []string) []store.Sender {
	ss := make([]store.Sender, len(lines))
	sender := store.SenderUnconfirmed
	for i, l := range lines {
		found := false
		for _, m := range marks[harness] {
			if strings.HasPrefix(l, m.prefix) {
				sender, found = m.sender, true
				break
			}
		}
		switch {
		case !found && approval.MatchString(l) && i+1 < len(lines) && detail.MatchString(lines[i+1]):
			sender = store.SenderAssistant
		case !found && !strings.HasPrefix(l, " ") && !inMarks[harness]:
			sender = store.SenderUnconfirmed
		}
		ss[i] = sender
	}
	return ss
}

// head returns the number of lines at the start of a read of harness that continue a block
// whose first line is above the read: lines with no sender mark that start with a space, or
// any line with no mark for a harness of inMarks. Their sender is not known (§6.5): a diff line
// or the rest of a reply, not only user text (#281, #338).
func head(harness string, lines []string) int {
	for i, l := range lines {
		if !strings.HasPrefix(l, " ") && !inMarks[harness] || marked(harness, l) {
			return i
		}
	}
	return len(lines)
}

// marked reports whether line l of harness starts with a sender mark.
func marked(harness, l string) bool {
	return slices.ContainsFunc(marks[harness], func(m mark) bool { return strings.HasPrefix(l, m.prefix) })
}

// call is how the screen of a harness shows a shell command above its tool output (#363).
type call struct {
	// line matches the call line. Its group is the command; a match with no group is a call
	// that only reads files.
	line *regexp.Regexp
	// failed matches the first line of the output of a command that failed; nil when the call
	// line shows the failure.
	failed *regexp.Regexp
	// end is the text that closes the command on its last line, or "".
	end string
}

// calls lists the shell calls of each harness [NEW, the recorded reads in testdata, #363, M2 eval
// run 5]. Codex shows a read, a search or a list of files as "• Explored" with no output, a
// command that worked as "• Ran {command}" and a command that failed as "• Failed (exit N)
// {command}"; the lines of a long command start with │. Claude shows "⏺ Bash({command})", and
// the output of a command that failed starts with "Error:".
var calls = map[string]call{
	"codex":  {line: regexp.MustCompile(`^• (?:Explored|Ran (.+))$`)},
	"claude": {line: regexp.MustCompile(`^⏺ Bash\((.+)$`), failed: regexp.MustCompile(`^  ⎿  Error: `), end: ")"},
}

// readers are the programs that print only the text of files, a list of files or their own
// arguments, or that change the folder [NEW, #363]. A run of them prints no error of a run of
// another program. An error of their own fails the command, unless a pipe hides its exit status.
var readers = []string{"awk", "cat", "cd", "column", "cut", "echo", "egrep", "fd", "fgrep", "find", "grep", "head",
	"jq", "ls", "nl", "printf", "rg", "sed", "sort", "tail", "tr", "tree", "uniq", "wc"}

// fileRead reports whether the tool output at line at of read r is text of files that a command
// printed, as a README that the agent reads (#363): the call line, the nearest line above with
// the mark of another sender, shows a command that worked, and each program of the command is
// one of readers. Such text is no error that a run printed, so it supports no failure (§6.5,
// §7.1). A call line above the read, a command cut with "…", or a program that is not listed
// keeps the output as it is.
func fileRead(r read, at int) bool {
	c, ok := calls[r.agent]
	if !ok {
		return false
	}
	i := at - 1
	for i >= 0 && (r.senders[i] == store.SenderTool || !marked(r.agent, r.lines[i])) {
		i--
	}
	if i < 0 {
		return false
	}
	m := c.line.FindStringSubmatch(r.lines[i])
	switch {
	case m == nil:
		return false
	case m[1] == "":
		return true
	}
	lines := []string{m[1]}
	for i++; i < len(r.lines) && r.senders[i] != store.SenderTool; i++ {
		lines = append(lines, strings.TrimPrefix(strings.TrimSpace(r.lines[i]), "│ "))
	}
	if i == len(r.lines) || c.failed != nil && c.failed.MatchString(r.lines[i]) {
		return false
	}
	cmd, closed := strings.CutSuffix(strings.Join(lines, "\n"), c.end)
	if !closed || strings.Contains(cmd, "…") {
		return false
	}
	for _, s := range commands(cmd) {
		f := strings.Fields(s)
		for len(f) > 0 && strings.Contains(f[0], "=") {
			f = f[1:] // a variable for the command
		}
		if len(f) == 0 {
			continue
		}
		prog := path.Base(f[0])
		if !slices.Contains(readers, prog) || prog == "find" && slices.ContainsFunc(f, runs) {
			return false
		}
	}
	return true
}

// runs reports whether a is an argument of find that runs another program.
func runs(a string) bool { return strings.HasPrefix(a, "-exec") || strings.HasPrefix(a, "-ok") }

// commands splits shell command s at the pipes, the lists (&&, ||, ;, &) and the new lines
// outside quotes. An & of a redirect, as in 2>&1 or &>, does not split it.
func commands(s string) []string {
	var cs []string
	var b strings.Builder
	var quote rune
	rs := []rune(s)
	for i := 0; i < len(rs); i++ {
		r := rs[i]
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			}
		case r == '\'' || r == '"':
			quote = r
		case r == '&' && (i > 0 && (rs[i-1] == '>' || rs[i-1] == '<') || i+1 < len(rs) && rs[i+1] == '>'):
		case r == '|' || r == '&' || r == ';' || r == '\n':
			if i+1 < len(rs) && rs[i+1] == r && r != '\n' {
				i++
			}
			cs = append(cs, b.String())
			b.Reset()
			continue
		}
		b.WriteRune(r)
	}
	return append(cs, b.String())
}

// promptLine returns line l of harness without its prompt mark, and whether l starts with it.
func promptLine(harness, l string) (string, bool) {
	for _, m := range marks[harness] {
		if m.sender == store.SenderUnconfirmed && strings.HasPrefix(l, m.prefix) {
			return l[len(m.prefix):], true
		}
	}
	return l, false
}

// part is one run of new lines with one sender: one observation.
type part struct {
	sender store.Sender
	lines  []string
	brain  bool // the part holds a Brain block, so its text after the block is Brain-origin
	at     int  // the line of the read where the part starts
	// unsure says that the sender of the part is not known: it is in the head of the read, or
	// under a whole known prompt in a Codex read (#353).
	unsure bool
	cut    bool // the first line is a cut copy of a known prompt (copies)
}

// parts drops the injected text from the new lines idx of r and groups the rest into runs of
// one sender. A prompt mark starts a new part, so each prompt is one part (#278), and the head
// of the read is a part of its own. open is the block that the last read ended in: the scan
// runs over the whole read from it, so a block that opened in old text or in an earlier read
// still drops its lines. parts returns the block that r ends in. Pane text has no reliable
// tags: a marker can be lost, and other text can copy it. So the filters lower noise; they do
// not prove clean text (§6.3, S41).
func parts(r read, idx []int, open *block) ([]part, *block) {
	added := make(map[int]bool, len(idx))
	for _, i := range idx {
		added[i] = true
	}
	var ps []part
	in := open
	for i, l := range r.lines {
		if added[i] && in == nil && compacted(l) {
			break
		}
		starts := false
		if in == nil {
			in = opens(l)
			starts = in != nil
		}
		if added[i] {
			_, prompt := promptLine(r.agent, l)
			unsure := i < r.head
			if len(ps) == 0 || ps[len(ps)-1].sender != r.senders[i] || ps[len(ps)-1].unsure != unsure ||
				(starts || prompt) && len(ps[len(ps)-1].lines) > 0 ||
				in == nil && ps[len(ps)-1].brain && !strings.HasPrefix(l, " ") {
				// A Brain block starts its own part, and the part ends at the next line at column
				// 0: the text around it stays genuine.
				ps = append(ps, part{sender: r.senders[i], at: i, unsure: unsure})
			}
			p := &ps[len(ps)-1]
			switch {
			case in != nil:
				p.brain = p.brain || *in == brainBlock
			case r.clean[i] != "":
				p.lines = append(p.lines, r.clean[i])
			}
		}
		if in != nil && strings.Contains(l, in.end) {
			in = nil
		}
	}
	kept := ps[:0]
	for _, p := range ps {
		if len(p.lines) > 0 {
			kept = append(kept, p)
		}
	}
	return kept, in
}

// opens returns the injected block that line l starts, or nil.
func opens(l string) *block {
	for i := range injected {
		if strings.Contains(l, injected[i].start) {
			return &injected[i]
		}
	}
	return nil
}

// compacted reports whether line l starts a compaction summary.
func compacted(l string) bool {
	for _, c := range compaction {
		if strings.Contains(l, c) {
			return true
		}
	}
	return false
}

// redact is the secret filter of §6.7 for the lines of one read of harness. It runs on the
// whole read, before the split into parts, so a secret over two senders still matches. It
// puts the mark [secret] in place of each hit, and a line that only the rest of a hit fills
// becomes "". A secret never refuses the whole read: there is no user to edit and retry
// [NEW]. The search sees each sender mark as spaces, so a secret line after a mark, as
// "› TOKEN=x", matches as it does at the line start.
func redact(harness string, lines []string) []string {
	text := strings.Join(lines, "\n")
	hits := secret.Find(unmarked(harness, text))
	if len(hits) == 0 {
		return lines
	}
	var b strings.Builder
	at := 0
	for _, h := range hits {
		if h.Start < at {
			h.Start = at
		}
		if h.End <= at {
			continue
		}
		b.WriteString(text[at:h.Start])
		b.WriteString("[secret]")
		b.WriteString(strings.Repeat("\n", strings.Count(text[h.Start:h.End], "\n")))
		at = h.End
	}
	b.WriteString(text[at:])
	return strings.Split(b.String(), "\n")
}

// filter cuts an excerpt to excerptLimit characters and says whether it cut (§6.7).
func filter(text string) (string, bool) {
	rs := []rune(text)
	if len(rs) <= excerptLimit {
		return text, false
	}
	return string(rs[:excerptLimit]), true
}

// unmarked returns text with the sender mark at the start of each line changed to spaces of
// the same byte length, so the byte ranges of a secret hit stay the same.
func unmarked(harness, text string) string {
	lines := strings.Split(text, "\n")
	for i, l := range lines {
		for _, m := range marks[harness] {
			if strings.HasPrefix(l, m.prefix) {
				lines[i] = strings.Repeat(" ", len(m.prefix)) + l[len(m.prefix):]
				break
			}
		}
	}
	return strings.Join(lines, "\n")
}
