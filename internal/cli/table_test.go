package cli

import (
	"bytes"
	"strings"
	"testing"
)

func TestDisplayWidthCountsCJKIdeographsAsTwoColumns(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want int
	}{
		{name: "empty", in: "", want: 0},
		{name: "ascii", in: "Linux", want: 5},
		{name: "digits and dots", in: "192.168.2.10", want: 12},
		{name: "hyphen and ascii", in: "Web-VMWare", want: 10},
		{name: "url stays single width", in: "https://192.168.2.20/ui/#/login", want: 31},
		// "DB-" is 3 ASCII columns; 测试数据库 is 5 CJK ideographs = 10 columns.
		{name: "mixed ascii and ideographs 2", in: "DB-测试数据库", want: 13},
		{name: "long mixed name", in: "host-vm-2.30.stress.internal.example.com", want: 40},
		{name: "fullwidth forms", in: "ＡＢＣ", want: 6},
		{name: "hiragana", in: "ひらがな", want: 8},
		{name: "hangul", in: "한국어", want: 6},
		{name: "emoji", in: "🔥", want: 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := displayWidth(tt.in); got != tt.want {
				t.Fatalf("displayWidth(%q) = %d, want %d", tt.in, got, tt.want)
			}
		})
	}
}

func TestDisplayWidthIgnoresCombiningMarks(t *testing.T) {
	// "e" + U+0301 combining acute is one rendered column, not two.
	if got := displayWidth("e\u0301"); got != 1 {
		t.Fatalf("displayWidth(e + combining acute) = %d, want 1", got)
	}
}

func TestPadRightPadsByDisplayWidth(t *testing.T) {
	// The padded string must occupy exactly the requested number of
	// columns, which is the property tabwriter could not provide.
	got := padRight("中文", 10)
	if displayWidth(got) != 10 {
		t.Fatalf("padRight(中文, 10) occupies %d columns, want 10", displayWidth(got))
	}
	if !strings.HasSuffix(got, "      ") {
		t.Fatalf("padRight(中文, 10) = %q, want six trailing spaces", got)
	}
	// An over-long cell is left alone rather than truncated.
	if got := padRight("abcdef", 3); got != "abcdef" {
		t.Fatalf("padRight(abcdef, 3) = %q, want it unchanged", got)
	}
}

// TestTableAlignsColumnsWithCJKNames is the regression test for the ragged
// listing: every row's second column must start at the same display column
// regardless of how many ideographs the first column holds.
func TestTableAlignsColumnsWithCJKNames(t *testing.T) {
	var buf bytes.Buffer
	tbl := newTable(&buf)
	tbl.setHeader("Name", "Address", "Platform", "Type")
	tbl.addRow("192.168.2.10", "192.168.2.10", "Linux", "Linux")
	// "DB-测试数据库": "DB-" is 3 ASCII columns, 测试数据库 is 5 CJK
	// ideographs = 10 columns, total 13 — the widest name, so it sets the
	// Address column start. Fictional asset name kept for CJK width coverage.
	tbl.addRow("DB-测试数据库", "bastion-int.example.com", "MariaDB", "MariaDB")
	tbl.addRow("Web-VMWare", "https://192.168.2.20/ui/#/login", "Website", "网站")
	tbl.addRow("host-vm-2.30", "192.168.2.30", "Linux", "Linux")
	tbl.setTrailer("\nTotal: 4 asset(s)")
	if err := tbl.render(); err != nil {
		t.Fatalf("render() = %v", err)
	}

	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	// header + 4 rows + a blank line + the trailer (its literal starts with \n)
	if len(lines) != 7 {
		t.Fatalf("got %d lines, want 7:\n%s", len(lines), buf.String())
	}

	// Every row's Address cell must begin at the same display column. Locate
	// each known address string in its line and measure the prefix before it,
	// which is the only reliable way to assert alignment for CJK text (a
	// search for a double space also matches padding inside a cell).
	addresses := []string{
		"192.168.2.10",
		"bastion-int.example.com",
		"https://192.168.2.20/ui/#/login",
		"192.168.2.30",
	}
	wantStart := displayWidth("DB-测试数据库") + 2
	for i, line := range lines[1:5] {
		// LastIndex: rows 0 and 3 contain their address in the Name cell too,
		// so the first match is not the Address column.
		idx := strings.LastIndex(line, addresses[i])
		if idx < 0 {
			t.Fatalf("row %d does not contain its address %q: %q", i, addresses[i], line)
		}
		if got := displayWidth(line[:idx]); got != wantStart {
			t.Errorf("row %d: address starts at display column %d, want %d\n%s",
				i, got, wantStart, line)
		}
	}

	if !strings.Contains(buf.String(), "Total: 4 asset(s)") {
		t.Fatalf("trailer missing:\n%s", buf.String())
	}
}

// TestTableTrimsTrailingPadding keeps the output free of invisible spaces.
func TestTableTrimsTrailingPadding(t *testing.T) {
	var buf bytes.Buffer
	tbl := newTable(&buf)
	tbl.setHeader("Name", "Type")
	tbl.addRow("web-01", "") // empty last cell must not pad the line
	if err := tbl.render(); err != nil {
		t.Fatalf("render() = %v", err)
	}
	for _, line := range strings.Split(strings.TrimRight(buf.String(), "\n"), "\n") {
		if line != strings.TrimRight(line, " ") {
			t.Errorf("line has trailing whitespace: %q", line)
		}
	}
}

func TestTableHandlesRaggedRowsAndNilWriter(t *testing.T) {
	// A nil writer must not panic; the table is discarded.
	nilTbl := newTable(nil)
	nilTbl.setHeader("A", "B")
	nilTbl.addRow("1")
	if err := nilTbl.render(); err != nil {
		t.Fatalf("render() with nil writer = %v", err)
	}

	// A row with more cells than the header must not panic or drop cells.
	var buf bytes.Buffer
	tbl := newTable(&buf)
	tbl.setHeader("A")
	tbl.addRow("1", "2")
	if err := tbl.render(); err != nil {
		t.Fatalf("render() with a ragged row = %v", err)
	}
	if !strings.Contains(buf.String(), "2") {
		t.Fatalf("extra cell was dropped:\n%s", buf.String())
	}
}

func TestRuneWidthZeroForControlCharacters(t *testing.T) {
	for _, r := range []rune{0, '\t', '\n', '\r', 0x1b} {
		if got := runeWidth(r); got != 0 {
			t.Errorf("runeWidth(%q) = %d, want 0", r, got)
		}
	}
}
