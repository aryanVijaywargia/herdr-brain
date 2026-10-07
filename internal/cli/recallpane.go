package cli

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/aryanVijaywargia/herdr-brain/internal/brief"
	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

// The keys of the recall popup (E20-S3). [NEW] keys: type words, Up and Down (or Ctrl+P and
// Ctrl+N) choose, Enter shows, Esc goes back or closes.
const (
	recallSearchKeys = "type words  Up/Down choose  Enter show  Esc close"
	recallItemKeys   = "Esc back  q close"
	keyCtrlN         = 0x0e
	keyCtrlP         = 0x10
)

// paneRecall runs `brain pane recall`, the recall popup (E20-S3): it searches the items that
// the brief of the focused pane can deliver, as brain recall does (§8.6), and shows one item
// in full with its source excerpts. It only reads: it writes no recall record, which is for an
// agent session, and no decision. herdr runs it in a popup, which has no HERDR_PANE_ID, so the
// caller is the focused pane of the action context (popupEnv).
func paneRecall(ctx context.Context, e Env) error {
	restore, err := cbreak(os.Stdin)
	if err != nil {
		return err
	}
	defer restore()
	return recallPopup(ctx, e, bufio.NewReader(os.Stdin), func() (int, int) { return termSize(os.Stdin) })
}

// recallPopup runs the recall popup with the keys of keys, in a screen of size.
func recallPopup(ctx context.Context, e Env, keys *bufio.Reader, size func() (rows, cols int)) (err error) {
	p := &recallPane{keys: keys, out: e.Stdout, size: size}
	e.Environ, err = popupEnv(e.Environ)
	if err != nil {
		return p.fail(err)
	}
	// No output repair: the popup changes no data (E20-S3 AC 4).
	c, err := e.open(ctx, false)
	if err != nil {
		return p.fail(err)
	}
	defer func() { err = errors.Join(err, c.Close()) }()
	p.cl = newCaller(e, c)
	return p.run(ctx)
}

// popupEnv returns environ with the caller of a popup: a popup gets no HERDR_PANE_ID and no
// HERDR_WORKSPACE_ID, so they come from the focused pane and the workspace of
// HERDR_PLUGIN_CONTEXT_JSON (§4, M0 E5-S3 popup facts). The resolver then asks herdr to
// confirm that pane, as for any call. A pane that has its own HERDR_PANE_ID keeps it.
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
		return nil, errors.New("no HERDR_PANE_ID and no HERDR_PLUGIN_CONTEXT_JSON: open the recall popup from the herdr action brain.recall")
	}
	if err := json.Unmarshal([]byte(raw), &ctx); err != nil {
		return nil, fmt.Errorf("read HERDR_PLUGIN_CONTEXT_JSON: %w", err)
	}
	if ctx.WorkspaceID == "" || ctx.FocusedPaneID == "" {
		return nil, errors.New("the herdr action context has no focused pane: focus a pane, then open the recall popup")
	}
	return append(environ[:len(environ):len(environ)], "HERDR_PANE_ID="+ctx.FocusedPaneID,
		"HERDR_WORKSPACE_ID="+ctx.WorkspaceID), nil
}

// recallHit is one item of a search, with its scope.
type recallHit struct {
	item  brief.Item
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
			}
			continue
		}
		if done := p.searchKey(ctx, k); done {
			return nil
		}
	}
}

// arrow keys, as key returns them. [NEW] runes outside the keys that a terminal sends.
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
			p.view, p.msg = p.show(ctx, p.hits[p.sel].item)
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

// search returns the items that brain recall prints for query, from the calling pane, each
// with its scope. It records nothing.
func (p *recallPane) search(ctx context.Context, query string) ([]recallHit, error) {
	words := recallWords(query)
	if len(words) == 0 {
		return nil, nil
	}
	var hits []recallHit
	err := p.cl.Store.Write(ctx, func(tx *sql.Tx) error {
		items, _, err := recallItems(ctx, tx, p.cl.Call, words)
		if err != nil {
			return err
		}
		for _, it := range items {
			row, err := store.ItemByID(ctx, tx, it.ID)
			if err != nil {
				return err
			}
			hits = append(hits, recallHit{item: it, scope: p.cl.target(row.Scope)})
		}
		return nil
	})
	return hits, err
}

// show returns the full view of item h, or "" and the error. The view has the item as the
// brief shows it, with its marks, its scope and state, the command for a change, and the source
// excerpts of the newest proposal of the item that has captured evidence, through the secret
// filter as in review (§7.4).
func (p *recallPane) show(ctx context.Context, h brief.Item) (view, msg string) {
	var b strings.Builder
	err := p.cl.Store.Write(ctx, func(tx *sql.Tx) error {
		it, cur, err := p.cl.item(ctx, tx, h.ID)
		if err != nil {
			return err
		}
		h.Text = cur.Text // the text can have changed since the search
		text, err := brief.Entries([]brief.Item{h})
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
	var body, foot strings.Builder
	if p.msg != "" {
		fmt.Fprintf(&foot, "\n%s\n", strings.TrimSpace(p.msg))
	}
	if p.view != "" {
		fmt.Fprintf(&foot, "\n%s\n", recallItemKeys)
		_, _ = io.WriteString(p.out, clearScreen+fitView(p.view, foot.String(), rows, cols, "")+foot.String()) // as in reviewPane.draw
		return
	}
	// The same scope rule as brain recall: a workspace with no key gets the global items only.
	scope := "the global items only: this workspace has no Brain key"
	if p.cl.key != "" {
		scope = "the global items and the items of " + p.cl.target(store.ScopeWorkspace)
	}
	fmt.Fprintf(&body, "Brain recall: %s\nwords: %s\n", scope, string(p.query))
	switch {
	case len(recallWords(string(p.query))) == 0:
		body.WriteString("type words to search the approved items\n")
	case len(p.hits) == 0 && p.msg == "":
		fmt.Fprintf(&body, "no Brain item matches %q\n", strings.Join(recallWords(string(p.query)), " "))
	}
	for i, h := range p.hits {
		mark := "  "
		if i == p.sel {
			mark = "> "
		}
		body.WriteString(cut(fmt.Sprintf("%s[%d] %s: %s", mark, h.item.ID, h.scope, oneLine(h.item.Text)), cols) + "\n")
	}
	fmt.Fprintf(&foot, "\n%s\n", recallSearchKeys)
	_, _ = io.WriteString(p.out, clearScreen+fitView(body.String(), foot.String(), rows, cols, "")+foot.String()) // as in reviewPane.draw
}

// cut returns line cut to fit cols columns, with "..." at the end of a cut line; 0 cols keeps
// the line.
func cut(line string, cols int) string {
	if cols <= 3 || utf8.RuneCountInString(line) < cols {
		return line
	}
	return string([]rune(line)[:cols-4]) + "..."
}

// fail shows err in the popup until a key: herdr closes the popup when brain exits, so the
// error that run prints to stderr is gone at once. It returns err.
func (p *recallPane) fail(err error) error {
	_, _ = fmt.Fprintf(p.out, "%s%v\n\nPress a key to close.", clearScreen, err) // as in reviewPane.draw
	_, _, _ = p.keys.ReadRune()                                                  // any key, or the end of input, closes
	return err
}
