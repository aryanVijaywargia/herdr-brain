package cli

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"strings"

	"github.com/aryanVijaywargia/herdr-brain/internal/extract"
	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

// evidenceLines is the most lines of stored text that the evidence view shows before and after
// a source excerpt (E20-S4).
const evidenceLines = 20

// The one lines of the evidence view when it has no text to show (E20-S4).
const (
	evidenceGone = "evidence: the stored text of the source excerpts is gone (forgotten, or past retention)\n"
	evidenceNone = "evidence: none: this proposal has no captured source text\n"
	// The stored text is in workspaces that the caller does not see: show hides it too.
	evidenceElsewhere = "evidence: the stored text of the source excerpts is in another workspace\n"
)

// tabWidth is the spaces that the evidence view shows for a tab, so that the pane counts the
// rows of a line by its runes (#254). [NEW] 4.
const tabWidth = 4

// view shows proposal r.id: its evidence view with --evidence, else the proposal.
func (cl caller) view(ctx context.Context, r review) error {
	if r.evidence {
		return cl.showEvidence(ctx, r.id)
	}
	return cl.show(ctx, r.id, r.detail)
}

// views refuses --detail (#254) or --evidence with anything else than a proposal id.
func (r review) views() error {
	for _, v := range []struct {
		on   bool
		name string
	}{{r.detail, "--detail"}, {r.evidence, "--evidence"}} {
		if v.on && (r.id == 0 || r.detail == r.evidence || r != review{id: r.id, detail: r.detail, evidence: r.evidence}) {
			return fmt.Errorf("%s goes with a proposal id alone", v.name)
		}
	}
	return nil
}

// showEvidence prints the evidence view of proposal id (E20-S4, §7.4): for each source excerpt
// that show lists, the stored text of its capture segment around it, up to evidenceLines lines
// before and after, through the secret filter. It reads only the observations that Brain
// stores, no transcript.
func (cl caller) showEvidence(ctx context.Context, id int64) error {
	read := cl.proposal
	if cl.surface == store.SurfacePane {
		read = cl.pending
	}
	var b strings.Builder
	err := cl.Store.Write(ctx, func(tx *sql.Tx) error {
		p, _, _, err := read(ctx, tx, id)
		if err != nil {
			return err
		}
		fmt.Fprintf(&b, "proposal %d (%s): the stored text around each source excerpt, up to %d lines before and after\n",
			p.ID, p.Status, evidenceLines)
		es, ok, err := evidence(ctx, tx, p)
		if err != nil {
			return err
		}
		shown := cl.shown(es)
		switch {
		case !ok:
			b.WriteString(evidenceNone)
		case len(es) == 0:
			b.WriteString(evidenceGone)
		case len(shown) == 0:
			b.WriteString(evidenceElsewhere)
		}
		for i, e := range shown {
			before, after, err := store.Around(ctx, tx, e.ObservationID, evidenceLines)
			if err != nil {
				return err
			}
			fmt.Fprintf(&b, "excerpt %d: %s\n", i+1, cl.excerptHead(e))
			writeLines(&b, "  | ", lastLines(textLines(before), evidenceLines))
			writeLines(&b, "  > ", storedLines(e.Excerpt))
			writeLines(&b, "  | ", firstLines(textLines(after), evidenceLines))
		}
		return nil
	})
	if err != nil {
		return err
	}
	_, err = io.WriteString(cl.Stdout, b.String())
	return err
}

// textLines are the lines of the stored text of obs, each through the secret filter. The filter
// runs on the text of each observation as a whole, so a cut never splits a secret from its
// pattern. A gap holds no text: it shows as one line.
func textLines(obs []store.Observation) []string {
	var lines []string
	for _, o := range obs {
		if o.Status == store.ObservationGap {
			lines = append(lines, "(a gap: Brain has no text here)")
			continue
		}
		lines = append(lines, storedLines(o.Excerpt)...)
	}
	return lines
}

// storedLines are the lines of stored text, through the secret filter, with each tab as
// tabWidth spaces.
func storedLines(text string) []string {
	text = strings.ReplaceAll(extract.Redact(text), "\t", strings.Repeat(" ", tabWidth))
	return strings.Split(strings.TrimRight(text, "\n"), "\n")
}

func lastLines(lines []string, n int) []string { return lines[max(0, len(lines)-n):] }

func firstLines(lines []string, n int) []string { return lines[:min(n, len(lines))] }

func writeLines(b *strings.Builder, mark string, lines []string) {
	for _, l := range lines {
		b.WriteString(strings.TrimRight(mark+l, " \r") + "\n")
	}
}
