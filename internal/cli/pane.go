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
	"unicode/utf8"

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
	return (&reviewPane{Env: e, id: id, keys: bufio.NewReader(os.Stdin), linger: linger,
		size: func() (int, int) { return termSize(os.Stdin) }}).run(ctx)
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

// termSize returns the rows and the columns of terminal f, or 0, 0 when stty cannot tell.
func termSize(f *os.File) (rows, cols int) {
	c := exec.Command("stty", "size")
	c.Stdin = f
	out, err := c.Output()
	if err != nil {
		return 0, 0
	}
	if _, err := fmt.Sscan(string(out), &rows, &cols); err != nil {
		return 0, 0
	}
	return rows, cols
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
	skill    bool   // the proposal is a skill: its file does not fit the one-line edit
	script   bool   // the proposal is a script (E17-S8): approve takes it as it is
	detail   bool   // show the claim id and the revision too (#254)
	// size returns the rows and the columns of the pane, read before each screen; nil or 0 rows
	// shows the whole view.
	size func() (rows, cols int)
}

// run shows the proposal and reads keys until a decision, q or the end of input. It reads
// the proposal again before each screen, so a forget or a decision in another surface while
// the pane waits for a key never shows the old text again (E5-S1).
func (p *reviewPane) run(ctx context.Context) error {
	for {
		view, err := p.show(ctx)
		if err != nil {
			return p.close(ctx, err)
		}
		p.view, p.dispute, p.skill = view, strings.Contains(view, "\n"+disputeLine), strings.Contains(view, "\n"+skillLine)
		p.script = strings.Contains(view, "\n"+scriptLine)
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

// show returns the view of the proposal, with its detail when p.detail is on.
func (p *reviewPane) show(ctx context.Context) (string, error) {
	if p.detail {
		return p.review(ctx, "--detail")
	}
	return p.review(ctx)
}

// key runs the action of key k. done is true when the proposal is decided, or stays in the
// queue with later. Any other error is on the screen, and the pane goes on. d turns the detail
// view on and off for every kind [NEW key d].
func (p *reviewPane) key(ctx context.Context, k rune) (done bool, err error) {
	switch {
	case k == 'd':
		p.detail, p.msg = !p.detail, ""
		return false, nil
	case p.dispute:
		return p.disputeKey(ctx, k)
	case p.script && !strings.ContainsRune("arl", k):
		return false, nil // a script takes no edit, change, scope, pin or conflict choice
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
	return p.result(p.review(ctx, args...))
}

// result shows out, or err, and reports whether the call is done.
func (p *reviewPane) result(out string, err error) (bool, error) {
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
		return p.result(out, err)
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

// edit reads a new text in the pane and approves the proposal with it. Esc goes back. A skill
// file has many lines, so the pane sends you to brain review for it [NEW].
func (p *reviewPane) edit(ctx context.Context) (bool, error) {
	if p.skill {
		p.msg = fmt.Sprintf("a skill file has many lines: edit it with brain review %d --approve --edit \"<text>\"", p.id)
		return false, nil
	}
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

// draw clears the pane and shows the proposal, the approve options, the last message, the keys
// and prompt. The keys stay at the bottom of the pane (#254): a view that does not fit shows
// its first lines, which hold what you decide on.
func (p *reviewPane) draw(prompt string) {
	var foot strings.Builder
	keys := "a approve  e edit  c change  r reject  l later  s scope  p pin  x conflict  d detail  q close"
	switch {
	case p.dispute:
		keys = "k keep  e edit  t retire  l later  d detail  q close"
	case p.script:
		keys = "a approve  r reject  l later  d detail  q close"
	default:
		scope := "as proposed"
		if p.scope != "" {
			scope = string(p.scope)
		}
		pin := map[bool]string{true: "yes", false: "no"}[p.pin]
		fmt.Fprintf(&foot, "approve with scope: %s, pin: %s, on a conflict: %s\n", scope, pin, cmp.Or(p.conflict, conflictKeep))
	}
	if p.msg != "" {
		fmt.Fprintf(&foot, "\n%s\n", strings.TrimSpace(p.msg))
	}
	fmt.Fprintf(&foot, "\n%s\n", keys)
	if prompt != "" {
		foot.WriteString(prompt)
	}
	_, _ = io.WriteString(p.Stdout, clearScreen+p.fit(foot.String())+foot.String()) // a pane with no screen has no one to tell
}

// fit returns the first lines of the view that fit above foot, and a line that says how many
// lines it leaves out. A prompt at the end of foot is one row: the text you type. [NEW]: the
// rows of a long line count at the pane's width.
func (p *reviewPane) fit(foot string) string {
	view := strings.TrimSuffix(p.view, "\n") + "\n"
	if p.size == nil {
		return view
	}
	height, width := p.size()
	if height <= 0 {
		return view
	}
	rows := func(line string) int {
		if width <= 0 {
			return 1
		}
		return max(1, (utf8.RuneCountInString(line)+width-1)/width)
	}
	free := height
	for _, l := range strings.Split(strings.TrimSuffix(foot, "\n"), "\n") {
		free -= rows(l)
	}
	lines := strings.SplitAfter(strings.TrimSuffix(view, "\n"), "\n")
	all := 0
	for _, l := range lines {
		all += rows(l)
	}
	if all <= free {
		return view
	}
	// The view keeps at least its first line, and the cut line takes one row.
	n, used := 1, rows(lines[0])
	for n < len(lines) && used+rows(lines[n])+1 <= free {
		used += rows(lines[n])
		n++
	}
	return strings.Join(lines[:n], "") + fmt.Sprintf("... %d more lines: brain review %d shows them all\n", len(lines)-n, p.id)
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
