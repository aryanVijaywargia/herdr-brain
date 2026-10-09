package cli

import (
	"bufio"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/aryanVijaywargia/herdr-brain/internal/extract"
	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

// sameAsLinks returns the same_as links that wait for review in a scope that the caller sees
// (#358, extract.Links).
func (cl caller) sameAsLinks(ctx context.Context, tx *sql.Tx) ([]extract.Link, error) {
	ls, err := extract.Links(ctx, tx)
	if err != nil {
		return nil, err
	}
	var seen []extract.Link
	for _, l := range ls {
		if cl.sees(scopeKey(l.Candidate.Scope, l.Candidate.WorkspaceKey)) {
			seen = append(seen, l)
		}
	}
	return seen, nil
}

// linkLines are the lines of the same_as links that wait for review and that the caller sees:
// the text of both claims, the reason, and the two actions (#358).
func (cl caller) linkLines(ctx context.Context, tx *sql.Tx) ([]string, error) {
	ls, err := cl.sameAsLinks(ctx, tx)
	if err != nil {
		return nil, err
	}
	lines := make([]string, len(ls))
	for i, l := range ls {
		lines[i] = fmt.Sprintf("%s\n  merge: brain review --merge %d | keep apart: brain review --apart %d",
			cl.linkView(l), l.Candidate.ID, l.Candidate.ID)
	}
	return lines, nil
}

// linkView is the text of link l: its candidate, its scope and the reason in the first line,
// then the text of each claim with its candidate.
func (cl caller) linkView(l extract.Link) string {
	reason := "an unclear match: Brain did not merge them"
	if l.Permission {
		reason = "a permission or safety rule: Brain merges it only with you"
	}
	other := "no candidate in this scope"
	if l.OtherCandidateID != 0 {
		other = fmt.Sprintf("candidate %d, %d occurrences", l.OtherCandidateID, l.OtherCount)
	}
	return fmt.Sprintf("link %d  %s  %s\n  claim %s: %s (candidate %d, %d occurrences)\n  same as %s: %s (%s)",
		l.Candidate.ID, cl.target(l.Candidate.Scope), reason, l.Claim.ID, oneLine(l.Text), l.Candidate.ID,
		l.Candidate.Count, l.Other.ID, oneLine(l.OtherText), other)
}

// linkCandidate checks that the caller sees candidate id of a link. When other is not "", the
// link must still name claim other: the claim that a pane showed before it waited for your key
// (go-quality rule 14).
func (cl caller) linkCandidate(ctx context.Context, tx *sql.Tx, id int64, other string) error {
	c, err := store.CandidateByID(ctx, tx, id)
	if errors.Is(err, sql.ErrNoRows) || err == nil && !cl.sees(scopeKey(c.Scope, c.WorkspaceKey)) {
		return fmt.Errorf("candidate %d: %w", id, errNotFound)
	}
	if err != nil || other == "" {
		return err
	}
	k, err := store.ClaimByID(ctx, tx, c.ClaimID)
	if err == nil && (k.SameAs != other || k.SameAsStatus != store.SameAsProposed) {
		err = fmt.Errorf("the link of candidate %d changed while you looked at it: look again", id)
	}
	return err
}

// mergeLink runs the review action merge on the same_as link of candidate id (#358,
// extract.MergeLink), which names claim other ("" for any): the two claims mean the same, and
// their evidence counts together. It approves nothing: it prints the candidate that got the
// evidence, with its proposal for your review when it has one, and the proposals of the merged
// claim that it superseded.
func (cl caller) mergeLink(ctx context.Context, id int64, other string) error {
	var lines []string
	gates := cl.gates()
	err := cl.Store.Write(ctx, func(tx *sql.Tx) error {
		lines = nil
		if err := cl.linkCandidate(ctx, tx, id, other); err != nil {
			return err
		}
		m, err := extract.MergeLink(ctx, tx, id, gates, cl.Now())
		if err != nil {
			return err
		}
		for _, p := range m.Superseded {
			lines = append(lines, fmt.Sprintf("proposal %d of the merged claim superseded: the candidate below has its evidence", p))
		}
		for _, to := range m.Candidates {
			c, err := store.CandidateByID(ctx, tx, to)
			if err != nil {
				return err
			}
			l := fmt.Sprintf("candidate %d: %d occurrences, %s", c.ID, c.Count, gateLabel(gates, c))
			ps, err := store.PendingCandidateProposalIDs(ctx, tx, to)
			if err != nil {
				return err
			}
			for _, p := range ps {
				l += fmt.Sprintf("; proposal %d waits for your review (brain review %d)", p, p)
			}
			lines = append(lines, l)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("%w; nothing was written", err)
	}
	_, err = fmt.Fprintf(cl.Stdout, "link of candidate %d merged: the two claims mean the same, and their evidence counts together\n%s",
		id, strings.Join(append(lines, ""), "\n"))
	return err
}

// keepApart runs the review action keep apart on the same_as link of candidate id (#358,
// extract.KeepApart), which names claim other ("" for any): the link ends and does not come
// back for the same pair.
func (cl caller) keepApart(ctx context.Context, id int64, other string) error {
	err := cl.Store.Write(ctx, func(tx *sql.Tx) (err error) {
		if err := cl.linkCandidate(ctx, tx, id, other); err != nil {
			return err
		}
		other, err = extract.KeepApart(ctx, tx, id)
		return err
	})
	if err != nil {
		return fmt.Errorf("%w; nothing was written", err)
	}
	_, err = fmt.Fprintf(cl.Stdout, "link of candidate %d kept apart: it does not come back for review with claim %s\n", id, other)
	return err
}

// The keys of the links list: in the review pane, q goes back to the proposal; in the popup,
// q closes it.
const (
	linksKeys      = "j down  k up  m merge  n keep apart  q back"
	linksPopupKeys = "j down  k up  m merge  n keep apart  q close"
)

// linksPane is the list of the same_as links that wait for review in the review pane, key m,
// and in the popup of the action brain.links, with no proposal (#358), as brain review lists
// them: m merges the link at the cursor, and n keeps it apart, each with one confirm. [NEW
// keys].
type linksPane struct {
	Env
	popup  bool // the list is the popup of brain.links
	keys   io.RuneReader
	size   func() (rows, cols int)
	links  []string // the view of each link
	ids    []int64  // the candidate of each link
	others []string // the claim that each link names, as the view shows it
	cursor int
	msg    string
}

// linksPopup runs `brain pane links`, the popup of the action brain.links (#358): the links
// list of the focused pane's workspace, with no proposal. herdr closes the popup when brain
// exits, so an error shows until a key.
func linksPopup(ctx context.Context, e Env, keys *bufio.Reader, size func() (rows, cols int)) (err error) {
	if e.Environ, err = popupEnv(e.Environ); err == nil {
		err = (&linksPane{Env: e, popup: true, keys: keys, size: size}).run(ctx)
	}
	if err != nil {
		return popupFail(e.Stdout, keys, err)
	}
	return nil
}

// run shows the list and reads keys until q or the end of input. It reads the list again
// before each screen, so a link that another surface decided goes.
func (l *linksPane) run(ctx context.Context) error {
	for {
		if _, err := callPane(ctx, l.Env, func(cl caller) error {
			return cl.Store.Write(ctx, func(tx *sql.Tx) error {
				ls, err := cl.sameAsLinks(ctx, tx)
				if err != nil {
					return err
				}
				l.links, l.ids, l.others = make([]string, len(ls)), make([]int64, len(ls)), make([]string, len(ls))
				for i, k := range ls {
					l.links[i], l.ids[i], l.others[i] = cl.linkView(k), k.Candidate.ID, k.Other.ID
				}
				return nil
			})
		}); err != nil {
			return err
		}
		l.cursor = max(0, min(l.cursor, len(l.links)-1))
		l.draw("")
		k, _, err := l.keys.ReadRune()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil || k == 'q' {
			return err
		}
		if err := l.key(ctx, k); err != nil {
			return err
		}
	}
}

// key runs the action of key k. Only a failed key read is an error: the error of an action is
// on the screen, and the list goes on.
func (l *linksPane) key(ctx context.Context, k rune) error {
	switch k {
	case 'j':
		l.cursor = min(l.cursor+1, len(l.links)-1)
	case 'k':
		l.cursor = max(l.cursor-1, 0)
	case 'm':
		return l.act(ctx, "merge", "the two claims mean the same", caller.mergeLink)
	case 'n':
		return l.act(ctx, "keep apart", "the two claims differ", caller.keepApart)
	}
	return nil
}

// act runs fn on the link at the cursor after one confirm, with the claim that the screen
// showed, so a link that changed meanwhile is not decided.
func (l *linksPane) act(ctx context.Context, verb, means string, fn func(caller, context.Context, int64, string) error) error {
	if len(l.links) == 0 {
		l.msg = "no link to " + verb
		return nil
	}
	id, other := l.ids[l.cursor], l.others[l.cursor]
	l.draw(fmt.Sprintf("%s link %d (%s)? press y to confirm, another key to go back", verb, id, means))
	k, _, err := l.keys.ReadRune()
	if err != nil {
		return err
	}
	if k != 'y' {
		l.msg = "nothing changed"
		return nil
	}
	out, err := callPane(ctx, l.Env, func(cl caller) error { return fn(cl, ctx, id, other) })
	l.msg = out
	if err != nil {
		l.msg = err.Error()
	}
	return nil
}

// draw clears the pane and shows the links, the last message, the keys and prompt. The keys
// stay at the bottom, and a list that does not fit shows the links from the one at the cursor.
func (l *linksPane) draw(prompt string) {
	var foot strings.Builder
	if l.msg != "" {
		fmt.Fprintf(&foot, "\n%s\n", strings.TrimSpace(l.msg))
	}
	keys := linksKeys
	if l.popup {
		keys = linksPopupKeys
	}
	fmt.Fprintf(&foot, "\n%s\n%s", keys, prompt)
	head := "same_as links that wait for review (global and this workspace)"
	var body strings.Builder
	if len(l.links) == 0 {
		fmt.Fprintf(&body, "%s\n\nnone\n", head)
	} else {
		fmt.Fprintf(&body, "%s\n", head)
		for i := l.cursor; i < len(l.links); i++ {
			mark := map[bool]string{true: "> ", false: "  "}[i == l.cursor]
			fmt.Fprintf(&body, "\n%s%s\n", mark, strings.ReplaceAll(l.links[i], "\n", "\n  "))
		}
	}
	height, width := paneDims(l.size)
	_, _ = io.WriteString(l.Stdout, clearScreen+fitView(body.String(), foot.String(), height, width, "")+foot.String()) // a pane with no screen has no one to tell
}

// linkCount is the number of same_as links that wait for review in the view of the caller of e
// (#358).
func linkCount(ctx context.Context, e Env) (n int, err error) {
	_, err = callPane(ctx, e, func(cl caller) error {
		return cl.Store.Write(ctx, func(tx *sql.Tx) error {
			ls, err := cl.sameAsLinks(ctx, tx)
			n = len(ls)
			return err
		})
	})
	return n, err
}
