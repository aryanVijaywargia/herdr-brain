package brained

import (
	"context"
	"database/sql"
	"strings"

	"github.com/aryanVijaywargia/herdr-brain/internal/fingerprint"
	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

// HookPrompt returns the full prompt of prompt text from a hook payload of harness, as the
// prompt index hashes it (E14-S1): the whole text with each secret masked as [secret]. It
// keeps the blank lines and the length that the excerpt drops, so a long prompt matches
// itself. A live match of a hook prompt (E14-S3) hashes the same text.
func HookPrompt(harness, text string) string {
	return strings.Join(redact(harness, strings.Split(text, "\n")), "\n")
}

// panePrompt returns the full prompt of the filtered lines of one part of a pane read of
// harness: the lines without their sender mark, so the prompt "❯ run make foo" in the pane and
// the hook prompt "run make foo" give one fingerprint [NEW]. The read has no blank lines.
func panePrompt(harness string, lines []string) string {
	ls := make([]string, len(lines))
	for i, l := range lines {
		ls[i] = l
		for _, m := range marks[harness] {
			if strings.HasPrefix(l, m.prefix) {
				ls[i] = l[len(m.prefix):]
				break
			}
		}
	}
	return strings.Join(ls, "\n")
}

// IndexPrompt adds the prompt index row of stored observation o when it is a prompt: text of
// an unconfirmed sender that Brain did not cause. full is its full prompt after the secret
// filter (HookPrompt, panePrompt). The row holds the fingerprint of full and its text cut to
// the excerpt limit. The caller runs it in the transaction that stored o (§6.7).
func IndexPrompt(ctx context.Context, tx *sql.Tx, o store.Observation, full string) error {
	if o.Sender != store.SenderUnconfirmed || o.BrainOrigin || o.Status == store.ObservationGap || o.Excerpt == "" {
		return nil
	}
	text, _ := filter(full)
	return store.IndexPrompt(ctx, tx, o.ID, fingerprint.Prompt(full), text)
}
