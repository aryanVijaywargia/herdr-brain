// Package fingerprint holds the exact prompt fingerprint of Brain (E14-S1): the trigger
// prompts of a proposal (E13-S5) and the live prompt match (E14) use this one function.
package fingerprint

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"golang.org/x/text/unicode/norm"
)

// Prompt returns the exact fingerprint of prompt text s: the hex SHA-256 of s in Unicode NFC,
// with the white space at its start and its end trimmed. Case and inner space stay, because
// `run make Foo` and `run make foo` can differ (E14-S1). Wider normalization belongs only to
// the close matcher (E14-S2).
func Prompt(s string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(norm.NFC.String(s))))
	return hex.EncodeToString(sum[:])
}
