package brained

import (
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
		if !found && !strings.HasPrefix(l, " ") {
			sender = store.SenderUnconfirmed
		}
		ss[i] = sender
	}
	return ss
}

// part is one run of new lines with one sender: one observation.
type part struct {
	sender store.Sender
	lines  []string
	brain  bool // the run held a Brain block, so its other text is Brain-origin
}

// parts drops the injected text from the new lines idx of r and groups the rest into runs of
// one sender. Pane text has no reliable tags: a marker can be lost, and other text can copy
// it. So the filters lower noise; they do not prove clean text (§6.3, S41).
func parts(r read, idx []int) []part {
	var ps []part
	var in *block
	for _, i := range idx {
		l, sender := r.lines[i], r.senders[i]
		if in == nil && compacted(l) {
			break
		}
		if len(ps) == 0 || ps[len(ps)-1].sender != sender {
			ps = append(ps, part{sender: sender})
		}
		p := &ps[len(ps)-1]
		if in == nil {
			in = opens(l)
		}
		if in == nil {
			p.lines = append(p.lines, l)
			continue
		}
		p.brain = p.brain || *in == brainBlock
		if strings.Contains(l, in.end) {
			in = nil
		}
	}
	kept := ps[:0]
	for _, p := range ps {
		if len(p.lines) > 0 {
			kept = append(kept, p)
		}
	}
	return kept
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

// filter is the excerpt filter of §6.7 for a text of harness, after the injected text is
// gone: it drops each secret and puts the mark [secret] in its place, and it cuts the text to
// excerptLimit characters. A secret never refuses the whole read: there is no user to edit and
// retry [NEW]. The search sees each sender mark as spaces, so a secret line after a mark, as
// "› TOKEN=x", matches as it does at the line start.
func filter(harness, text string) (string, bool) {
	hits := secret.Find(unmarked(harness, text))
	if len(hits) > 0 {
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
			at = h.End
		}
		b.WriteString(text[at:])
		text = b.String()
	}
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
