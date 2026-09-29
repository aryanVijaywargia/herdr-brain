package main

import (
	"regexp"
	"slices"
	"strings"
)

// Label is the answer label of one reply (HLD §12.1).
type Label string

// The answer labels.
const (
	Pass     Label = "PASS"
	Stale    Label = "STALE"
	Invented Label = "INVENTED"
	Miss     Label = "MISS"
	Unsure   Label = "UNSURE"
)

// Check is what a reply is checked against: the expected answers, the stale answers, and
// whether the right answer is "I do not know". Markers are filled in already.
type Check struct {
	Expect  []string
	Stale   []string
	Decline bool
}

// negationWindow is how many words before an answer a negation word counts. [NEW] 2: it holds
// "not bun", "don't use bun" and "instead of bun".
const negationWindow = 2

// negations are the words that turn an answer next to them into its negation.
var negations = []string{"not", "no", "never", "don't", "dont", "doesn't", "isn't", "aren't",
	"won't", "without", "avoid", "instead", "than"}

// refusals are the phrases that say the agent does not know. [NEW] list.
var refusals = []string{"i don't know", "i do not know", "not sure", "no information",
	"don't have", "do not have", "can't find", "cannot find", "couldn't find", "could not find", "not aware", "no record", "cannot determine",
	"can't determine", "can't tell", "cannot tell", "unknown"}

// word splits text into words: letters, digits, "'" and "-".
var word = regexp.MustCompile(`[\p{L}\p{N}'-]+`)

// label decides the answer label of reply from the rules of §12.1, and names the rule. The
// rules look only for the exact answers of the probe, so anything that they cannot decide is
// UNSURE (S73): both an expected and a stale answer, a negation, a refusal with a claim.
func label(reply string, c Check) (Label, string) {
	text := normalize(reply)
	refused := refusal(text)
	expect, negE := find(text, c.Expect)
	stale, negS := find(text, c.Stale)
	claim := expect || stale
	switch {
	case negE || negS:
		return Unsure, "an answer next to a negation"
	case refused && claim:
		return Unsure, "a refusal together with a claim"
	case c.Decline && refused:
		return Pass, "declined, with no claim"
	case c.Decline && claim:
		return Invented, "a claim about the test fact, which Brain never supplied"
	case c.Decline:
		return Unsure, "no refusal and no known claim"
	case expect && stale:
		return Unsure, "both the expected and a stale answer (S73)"
	case expect:
		return Pass, "the expected answer"
	case stale:
		return Stale, "a stale answer"
	case refused:
		return Miss, "declined: the reply does not use the item"
	}
	return Miss, "no expected answer: the reply does not use the item"
}

// normalize lowers text and makes each typographic apostrophe a plain one.
func normalize(s string) string {
	return strings.ToLower(strings.ReplaceAll(s, "’", "'"))
}

func refusal(text string) bool {
	for _, r := range refusals {
		if strings.Contains(text, r) {
			return true
		}
	}
	return false
}

// find reports whether text holds one of terms as a whole word, and whether one such
// occurrence follows a negation word.
func find(text string, terms []string) (found, negated bool) {
	for _, t := range terms {
		t = normalize(strings.TrimSpace(t))
		if t == "" {
			continue
		}
		// A term is a whole word: no letter or digit right before or after it, so "bun" is not
		// in "bundle" and kest-1234 is not in kest-12345.
		re := regexp.MustCompile(`(^|[^\p{L}\p{N}])` + regexp.QuoteMeta(t) + `($|[^\p{L}\p{N}])`)
		for _, m := range re.FindAllStringSubmatchIndex(text, -1) {
			found = true
			if negatedAt(text[:m[3]]) { // m[3]: the end of the boundary, the start of the term
				negated = true
			}
		}
	}
	return found, negated
}

// negatedAt reports whether one of the last negationWindow words of before is a negation.
func negatedAt(before string) bool {
	ws := word.FindAllString(before, -1)
	return slices.ContainsFunc(ws[max(0, len(ws)-negationWindow):], func(w string) bool {
		return slices.Contains(negations, w)
	})
}
