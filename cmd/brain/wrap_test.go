package main

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"
)

// checkWrap reports a line of got that is wider than width, unless it is one word with no
// indent, and a word that wrapping broke or changed: got must hold the words of want, in
// order. It returns the count of JSON hook entries of want; each must end a line of got whole.
func checkWrap(t *testing.T, got, want string, width int) int {
	t.Helper()
	lines := strings.Split(got, "\n")
	for _, line := range lines {
		if utf8.RuneCountInString(line) > width && (strings.HasPrefix(line, " ") || wordEnd(line) != len(line)) {
			t.Errorf("width %d: line of %d columns: %q", width, utf8.RuneCountInString(line), line)
		}
	}
	if g, w := strings.Fields(got), strings.Fields(want); !slices.Equal(g, w) {
		t.Errorf("width %d: the words changed:\n%s\nwant the words of:\n%s", width, got, want)
	}
	entries := 0
	for _, line := range strings.Split(want, "\n") {
		i := strings.Index(line, `{"`)
		if i < 0 {
			continue
		}
		entries++
		if !slices.ContainsFunc(lines, func(l string) bool { return strings.HasSuffix(l, line[i:]) }) {
			t.Errorf("width %d: the hook entry %s is not whole on one line:\n%s", width, line[i:], got)
		}
	}
	return entries
}

// AC 2 (#255): the real setup text wraps at word ends at popup widths of 60, 80 and 100
// columns. A run of setup shows the herdr check, the extractor text and the plan.
func TestSetupWrap(t *testing.T) {
	plain := func(io.Writer) int { return 0 }
	runSetup := func(width func(io.Writer) int) string {
		t.Helper()
		d := osSetupDeps(os.Environ())
		d.width = width
		var out bytes.Buffer
		// codex as the extractor with its default model, then no: setup writes nothing.
		in := bufio.NewReader(strings.NewReader("codex\n\nn\n"))
		if err := setup(context.Background(), d, nil, in, &out); err != nil {
			t.Fatalf("setup() = %v\n%s", err, out.String())
		}
		return out.String()
	}
	setupHome(t, "0.9.1")
	want := runSetup(plain)
	if !strings.Contains(want, locality) {
		t.Fatalf("setup does not show the extractor text:\n%s", want)
	}
	for _, width := range []int{60, 80, 100} {
		t.Run(strconv.Itoa(width), func(t *testing.T) {
			got := runSetup(func(io.Writer) int { return width })
			if n := checkWrap(t, got, want, width); n == 0 {
				t.Errorf("setup shows no hook entry, so the test checks none:\n%s", want)
			}
			// The constant texts too: the Codex trust steps show only for a hook in place.
			for _, text := range []string{codexTrust, firstUse, locality, historyScan} {
				var b bytes.Buffer
				if _, err := io.WriteString(wrapTo(&b, width), text); err != nil {
					t.Fatal(err)
				}
				checkWrap(t, b.String(), text, width)
			}
		})
	}
}

// AC 1 (#255): a continuation line keeps the indent of its line, a bullet line goes on under
// its text, the space after a question shows before the answer, and an unknown width leaves
// the text as it is.
func TestWrapTo(t *testing.T) {
	var b bytes.Buffer
	w := wrapTo(&b, 20)
	for _, s := range []string{"  1. one two three four five\n", "Apply all? [y/N] ", "y\n"} {
		if _, err := io.WriteString(w, s); err != nil {
			t.Fatal(err)
		}
	}
	if want := "  1. one two three\n  four five\nApply all? [y/N] y\n"; b.String() != want {
		t.Errorf("wrapped = %q, want %q", b.String(), want)
	}

	// A wrapped bullet line goes on under the text of the bullet.
	b.Reset()
	if _, err := io.WriteString(wrapTo(&b, 12), "- one two three four five\n"); err != nil {
		t.Fatal(err)
	}
	if want := "- one two\n  three four\n  five\n"; b.String() != want {
		t.Errorf("bullet = %q, want %q", b.String(), want)
	}

	b.Reset()
	long := strings.Repeat("x", 30)
	if _, err := io.WriteString(wrapTo(&b, 20), "a "+long+" b\n"); err != nil {
		t.Fatal(err)
	}
	if want := "a\n" + long + "\nb\n"; b.String() != want {
		t.Errorf("long word = %q, want %q", b.String(), want)
	}

	// A quoted JSON string stays whole, and an indent that does not fit with a word goes.
	b.Reset()
	entry := `{"command":"/a b/brain hook","timeout":5}`
	if _, err := io.WriteString(wrapTo(&b, 40), "- Add: "+entry+"\n    "+long+long+"\n"); err != nil {
		t.Fatal(err)
	}
	if want := "- Add:\n" + entry + "\n" + long + long + "\n"; b.String() != want {
		t.Errorf("quoted = %q, want %q", b.String(), want)
	}
	if n := wordEnd(`"a \" b" c`); n != 8 {
		t.Errorf(`wordEnd of an escaped quote = %d, want 8`, n)
	}

	if w := wrapTo(&b, 0); w != io.Writer(&b) {
		t.Error("width 0 wraps; want the writer as it is")
	}
	if termWidth(&b) != 0 {
		t.Error("termWidth of a buffer is not 0")
	}
}
