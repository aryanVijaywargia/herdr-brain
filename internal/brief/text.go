package brief

import (
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// endMarks are the marks that end a sentence. A situation never ends with one, and a ':'
// or ',' at its end goes too.
const endMarks = ".!?"

// conditionWords start a situation that reads as a condition already, so ClaimText adds no
// "When" before it [NEW].
var conditionWords = []string{"when", "whenever", "if", "before", "after", "while", "during", "once"}

// ClaimText is the item text of a claim key (#284): "When <situation>: <rule>", or the rule
// alone for an empty situation. The situation loses its end marks, so the text never holds
// ".:". A situation that starts with a condition word keeps it in place of "When".
func ClaimText(situation, rule string) string {
	s := strings.TrimRight(strings.TrimSpace(situation), endMarks+":;, ")
	r := strings.TrimSpace(rule)
	if s == "" {
		return Clean(r)
	}
	first, _, _ := strings.Cut(s, " ")
	if !slices.Contains(conditionWords, strings.ToLower(first)) {
		s = "When " + lowerFirst(s, first)
	} else {
		s = upperFirst(s)
	}
	return Clean(s + ": " + r)
}

// lowerFirst returns s with its first letter in lower case when its first word is a normal
// capital word ("Running"), not an acronym or a name with more capitals ("NPM", "GitHub").
func lowerFirst(s, first string) string {
	r, n := utf8.DecodeRuneInString(s)
	if !unicode.IsUpper(r) || strings.IndexFunc(first[n:], unicode.IsUpper) >= 0 {
		return s
	}
	return string(unicode.ToLower(r)) + s[n:]
}

// upperFirst returns s with its first letter in upper case.
func upperFirst(s string) string {
	r, n := utf8.DecodeRuneInString(s)
	return string(unicode.ToUpper(r)) + s[n:]
}

// markColon is an end mark before the ": " that joins a situation and a rule in old item
// text ("Running the check.: Run npm test").
var markColon = regexp.MustCompile(`[.!?]+: `)

// doubleEnd is a run of two or more end marks at the end of a text.
var doubleEnd = regexp.MustCompile(`([.!?])[.!?]+$`)

// Clean returns item text with no end mark before the ": " of a claim text and no double end
// mark at its end, so old items that Brain made as "situation.: rule" read clean (#284). An
// ellipsis at the end stays.
func Clean(text string) string {
	text = markColon.ReplaceAllString(text, ": ")
	if strings.HasSuffix(text, "...") && !strings.HasSuffix(text, "....") {
		return text
	}
	return doubleEnd.ReplaceAllString(text, "$1")
}
