package cli

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/aryanVijaywargia/herdr-brain/internal/secret"
	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

const paneUsage = `usage: brain pane review --proposal <id>`

// linger is how long the pane shows its last message before it exits, and herdr closes it.
// [NEW] 2 s.
const linger = 2 * time.Second

const (
	clearScreen = "\x1b[H\x1b[2J" // cursor home, clear the screen
	keyEsc      = 0x1b
	keyDel      = 0x7f
	keyBS       = 0x08
)

// Pane runs `brain pane` with args after "pane" (E5-S1): the review pane of one proposal.
// herdr runs it in a plugin pane, and the proposal id comes from the pane's --env (§5.1).
func Pane(ctx context.Context, e Env, args []string) error {
	id, err := parsePane(args)
	if err != nil {
		return err
	}
	restore, err := cbreak(os.Stdin)
	if err != nil {
		return err
	}
	defer restore()
	return (&reviewPane{Env: e, id: id, keys: bufio.NewReader(os.Stdin), linger: linger}).run(ctx)
}

func parsePane(args []string) (int64, error) {
	if len(args) == 0 || args[0] != "review" {
		return 0, errors.New(paneUsage)
	}
	fs := flag.NewFlagSet("pane review", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	id := fs.String("proposal", "", "")
	if err := fs.Parse(args[1:]); err != nil {
		return 0, fmt.Errorf("%w\n%s", err, paneUsage)
	}
	n, err := strconv.ParseInt(*id, 10, 64)
	if fs.NArg() != 0 || err != nil || n <= 0 {
		return 0, fmt.Errorf("bad proposal id %q\n%s", *id, paneUsage)
	}
	return n, nil
}

// cbreak makes the terminal f send each key at once, with no echo, and returns the function
// that sets the old mode back. Plain stty: no new dependency (E5-S1).
func cbreak(f *os.File) (func(), error) {
	stty := func(args ...string) ([]byte, error) {
		c := exec.Command("stty", args...)
		c.Stdin = f
		return c.Output()
	}
	old, err := stty("-g")
	if err != nil {
		return nil, fmt.Errorf("the review pane needs a terminal: %w", err)
	}
	if _, err := stty("-icanon", "-echo"); err != nil {
		return nil, fmt.Errorf("the review pane needs a terminal: %w", err)
	}
	return func() { _, _ = stty(strings.TrimSpace(string(old))) }, nil
}

// reviewPane is the screen of one proposal. Each key runs `brain review` args with surface
// pane, so the pane has the E4 checks and nothing else: the edit, the scope and the pin are
// only arguments of the approve.
type reviewPane struct {
	Env
	id     int64
	keys   io.RuneReader
	linger time.Duration
	view   string      // the E4-S3 view of the proposal, read before each screen
	scope  store.Scope // "" keeps the proposed scope
	pin    bool
	// conflict is the approve choice for a conflict: "" (keep both, with the mark), replace or
	// exception [NEW key x].
	conflict string
	msg      string // the result of the last key
	dispute  bool   // the proposal is a dispute (§9.1): the pane offers keep, edit and retire
}

// run shows the proposal and reads keys until a decision, q or the end of input. It reads
// the proposal again before each screen, so a forget or a decision in another surface while
// the pane waits for a key never shows the old text again (E5-S1).
func (p *reviewPane) run(ctx context.Context) error {
	for {
		view, err := p.review(ctx)
		if err != nil {
			return p.close(ctx, err)
		}
		p.view, p.dispute = view, strings.Contains(view, "\n"+disputeLine)
		p.draw("")
		k, _, err := p.keys.ReadRune()
		if errors.Is(err, io.EOF) {
			return nil // the input is gone: nothing more to decide
		}
		if err != nil || k == 'q' {
			return err
		}
		done, err := p.key(ctx, k)
		if done || errors.Is(err, errDecided) {
			return p.close(ctx, err)
		}
	}
}

// key runs the action of key k. done is true when the proposal is decided, or stays in the
// queue with later. Any other error is on the screen, and the pane goes on.
func (p *reviewPane) key(ctx context.Context, k rune) (done bool, err error) {
	if p.dispute {
		return p.disputeKey(ctx, k)
	}
	switch k {
	case 'a':
		return p.approve(ctx, nil)
	case 'e':
		return p.edit(ctx)
	case 'c':
		return p.change(ctx)
	case 'x':
		p.conflict = map[string]string{"": conflictReplace, conflictReplace: conflictException}[p.conflict]
		p.msg = ""
	case 'r':
		return p.decide(ctx, "--reject")
	case 'l':
		return p.decide(ctx, "--later")
	case 's':
		p.scope = map[store.Scope]store.Scope{"": store.ScopeGlobal, store.ScopeGlobal: store.ScopeWorkspace}[p.scope]
		p.msg = ""
	case 'p':
		p.pin, p.msg = !p.pin, ""
	}
	return false, nil
}

// disputeKey runs the action of key k on a dispute: keep, edit then approve, retire or later
// (§9.1). [NEW keys k and t]. The other keys do nothing: reject is refused for a dispute.
func (p *reviewPane) disputeKey(ctx context.Context, k rune) (done bool, err error) {
	switch k {
	case 'k':
		return p.decide(ctx, "--keep")
	case 'e':
		return p.edit(ctx)
	case 't':
		return p.decide(ctx, "--retire")
	case 'l':
		return p.decide(ctx, "--later")
	}
	return false, nil
}

// decide runs `brain review <id> args` and shows its output, or its error.
func (p *reviewPane) decide(ctx context.Context, args ...string) (bool, error) {
	return p.show(p.review(ctx, args...))
}

// show shows out, or err, and reports whether the call is done.
func (p *reviewPane) show(out string, err error) (bool, error) {
	p.msg = out
	if err != nil {
		p.msg = err.Error()
		return false, err
	}
	return true, nil
}

// review runs `brain review <id> args` with surface pane and returns its output.
func (p *reviewPane) review(ctx context.Context, args ...string) (string, error) {
	var out bytes.Buffer
	e := p.Env
	e.Stdout = &out
	err := runReview(ctx, e, store.SurfacePane, append([]string{strconv.FormatInt(p.id, 10)}, args...))
	return out.String(), err
}

// approve approves the proposal with edit, the scope and the pin. On a permission warning it
// asks for y first (§7.4). done is true when the proposal is decided.
func (p *reviewPane) approve(ctx context.Context, edit *string) (done bool, err error) {
	args := []string{"--approve"}
	if edit != nil {
		args = append(args, "--edit", *edit)
	}
	if p.scope != "" {
		args = append(args, "--scope", string(p.scope))
	}
	if p.pin {
		args = append(args, "--pin")
	}
	if p.conflict != "" {
		args = append(args, "--"+p.conflict)
	}
	out, err := p.review(ctx, args...)
	if !errors.Is(err, errConfirm) {
		return p.show(out, err)
	}
	p.draw("this may change a permission, an allow rule or a safety rule: press y to approve, another key to go back")
	k, _, rerr := p.keys.ReadRune()
	if rerr != nil {
		return false, rerr
	}
	if k != 'y' {
		p.msg = "not approved"
		return false, nil
	}
	return p.decide(ctx, append(args, "--confirm")...)
}

// edit reads a new text in the pane and approves the proposal with it. Esc goes back.
func (p *reviewPane) edit(ctx context.Context) (bool, error) {
	s, ok, err := p.line("new text (Enter approves, Esc goes back): ", "the edit is empty")
	if !ok {
		return false, err
	}
	return p.approve(ctx, &s)
}

// change reads your change request in the pane and asks for a change with it (S56): the
// proposal is not published, and the pane closes as after the other decisions. Esc goes back.
// [NEW key c].
func (p *reviewPane) change(ctx context.Context) (bool, error) {
	s, ok, err := p.line("change request (Enter sends, Esc goes back): ", "the change request is empty")
	if !ok {
		return false, err
	}
	return p.decide(ctx, "--change", s)
}

// line reads one line of text in the pane after prompt. The text stays in memory: no temp
// file, no $EDITOR (E5-S1). ok is false on Esc, on an empty text (with message empty), and on
// a text that the secret filter refuses: the filter runs before E4 gets the text, and a
// refused text is dropped.
func (p *reviewPane) line(prompt, empty string) (s string, ok bool, err error) {
	var text []rune
	// Each key redraws only the text-entry line, from the saved cursor: the proposal can be
	// forgotten while you type, and the pane must not show its text again.
	p.draw("\x1b7")
	for {
		_, _ = io.WriteString(p.Stdout, "\x1b8\x1b[J"+prompt+string(text)) // as in draw
		k, _, err := p.keys.ReadRune()
		if err != nil {
			return "", false, err
		}
		if k == keyEsc {
			p.msg = ""
			return "", false, nil
		}
		switch {
		case k == '\r' || k == '\n':
			s := string(text)
			if strings.TrimSpace(s) == "" {
				p.msg = empty
				return "", false, nil
			}
			if err := secret.Check(s); err != nil {
				p.msg = err.Error()
				return "", false, err
			}
			return s, true, nil
		case k == keyDel || k == keyBS:
			if len(text) > 0 {
				text = text[:len(text)-1]
			}
		case unicode.IsPrint(k):
			text = append(text, k)
		}
	}
}

// draw clears the pane and shows the proposal, the approve options, the keys, the last
// message and prompt.
func (p *reviewPane) draw(prompt string) {
	scope := "as proposed"
	if p.scope != "" {
		scope = string(p.scope)
	}
	pin := map[bool]string{true: "yes", false: "no"}[p.pin]
	conflict := cmp.Or(p.conflict, conflictKeep)
	var b strings.Builder
	if p.dispute {
		fmt.Fprintf(&b, "%s%s\n", clearScreen, p.view)
		b.WriteString("k keep  e edit  t retire  l later  q close\n")
	} else {
		fmt.Fprintf(&b, "%s%s\napprove with scope: %s, pin: %s, on a conflict: %s\n\n", clearScreen, p.view, scope, pin, conflict)
		b.WriteString("a approve  e edit  c change  r reject  l later  s scope  p pin  x conflict  q close\n")
	}
	if p.msg != "" {
		fmt.Fprintf(&b, "\n%s\n", strings.TrimSpace(p.msg))
	}
	if prompt != "" {
		fmt.Fprintf(&b, "\n%s", prompt)
	}
	_, _ = io.WriteString(p.Stdout, b.String()) // a pane with no screen has no one to tell
}

// close shows the last message, or err, for the linger time, and ends the pane. It shows err
// and returns it too: herdr closes the pane on exit, so the error that run prints to stderr
// is gone at once. A proposal that another surface decided is not an error of the pane.
func (p *reviewPane) close(ctx context.Context, err error) error {
	switch {
	case errors.Is(err, errDecided):
		p.msg, err = fmt.Sprintf("proposal %d: already decided; this pane closes", p.id), nil
	case err != nil:
		p.msg = err.Error()
	}
	_, _ = fmt.Fprintf(p.Stdout, "%s%s\n", clearScreen, strings.TrimSpace(p.msg)) // as in draw
	select {
	case <-ctx.Done():
	case <-time.After(p.linger):
	}
	return err
}
