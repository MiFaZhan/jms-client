package cli

import (
	"fmt"
	"io"
	"strings"
)

// displayWidth returns the number of terminal columns s occupies.
//
// text/tabwriter counts runes, but a CJK ideograph occupies two columns
// while counting as one rune. A table padded by rune count therefore
// drifts right by one column per ideograph, which is what made the asset
// listing ragged for Chinese asset names.
func displayWidth(s string) int {
	width := 0
	for _, r := range s {
		width += runeWidth(r)
	}
	return width
}

// runeWidth returns the column width of one rune: 0 for a combining or
// zero-width rune, 2 for an East Asian wide/fullwidth rune, 1 otherwise.
//
// The ranges cover the scripts this CLI actually renders: CJK ideographs,
// kana, Hangul, fullwidth forms and emoji. That is the practical subset of
// Unicode East Asian Width; pulling in golang.org/x/text for the full
// property table would be a dependency for a CLI table.
func runeWidth(r rune) int {
	switch {
	case r < 32: // control characters render no column
		return 0
	case r == 0x200B, r == 0x200C, r == 0x200D, r == 0xFEFF: // zero-width
		return 0
	case r >= 0x0300 && r <= 0x036F: // combining diacritical marks
		return 0
	case r >= 0x1AB0 && r <= 0x1AFF: // combining diacritical marks extended
		return 0
	case r >= 0x1DC0 && r <= 0x1DFF: // combining diacritical marks supplement
		return 0
	case r >= 0x20D0 && r <= 0x20FF: // combining marks for symbols
		return 0
	case r >= 0xFE00 && r <= 0xFE0F: // variation selectors
		return 0
	case r >= 0xFE20 && r <= 0xFE2F: // combining half marks
		return 0
	}

	switch {
	case r >= 0x1100 && r <= 0x115F: // Hangul Jamo initial consonants
		return 2
	case r >= 0x2E80 && r <= 0x303E: // CJK radicals, Kangxi, CJK symbols
		return 2
	case r >= 0x3041 && r <= 0x33FF: // kana, Bopomofo, CJK compatibility
		return 2
	case r >= 0x3400 && r <= 0x4DBF: // CJK Unified Ideographs Extension A
		return 2
	case r >= 0x4E00 && r <= 0x9FFF: // CJK Unified Ideographs
		return 2
	case r >= 0xA000 && r <= 0xA4CF: // Yi
		return 2
	case r >= 0xAC00 && r <= 0xD7A3: // Hangul syllables
		return 2
	case r >= 0xF900 && r <= 0xFAFF: // CJK compatibility ideographs
		return 2
	case r >= 0xFE10 && r <= 0xFE19: // vertical forms
		return 2
	case r >= 0xFE30 && r <= 0xFE6F: // CJK compatibility forms
		return 2
	case r >= 0xFF00 && r <= 0xFF60: // fullwidth forms
		return 2
	case r >= 0xFFE0 && r <= 0xFFE6: // fullwidth signs
		return 2
	case r >= 0x1F300 && r <= 0x1F64F: // emoji
		return 2
	case r >= 0x1F900 && r <= 0x1F9FF:
		return 2
	case r >= 0x20000 && r <= 0x3FFFD: // CJK Extension B and beyond
		return 2
	}
	return 1
}

// padRight pads s with spaces to at least width display columns. A string
// already at or beyond width is returned unchanged, so an over-long cell
// widens its column instead of being truncated.
func padRight(s string, width int) string {
	missing := width - displayWidth(s)
	if missing <= 0 {
		return s
	}
	return s + strings.Repeat(" ", missing)
}

// table renders a column-aligned text table.
//
// It replaces text/tabwriter for this CLI, which cannot align CJK text
// because it measures runes rather than display columns.
type table struct {
	out     io.Writer
	header  []string
	rows    [][]string
	gap     string
	trailer string
}

// newTable returns a table that writes to out.
func newTable(out io.Writer) *table {
	if out == nil {
		out = io.Discard
	}
	return &table{out: out, gap: "  "}
}

// setHeader records the header row.
func (t *table) setHeader(cells ...string) {
	t.header = cells
}

// addRow appends one data row.
func (t *table) addRow(cells ...string) {
	t.rows = append(t.rows, cells)
}

// setTrailer records a line printed after the table.
func (t *table) setTrailer(line string) {
	t.trailer = line
}

// render writes the header, the rows and the trailer.
//
// Every line is right-trimmed so the output carries no invisible padding.
func (t *table) render() error {
	columns := len(t.header)
	for _, row := range t.rows {
		if len(row) > columns {
			columns = len(row)
		}
	}
	widths := make([]int, columns)
	for i, cell := range t.header {
		widths[i] = displayWidth(cell)
	}
	for _, row := range t.rows {
		for i, cell := range row {
			if w := displayWidth(cell); w > widths[i] {
				widths[i] = w
			}
		}
	}

	if len(t.header) > 0 {
		if _, err := fmt.Fprintln(t.out, t.renderLine(t.header, widths)); err != nil {
			return err
		}
	}
	for _, row := range t.rows {
		if _, err := fmt.Fprintln(t.out, t.renderLine(row, widths)); err != nil {
			return err
		}
	}
	if t.trailer != "" {
		if _, err := fmt.Fprintln(t.out, t.trailer); err != nil {
			return err
		}
	}
	return nil
}

// renderLine pads and joins one row, dropping the padding that follows the
// last non-empty cell.
func (t *table) renderLine(row []string, widths []int) string {
	var b strings.Builder
	last := -1
	for i := range row {
		if i < len(widths) && strings.TrimSpace(row[i]) != "" {
			last = i
		}
	}
	for i := 0; i < len(row) && i < len(widths); i++ {
		if i > 0 {
			b.WriteString(t.gap)
		}
		if i == last {
			b.WriteString(row[i])
			continue
		}
		b.WriteString(padRight(row[i], widths[i]))
	}
	return strings.TrimRight(b.String(), " ")
}
