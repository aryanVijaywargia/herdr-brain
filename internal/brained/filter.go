package brained

import (
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
// text there: the screen rules drop the indented lines at the top of a Codex read. Codex draws
// a prompt at once, so a new line with no mark under an old prompt line is no text of that
// prompt either: Codex draws a tall block from its bottom, so the end of tool output or of a
// reply shows under the prompt, with its first line above the screen (#353, M2 eval run 4).
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
	unsure bool // the part is in the head of the read, or under an old prompt line, so its sender is not known
}

// parts drops the injected text from the new lines idx of r and groups the rest into runs of
// one sender. A prompt mark starts a new part, so each prompt is one part (#278), and the head
// of the read is a part of its own. For a harness of inMarks, new lines with no mark under an
// old prompt line are a part of their own too, whose sender is not known (#353). open is the
// block that the last read ended in: the scan runs over the whole read from it, so a block
// that opened in old text or in an earlier read still drops its lines. parts returns the block
// that r ends in. Pane text has no reliable tags: a marker can be lost, and other text can copy
// it. So the filters lower noise; they do not prove clean text (§6.3, S41).
func parts(r read, idx []int, open *block) ([]part, *block) {
	added := make(map[int]bool, len(idx))
	for _, i := range idx {
		added[i] = true
	}
	var ps []part
	in := open
	old := false // the last line with a sender mark is old
	for i, l := range r.lines {
		if added[i] && in == nil && compacted(l) {
			break
		}
		starts := false
		if in == nil {
			in = opens(l)
			starts = in != nil
		}
		if marked(r.agent, l) {
			old = !added[i]
		}
		if added[i] {
			_, prompt := promptLine(r.agent, l)
			unsure := i < r.head || inMarks[r.agent] && old && r.senders[i] == store.SenderUnconfirmed
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
