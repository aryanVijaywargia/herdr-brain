// Package secret finds text that looks like a secret, with a fixed list of known patterns
// (HLD §6.7, S12). It cannot find every secret: only the supported pattern classes below.
//
// Nothing that this package returns holds the matched text. A Hit names its class and its
// byte range, so an M1 text write can refuse with the class name, and a later caller can
// remove the range before it stores the text.
package secret

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// class is one supported pattern class. The name is safe to print.
type class struct {
	name string
	re   *regexp.Regexp
}

// classes is the supported list. The prefixes are the public formats of each provider. The
// minimum lengths after a prefix are [NEW]: long enough that ordinary words do not match.
var classes = []class{
	{"github-token", regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{36,}|\bgithub_pat_[A-Za-z0-9_]{22,}`)},
	{"gitlab-token", regexp.MustCompile(`\bglpat-[A-Za-z0-9_-]{20,}`)},
	{"anthropic-api-key", regexp.MustCompile(`\bsk-ant-[A-Za-z0-9_-]{20,}`)},
	// The old OpenAI form has no dash after "sk-", so it never matches an Anthropic key.
	{"openai-api-key", regexp.MustCompile(`\bsk-(?:proj|svcacct|admin)-[A-Za-z0-9_-]{20,}|\bsk-[A-Za-z0-9]{32,}`)},
	{"aws-access-key-id", regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`)},
	{"google-api-key", regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{35}`)},
	{"slack-token", regexp.MustCompile(`\bxox[abposr]-[A-Za-z0-9-]{10,}`)},
	{"stripe-key", regexp.MustCompile(`\b(?:sk|rk)_(?:live|test)_[A-Za-z0-9]{16,}`)},
	{"npm-token", regexp.MustCompile(`\bnpm_[A-Za-z0-9]{36,}`)},
	// A block with no END line runs to the end of the text, so a cut block still matches.
	{"private-key", regexp.MustCompile(`(?s)-----BEGIN [A-Z ]*PRIVATE KEY-----.*?(?:-----END [A-Z ]*PRIVATE KEY-----|\z)`)},
	{"password-pair", regexp.MustCompile(`(?i)\b(?:password|passwd|pwd)\s*[:=]\s*\S+`)},
	// An .env line whose upper-case name has a secret-like word. The word list is [NEW].
	{"env-secret", regexp.MustCompile(
		`(?m)^[ \t]*(?:export[ \t]+)?[A-Z0-9_]*(?:SECRET|TOKEN|PASSWORD|PASSWD|API_?KEY|ACCESS_KEY|PRIVATE_KEY|CREDENTIALS?)[A-Z0-9_]*[ \t]*=[ \t]*\S+`)},
}

// Hit is one match of a supported pattern: text[Start:End] is the secret.
type Hit struct {
	Class      string
	Start, End int
}

// Find returns every hit in text, ordered by Start. Two classes can match the same bytes, for
// example PASSWORD=x is both an .env line and a password pair; Find returns both hits.
func Find(text string) []Hit {
	var hits []Hit
	for _, c := range classes {
		for _, m := range c.re.FindAllStringIndex(text, -1) {
			hits = append(hits, Hit{Class: c.name, Start: m[0], End: m[1]})
		}
	}
	slices.SortStableFunc(hits, func(a, b Hit) int { return a.Start - b.Start })
	return hits
}

// Check returns an *Error when text has a hit. An M1 text write calls it and refuses the
// write on an error (E4-S1): Brain does not change the user's text without asking.
func Check(text string) error {
	hits := Find(text)
	if len(hits) == 0 {
		return nil
	}
	var names []string
	for _, h := range hits {
		if !slices.Contains(names, h.Class) {
			names = append(names, h.Class)
		}
	}
	return &Error{Classes: names}
}

// Error says that the text has a secret. It holds only the class names, never the matched
// text, so its message is safe to print and to log.
type Error struct {
	Classes []string
}

func (e *Error) Error() string {
	return fmt.Sprintf("the text looks like it has a secret (%s): remove it and try again",
		strings.Join(e.Classes, ", "))
}
