package signal

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

// text is one observation with text, with its normal form and its word set.
type text struct {
	in    store.SignalInput
	norm  string
	words map[string]bool
}

func newText(in store.SignalInput) text {
	t := text{in: in, norm: normalize(in.Excerpt), words: map[string]bool{}}
	for _, w := range strings.Fields(t.norm) {
		t.words[w] = true
	}
	return t
}

// normalize returns s in lower case, with typographic apostrophes as "'", every other
// character that is not a letter or a digit as a space, and single spaces. It is the small
// local normal form of the rules; E14-S1 defines the prompt fingerprint.
func normalize(s string) string {
	s = strings.Map(func(r rune) rune {
		switch {
		case r == '’' || r == '‘' || r == '\'':
			return '\''
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			return unicode.ToLower(r)
		}
		return ' '
	}, s)
	return strings.Join(strings.Fields(s), " ")
}

// hasAny reports whether t holds one of markers as whole words.
func (t text) hasAny(markers []string) bool {
	padded := " " + t.norm + " "
	for _, m := range markers {
		if strings.Contains(padded, " "+m+" ") {
			return true
		}
	}
	return false
}

// similar reports whether t and e have the same normal form, or share at least limit of
// their words (Jaccard).
func (t text) similar(e text, limit float64) bool {
	if t.norm == e.norm {
		return true
	}
	shared := 0
	for w := range t.words {
		if e.words[w] {
			shared++
		}
	}
	all := len(t.words) + len(e.words) - shared
	return all > 0 && float64(shared)/float64(all) >= limit
}

// listItem matches a line that starts a list item: "1.", "2)", "-", "*" or "•".
var listItem = regexp.MustCompile(`^\s*(\d+[.)]|[-*•])\s+`)

// clause splits text into clauses at the end of a sentence and at "then".
var clause = regexp.MustCompile(`(?i)[.;!?]\s+|\bthen\b`)

// isProcedure reports whether t is a long multi-step instruction.
func (t text) isProcedure() bool {
	return utf8.RuneCountInString(t.in.Excerpt) >= minProcedureRunes && steps(t.in.Excerpt) >= minSteps
}

// steps counts the steps of instruction s: its list items, or its clauses of two or more
// words, whichever is more.
func steps(s string) int {
	items := 0
	for _, l := range strings.Split(s, "\n") {
		if listItem.MatchString(l) {
			items++
		}
	}
	clauses := 0
	for _, c := range clause.Split(strings.ReplaceAll(s, "\n", " "), -1) {
		if len(strings.Fields(c)) >= 2 {
			clauses++
		}
	}
	return max(items, clauses)
}

// cutRunes returns s cut to at most n characters.
func cutRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}
