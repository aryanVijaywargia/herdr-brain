package brained

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
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
)

// Capture is the Handler that reads panes (E11-S3, HLD §6.2). It keeps the last read of each
// pane in memory only, and it stores the new part of each read as one observation. It only
// reads: it sends no keys and no scroll to any pane.
type Capture struct {
	sock    herdr.Socket
	now     func() time.Time
	profile func(harness string) (*adapter.Profile, error)
	panes   map[string]*capturePane
	noise   map[string]*adapter.Profile // by harness; nil when the harness has no profile
}

// NewCapture returns a Capture that reads panes over sock.
func NewCapture(sock herdr.Socket) *Capture {
	return &Capture{sock: sock, now: time.Now, profile: adapter.Load, panes: map[string]*capturePane{},
		noise: map[string]*adapter.Profile{}}
}

// capturePane is what Capture keeps of one pane.
type capturePane struct {
	agent     string // the harness
	native    string // the agent session id from herdr; "" while it has none
	key       store.SessionKey
	session   int64    // the sessions row; 0 until it is stored
	gen       int64    // the capture generation of the session, noted at the baseline
	segment   int64    // the open segment; 0 when there is none
	workspace string   // the herdr workspace id of the last read
	base      Learning // the learning state of the last baseline
	lines     []string // the last read, without noise and blank lines
	back      []string // the read before lines
	ok        bool     // lines is a usable base
	open      *block   // the injected block that the last read ends in; nil when none
}

// read is one checked pane read.
type read struct {
	pane    herdr.Pane // the pane after the read
	agent   string     // "" when the pane has no agent
	read    herdr.PaneRead
	lines   []string
	senders []store.Sender // the sender of each line (§6.5)
	clean   []string       // lines with the secrets dropped (§6.7)
	full    bool           // the read holds the whole history of the pane
}

// errUnsure says that the pane changed during the read.
var errUnsure = errors.New("the pane changed during the read")

// Handle does one step.
func (c *Capture) Handle(ctx context.Context, s Step) error {
	cp := c.panes[s.PaneID]
	switch s.Kind {
	case Baseline:
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
	case r.agent == "":
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
	err = c.write(ctx, s, func(tx *sql.Tx, now time.Time, learning Learning) error {
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
					Source: store.SourceHerdr, SegmentID: cp.segment, Event: gapLost, Status: store.ObservationGap, At: now})
			}
			return nil // a repaint or a resize: nothing new (S38)
		}
		for i, p := range ps {
			excerpt, truncated := filter(strings.Join(p.lines, "\n"))
			o := store.Observation{CaptureIdentity: fmt.Sprintf("%s/%d", identity(s, cp.segment, now), i+1),
				Source: store.SourceHerdr, SegmentID: cp.segment, Event: eventOf(s), Excerpt: excerpt, Truncated: truncated,
				Status: status, Sender: p.sender, Counts: true, BrainOrigin: p.brain, At: now}
			if status == store.ObservationPartial {
				// A partial read does not start inside the old text, so a text that the segment
				// holds can be old text shown again: Brain cannot tell a replay from new
				// evidence, so it stores it and does not count it (§6.4, S34). A complete read
				// that repeats a text is a real repeat (S40).
				seen, err := store.SeenExcerpt(ctx, tx, cp.segment, p.sender, excerpt)
				if err != nil {
					return err
				}
				o.Counts = !seen
			}
			if err := record(ctx, tx, cp, o); err != nil {
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
	cp.native, cp.lines, cp.back, cp.open = native, r.lines, cp.lines, open
	return nil
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
		Source: store.SourceHerdr, SegmentID: cp.segment, Event: gap, Status: store.ObservationGap, At: now})
}

// record stores observation o of the session of cp and moves the cursor of the session in tx,
// so a crash leaves both or neither (§6.4). tx has run the exclusion check (Capture.write), and
// the caller writes only when the learning state is the one of its base. A known capture
// identity is a retry or a duplicate: it adds nothing and moves no cursor. A gap has no text,
// so it never counts. A hook record with the same text enriches the new observation (S35).
// A forget of the session since the baseline fails the write with store.ErrForgotten.
func record(ctx context.Context, tx *sql.Tx, cp *capturePane, o store.Observation) error {
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
	if err := Match(ctx, tx, cp.agent, cp.session, o); err != nil {
		return err
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
				Source: store.SourceHerdr, SegmentID: cp.segment, Event: gap, Status: store.ObservationGap, At: now})
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
// needs no learning check.
func (c *Capture) end(ctx context.Context, s Step, cp *capturePane) error {
	if cp != nil && cp.session != 0 && !c.held(cp.key, s.PaneID) {
		err := c.write(ctx, s, func(tx *sql.Tx, now time.Time, _ Learning) error {
			return store.EndSession(ctx, tx, cp.key, now)
		})
		if err != nil {
			return err // the state stays, so the next end step closes the session
		}
	}
	delete(c.panes, s.PaneID)
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
		clean: redact(agent, all),
		full:  source == herdr.ReadRecentUnwrapped && raw < lines && !rd.Truncated}, nil
}

// lines splits text into lines without trailing space, and drops blank lines and the noise
// lines of the harness profile.
func (c *Capture) lines(harness, text string) []string {
	p, ok := c.noise[harness]
	if !ok {
		var err error
		if p, err = c.profile(harness); err != nil {
			p = nil // a harness with no profile has no noise patterns
		}
		c.noise[harness] = p
	}
	var ls []string
	for _, l := range strings.Split(text, "\n") {
		l = strings.TrimRightFunc(l, unicode.IsSpace)
		if strings.TrimSpace(l) == "" || p != nil && p.Noise(l) {
			continue
		}
		ls = append(ls, l)
	}
	return ls
}

// newPart returns the indexes of the lines of r that the last read old does not hold, and the status of the
// new part (§6.2 steps 4-5). A line diff keeps each new line, also one with the same text as
// an old line (S40). A new block that is only old text with other line breaks is a resize or
// a rewrap, so it is not new (S38) [NEW, design choice]. The part is complete when the read
// starts inside the old text or holds the whole history; else text may be lost between the
// reads, and it is partial.
//
// A dialog at the bottom of a screen hides the top, and the text comes back when the dialog
// closes. So when the last old line is gone, the lines above the first old line are not new
// if the read back, before old, holds them all [NEW, real Claude read,
// testdata/claude-s31-3-working.txt]. When the last old line stays, the lines above are a
// repeat (S40).
func newPart(old, back []string, r read) ([]int, store.ObservationStatus) {
	matched := lcs(old, r.lines)
	if first := slices.Index(matched, true); first > 0 && !slices.Contains(r.lines, old[len(old)-1]) &&
		!slices.ContainsFunc(r.lines[:first], func(l string) bool { return !slices.Contains(back, l) }) {
		for i := range first {
			matched[i] = true
		}
	}
	oldText := squash(strings.Join(old, ""))
	var added, block []int
	// A rewrap splits lines anew, so a block with its text in the old read and a line that
	// the old read does not hold is a rewrap. A block of old lines only is a repeat (S40).
	flush := func() {
		var text strings.Builder
		for _, i := range block {
			text.WriteString(r.lines[i])
		}
		rewrap := strings.Contains(oldText, squash(text.String())) &&
			slices.ContainsFunc(block, func(i int) bool { return !slices.Contains(old, r.lines[i]) })
		if !rewrap {
			added = append(added, block...)
		}
		block = nil
	}
	for i := range r.lines {
		if matched[i] {
			flush()
			continue
		}
		block = append(block, i)
	}
	flush()
	status := store.ObservationPartial
	if !r.read.Truncated && (len(old) == 0 || r.full || len(r.lines) > 0 && matched[0]) {
		status = store.ObservationComplete
	}
	return added, status
}

// lcs marks the lines of b that are in a longest common subsequence of a and b.
func lcs(a, b []string) []bool {
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
	matched := make([]bool, m)
	for i, j := 0, 0; i < n && j < m; {
		switch {
		case a[i] == b[j]:
			matched[j] = true
			i++
			j++
		case t[i+1][j] >= t[i][j+1]:
			i++
		default:
			j++
		}
	}
	return matched
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
