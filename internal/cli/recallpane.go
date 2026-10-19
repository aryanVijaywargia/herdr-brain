package cli

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/aryanVijaywargia/herdr-brain/internal/brief"
	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

// The keys of the recall popup (E20-S3): type words, Up and Down (or Ctrl+P and
// Ctrl+N) choose, Enter shows, Esc goes back or closes.
const (
	recallSearchKeys = "type words  Up/Down choose  Enter show  Esc close"
	recallItemKeys   = "Up/Down scroll  Esc back  q close"
	keyCtrlN         = 0x0e
	keyCtrlP         = 0x10
)

// recallPopup runs `brain pane recall`, the recall popup (E20-S3), with the keys of keys, in
// a screen of size: it searches the items that the brief of the focused pane can deliver, as
// brain recall does, and shows one item in full with its source excerpts. It only
// reads: it writes no recall record, which is for an agent session, and no decision. herdr
// runs it in a popup, which has no HERDR_PANE_ID, so the caller is the focused pane of the
// action context (popupEnv).
func recallPopup(ctx context.Context, e Env, keys *bufio.Reader, size func() (rows, cols int)) (err error) {
	p := &recallPane{keys: keys, out: e.Stdout, size: size}
	e.Environ, err = popupEnv(e.Environ)
	if err != nil {
		return p.fail(err)
	}
	// No output repair: the popup changes no data (E20-S3 AC 4).
	c, err := e.open(ctx, openRead)
	if err != nil {
		return p.fail(err)
	}
	defer func() { err = errors.Join(err, c.Close()) }()
	p.cl = newCaller(e, c)
	return p.run(ctx)
}

// popupEnv returns environ with the caller of a popup: a popup gets no HERDR_PANE_ID and no
// HERDR_WORKSPACE_ID, so they come from the focused pane and the workspace of
// HERDR_PLUGIN_CONTEXT_JSON. The resolver then asks herdr to confirm that pane, as for any call. A
// pane that has its own HERDR_PANE_ID keeps it.
func popupEnv(environ []string) ([]string, error) {
	if lookupEnv(environ, "HERDR_PANE_ID") != "" {
		return environ, nil
	}
	var ctx struct {
		WorkspaceID   string `json:"workspace_id"`
		FocusedPaneID string `json:"focused_pane_id"`
	}
	raw := lookupEnv(environ, "HERDR_PLUGIN_CONTEXT_JSON")
	if raw == "" {
		return nil, errors.New("no HERDR_PANE_ID and no HERDR_PLUGIN_CONTEXT_JSON: open the popup from its herdr action (brain.recall, brain.retired, brain.links)")
	}
	if err := json.Unmarshal([]byte(raw), &ctx); err != nil {
		return nil, fmt.Errorf("read HERDR_PLUGIN_CONTEXT_JSON: %w", err)
	}
	if ctx.WorkspaceID == "" || ctx.FocusedPaneID == "" {
		return nil, errors.New("the herdr action context has no focused pane: focus a pane, then open the popup")
	}
	return append(environ[:len(environ):len(environ)], "HERDR_PANE_ID="+ctx.FocusedPaneID,
		"HERDR_WORKSPACE_ID="+ctx.WorkspaceID), nil
}

// recallHit is one result of a search, an item, a skill or a tool, with its scope.
type recallHit struct {
	m     recallMatch
	scope string
}

// recallPane is the screen of the recall popup: the search, or the full view of one item.
type recallPane struct {
	cl    caller
	keys  *bufio.Reader
	out   io.Writer
	query []rune
	hits  []recallHit
	sel   int
	view  string // the full view of the chosen item; "" shows the search
	top   int    // the first line of view that shows
	msg   string // an error of the last search or view
	size  func() (rows, cols int)
}

// run draws the screen and reads keys until Esc on the search, q on an item, or the end of
// input. Each key on the search searches again: the store can change while the popup is open.
func (p *recallPane) run(ctx context.Context) error {
	for {
		p.draw()
		k, err := p.key()
		if errors.Is(err, io.EOF) {
			return nil // the input is gone: nothing more to show
		}
		if err != nil {
			return err
		}
		if p.view != "" {
			switch k {
			case 'q':
				return nil
			case keyEsc, keyDel, keyBS, '\r', '\n':
				p.view = ""
			case keyUp, keyCtrlP:
				p.top = max(0, p.top-1)
			case keyDown, keyCtrlN:
				p.top = min(strings.Count(p.view, "\n")-1, p.top+1)
			}
			continue
		}
		if done := p.searchKey(ctx, k); done {
			return nil
		}
	}
}

// arrow keys, as key returns them: runes outside the keys that a terminal sends.
const (
	keyUp   = unicode.MaxRune + 1
	keyDown = unicode.MaxRune + 2
)

// key reads one key. A terminal sends an arrow as Esc [ A or Esc [ B in one write, so an Esc
// with less than two more bytes buffered is the Esc key.
func (p *recallPane) key() (rune, error) {
	k, _, err := p.keys.ReadRune()
	if err != nil || k != keyEsc || p.keys.Buffered() < 2 {
		return k, err
	}
	b, err := p.keys.Peek(2) // the two bytes are buffered: Peek does not wait
	if err != nil || b[0] != '[' || b[1] != 'A' && b[1] != 'B' {
		return k, err
	}
	if _, err := p.keys.Discard(2); err != nil {
		return 0, err
	}
	if b[1] == 'A' {
		return keyUp, nil
	}
	return keyDown, nil
}

// searchKey runs key k on the search. done is true when the popup closes.
func (p *recallPane) searchKey(ctx context.Context, k rune) (done bool) {
	switch {
	case k == keyEsc:
		return true
	case k == keyUp || k == keyCtrlP:
		p.sel = max(0, p.sel-1)
		return false
	case k == keyDown || k == keyCtrlN:
		p.sel = min(max(0, len(p.hits)-1), p.sel+1)
		return false
	case k == '\r' || k == '\n':
		if len(p.hits) > 0 {
			p.view, p.msg = p.show(ctx, p.hits[p.sel].m)
			p.top = 0
		}
		if p.msg != "" {
			p.hits, p.sel = nil, 0 // a moved pane must not see the old list again
		}
		return false
	case k == keyDel || k == keyBS:
		if len(p.query) == 0 {
			return false
		}
		p.query = p.query[:len(p.query)-1]
	case unicode.IsPrint(k):
		p.query = append(p.query, k)
	default:
		return false
	}
	p.hits, p.sel, p.msg = nil, 0, ""
	if hits, err := p.search(ctx, string(p.query)); err != nil {
		p.msg = err.Error()
	} else {
		p.hits = hits
	}
	return false
}

// search returns the items, skills and tools that brain recall prints for query, from the
// calling pane, each with its scope. It records nothing.
func (p *recallPane) search(ctx context.Context, query string) ([]recallHit, error) {
	words := recallWords(query)
	if len(words) == 0 {
		return nil, nil
	}
	var hits []recallHit
	err := p.cl.Store.Write(ctx, func(tx *sql.Tx) error {
		// The pane can move to another workspace while the popup is open, so check it again
		// before each read, as brain recall does.
		if err := p.cl.recheck(ctx, p.cl.Call); err != nil {
			return err
		}
		ms, err := recallItems(ctx, tx, p.cl.Call, words)
		if err != nil {
			return err
		}
		for _, m := range ms {
			row, err := store.ItemByID(ctx, tx, m.ref.Item)
			if err != nil {
				return err
			}
			hits = append(hits, recallHit{m: m, scope: p.cl.target(row.Scope)})
		}
		return nil
	})
	return hits, err
}

// show returns the full view of result m, or "" and the error. The view has the item, skill or
// tool as the brief shows it, with its marks, its scope and state, the command for a change, and
// the source excerpts of the newest proposal of the item that has captured evidence, through the
// secret filter as in review.
func (p *recallPane) show(ctx context.Context, m recallMatch) (view, msg string) {
	var b strings.Builder
	err := p.cl.Store.Write(ctx, func(tx *sql.Tx) error {
		if err := p.cl.recheck(ctx, p.cl.Call); err != nil {
			return err
		}
		it, cur, err := p.cl.item(ctx, tx, m.ref.Item)
		if err != nil {
			return err
		}
		text, err := p.entry(ctx, tx, m, cur)
		if err != nil {
			return err
		}
		fmt.Fprintf(&b, "item %d (%s, %s), revision %d, %s\n", it.ID, it.Kind, it.State, cur.Revision, p.cl.target(it.Scope))
		b.WriteString(text)
		fmt.Fprintf(&b, "change:   brain review --item %d --edit \"<text>\" | --retire\n", it.ID)
		pid, lines, err := p.sourceExcerpts(ctx, tx, cur)
		if err != nil {
			return err
		}
		if pid == 0 {
			b.WriteString("excerpts: none: no captured source text made this item\n")
			return nil
		}
		fmt.Fprintf(&b, "proposal: brain review %d (its evidence view: brain review %d --evidence)\n", pid, pid)
		if len(lines) == 0 {
			b.WriteString("excerpts: none: the source text is gone, or in another workspace\n")
			return nil
		}
		fmt.Fprintf(&b, "excerpts:\n  %s\n", strings.Join(lines, "\n  "))
		return nil
	})
	if err != nil {
		return "", err.Error()
	}
	return b.String(), ""
}

// entry is result m as the brief shows it now, at revision cur: the text of an item can have
// changed since the search. A skill or a tool shows as the brief has it now, so one that the
// brief of the pane no longer has does not show.
func (p *recallPane) entry(ctx context.Context, tx *sql.Tx, m recallMatch, cur store.ItemRevision) (string, error) {
	if m.kind == "" {
		m.item.Text = cur.Text
		return brief.Entries([]brief.Item{m.item})
	}
	ms, err := recallable(ctx, tx, p.cl.Call)
	if err != nil {
		return "", err
	}
	i := slices.IndexFunc(ms, func(n recallMatch) bool { return n.ref.Item == m.ref.Item })
	switch {
	case i < 0:
		return "", fmt.Errorf("item %d is no longer in the brief of this pane", m.ref.Item)
	case m.kind == store.KindSkill:
		return brief.Skills([]brief.Skill{ms[i].skill}), nil
	}
	return brief.Tools([]brief.Tool{ms[i].tool}), nil
}

// sourceExcerpts returns the newest proposal of the revisions of an item, up to cur, that has
// captured evidence, and its excerpt lines as review shows them; 0 when no proposal has. A
// revision of your own (an edit in review, an undo, brain remember) has none.
func (p *recallPane) sourceExcerpts(ctx context.Context, tx *sql.Tx, cur store.ItemRevision) (int64, []string, error) {
	for rev := cur.Revision; rev >= 1; rev-- {
		r, err := store.ItemRevisionByKey(ctx, tx, cur.ItemID, rev)
		if err != nil {
			return 0, nil, err
		}
		if r.ProposalID == nil {
			continue
		}
		pr, err := store.ProposalByID(ctx, tx, *r.ProposalID)
		if err != nil {
			return 0, nil, err
		}
		lines, captured, err := p.cl.excerpts(ctx, tx, pr)
		if err != nil || captured {
			return pr.ID, lines, err
		}
	}
	return 0, nil, nil
}

// draw clears the popup and shows the search or the item view, the last message and the keys.
func (p *recallPane) draw() {
	rows, cols := 0, 0
	if p.size != nil {
		rows, cols = p.size()
	}
	var foot strings.Builder
	if p.msg != "" {
		fmt.Fprintf(&foot, "\n%s\n", strings.TrimSpace(p.msg))
	}
	var body string
	if p.view != "" {
		fmt.Fprintf(&foot, "\n%s\n", recallItemKeys)
		lines := strings.SplitAfter(strings.TrimSuffix(p.view, "\n"), "\n")
		p.top = min(p.top, lastTop(lines, foot.String(), rows, cols))
		body = strings.Join(lines[p.top:], "")
		if p.top > 0 {
			body = fmt.Sprintf("... %d more lines above\n", p.top) + body
		}
		body = fitView(body, foot.String(), rows, cols, ": Down shows them")
	} else {
		fmt.Fprintf(&foot, "\n%s\n", recallSearchKeys)
		body = p.list(rows, cols, foot.String())
	}
	_, _ = io.WriteString(p.out, clearScreen+body+foot.String()) // as in reviewPane.draw
}

// lastTop is the first line of the view whose lines from there to the end fit in the rows
// above foot, with the line that counts the lines above: Down stops when the end shows.
func lastTop(lines []string, foot string, rows, cols int) int {
	if rows <= 0 {
		return len(lines) - 1
	}
	free := rows - footRows(foot, cols) - 1 // the line that counts the lines above
	top, used := len(lines), 0
	for top > 0 && used+lineRows(lines[top-1], cols) <= free {
		top--
		used += lineRows(lines[top], cols)
	}
	return min(top, len(lines)-1)
}

// footRows is the rows that foot takes, with the cursor row after its last newline, as
// fitView counts them.
func footRows(foot string, cols int) int {
	n := 0
	for _, l := range strings.Split(foot, "\n") {
		n += lineRows(l, cols)
	}
	return n
}

// list is the search screen above foot: the heading, the words, and the hits that fit, with the
// chosen one always among them.
func (p *recallPane) list(rows, cols int, foot string) string {
	var b strings.Builder
	// The same scope rule as brain recall: a workspace with no key gets the global items only.
	scope := "the global items only: this workspace has no Brain key"
	if p.cl.key != "" {
		scope = "the global items and the items of " + p.cl.target(store.ScopeWorkspace)
	}
	fmt.Fprintf(&b, "Brain recall: %s\nwords: %s\n", scope, string(p.query))
	switch {
	case len(recallWords(string(p.query))) == 0:
		b.WriteString("type words to search the approved items\n")
	case len(p.hits) == 0 && p.msg == "":
		fmt.Fprintf(&b, "no Brain item matches %q\n", strings.Join(recallWords(string(p.query)), " "))
	}
	// Each hit takes one row: cut keeps it to the width.
	free := max(1, len(p.hits))
	if rows > 0 {
		free = max(1, rows-footRows(b.String()+foot, cols))
	}
	first := max(0, p.sel-free+1)
	for i, h := range p.hits[first:min(len(p.hits), first+free)] {
		mark := "  "
		if first+i == p.sel {
			mark = "> "
		}
		b.WriteString(cut(fmt.Sprintf("%s[%d] %s: %s", mark, h.m.ref.Item, h.scope, h.m.line()), cols) + "\n")
	}
	return b.String()
}

// line is result m on one line in the search list: the text of an item, or "skill" or "tool",
// the name and the description.
func (m recallMatch) line() string {
	switch m.kind {
	case store.KindSkill:
		return oneLine("skill " + m.skill.Name + ": " + m.skill.Description)
	case store.KindTool:
		return oneLine("tool " + m.tool.Name + ": " + m.tool.Description)
	}
	return oneLine(m.item.Text)
}

// cut returns line cut to fit cols columns, with "..." at the end of a cut line; 0 cols keeps
// the line.
func cut(line string, cols int) string {
	if cols <= 3 || utf8.RuneCountInString(line) < cols {
		return line
	}
	return string([]rune(line)[:cols-4]) + "..."
}

// fail shows err in the popup until a key, and returns it.
func (p *recallPane) fail(err error) error { return popupFail(p.out, p.keys, err) }

// popupFail shows err in a popup on out until a key of keys: herdr closes the popup when brain
// exits, so the error that run prints to stderr is gone at once. It returns err.
func popupFail(out io.Writer, keys io.RuneReader, err error) error {
	_, _ = fmt.Fprintf(out, "%s%v\n\nPress a key to close.", clearScreen, err) // as in reviewPane.draw
	_, _, _ = keys.ReadRune()                                                  // any key, or the end of input, closes
	return err
}
