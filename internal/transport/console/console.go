// Package console puts the local terminal into raw mode and forwards
// keystrokes and window resizes to a remote terminal.
//
// Windows needs the console API rather than termios, and the Python
// implementation needed three rounds of fixes for VT sequences, the GBK code
// page and held-key repeat counts (DESIGN.md §11.5), so this is written
// against x/term plus x/sys/windows rather than hand-rolled escape handling.
//
// The three Windows lessons are encoded here rather than re-learned:
//
//   - raw mode is a console-mode change, and the console host translates
//     navigation keys into VT characters itself, so no escape decoding is
//     needed and none is attempted;
//   - output goes out as a byte stream, which keeps a GBK code page from
//     turning UTF-8 into mojibake;
//   - only stdin decides whether the console is interactive, so redirecting
//     stdout does not disable raw mode.
package console

import (
	"errors"
	"io"
	"os"
	"sync"
)

// Size is a terminal window size in character cells.
type Size struct {
	Cols int
	Rows int
}

// ResizeFunc is called when the local window changes size.
type ResizeFunc func(Size)

// Options tunes an interactive session.
type Options struct {
	// In is the local input; nil means os.Stdin.
	In io.Reader
	// Out is the local output; nil means os.Stdout.
	Out io.Writer
	// OnResize is consulted when the window size changes.
	OnResize ResizeFunc
	// Initial is the starting size; the zero value means DefaultSize.
	Initial Size

	// ops overrides the platform primitives; nil means the real ones. It
	// exists so a test can drive resize handling and size reporting without
	// a console.
	ops *consoleOps
}

// Session is a raw-mode local console attached to a remote terminal.
type Session struct {
	// Read returns the next bytes typed locally.
	Read func(p []byte) (int, error)
	// Write sends remote output to the local screen.
	Write func(p []byte) (int, error)
	// Size reports the current local window size.
	Size func() Size
}

// DefaultSize is used when the real window size cannot be read.
var DefaultSize = Size{Cols: 200, Rows: 50}

// Open puts the local terminal into raw mode.
//
// Restore must be called (typically deferred) to put it back. It is
// idempotent and is also run on every error path here, so a failed Open never
// leaves the terminal unusable.
func Open(opts Options) (*Session, func(), error) {
	ops := opts.ops
	if ops == nil {
		ops = platform
	}
	in := opts.In
	if in == nil {
		in = os.Stdin
	}
	out := opts.Out
	if out == nil {
		out = os.Stdout
	}
	inFile, ok := in.(*os.File)
	if !ok {
		return nil, nil, errors.New("console: interactive mode requires a terminal on stdin")
	}
	if !ops.isTerminal(int(inFile.Fd())) {
		// Refusing beats corrupting the terminal: a redirected stdin has no
		// raw mode to set, and pretending otherwise would mangle output.
		return nil, nil, errors.New("console: interactive mode requires a terminal on stdin")
	}

	initial := opts.Initial
	if initial.Cols <= 0 || initial.Rows <= 0 {
		initial = DefaultSize
	}
	if size, err := ops.size(int(inFile.Fd())); err == nil {
		initial = size
	}

	restoreRaw, err := ops.enterRaw(int(inFile.Fd()))
	if err != nil {
		return nil, nil, err
	}
	if restoreRaw == nil {
		restoreRaw = func() {}
	}

	var (
		once    sync.Once
		stop    = make(chan struct{})
		mu      sync.Mutex
		current = initial
	)
	restore := func() {
		once.Do(func() {
			close(stop)
			restoreRaw()
		})
	}

	if opts.OnResize != nil {
		report := opts.OnResize
		go func() {
			ops.watchResize(stop, func(size Size) {
				mu.Lock()
				current = size
				mu.Unlock()
				report(size)
			})
		}()
	}

	// Remote output is written as a byte stream. On Windows a console handle
	// is wrapped so the bytes are decoded and handed to WriteConsoleW, which
	// is what survives a GBK code page (DESIGN.md §11.5).
	sink := out
	if f, ok := out.(*os.File); ok && ops.newWriter != nil {
		sink = ops.newWriter(f)
	}

	session := &Session{
		Read:  in.Read,
		Write: sink.Write,
		Size: func() Size {
			if size, err := ops.size(int(inFile.Fd())); err == nil {
				return size
			}
			mu.Lock()
			defer mu.Unlock()
			return current
		},
	}
	return session, restore, nil
}

// IsTerminal reports whether fd is a terminal.
//
// It is stdin-only on Windows, matching the fix the Python implementation
// needed for redirected output (DESIGN.md §11.5): checking stdout reports
// false for `jms login > file`, which would silently disable raw mode for a
// perfectly interactive session.
func IsTerminal(fd int) bool {
	if stdinOnlyTerminal && !isStdin(fd) {
		return false
	}
	return platform.isTerminal(fd)
}

// consoleOps bundles the per-platform console primitives.
//
// Each field is a function so a test can substitute one primitive while
// keeping the rest real.
type consoleOps struct {
	// isTerminal reports whether fd is an interactive console.
	isTerminal func(fd int) bool
	// size reads the window size of fd.
	size func(fd int) (Size, error)
	// enterRaw switches fd to raw mode and returns the idempotent restore
	// function.
	enterRaw func(fd int) (func(), error)
	// watchResize reports every window-size change until stop is closed. It
	// returns when it stops watching.
	watchResize func(stop <-chan struct{}, report func(Size))
	// newWriter wraps a local output file in the platform's byte-stream
	// writer; nil means plain file writes are already correct here.
	newWriter func(f *os.File) io.Writer
}

// isStdin reports whether fd is the standard input of this process.
func isStdin(fd int) bool {
	return fd >= 0 && fd == int(os.Stdin.Fd())
}
