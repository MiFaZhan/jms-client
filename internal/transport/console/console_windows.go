//go:build windows

package console

import (
	"fmt"
	"io"
	"os"
	"time"
	"unicode/utf16"
	"unicode/utf8"

	"golang.org/x/sys/windows"
	"golang.org/x/term"
)

// stdinOnlyTerminal is true here: a redirected stdout must not disable raw
// mode (DESIGN.md §11.5).
const stdinOnlyTerminal = true

// resizePoll is how often the Windows console size is sampled. The console
// API reports a resize as an input record, and the relay loop owns the input
// queue, so sampling is the only way to watch the size without stealing
// keystrokes.
const resizePoll = 250 * time.Millisecond

// platform binds the Windows console primitives.
//
// Raw mode is a console-mode change rather than a termios change, and the
// console host itself translates navigation keys into VT characters, so no
// escape decoding is attempted. The mode bits mirror the Python
// implementation's Windows console work: virtual-terminal input plus window
// events, with line input, echo, processed input, mouse and quick-edit
// cleared, because quick-edit would freeze the relay on a stray click.
var platform = &consoleOps{
	isTerminal: func(fd int) bool {
		return term.IsTerminal(fd)
	},
	size: func(fd int) (Size, error) {
		cols, rows, err := term.GetSize(fd)
		if err != nil {
			return Size{}, fmt.Errorf("console: cannot read the window size: %w", err)
		}
		return Size{Cols: cols, Rows: rows}, nil
	},
	enterRaw:    enterRaw,
	watchResize: watchResize,
	newWriter: func(f *os.File) io.Writer {
		return &consoleWriter{f: f}
	},
}

// enterRaw switches the console handle to raw mode and returns the idempotent
// restore function.
//
// The snapshot is taken immediately before the change, not at construction:
// another library may have adjusted the mode since the process started.
func enterRaw(fd int) (func(), error) {
	handle := windows.Handle(fd)
	var mode uint32
	if err := windows.GetConsoleMode(handle, &mode); err != nil {
		return nil, fmt.Errorf("console: cannot read the console mode: %w", err)
	}
	raw := (mode | windows.ENABLE_VIRTUAL_TERMINAL_INPUT | windows.ENABLE_WINDOW_INPUT |
		windows.ENABLE_EXTENDED_FLAGS) &^ (windows.ENABLE_LINE_INPUT | windows.ENABLE_ECHO_INPUT |
		windows.ENABLE_PROCESSED_INPUT | windows.ENABLE_QUICK_EDIT_MODE | windows.ENABLE_MOUSE_INPUT)
	if err := windows.SetConsoleMode(handle, raw); err != nil {
		return nil, fmt.Errorf("console: cannot switch stdin to raw mode: %w", err)
	}

	// Always restore the input mode. Leaving ENABLE_LINE_INPUT,
	// ENABLE_ECHO_INPUT and ENABLE_PROCESSED_INPUT cleared corrupts the
	// user's next command prompt after jms exits; arrow keys then appear as
	// raw escape sequences or otherwise behave incorrectly.
	restoreInput := func() { _ = windows.SetConsoleMode(handle, mode) }

	// Raw mode is useless if the console does not parse the VT sequences the
	// remote shell emits, so the output handle is switched too. A redirected
	// stdout has no console mode and needs no change.
	if outHandle, err := windows.GetStdHandle(windows.STD_OUTPUT_HANDLE); err == nil && outHandle != 0 {
		var outMode uint32
		if err := windows.GetConsoleMode(outHandle, &outMode); err == nil {
			vt := outMode | windows.ENABLE_PROCESSED_OUTPUT |
				windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING | windows.ENABLE_WRAP_AT_EOL_OUTPUT
			if err := windows.SetConsoleMode(outHandle, vt); err != nil {
				// Roll the input mode back so a failed raw mode never leaves
				// the console half-configured.
				restoreInput()
				return nil, fmt.Errorf("console: cannot enable VT output: %w", err)
			}
			return func() {
				_ = windows.SetConsoleMode(outHandle, outMode)
				restoreInput()
			}, nil
		}
	}
	return restoreInput, nil
}

// watchResize reports console window-size changes until stop is closed.
//
// Only real changes are reported, so a consumer can forward every report to
// the remote PTY without flooding it.
func watchResize(stop <-chan struct{}, report func(Size)) {
	handle, err := windows.GetStdHandle(windows.STD_INPUT_HANDLE)
	if err != nil || handle == 0 {
		return
	}
	last := Size{}
	if cols, rows, err := term.GetSize(int(handle)); err == nil {
		last = Size{Cols: cols, Rows: rows}
	}
	ticker := time.NewTicker(resizePoll)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
		}
		cols, rows, err := term.GetSize(int(handle))
		if err != nil {
			continue
		}
		size := Size{Cols: cols, Rows: rows}
		if size == last || size.Cols <= 0 || size.Rows <= 0 {
			continue
		}
		last = size
		report(size)
	}
}

// Write sends remote output as a UTF-8 byte stream decoded into the console's
// native wide-character API.
//
// This is the GBK fix: WriteConsoleW bypasses the ANSI code page entirely, so
// a cp936 console shows the remote output instead of mojibake, and because the
// bytes are decoded rather than reinterpreted, VT sequences survive intact.
// A redirected stdout has no console mode and takes the raw byte path.
func (w *consoleWriter) Write(p []byte) (int, error) {
	f := w.f
	if f == nil || len(p) == 0 {
		return 0, nil
	}
	handle := windows.Handle(f.Fd())
	var mode uint32
	if err := windows.GetConsoleMode(handle, &mode); err != nil {
		// Not a console handle (redirected to a file or a pipe): the bytes
		// are already correct and must not be buffered, or the tail of the
		// stream would never be flushed.
		return f.Write(p)
	}
	if text := w.decode(p); len(text) > 0 {
		if err := writeConsoleText(handle, text); err != nil {
			return 0, err
		}
	}
	return len(p), nil
}

// decode turns the next chunk of the byte stream into text for the console,
// carrying a rune that is split across two chunks over to the next call.
//
// Every input byte is accounted for, so Write can honour the io.Writer
// contract while the console still sees whole runes only.
func (w *consoleWriter) decode(p []byte) string {
	data := p
	if len(w.pending) > 0 {
		data = make([]byte, 0, len(w.pending)+len(p))
		data = append(data, w.pending...)
		data = append(data, p...)
	}
	text, consumed := decodeUTF8(data)
	w.pending = append(w.pending[:0], data[consumed:]...)
	return text
}

// consoleWriter adapts a console handle to the byte-stream io.Writer the
// Session exposes.
type consoleWriter struct {
	f *os.File
	// pending holds a UTF-8 rune split across two Write calls.
	pending []byte
}

// decodeUTF8 decodes as much of p as forms complete runes and reports how
// many bytes it consumed.
//
// A multi-byte rune split across two reads is left for the next call, so a
// GBK console never shows a replacement character mid-stream.
func decodeUTF8(p []byte) (string, int) {
	end := len(p)
	for end > 0 && !utf8.Valid(p[:end]) {
		end--
	}
	if end > 0 {
		return string(p[:end]), end
	}
	if incompleteUTF8(p) {
		return "", 0
	}
	// Genuinely invalid bytes are not recoverable by waiting, so they are
	// passed through and the console renders a replacement character instead
	// of the relay stalling.
	return string(p), len(p)
}

// incompleteUTF8 reports whether p is a strict prefix of a valid UTF-8
// sequence, which is the only case worth waiting for.
func incompleteUTF8(p []byte) bool {
	if len(p) == 0 || len(p) > utf8.UTFMax {
		return false
	}
	var need int
	switch {
	case p[0]&0xE0 == 0xC0:
		need = 2
	case p[0]&0xF0 == 0xE0:
		need = 3
	case p[0]&0xF8 == 0xF0:
		need = 4
	default:
		return false
	}
	if len(p) >= need {
		return false
	}
	for _, b := range p[1:] {
		if b&0xC0 != 0x80 {
			return false
		}
	}
	padded := make([]byte, need)
	copy(padded, p)
	for i := len(p); i < need; i++ {
		padded[i] = 0x80
	}
	return utf8.Valid(padded)
}

// writeConsoleText writes the text as UTF-16 in bounded chunks.
//
// The encoding is done here rather than with windows.UTF16FromString because
// that helper rejects an embedded NUL, and a remote terminal stream may
// legitimately contain one.
func writeConsoleText(handle windows.Handle, text string) error {
	const runeChunk = 4096
	for start := 0; start < len(text); {
		end := start + runeChunk
		if end > len(text) {
			end = len(text)
		}
		for end < len(text) && !utf8.RuneStart(text[end]) {
			end++
		}
		units := utf16.Encode([]rune(text[start:end]))
		for len(units) > 0 {
			var written uint32
			if err := windows.WriteConsole(handle, &units[0], uint32(len(units)), &written, nil); err != nil {
				return fmt.Errorf("console: cannot write output: %w", err)
			}
			if written == 0 {
				return fmt.Errorf("console: local terminal output closed")
			}
			units = units[written:]
		}
		start = end
	}
	return nil
}
