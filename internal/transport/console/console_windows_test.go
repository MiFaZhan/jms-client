//go:build windows

package console

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// TestRedirectedOutputWritesTheRawBytes keeps the GBK fix from breaking
// redirection: a file has no console mode, so the bytes must pass through
// unbuffered and unchanged.
func TestRedirectedOutputWritesTheRawBytes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "out.txt")
	file, err := os.Create(path)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer file.Close()

	payload := []byte("\x1b[31m中\x1b[0m\n")
	sink := platform.newWriter(file)
	n, err := sink.Write(payload)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if n != len(payload) {
		t.Fatalf("Write returned %d, wanted %d", n, len(payload))
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if !bytes.Equal(data, payload) {
		t.Fatalf("file holds %q, wanted %q", data, payload)
	}
}

// TestDecodeUTF8KeepsASplitRuneForTheNextWrite is the Windows byte-stream
// rule: a multi-byte rune split across two reads must not become a
// replacement character on a GBK console.
func TestDecodeUTF8KeepsASplitRuneForTheNextWrite(t *testing.T) {
	full := []byte("中")
	if len(full) != 3 {
		t.Fatalf("expected a three-byte rune, got %d bytes", len(full))
	}
	for split := 1; split < len(full); split++ {
		head, tail := full[:split], full[split:]
		text, consumed := decodeUTF8(head)
		if consumed != 0 || text != "" {
			t.Fatalf("split at %d: decodeUTF8(%q) = %q, %d; wanted nothing consumed",
				split, head, text, consumed)
		}
		text, consumed = decodeUTF8(append(append([]byte(nil), head...), tail...))
		if text != "中" || consumed != 3 {
			t.Fatalf("split at %d: rejoined decode = %q, %d", split, text, consumed)
		}
	}
}

// TestDecodeUTF8PassesInvalidBytesThrough keeps a stream of truly invalid
// bytes from stalling the relay.
func TestDecodeUTF8PassesInvalidBytesThrough(t *testing.T) {
	invalid := []byte{0xff, 0xfe, 'a'}
	text, consumed := decodeUTF8(invalid)
	if consumed != len(invalid) {
		t.Fatalf("consumed %d of %d invalid bytes; the relay would stall", consumed, len(invalid))
	}
	if text == "" {
		t.Fatal("invalid bytes were silently dropped instead of being rendered")
	}
}

// TestDecodeUTF8AcceptsPlainASCIIAndTrailingASCII covers the common case: a
// rune split is only worth waiting for when the remainder really is a rune.
func TestDecodeUTF8AcceptsPlainASCIIAndTrailingASCII(t *testing.T) {
	text, consumed := decodeUTF8([]byte("ls -la\n"))
	if text != "ls -la\n" || consumed != 7 {
		t.Fatalf("decodeUTF8 = %q, %d", text, consumed)
	}
	// A rune lead byte followed by ASCII is not a prefix of any valid
	// sequence, so it must be rendered instead of buffered forever.
	broken := []byte{0xE4, 'x'}
	text, consumed = decodeUTF8(broken)
	if consumed != len(broken) {
		t.Fatalf("consumed %d of %d bytes, wanted all of them", consumed, len(broken))
	}
	if text == "" {
		t.Fatal("the broken sequence produced no output")
	}
}

// TestConsoleWriterBuffersASplitRune proves the rune buffering happens across
// two chunks, not just inside one decode.
func TestConsoleWriterBuffersASplitRune(t *testing.T) {
	w := &consoleWriter{}
	full := []byte("中")
	if got := w.decode(full[:1]); got != "" {
		t.Fatalf("the first chunk produced %q; the split rune must be held", got)
	}
	if got := w.decode(full[1:]); got != "中" {
		t.Fatalf("the second chunk produced %q, wanted the rejoined rune", got)
	}
	if len(w.pending) != 0 {
		t.Fatalf("buffered %q after the rune was completed", w.pending)
	}
}

// TestConsoleWriterDoesNotBufferInvalidBytes keeps a stream that is not valid
// UTF-8 from stalling.
func TestConsoleWriterDoesNotBufferInvalidBytes(t *testing.T) {
	w := &consoleWriter{}
	invalid := []byte{0xff, 0xfe}
	if got := w.decode(invalid); got == "" {
		t.Fatal("invalid bytes were held back; the relay would stall")
	}
	if len(w.pending) != 0 {
		t.Fatalf("invalid bytes stayed buffered: %q", w.pending)
	}
}
