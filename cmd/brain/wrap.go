package main

import (
	"io"
	"os"
	"strings"
	"syscall"
	"unicode/utf8"
	"unsafe"
)

// termWidth returns the column count of the terminal of w, or 0 when w is not a terminal or
// its width is unknown (#255).
func termWidth(w io.Writer) int {
	f, ok := w.(*os.File)
	if !ok {
		return 0
	}
	var ws struct{ row, col, x, y uint16 }                                                                               // struct winsize of the TIOCGWINSZ ioctl
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), uintptr(syscall.TIOCGWINSZ), uintptr(unsafe.Pointer(&ws))) //nolint:gosec // the ioctl needs the address of ws
	if errno != 0 {
		return 0
	}
	return int(ws.col)
}

// wrapWriter breaks the lines that it writes to w at word ends, so that no line is wider than
// width columns (#255). A pane breaks a long line at its edge, in the middle of a word. A
// continuation line keeps the indent of its line. A word wider than width stays whole on its
// own line. Each Write must end at a word end, as each fmt.Fprint call of setup does.
type wrapWriter struct {
	w      io.Writer
	width  int
	col    int  // the columns written on the current line
	spaces int  // the spaces that are not written yet
	indent int  // the leading spaces of the current line
	start  bool // no word on the current line yet
}

// wrapTo returns w, wrapped at width columns. A width of 0 or less is unknown: w then gets the
// text as it is, and the pane wraps it.
func wrapTo(w io.Writer, width int) io.Writer {
	if width <= 0 {
		return w
	}
	return &wrapWriter{w: w, width: width, start: true}
}

func (x *wrapWriter) Write(p []byte) (int, error) {
	var b strings.Builder
	for s := string(p); s != ""; {
		switch {
		case s[0] == '\n':
			b.WriteByte('\n')
			x.col, x.spaces, x.indent, x.start = 0, 0, 0, true
			s = s[1:]
		case s[0] == ' ':
			n := len(s) - len(strings.TrimLeft(s, " "))
			x.spaces += n
			if x.start {
				x.indent += n
			}
			s = s[n:]
		default:
			n := strings.IndexAny(s, " \n")
			if n < 0 {
				n = len(s)
			}
			word := s[:n]
			wide := utf8.RuneCountInString(word)
			if !x.start && x.col+x.spaces+wide > x.width {
				b.WriteByte('\n')
				x.col, x.spaces = 0, x.indent
			}
			b.WriteString(strings.Repeat(" ", x.spaces))
			b.WriteString(word)
			x.col += x.spaces + wide
			x.spaces, x.start = 0, false
			s = s[n:]
		}
	}
	// The space after a question shows before the answer.
	if x.spaces > 0 && x.col+x.spaces <= x.width {
		b.WriteString(strings.Repeat(" ", x.spaces))
		x.col += x.spaces
		x.spaces = 0
	}
	if _, err := io.WriteString(x.w, b.String()); err != nil {
		return 0, err
	}
	return len(p), nil
}
