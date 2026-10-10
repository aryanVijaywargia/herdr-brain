package brained

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/aryanVijaywargia/herdr-brain/internal/adapter"
	"github.com/aryanVijaywargia/herdr-brain/internal/herdr"
	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

const (
	// recentLines is the length of a recent_unwrapped read (§6.2 step 2: about 120 lines).
	recentLines = 120
	// excerptLimit is the longest excerpt in characters (§6.7, §11: ≤ 500 characters).
	excerptLimit = 500
)

// inline lists the agents that draw inline, so their rows go to the scrollback and a
// recent_unwrapped read works while they work (M0 E11-S1b). Every other agent gets a visible
// read: Claude draws in the alternate screen, where a longer read fails while it works and
// scrolls the pane at idle (M0 E11-S1). A visible read never scrolls a pane.
var inline = map[string]bool{"codex": true}

// The events of a gap observation, beside the Gap of a baseline.
const (
	gapNoBase  = "no base"        // a change came without a usable last read
	gapGone    = "pane gone"      // the pane went away before the read returned (S33)
	gapFailed  = "read failed"    // the read failed
	gapMoved   = "pane moved"     // the pane is in another workspace or has a new id (S36)
	gapUnsure  = "unstable read"  // the pane changed during the read
	gapSession = "session change" // another agent or agent session holds the pane
	gapLost    = "screen lost"    // the screen lost the old text, with nothing new
	gapForget  = "forgotten"      // brain forget --session deleted the evidence of the session
	// gapDetected: herdr detected the agent again in a pane with no agent session id, so the
	// last run may have ended (E20-S10).
	gapDetected = "agent detected again"
)

// Capture is the Handler that reads panes (E11-S3, HLD §6.2). It keeps the last read of each
// pane in memory only, and it stores the new part of each read as one observation. It only
// reads: it sends no keys and no scroll to any pane.
type Capture struct {
	sock    herdr.Socket
	environ []string                                                                  // the brained environment; a suggestion process gets it with the pane's herdr variables
	start   func(ctx context.Context, environ []string, d Dispatch) error             // starts a suggestion (Start)
	stale   func(ctx context.Context, g Gate, key string, now func() time.Time) error // the stale check of a workspace (Stale)
	now     func() time.Time
	profile func(harness string) (*adapter.Profile, error)
	panes   map[string]*capturePane
	noise   map[string]*adapter.Profile // by harness; nil when the harness has no profile
}

// NewCapture returns a Capture that reads panes over sock. environ is the brained
// environment.
func NewCapture(sock herdr.Socket, environ []string) *Capture {
	return &Capture{sock: sock, environ: environ, start: Start, stale: Stale, now: time.Now, profile: adapter.Load,
		panes: map[string]*capturePane{}, noise: map[string]*adapter.Profile{}}
}

// capturePane is what Capture keeps of one pane.
type capturePane struct {
	agent     string // the harness
	native    string // the agent session id from herdr; "" while it has none
	key       store.SessionKey
	session   int64         // the sessions row; 0 until it is stored
	gen       int64         // the capture generation of the session, noted at the baseline
	segment   int64         // the open segment; 0 when there is none
	workspace string        // the herdr workspace id of the last read
	base      Learning      // the learning state of the last baseline
	lines     []string      // the last read, without noise and blank lines
	back      []string      // the read before lines
	ok        bool          // lines is a usable base
	open      *block        // the injected block that the last read ends in; nil when none
	reserved  []Dispatch    // the suggestions that the write of the current read reserved
	status    string        // the status of the last step
	turn      int           // the number of turns that the steps showed: a working or blocked status after another one
	sent      []sent        // the prompts that the session shows, from its reads
	hooks     map[int64]int // the turn in which capture first saw each prompt hook record, by id (#349)
}

// sent is one prompt that a read of the session showed: its text without the mark and white
// space, and the turn of that read (#278).
type sent struct {
	text  string
	first string // the text of its first line
	turn  int
}

// read is one checked pane read.
type read struct {
	pane    herdr.Pane // the pane after the read
	agent   string     // "" when the pane has no agent
	read    herdr.PaneRead
	lines   []string
	senders []store.Sender // the sender of each line (§6.5)
	clean   []string       // lines with the secrets dropped (§6.7)
	head    int            // the number of lines at the start whose sender is not known
	full    bool           // the read holds the whole history of the pane
}

// errUnsure says that the pane changed during the read.
var errUnsure = errors.New("the pane changed during the read")

// Handle does one step.
func (c *Capture) Handle(ctx context.Context, s Step) error {
	cp := c.panes[s.PaneID]
	switch s.Kind {
	case Baseline:
		if s.Gap == GapNone && cp != nil && cp.ok && s.Event != nil && s.Event.Kind == herdr.PaneAgentDetected {
			// The pane has a base, so herdr repeated the detection, or lost the release of the
			// last agent (an end drops the base) (E20-S10). With no agent session id, Brain
			// cannot tell a new run of the same harness: the new baseline records the gap.
			if cp.native == "" {
				return c.baseline(ctx, s, gapDetected)
			}
			// The text since the last read is new text, and another agent or session gets its
			// baseline with a gap in change. The agent status stays, so no turn starts.
			ev := *s.Event
			ev.Status = cp.status
			s.Kind, s.Event = Change, &ev
			return c.change(ctx, s, cp)
		}
		return c.baseline(ctx, s, string(s.Gap))
	case Change:
		switch {
		case cp == nil:
			return c.baseline(ctx, s, gapNoBase)
		case !cp.ok:
			return c.baseline(ctx, s, "") // the gap is recorded
		}
		return c.change(ctx, s, cp)
	case End:
		return c.end(ctx, s, cp)
	}
	return fmt.Errorf("unknown step kind %q", s.Kind)
}

// baseline reads the pane and keeps the read as its base. It opens the agent session of the
// pane and starts a new segment, with a gap observation when gap is not "".
func (c *Capture) baseline(ctx context.Context, s Step, gap string) error {
	cp := c.panes[s.PaneID]
	// A forget during the read may delete text that the read shows, so that read is no base
	// of the new generation: the baseline notes the forgets before it reads (§6.7).
	var forgets int64
	err := c.write(ctx, s, func(tx *sql.Tx, _ time.Time, _ Learning) (err error) {
		forgets, err = store.Forgets(ctx, tx)
		return err
	})
	if err != nil {
		return err
	}
	r, err := c.read(ctx, s.PaneID)
	switch {
	case err != nil:
		return c.lose(ctx, s, cp, err)
	case r.agent == "" || nativeID(r.pane) == "" && c.probe(ctx, s.PaneID, r.agent):
		// No agent, or a CLI probe that is no agent session (#317).
		return c.end(ctx, s, cp)
	case r.pane.ID != s.PaneID || r.pane.WorkspaceID != s.WorkspaceID:
		// The pane moved: its new id or workspace gets its own baseline.
		return c.lose(ctx, s, cp, errMoved)
	}
	native := nativeID(r.pane)
	var old *capturePane
	if cp == nil || cp.agent != r.agent || !sameSession(cp.native, native) {
		old = cp
		cp = &capturePane{agent: r.agent, native: native,
			key: store.SessionKey{HerdrSession: s.Gate.Binding.Session, Harness: r.agent, Kind: store.KeyAgent, ID: native}}
		if native == "" {
			id, err := captureID()
			if err != nil {
				return err
			}
			cp.key.Kind, cp.key.ID = store.KeyCapture, id
		}
		c.panes[s.PaneID] = cp
	}
	cp.workspace, cp.base, cp.lines, cp.back, cp.ok, cp.segment = r.pane.WorkspaceID, s.Learning, r.lines, nil, true, 0
	_, cp.open = parts(r, nil, nil)
	cp.step(s)
	all := make([]int, len(r.lines))
	for i := range all {
		all[i] = i
	}
	// The prompts of the base read are known: a later read that shows one again in the same
	// turn shows a copy.
	ps, _ := parts(r, all, nil)
	for _, p := range ps {
		if k := keyOf(r.agent, p); k.mark && p.sender == store.SenderUnconfirmed && !p.brain {
			cp.sent = append(cp.sent, sent{k.text, k.first, cp.turn})
		}
	}
	stale := false
	err = c.write(ctx, s, func(tx *sql.Tx, now time.Time, learning Learning) error {
		if old != nil && old.session != 0 && !c.held(old.key, s.PaneID) {
			if err := store.EndSession(ctx, tx, old.key, now); err != nil {
				return err
			}
		}
		if !sameLearning(learning, s.Learning) || !learning.On {
			return nil // the next change sees the switch and takes a new baseline
		}
		n, err := store.Forgets(ctx, tx)
		if stale = n != forgets; err != nil || stale {
			return err
		}
		return c.begin(ctx, tx, s, cp, r, gap, now)
	})
	if err != nil || !stale {
		return err
	}
	return c.baseline(ctx, s, gap) // a forget ran during the read: read the pane again
}

// change reads the pane and stores its new part as one observation.
func (c *Capture) change(ctx context.Context, s Step, cp *capturePane) error {
	r, err := c.read(ctx, s.PaneID)
	switch {
	case err != nil:
		return c.lose(ctx, s, cp, err)
	case r.agent == "":
		return c.end(ctx, s, cp)
	case r.pane.ID != s.PaneID || r.pane.WorkspaceID != cp.workspace:
		return c.lose(ctx, s, cp, errMoved)
	}
	native := nativeID(r.pane)
	if r.agent != cp.agent || !sameSession(cp.native, native) {
		// Another agent or another run: its text never joins the old session (S33).
		return c.baseline(ctx, s, gapSession)
	}
	added, status := newPart(cp.lines, cp.back, r)
	ps, open := parts(r, added, cp.open)
	cp.step(s)
	cp.reserved = nil
	var fresh []sent
	err = c.write(ctx, s, func(tx *sql.Tx, now time.Time, learning Learning) error {
		// The prompt hook records of the session are prompts that the CLI sent: the pane shows
		// them again too (#349).
		var hooks []hookPrompt
		if cp.session != 0 && slices.ContainsFunc(ps, func(p part) bool { return p.sender == store.SenderUnconfirmed && !p.brain }) {
			var err error
			if hooks, err = cp.hookPrompts(ctx, tx); err != nil {
				return err
			}
		}
		ps = cp.quotes(r, ps, hooks)
		ps, fresh = cp.copies(r, added, ps, hooks)
		if learning.On && cp.session != 0 {
			// The read is no base of a new segment either when a forget ran since the baseline.
			if err := store.CheckGeneration(ctx, tx, cp.session, cp.gen); err != nil {
				return err
			}
		}
		if !sameLearning(learning, cp.base) {
			// Learning switched since the base: drop the read, and it is the new base.
			cp.base, cp.segment = learning, 0
			if !learning.On {
				return nil
			}
			return c.begin(ctx, tx, s, cp, r, string(GapLearning), now)
		}
		if !learning.On {
			return nil
		}
		if cp.native == "" && native != "" {
			// herdr gives the Codex id after the first prompt of a run. The key stays.
			if err := store.EnrichSession(ctx, tx, cp.key, native); err != nil {
				return err
			}
		}
		if cp.segment == 0 {
			return c.begin(ctx, tx, s, cp, r, gapNoBase, now)
		}
		if len(added) == 0 {
			if status == store.ObservationPartial && len(cp.lines) > 0 {
				// The screen lost the old text and shows nothing new, as an alternate screen
				// that closes: the lost text shows as a gap (S38).
				return record(ctx, tx, cp, store.Observation{CaptureIdentity: identity(s, cp.segment, now),
					Source: store.SourceHerdr, SegmentID: cp.segment, Event: gapLost, Status: store.ObservationGap, At: now}, "")
			}
			return nil // a repaint or a resize: nothing new (S38)
		}
		for i, p := range ps {
			if p.unsure && inMarks[cp.agent] {
				// A Codex part whose sender is not known holds no prompt: tool output or a reply at
				// the head of the read (#338), under an old prompt line (#353) or under a cut
				// copy (#349).
				continue
			}
			excerpt, truncated := filter(strings.Join(p.lines, "\n"))
			o := store.Observation{CaptureIdentity: fmt.Sprintf("%s/%d", identity(s, cp.segment, now), i+1),
				Source: store.SourceHerdr, SegmentID: cp.segment, Event: eventOf(s), Excerpt: excerpt, Truncated: truncated,
				Status: status, Sender: p.sender, Counts: true, BrainOrigin: p.brain, At: now}
			copied, err := hookCopy(ctx, tx, cp, p, o, hooks, lastPrompt(r))
			if err != nil {
				return err
			}
			if copied {
				continue // a copy of a sent prompt (#349)
			}
			if status == store.ObservationPartial {
				// A partial read does not start inside the old text, so a text that the segment
				// holds can be old text shown again: Brain cannot tell a replay from new
				// evidence, so it stores it and does not count it (§6.4, S34). A complete read
				// that repeats a text is a real repeat (S40).
				seen, err := store.SeenExcerpt(ctx, tx, cp.segment, p.sender, excerpt)
				if err != nil {
					return err
				}
				// A screen that grows after a resize shows the text of the last reads again,
				// with other line breaks (S38).
				shown := strings.Contains(squash(strings.Join(cp.back, "")+strings.Join(cp.lines, "")),
					squash(strings.Join(p.lines, "")))
				o.Counts = !seen && !shown
			}
			if p.unsure {
				o.Counts = false // the sender is not known (§6.4, #281)
			}
			if err := record(ctx, tx, cp, o, panePrompt(cp.agent, p.lines)); err != nil {
				return err
			}
		}
		return nil
	})
	if errors.Is(err, store.ErrForgotten) {
		// brain forget --session deleted the evidence of the session after the baseline: the
		// read may hold forgotten text, so it is dropped, and a new baseline follows (§6.7).
		return c.baseline(ctx, s, gapForget)
	}
	if err != nil {
		return err
	}
	if !slices.Equal(r.lines, cp.lines) {
		// A read of the same screen keeps back, the screen before it, for a screen that
		// grows again (S38).
		cp.back = cp.lines
	}
	cp.native, cp.lines, cp.open = native, r.lines, open
	cp.sent = append(cp.sent, fresh...)
	return c.suggest(ctx, s, cp)
}

// step notes the status of step s. A working or blocked status after an end of a turn starts
// a new turn: a prompt was sent, or the agent works on its own. An unknown status in a turn
// does not end it, so a repaint after it is no new turn.
func (cp *capturePane) step(s Step) {
	status := eventOf(s)
	if busy(status) && !busy(cp.status) && cp.status != "unknown" {
		cp.turn++
	}
	cp.status = status
}

// busy reports whether status is a status of an agent at work.
func busy(status string) bool {
	return status == "working" || status == "blocked"
}

// copies drops the prompts of ps, the parts of the new lines added of r, that a read showed
// before: one sent prompt is one occurrence (§6.4, #278). Codex shows a prompt again at later
// reads, cut to one line, with a banner or an earlier prompt above it, or with another wrap.
// A prompt that the session shows is a copy when its first line starts the text of a known
// prompt, or the known first line starts it, and its other lines are text of that prompt. A
// copy is a new send only when it is the last prompt of the read and a new turn started since,
// or it has the same text and the read shows the known one too, above it (S40). In a new turn
// the last prompt is the prompt of the turn, also when it is only the start of a known one,
// unless its first line is cut (cp.cut): Codex shows a long prompt again as its first line and
// "…", and a prompt that you send is shown in full (#338, testdata/codex-i278-2-blocked.txt).
// A Codex cut copy is one line, so the lines under it are lines of the next block (#349).
// copies marks a cut copy, and the write drops it unless it is the first read of its own hook
// record (hookCopy). A part with no prompt mark and the sender of a prompt, as the wrapped end
// of a prompt, is dropped when all its lines are text of a known prompt. In Codex, such a part
// under a whole known prompt is the end of a block whose first line is above the screen, so its
// sender is not known (cp.under, #353). copies returns the parts that stay and the prompts that
// they send.
func (cp *capturePane) copies(r read, added []int, ps []part, hooks []hookPrompt) ([]part, []sent) {
	last := lastPrompt(r)
	var kept []part
	var fresh []sent
	for _, p := range ps {
		if p.sender != store.SenderUnconfirmed || p.brain {
			kept = append(kept, p)
			continue
		}
		k := keyOf(r.agent, p)
		if !k.mark {
			if !slices.ContainsFunc(cp.sent, func(s sent) bool { return k.within(s.text) }) {
				p.unsure = p.unsure || inMarks[r.agent] && cp.under(r, added, p.at, hooks)
				kept = append(kept, p)
			}
			continue
		}
		if (len(p.lines) == 1 || inMarks[r.agent]) && cp.cut(r.agent, p.lines[0], hooks) {
			p.cut = true
			kept = append(kept, p)
			continue
		}
		var known []sent
		for _, s := range cp.sent {
			if k.copyOf(s) {
				known = append(known, s)
			}
		}
		again := p.at == last && !p.unsure &&
			(!slices.ContainsFunc(known, func(s sent) bool { return s.turn == cp.turn }) ||
				slices.ContainsFunc(known, func(s sent) bool { return s.text == k.text }) && shownAbove(r, added, k, p.at))
		if len(known) == 0 || again {
			kept = append(kept, p)
			fresh = append(fresh, sent{k.text, k.first, cp.turn})
		}
	}
	return kept, fresh
}

// lastPrompt returns the last line of r that starts with the prompt mark, or -1.
func lastPrompt(r read) int {
	last := -1
	for i, l := range r.lines {
		if _, ok := promptLine(r.agent, l); ok {
			last = i
		}
	}
	return last
}

// under reports whether new line at of r, with no mark, is under a whole known prompt in old
// lines: the lines from the prompt line above it to it hold the text of a known prompt or of a
// prompt hook record, or they are one cut copy. Codex draws a prompt at once, so the line is no
// text of that prompt: Codex draws a tall block from its bottom, and the end of its tool output
// or reply shows under the prompt, with the first line of the block above the screen (#353).
// A new prompt whose first line is the same as an old one, with other lines, is no known prompt.
func (cp *capturePane) under(r read, added []int, at int, hooks []hookPrompt) bool {
	start := at - 1
	for start >= 0 && !marked(r.agent, r.lines[start]) {
		start--
	}
	if start < 0 || slices.ContainsFunc(added, func(i int) bool { return i >= start && i < at }) {
		return false
	}
	if _, ok := promptLine(r.agent, r.lines[start]); !ok {
		return false
	}
	text := keyOf(r.agent, part{lines: r.clean[start:at]}).text
	return at-start == 1 && cp.cut(r.agent, r.lines[start], hooks) ||
		slices.ContainsFunc(cp.sent, func(s sent) bool { return s.text == text }) ||
		slices.ContainsFunc(hooks, func(h hookPrompt) bool { return h.text == text })
}

// cut reports whether prompt line l of harness is a cut copy of a known prompt: it ends with
// "…" and holds at least the first line of a known prompt of more lines (#338), or a start of
// the text of a prompt hook record (#349). A short prompt that ends with "…" is a new send.
func (cp *capturePane) cut(harness, l string, hooks []hookPrompt) bool {
	if !strings.HasSuffix(l, "…") {
		return false
	}
	k := keyOf(harness, part{lines: []string{l}})
	return slices.ContainsFunc(cp.sent, func(s sent) bool {
		return k.copyOf(s) && s.first != s.text && len(k.text) >= len(s.first) && len(k.text) < len(s.text)
	}) || slices.ContainsFunc(hooks, func(h hookPrompt) bool { return len(k.text) < len(h.text) && h.shows(k.text) })
}

// quotes splits a Codex prompt part of ps that starts with a quote of text that the last reads
// showed, outside a prompt line: Codex shows your answer to its question as the quoted question
// and the answer, and its question was on the screen before, in its question view (#341). The
// quote, with the next lines while their text goes on in the shown text, is Codex text; the
// lines after it are yours. At a later read the question view is gone, and Codex may show the
// quote and the answer again: then the quote ends where the rest of the part shows a prompt
// hook record, the answer that Codex sent (#349). A quote of text that the reads did not show
// is your prompt, and it stays. [NEW: the Codex source, tui/src/async_question_reply.rs and the
// question views]
func (cp *capturePane) quotes(r read, ps []part, hooks []hookPrompt) []part {
	if !inMarks[r.agent] {
		return ps
	}
	var shown strings.Builder
	for _, l := range append(slices.Clone(cp.back), cp.lines...) {
		if _, ok := promptLine(r.agent, l); !ok {
			shown.WriteString(squash(l))
		}
	}
	var out []part
	for _, p := range ps {
		q, ok := strings.CutPrefix(p.lines[0], quoteMark)
		quote := strings.TrimSuffix(squash(q), "…")
		n := 0
		switch {
		case !ok || p.sender != store.SenderUnconfirmed || quote == "":
		case strings.Contains(shown.String(), quote):
			n = 1
			for n < len(p.lines) && strings.Contains(shown.String(), quote+squash(p.lines[n])) {
				quote += squash(p.lines[n])
				n++
			}
		default:
			n = answered(p.lines, hooks)
		}
		if n == 0 {
			out = append(out, p)
			continue
		}
		out = append(out, part{sender: store.SenderAssistant, lines: p.lines[:n], brain: p.brain, at: p.at, unsure: p.unsure})
		if n < len(p.lines) {
			out = append(out, part{sender: p.sender, lines: p.lines[n:], brain: p.brain, at: p.at + n, unsure: p.unsure})
		}
	}
	return out
}

// answered returns the first line of lines, after the first one, from which the lines show a
// prompt hook record of hooks, or 0 when none does.
func answered(lines []string, hooks []hookPrompt) int {
	for n := 1; n < len(lines); n++ {
		text := keyOf("", part{lines: lines[n:]}).text
		if slices.ContainsFunc(hooks, func(h hookPrompt) bool { return h.shows(text) }) {
			return n
		}
	}
	return 0
}

// hookPrompt is a prompt hook record of the session: the text of its excerpt, as promptKey
// takes it, whether the excerpt limit cut it, and the turn in which capture first saw it
// (#349).
type hookPrompt struct {
	text      string
	truncated bool
	turn      int
}

// shows reports whether pane prompt text, as promptKey takes it, shows hook prompt h: the text
// of h or a start of it, or the cut excerpt of h at its start.
func (h hookPrompt) shows(text string) bool {
	return text != "" && (strings.HasPrefix(h.text, text) || h.truncated && strings.HasPrefix(text, h.text))
}

// hookPrompts returns the prompt hook records of the session of cp. A record that capture sees
// for the first time gets the current turn.
func (cp *capturePane) hookPrompts(ctx context.Context, tx *sql.Tx) ([]hookPrompt, error) {
	obs, err := store.HookPrompts(ctx, tx, cp.session)
	if err != nil {
		return nil, err
	}
	if cp.hooks == nil {
		cp.hooks = map[int64]int{}
	}
	hs := make([]hookPrompt, len(obs))
	for i, o := range obs {
		turn, ok := cp.hooks[o.ID]
		if !ok {
			turn = cp.turn
			cp.hooks[o.ID] = turn
		}
		hs[i] = hookPrompt{keyOf("", part{lines: strings.Split(o.Excerpt, "\n")}).text, o.Truncated, turn}
	}
	return hs, nil
}

// hookCopy reports whether prompt part p, to be stored as observation o of the session of cp,
// is a copy of a sent prompt that a prompt hook record of hooks holds: one send is one
// occurrence (§6.4, #349). The first read of a sent prompt is the pane counterpart of its hook
// record (S35), and it stays. A cut copy (copies) is a copy. Any other read that shows the text
// of a hook record, or a start of it, is a copy too: a repaint, or the prompt after the read at
// its send failed, when its hook record counts alone. Only the last prompt of the read, last,
// in a turn after the turn in which capture saw each such hook record, is a new send without a
// hook record of its own, as copies takes it: a hook can fail.
func hookCopy(ctx context.Context, tx *sql.Tx, cp *capturePane, p part, o store.Observation, hooks []hookPrompt, last int) (bool, error) {
	if p.sender != store.SenderUnconfirmed || p.brain || p.unsure {
		return false, nil
	}
	k := keyOf(cp.agent, p)
	var shown []hookPrompt
	for _, h := range hooks {
		if h.shows(k.text) {
			shown = append(shown, h)
		}
	}
	switch {
	case p.cut && len(hooks) == 0:
		return true, nil
	case !p.cut && len(shown) == 0,
		!p.cut && k.mark && p.at == last && !slices.ContainsFunc(shown, func(h hookPrompt) bool { return h.turn == cp.turn }):
		return false, nil
	}
	c, err := counterpart(ctx, tx, cp.agent, cp.session, o)
	return c == 0, err
}

// shownAbove reports whether a line of r above at, one that the last read holds too, starts
// a prompt with the text of k.
func shownAbove(r read, added []int, k promptKey, at int) bool {
	for i := range at {
		if _, ok := promptLine(r.agent, r.lines[i]); !ok || slices.Contains(added, i) {
			continue
		}
		end := i + 1
		for end < len(r.lines) && end < at && strings.HasPrefix(r.lines[end], " ") {
			end++
		}
		if keyOf(r.agent, part{lines: r.clean[i:end]}).text == k.text {
			return true
		}
	}
	return false
}

// promptKey is the text of a part without the prompt mark, the white space and the "…" at
// the end of a cut line: the text of each line, of the first line and of all lines.
type promptKey struct {
	mark  bool // the part starts with the prompt mark
	lines []string
	first string
	text  string
}

func keyOf(harness string, p part) promptKey {
	var k promptKey
	for i, l := range p.lines {
		if i == 0 {
			l, k.mark = promptLine(harness, l)
		}
		k.lines = append(k.lines, strings.TrimSuffix(squash(l), "…"))
	}
	if len(k.lines) > 0 {
		k.first = k.lines[0]
	}
	k.text = strings.Join(k.lines, "")
	return k
}

// copyOf reports whether k shows known prompt s: its first line starts the text of s, or the
// cut first line of s starts it, and its other lines are text of s.
func (k promptKey) copyOf(s sent) bool {
	if k.first == "" || !strings.HasPrefix(s.text, k.first) && (s.first == s.text || !strings.HasPrefix(k.first, s.first)) {
		return false
	}
	return promptKey{lines: k.lines[1:]}.within(s.text)
}

// within reports whether each line of k is text of known prompt text.
func (k promptKey) within(text string) bool {
	for _, l := range k.lines {
		if !strings.Contains(text, l) {
			return false
		}
	}
	return true
}

// suggest starts the suggestions that the last write of cp reserved, after its commit: the
// fallback path of §5.5. Each process gets the herdr variables of the pane of s, so it
// resolves the pane again (E2-S3).
func (c *Capture) suggest(ctx context.Context, s Step, cp *capturePane) error {
	var errs []error
	for _, d := range cp.reserved {
		d.Store = s.Gate.Binding.Store
		errs = append(errs, c.start(ctx, paneEnv(c.environ, s.Gate.Binding, s.PaneID, cp.workspace), d))
	}
	cp.reserved = nil
	return errors.Join(errs...)
}

// begin opens the session of cp, starts a new segment and records gap when it is not "".
func (c *Capture) begin(ctx context.Context, tx *sql.Tx, s Step, cp *capturePane, r read, gap string, now time.Time) error {
	key, err := s.Gate.workspaceKey(r.pane.WorkspaceID)
	if err != nil {
		return err
	}
	if cp.session == 0 || cp.key.Kind == store.KeyAgent {
		// An agent key opens its row again after an end; a capture key is stored once,
		// before any evidence.
		if cp.session, err = store.OpenSession(ctx, tx, cp.key, s.PaneID, key, now); err != nil {
			return err
		}
	}
	// The read of a new baseline is never stored, so the text of later reads is collected
	// after this note.
	if cp.gen, err = store.Generation(ctx, tx, cp.session); err != nil {
		return err
	}
	if native := nativeID(r.pane); cp.key.Kind == store.KeyCapture && native != "" {
		if err := store.EnrichSession(ctx, tx, cp.key, native); err != nil {
			return err
		}
	}
	if cp.segment, err = store.StartSegment(ctx, tx, cp.session, s.PaneID, key, r.read.Source, now); err != nil {
		return err
	}
	if gap == "" {
		return nil
	}
	return record(ctx, tx, cp, store.Observation{CaptureIdentity: identity(s, cp.segment, now),
		Source: store.SourceHerdr, SegmentID: cp.segment, Event: gap, Status: store.ObservationGap, At: now}, "")
}

// record stores observation o of the session of cp and moves the cursor of the session in tx,
// so a crash leaves both or neither (§6.4). tx has run the exclusion check (Capture.write), and
// the caller writes only when the learning state is the one of its base. A known capture
// identity is a retry or a duplicate: it adds nothing and moves no cursor. A gap has no text,
// so it never counts. A hook record with the same text enriches the new observation (S35).
// A prompt gets its prompt index row from full, its full text (E14-S1), and a prompt that
// matches a ready proposal reserves its suggestion in cp.reserved (E14-S3). A forget of the
// session since the baseline fails the write with store.ErrForgotten.
func record(ctx context.Context, tx *sql.Tx, cp *capturePane, o store.Observation, full string) error {
	if err := store.CheckGeneration(ctx, tx, cp.session, cp.gen); err != nil {
		return err
	}
	if o.Status == store.ObservationGap {
		o.Counts = false
	}
	id, err := store.AddObservation(ctx, tx, o)
	if err != nil || id == 0 {
		return err
	}
	o.ID = id
	if err := IndexPrompt(ctx, tx, o, full); err != nil {
		return err
	}
	if err := Match(ctx, tx, cp.agent, cp.session, o); err != nil {
		return err
	}
	// The reservation comes after Match: a hook record of the same prompt has reserved it then
	// (S57).
	d, err := Reserve(ctx, tx, o, full, o.At)
	if err != nil {
		return err
	}
	if d.Suggestion != 0 {
		cp.reserved = append(cp.reserved, d)
	}
	return store.MoveCursor(ctx, tx, cp.session, o.Source, o.CaptureIdentity)
}

// errMoved says that the pane has a new id or workspace.
var errMoved = errors.New("the pane moved")

// lose drops the read that failed with err: it records a gap in the open segment, and the
// next change takes a new baseline. A pane that is gone is no error: its end step follows.
func (c *Capture) lose(ctx context.Context, s Step, cp *capturePane, err error) error {
	gap := gapFailed
	switch {
	case errors.Is(err, herdr.ErrPaneNotFound):
		gap = gapGone
	case errors.Is(err, errMoved):
		gap = gapMoved
	case errors.Is(err, errUnsure):
		gap = gapUnsure
	}
	if cp == nil {
		if gap == gapFailed {
			return err
		}
		return nil
	}
	cp.ok, cp.lines, cp.back = false, nil, nil
	if cp.segment != 0 {
		werr := c.write(ctx, s, func(tx *sql.Tx, now time.Time, learning Learning) error {
			if !sameLearning(learning, cp.base) || !learning.On {
				return nil
			}
			return record(ctx, tx, cp, store.Observation{CaptureIdentity: identity(s, cp.segment, now),
				Source: store.SourceHerdr, SegmentID: cp.segment, Event: gap, Status: store.ObservationGap, At: now}, "")
		})
		cp.segment = 0
		if werr != nil && !errors.Is(werr, store.ErrForgotten) {
			return werr // a forgotten session needs no gap: the next change takes a new baseline
		}
	}
	if gap == gapFailed {
		return err
	}
	return nil
}

// end ends the session of the pane and drops its last read. An end writes no text, so it
// needs no learning check. After a session ends, the stale check reads the anchors of the
// items of its workspace (§9.1).
func (c *Capture) end(ctx context.Context, s Step, cp *capturePane) error {
	ended := cp != nil && cp.session != 0 && !c.held(cp.key, s.PaneID)
	if ended {
		err := c.write(ctx, s, func(tx *sql.Tx, now time.Time, _ Learning) error {
			return store.EndSession(ctx, tx, cp.key, now)
		})
		if err != nil {
			return err // the state stays, so the next end step closes the session
		}
	}
	delete(c.panes, s.PaneID)
	if !ended {
		return nil
	}
	key, err := s.Gate.workspaceKey(cp.workspace)
	if err != nil || key == "" {
		return err
	}
	if err := c.stale(ctx, s.Gate, key, c.now); err != nil {
		return fmt.Errorf("stale check of workspace %s: %w", key, err)
	}
	return nil
}

// held says whether a pane other than paneID holds session key k. After a move, the pane
// with the new id holds the session, and the end of the old id must not end it (S36). Its
// new segment has ended the old one.
func (c *Capture) held(k store.SessionKey, paneID string) bool {
	for id, cp := range c.panes {
		if id != paneID && cp.key == k {
			return true
		}
	}
	return false
}

// write runs fn in one store transaction, with the time and the learning state of the pane
// in that transaction (the Handler rule).
func (c *Capture) write(ctx context.Context, s Step, fn func(tx *sql.Tx, now time.Time, learning Learning) error) (err error) {
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	st, err := store.Open(ctx, s.Gate.Binding.Store, busyTimeout)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, st.Close()) }()
	return st.Write(ctx, func(tx *sql.Tx) error {
		learning, err := s.Gate.Learning(ctx, tx, s.PaneID, s.WorkspaceID)
		if err != nil {
			return err
		}
		return fn(tx, c.now().UTC(), learning)
	})
}

// read reads pane id: pane.get, pane.read, pane.get. The read counts only when the pane holds
// the same agent, agent session and workspace before and after it (§6.2 step 3). An agent
// session id that herdr gives during the read keeps the read.
func (c *Capture) read(ctx context.Context, id string) (read, error) {
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	before, err := c.sock.Pane(ctx, id)
	if err != nil {
		return read{}, err
	}
	if before.Agent == nil {
		return read{pane: before}, nil
	}
	agent := *before.Agent
	source, lines := herdr.ReadVisible, 0
	if inline[agent] {
		source, lines = herdr.ReadRecentUnwrapped, recentLines
	}
	rd, err := c.sock.Read(ctx, id, source, lines)
	if err != nil {
		return read{}, err
	}
	after, err := c.sock.Pane(ctx, id)
	if err != nil {
		return read{}, err
	}
	switch {
	case after.ID != before.ID || after.WorkspaceID != before.WorkspaceID || rd.WorkspaceID != before.WorkspaceID:
		return read{}, errMoved
	case after.Agent == nil || *after.Agent != agent || !sameSession(nativeID(before), nativeID(after)):
		return read{}, errUnsure
	}
	all := c.lines(agent, rd.Text)
	raw := strings.Count(strings.TrimRight(rd.Text, "\n"), "\n") + 1
	return read{pane: after, agent: agent, read: rd, lines: all, senders: senders(agent, all),
		clean: redact(agent, all), head: head(agent, all),
		full: source == herdr.ReadRecentUnwrapped && raw < lines && !rd.Truncated}, nil
}

// shell reports whether name is the name of a shell that a pane runs [NEW]: a shell is no
// agent run.
func shell(name string) bool {
	switch name {
	case "sh", "bash", "zsh", "fish", "dash", "ash", "ksh", "mksh", "oksh", "yash", "tcsh", "csh", "nu", "pwsh",
		"elvish", "xonsh":
		return true
	}
	return false
}

// probe reports whether herdr saw a short CLI run as agent in pane id, such as the
// "codex --version" of brain status in a shell pane (#317): each foreground process of the
// pane is a shell, Brain's own CLI or a run with --version or -v, so no agent runs there. A
// real agent is its own foreground process while it runs. The leader of the foreground process
// group is the first process of the command line that the shell runs: when it is Brain's CLI
// or such a run, the line runs no agent either, also when the other processes of a pipe run
// with it, as grep in "brain status | grep codex" (#351). An agent and the tools that it runs
// have the agent, or a script that starts it, as their leader; a process that runs the agent
// in a pipe after Brain's CLI keeps the session. A leader that is no foreground process any
// more has exited: the line ends, and the processes left, as grep after "brain status" in
// "brain status | grep ...", or none, run no agent unless one of them runs it (#380). When
// herdr gives no process group and no process, or no argv, it reports false, and the baseline
// opens the session as before.
func (c *Capture) probe(ctx context.Context, id, agent string) bool {
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	pi, err := c.sock.Processes(ctx, id)
	if err != nil {
		return false
	}
	i := slices.IndexFunc(pi.Processes, func(p herdr.Process) bool { return p.PID == pi.Group })
	if (i >= 0 && probeRun(pi.Processes[i]) || i < 0 && pi.Group != 0) &&
		!slices.ContainsFunc(pi.Processes, func(p herdr.Process) bool { return runsAgent(p, agent) }) {
		return true
	}
	if len(pi.Processes) == 0 {
		return false
	}
	return !slices.ContainsFunc(pi.Processes, func(p herdr.Process) bool {
		return len(p.Argv) == 0 || !probeRun(p) && !shell(program(p))
	})
}

// program returns the program name of foreground process p from its argv, without the "-" of a
// login shell.
func program(p herdr.Process) string {
	return strings.TrimPrefix(filepath.Base(p.Argv[0]), "-")
}

// probeRun reports whether foreground process p is Brain's own CLI or a run with --version or
// -v.
func probeRun(p herdr.Process) bool {
	return len(p.Argv) > 0 &&
		(program(p) == "brain" || slices.ContainsFunc(p.Argv[1:], func(a string) bool { return a == "--version" || a == "-v" }))
}

// runsAgent reports whether foreground process p runs agent, and not as a probe: its program or
// its name is the agent, or it runs the agent as a script path, as "node /usr/local/bin/codex"
// [spike/m0/data/e11s1b-capture.txt]. A word "claude" that grep looks for is no script path.
func runsAgent(p herdr.Process, agent string) bool {
	if len(p.Argv) == 0 || probeRun(p) {
		return false
	}
	return program(p) == agent || p.Name == agent ||
		len(p.Argv) > 1 && strings.Contains(p.Argv[1], "/") && filepath.Base(p.Argv[1]) == agent
}

// lines splits text into lines without trailing space, and drops blank lines. The screen
// rules of the harness profile also cut its side panel and drop its noise (#258).
func (c *Capture) lines(harness, text string) []string {
	p, ok := c.noise[harness]
	if !ok {
		var err error
		if p, err = c.profile(harness); err != nil {
			p = nil // a harness with no profile has no screen rules
		}
		c.noise[harness] = p
	}
	if p != nil {
		return p.Screen(text)
	}
	var ls []string
	for _, l := range strings.Split(text, "\n") {
		if l = strings.TrimRightFunc(l, unicode.IsSpace); strings.TrimSpace(l) != "" {
			ls = append(ls, l)
		}
	}
	return ls
}

// newPart returns the indexes of the lines of r that the last read old does not hold, and the status of the
// new part (§6.2 steps 4-5). A line diff keeps each new line, also one with the same text as
// an old line (S40). Old text with other line breaks is a resize or a rewrap, so it is not new
// (S38) [NEW, design choice]. The part is complete when the read starts inside the old text or
// holds the whole history; else text may be lost between the reads, and it is partial.
//
// A dialog at the bottom of a screen hides the top, and the text comes back when the dialog
// closes. So when the last old line is gone, the lines above the first old line are not new
// if the read back, before old, holds them all [NEW, real Claude read,
// testdata/claude-s31-3-working.txt]. When the last old line stays, the lines above are a
// repeat (S40).
func newPart(old, back []string, r read) ([]int, store.ObservationStatus) {
	const hidden = -2 // a line that a closed dialog hid
	pair := lcs(old, r.lines)
	if first := slices.IndexFunc(pair, func(p int) bool { return p >= 0 }); first > 0 && !slices.Contains(r.lines, old[len(old)-1]) &&
		!slices.ContainsFunc(r.lines[:first], func(l string) bool { return !slices.Contains(back, l) }) {
		for i := range first {
			pair[i] = hidden
		}
	}
	// A rewrap splits lines anew, so lines from the start of a block with text of the old lines
	// that the block replaces, up to the end of an old line, and with a line that old does not
	// hold, are a rewrap: the rest of the block is new, as a new turn after a resize. A block
	// after the last old line is new, also when it repeats old text (S40). A block of old lines
	// only is a repeat (S40).
	prev := -1 // the old line of the last matched line
	rewrap := func(b []int, span squashed) bool {
		var text strings.Builder
		for _, i := range b {
			text.WriteString(squash(r.lines[i]))
		}
		return span.holdsToEnd(text.String()) &&
			slices.ContainsFunc(b, func(i int) bool { return !slices.Contains(old, r.lines[i]) })
	}
	var added, block []int
	flush := func(end int) {
		n := len(block)
		if n > 0 {
			span := squashLines(old[prev+1 : end])
			for n > 0 && !rewrap(block[:n], span) {
				n--
			}
		}
		added = append(added, block[n:]...)
		block = nil
	}
	for i, p := range pair {
		switch {
		case p == hidden:
		case p >= 0:
			flush(p)
			prev = p
		default:
			block = append(block, i)
		}
	}
	flush(len(old))
	// A read that starts with a rewrap of the first old lines starts inside the old text too,
	// also when no line at the top of the screen stays the same over a resize (S38, #316).
	start := len(r.lines) > 0 && (pair[0] != -1 ||
		!slices.Contains(added, 0) && strings.HasPrefix(squashLines(old).text, squash(r.lines[0])))
	status := store.ObservationPartial
	if !r.read.Truncated && (len(old) == 0 || r.full || start) {
		status = store.ObservationComplete
	}
	return added, status
}

// lcs returns for each line of b the index of the same line of a in a longest common
// subsequence of a and b, or -1.
func lcs(a, b []string) []int {
	n, m := len(a), len(b)
	t := make([][]int, n+1)
	for i := range t {
		t[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if a[i] == b[j] {
				t[i][j] = t[i+1][j+1] + 1
			} else {
				t[i][j] = max(t[i+1][j], t[i][j+1])
			}
		}
	}
	pair := make([]int, m)
	for j := range pair {
		pair[j] = -1
	}
	for i, j := 0, 0; i < n && j < m; {
		switch {
		case a[i] == b[j]:
			pair[j] = i
			i++
			j++
		case t[i+1][j] >= t[i][j+1]:
			i++
		default:
			j++
		}
	}
	return pair
}

// squash removes all white space, so a rewrap does not change the text.
func squash(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return r
	}, s)
}

// squashed is the text of lines without white space, with the offsets where a line ends.
type squashed struct {
	text string
	end  map[int]bool
}

func squashLines(lines []string) squashed {
	var b strings.Builder
	s := squashed{end: map[int]bool{}}
	for _, l := range lines {
		b.WriteString(squash(l))
		s.end[b.Len()] = true
	}
	s.text = b.String()
	return s
}

// holdsToEnd reports whether s holds text up to the end of a line, so a new line that is only
// the start of an old line ("• Done" and "• Done.") is not old text.
func (s squashed) holdsToEnd(text string) bool {
	for off := 0; off < len(s.text); off++ {
		i := strings.Index(s.text[off:], text)
		if i < 0 {
			return false
		}
		off += i
		if s.end[off+len(text)] {
			return true
		}
	}
	return false
}

// identity is the capture identity of an observation: the binding, the segment and the read
// time, because herdr events have no id (§6.4).
func identity(s Step, segment int64, now time.Time) string {
	return fmt.Sprintf("%s/%d/%s", s.Gate.Binding.Session, segment, now.Format(time.RFC3339Nano))
}

// eventOf is the event of a change: the new agent status.
func eventOf(s Step) string {
	if s.Event != nil && s.Event.Status != "" {
		return s.Event.Status
	}
	return string(s.Kind)
}

// nativeID returns the agent session id that herdr gives for p, or "".
func nativeID(p herdr.Pane) string {
	if p.AgentSession == nil {
		return ""
	}
	return p.AgentSession.Value
}

// sameSession reports whether agent session id b, read after a, is the same session. A new
// id after none is: herdr gives the Codex id only after the first prompt of a run.
func sameSession(a, b string) bool {
	return a == b || a == ""
}

// sameLearning reports whether two learning states are the same.
func sameLearning(a, b Learning) bool {
	return a.On == b.On && a.Changed.Equal(b.Changed)
}

// captureID returns a new local capture id.
func captureID() (string, error) {
	b := make([]byte, 16) // [NEW] 128 random bits
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("capture id: %w", err)
	}
	return hex.EncodeToString(b), nil
}
