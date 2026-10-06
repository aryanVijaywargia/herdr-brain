package adapter

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode"

	"golang.org/x/text/width"
)

// noiseBlock is one entry of noise_blocks, compiled.
type noiseBlock struct{ start, next *regexp.Regexp }

// checkScreen compiles the screen rules of the profile: noise_lines, noise_blocks and
// side_panel (§6.2).
func (p *Profile) checkScreen() error {
	for _, n := range p.NoiseLines {
		re, err := wholeLine(n)
		if err != nil {
			return fmt.Errorf("noise_lines %q: %w", n, err)
		}
		p.noise = append(p.noise, re)
	}
	for _, b := range p.NoiseBlocks {
		start, err := wholeLine(b.Start)
		if err != nil {
			return fmt.Errorf("noise_blocks start %q: %w", b.Start, err)
		}
		next, err := wholeLine(b.Next)
		if err != nil {
			return fmt.Errorf("noise_blocks next %q: %w", b.Next, err)
		}
		p.blocks = append(p.blocks, noiseBlock{start, next})
	}
	if p.SidePanel.Header == "" {
		return nil
	}
	if p.SidePanel.MinColumn < 1 {
		return errors.New("side_panel.min_column is less than 1")
	}
	re, err := regexp.Compile(p.SidePanel.Header)
	if err != nil {
		return fmt.Errorf("side_panel.header %q: %w", p.SidePanel.Header, err)
	}
	p.panel = re
	return nil
}

// wholeLine compiles pattern s so that it matches a whole line.
func wholeLine(s string) (*regexp.Regexp, error) {
	return regexp.Compile(`^(?:` + s + `)$`)
}

// Noise reports whether line is screen noise by the profile's noise_lines.
func (p *Profile) Noise(line string) bool {
	for _, re := range p.noise {
		if re.MatchString(line) {
			return true
		}
	}
	return false
}

// Screen splits the screen text of one pane read into lines without trailing space. It cuts
// the side panel off each line, and drops blank lines and the noise of the profile: the
// noise lines and the noise blocks (§6.2, #258).
func (p *Profile) Screen(text string) []string {
	rows := strings.Split(text, "\n")
	col := p.panelColumn(rows)
	var ls []string
	var next *regexp.Regexp // the Next of the open noise block; nil when none
	for _, row := range rows {
		l := strings.TrimRightFunc(cut(row, col), unicode.IsSpace)
		switch {
		case strings.TrimSpace(l) == "":
			continue
		case next != nil && next.MatchString(l):
			continue
		}
		next = p.blockStart(l)
		if next != nil || p.Noise(l) {
			continue
		}
		ls = append(ls, l)
	}
	return ls
}

// blockStart returns the Next of the noise block that line l starts, or nil.
func (p *Profile) blockStart(l string) *regexp.Regexp {
	for _, b := range p.blocks {
		if b.start.MatchString(l) {
			return b.next
		}
	}
	return nil
}

// panelColumn returns the display column where the side panel starts in rows, or -1 when no
// row holds its header. The header must start at min_column or right of it, after blank text
// or a gap of two spaces, as a column does: a reply that names a diff stays main text.
func (p *Profile) panelColumn(rows []string) int {
	if p.panel == nil {
		return -1
	}
	for _, row := range rows {
		loc := p.panel.FindStringIndex(row)
		if loc == nil {
			continue
		}
		left := row[:loc[0]]
		if c := columns(left); c >= p.SidePanel.MinColumn && (strings.TrimSpace(left) == "" || strings.HasSuffix(left, "  ")) {
			return c
		}
	}
	return -1
}

// cut returns the part of row left of display column col; all of row when col < 0.
func cut(row string, col int) string {
	if col < 0 {
		return row
	}
	c := 0
	for i, r := range row {
		if c += runeWidth(r); c > col {
			return row[:i]
		}
	}
	return row
}

// columns returns the display width of s.
func columns(s string) int {
	c := 0
	for _, r := range s {
		c += runeWidth(r)
	}
	return c
}

// runeWidth is the number of terminal columns of r: 2 for a wide East Asian rune, else 1.
func runeWidth(r rune) int {
	switch width.LookupRune(r).Kind() {
	case width.EastAsianWide, width.EastAsianFullwidth:
		return 2
	}
	return 1
}
