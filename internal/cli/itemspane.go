package cli

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

// The keys of the items list: in the review pane, q goes back to the proposal; in the popup,
// q closes it.
const (
	itemsKeys      = "j down  k up  space select  r restore  a archive  u undo  q back"
	itemsPopupKeys = "j down  k up  space select  r restore  a archive  u undo  q close"
)

// listedItem is one line of the items list: a retired item, or an archive candidate.
type listedItem struct {
	itemRev
	retired bool // false for an archive candidate
	line    string
}

// itemRev is an item and the revision that an action expects as its current one.
type itemRev struct{ id, rev int64 }

// itemsPane is the list of the retired items and the archive candidates in the review pane
// (E20-S7): the global items and those of the caller's workspace, as brain review
// --retired and brain archive show them. You restore or archive the selected items, or the
// item at the cursor, with one confirm, and u undoes the last such action. [NEW keys]. The
// popup of the action brain.retired shows the same list with no proposal (E20-S11).
type itemsPane struct {
	Env
	popup  bool // the list is the popup: only q closes it, as i opens nothing
	closed bool // q at a confirm of the popup: the popup closes, and nothing changes
	keys   io.RuneReader
	size   func() (rows, cols int)
	items  []listedItem
	cursor int
	top    int // the first item on the screen
	picked map[int64]bool
	msg    string
	last   []itemRev // the items of the last action and the revisions it wrote, which u undoes
}

// retiredPopup runs `brain pane retired`, the popup of the action brain.retired (E20-S11): the
// items list of the focused pane's workspace, with no proposal. herdr closes the popup when
// brain exits, so an error shows until a key.
func retiredPopup(ctx context.Context, e Env, keys *bufio.Reader, size func() (rows, cols int)) (err error) {
	if e.Environ, err = popupEnv(e.Environ); err == nil {
		err = (&itemsPane{Env: e, popup: true, keys: keys, size: size}).run(ctx)
	}
	if err != nil {
		return popupFail(e.Stdout, keys, err)
	}
	return nil
}

// run shows the list and reads keys until q, i (not in the popup) or the end of input. It reads the list again
// before each screen, so an item that changed in another surface shows its new state.
func (l *itemsPane) run(ctx context.Context) error {
	l.picked = map[int64]bool{}
	for {
		if _, err := callPane(ctx, l.Env, func(cl caller) (err error) {
			l.items, err = cl.listedItems(ctx)
			return err
		}); err != nil {
			return err
		}
		l.cursor = max(0, min(l.cursor, len(l.items)-1))
		l.draw("")
		k, _, err := l.keys.ReadRune()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil || k == 'q' || (k == 'i' && !l.popup) {
			return err
		}
		if err := l.key(ctx, k); err != nil || l.closed {
			return err
		}
	}
}

// key runs the action of key k. Only a failed key read is an error: the error of an action
// is on the screen, and the list goes on.
func (l *itemsPane) key(ctx context.Context, k rune) error {
	switch k {
	case 'j':
		l.cursor = min(l.cursor+1, len(l.items)-1)
	case 'k':
		l.cursor = max(l.cursor-1, 0)
	case ' ':
		if len(l.items) > 0 {
			id := l.items[l.cursor].id
			l.picked[id] = !l.picked[id]
		}
	case 'r':
		return l.act(ctx, true)
	case 'a':
		return l.act(ctx, false)
	case 'u':
		l.undo(ctx)
	}
	return nil
}

// act restores the chosen retired items, or archives the chosen archive candidates, after
// one confirm.
func (l *itemsPane) act(ctx context.Context, restore bool) error {
	verb, done, kind := "archive", "archived", "an archive candidate"
	if restore {
		verb, done, kind = "restore", "restored", "a retired item"
	}
	ids := l.chosen(restore)
	if len(ids) == 0 {
		l.msg = fmt.Sprintf("select %s to %s", kind, verb)
		return nil
	}
	l.draw(fmt.Sprintf("%s %s? press y to confirm, another key to go back", verb, itemWords(ids)))
	k, _, err := l.keys.ReadRune()
	if err != nil {
		return err
	}
	if k == 'q' && l.popup {
		l.closed = true // the keys say q closes the popup
		return nil
	}
	if k != 'y' {
		l.msg = "nothing changed"
		return nil
	}
	out, err := callPane(ctx, l.Env, func(cl caller) error { return cl.setRetired(ctx, ids, restore) })
	if err != nil {
		l.msg = err.Error()
		return nil //nolint:nilerr // the error is on the screen, and the list goes on
	}
	l.last, l.picked = make([]itemRev, len(ids)), map[int64]bool{}
	for i, r := range ids {
		l.last[i] = itemRev{r.id, r.rev + 1}
	}
	l.msg = fmt.Sprintf("%s %s; u undoes it\n%s", done, itemWords(ids), out)
	return nil
}

// chosen is the selected items of the kind of the action, or the item at the cursor when
// none is selected.
func (l *itemsPane) chosen(restore bool) []itemRev {
	var ids []itemRev
	some := false
	for _, it := range l.items {
		if l.picked[it.id] {
			some = true
			if it.retired == restore {
				ids = append(ids, it.itemRev)
			}
		}
	}
	if !some && len(l.items) > 0 && l.items[l.cursor].retired == restore {
		ids = append(ids, l.items[l.cursor].itemRev)
	}
	return ids
}

// undo undoes the latest revision of each item of the last action, in one transaction.
func (l *itemsPane) undo(ctx context.Context) {
	if len(l.last) == 0 {
		l.msg = "no action to undo"
		return
	}
	out, err := callPane(ctx, l.Env, func(cl caller) error { return cl.undoAll(ctx, l.last) })
	if err != nil {
		l.msg = err.Error()
		return
	}
	l.msg = fmt.Sprintf("undid the last action on %s\n%s", itemWords(l.last), out)
	l.last = nil
}

// callPane runs fn with a caller of surface pane of e, and returns what fn printed: the pane
// owns the screen.
func callPane(ctx context.Context, e Env, fn func(caller) error) (out string, err error) {
	var buf bytes.Buffer
	e.Stdout = &buf
	c, err := e.Open(ctx)
	if err != nil {
		return "", err
	}
	defer func() { err = errors.Join(err, c.Close()) }()
	cl := newCaller(e, c)
	cl.surface = store.SurfacePane
	err = fn(cl)
	return buf.String(), err
}

// draw clears the pane and shows the list, the last message, the keys and prompt. The keys
// stay at the bottom, and a list that does not fit shows the items around the cursor.
func (l *itemsPane) draw(prompt string) {
	var foot strings.Builder
	if l.msg != "" {
		fmt.Fprintf(&foot, "\n%s\n", strings.TrimSpace(l.msg))
	}
	keys := itemsKeys
	if l.popup {
		keys = itemsPopupKeys
	}
	fmt.Fprintf(&foot, "\n%s\n%s", keys, prompt)
	height, width := paneDims(l.size)
	head := "retired items and archive candidates (global and this workspace)"
	var body strings.Builder
	if len(l.items) == 0 {
		fmt.Fprintf(&body, "%s\n\nnone\n", head)
		_, _ = io.WriteString(l.Stdout, clearScreen+body.String()+foot.String()) // a pane with no screen has no one to tell
		return
	}
	free := len(l.items)
	if height > 0 {
		free = max(1, height-footRows(foot.String(), width)-2) // the head and the blank line
	}
	l.top = min(l.top, l.cursor)
	l.top = max(l.top, l.cursor-free+1)
	end := min(len(l.items), l.top+free)
	if free < len(l.items) {
		head = fmt.Sprintf("%d-%d of %d %s", l.top+1, end, len(l.items), head)
	}
	fmt.Fprintf(&body, "%s\n\n", cut(head, width))
	for i := l.top; i < end; i++ {
		it := l.items[i]
		mark := map[bool]string{true: ">", false: " "}[i == l.cursor] + map[bool]string{true: " [x] ", false: " [ ] "}[l.picked[it.id]]
		fmt.Fprintf(&body, "%s\n", cut(mark+it.line, width))
	}
	_, _ = io.WriteString(l.Stdout, clearScreen+body.String()+foot.String()) // as above
}

// maxWords is the most item ids that a message names; more ids show as a count, so the
// message fits a short pane. [NEW] 5.
const maxWords = 5

// itemWords is "item 3", "2 items (3, 5)", or "12 items" above maxWords.
func itemWords(ids []itemRev) string {
	switch {
	case len(ids) == 1:
		return fmt.Sprintf("item %d", ids[0].id)
	case len(ids) > maxWords:
		return fmt.Sprintf("%d items", len(ids))
	}
	s := make([]string, len(ids))
	for i, r := range ids {
		s[i] = fmt.Sprint(r.id)
	}
	return fmt.Sprintf("%d items (%s)", len(ids), strings.Join(s, ", "))
}

// listedItems reads the retired items and then the archive candidates that the caller sees,
// with their text. It changes nothing.
func (cl caller) listedItems(ctx context.Context) ([]listedItem, error) {
	var ls []listedItem
	err := cl.Store.Write(ctx, func(tx *sql.Tx) error {
		ids, err := store.ItemIDsInState(ctx, tx, store.StateRetired)
		if err != nil {
			return err
		}
		for _, id := range ids {
			_, cur, err := cl.item(ctx, tx, id)
			if errors.Is(err, errNotFound) {
				continue
			}
			if err != nil {
				return err
			}
			ls = append(ls, listedItem{itemRev: itemRev{id, cur.Revision}, retired: true,
				line: fmt.Sprintf("item %d  retired    %s  %s", id, cl.target(cur.Scope), oneLine(cur.Text))})
		}
		cs, err := cl.archiveCandidates(ctx, tx)
		if err != nil {
			return err
		}
		for _, c := range cs {
			_, cur, err := cl.item(ctx, tx, c.ItemID)
			if err != nil {
				return err
			}
			ls = append(ls, listedItem{itemRev: itemRev{c.ItemID, cur.Revision}, line: fmt.Sprintf("item %d  candidate  %s  idle %s  %s", c.ItemID,
				cl.target(cur.Scope), age(cl.Now().Sub(c.Since)), oneLine(cur.Text))})
		}
		return nil
	})
	return ls, err
}

// setRetired restores the retired items ids, or retires the live items ids, in one
// transaction, so one undo of each item reverses the whole action. Each item must still be at
// the revision that the list showed.
func (cl caller) setRetired(ctx context.Context, ids []itemRev, restore bool) error {
	err := cl.Store.Write(ctx, func(tx *sql.Tx) error {
		for _, r := range ids {
			if err := cl.atRevision(ctx, tx, r); err != nil {
				return err
			}
			var err error
			if restore {
				_, err = cl.restoreItem(ctx, tx, r.id)
			} else {
				err = cl.retireItem(ctx, tx, r.id)
			}
			if err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return skillError(err)
	}
	return cl.deliver(ctx, nil)
}

// undoAll undoes the revisions ids, the ones that one action wrote, in one transaction.
// An item that changed since then makes the whole undo fail: it would undo that change.
func (cl caller) undoAll(ctx context.Context, ids []itemRev) error {
	err := cl.Store.Write(ctx, func(tx *sql.Tx) error {
		for _, r := range ids {
			if err := cl.atRevision(ctx, tx, r); err != nil {
				return err
			}
			if _, _, _, err := cl.undoItem(ctx, tx, r.id); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	return cl.deliver(ctx, nil)
}

// atRevision fails when item r.id is no longer at revision r.rev: another surface changed it.
func (cl caller) atRevision(ctx context.Context, tx *sql.Tx, r itemRev) error {
	it, _, err := cl.item(ctx, tx, r.id)
	if err == nil && it.CurrentRevision != r.rev {
		err = fmt.Errorf("item %d changed in another surface (revision %d, not %d); nothing was written", r.id, it.CurrentRevision, r.rev)
	}
	return err
}
