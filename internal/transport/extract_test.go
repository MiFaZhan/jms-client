package transport

import (
	"regexp"
	"strings"
	"testing"
)

// TestExtractBetweenHandlesTheLiveStreamShape uses the exact byte sequence a
// real KoKo session produced, captured with a temporary frame dump:
//
//	; echo __JMSRC:${__rc}__\r\n ESC[?2004l\r HELLO\r\n marker\r\n __JMSRC:0__\r\n
//
// Three things must happen to it: the rc-capture echo tail is dropped, the
// bracketed-paste toggle is stripped, and CRLF becomes LF.
func TestExtractBetweenHandlesTheLiveStreamShape(t *testing.T) {
	const marker = "__JMSDONE_123__"
	// The PTY echoes the wrapped command line first (which contains the marker
	// once), then the command output, then the sentinel echo and exit status.
	stream := "echo HELLO; __rc=$?; echo " + marker + "; echo __JMSRC:${__rc}__\r\n" +
		"\x1b[?2004l\r" +
		"HELLO\r\n" +
		"testhost01\r\n" +
		marker + "\r\n__JMSRC:0__\r\n"

	got := extractBetween(stream, marker)
	want := "HELLO\ntesthost01"
	if got != want {
		t.Fatalf("extractBetween = %q, want %q", got, want)
	}
}

// TestExtractBetweenSkipsTheEchoedCommandLine covers the case where the shell
// echoed the whole wrapped command, so the first marker sits inside that echo
// and everything up to its newline is not output.
func TestExtractBetweenSkipsTheEchoedCommandLine(t *testing.T) {
	const marker = "__JMSDONE_456__"
	stream := "testuser@host:~$ echo hi; __rc=$?; echo " + marker + "; echo __JMSRC:${__rc}__\r\n" +
		"hi\r\n" +
		marker + "\r\n__JMSRC:0__\r\n"

	got := extractBetween(stream, marker)
	if got != "hi" {
		t.Fatalf("extractBetween = %q, want %q", got, "hi")
	}
}

func TestExtractBetweenWithoutClosingMarkerIsEmpty(t *testing.T) {
	const marker = "__JMSDONE_789__"
	// Only the echo arrived: the command is still running, so there is no
	// output to report yet.
	stream := "echo x; echo " + marker + "; echo __JMSRC:${__rc}__\r\n"
	if got := extractBetween(stream, marker); got != "" {
		t.Fatalf("extractBetween = %q, want empty while the command is still running", got)
	}
}

func TestStripANSIRemovesPasteTogglesAndBackspaces(t *testing.T) {
	cases := []struct{ in, want string }{
		{"\x1b[?2004lHELLO", "HELLO"},
		{"\x1b[?2004hHELLO\x1b[?2004l", "HELLO"},
		// The pattern deletes a backspace together with the character it
		// overwrites: the reference's ANSI_RE does the same, so text before
		// the backspace survives and the overwritten pair is gone.
		{"a\x08b", "a"},
		{"\x1b]0;title\x07text", "text"}, // an OSC title sequence
		{"plain", "plain"},
	}
	for _, tc := range cases {
		if got := stripANSI(tc.in); got != tc.want {
			t.Errorf("stripANSI(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestNormalizeNewlinesFoldsCRLFAndBareCR(t *testing.T) {
	if got := normalizeNewlines("a\r\nb\rc"); got != "a\nb\nc" {
		t.Fatalf("normalizeNewlines = %q, want %q", got, "a\nb\nc")
	}
}

// TestParseMarkersStillFindsTheExitCode guards the pairing: extractBetween and
// parseMarkers read the same stream, so a change to one must not break the
// other.
func TestParseMarkersStillFindsTheExitCode(t *testing.T) {
	const marker = "__JMSDONE_999__"
	stream := "echo x; echo " + marker + "\r\nout\r\n" + marker + "\r\n__JMSRC:7__\r\n"
	code, ok := parseMarkers(stream, marker)
	if !ok {
		t.Fatal("parseMarkers did not find the exit code")
	}
	if code != 7 {
		t.Fatalf("parseMarkers = %d, want 7", code)
	}
}

var _ = regexp.MustCompile
var _ = strings.TrimSpace
