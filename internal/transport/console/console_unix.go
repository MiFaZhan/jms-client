//go:build !windows

package console

import (
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"golang.org/x/term"
)

// stdinOnlyTerminal is false here: a Unix terminal is a property of the file
// descriptor, and term.IsTerminal is already correct for both streams.
const stdinOnlyTerminal = false

// platform binds the POSIX console primitives.
var platform = &consoleOps{
	isTerminal:  term.IsTerminal,
	size:        sizeOf,
	enterRaw:    enterRaw,
	watchResize: watchResize,
}

// sizeOf reads a terminal's window size.
func sizeOf(fd int) (Size, error) {
	cols, rows, err := term.GetSize(fd)
	if err != nil {
		return Size{}, fmt.Errorf("console: cannot read the window size: %w", err)
	}
	return Size{Cols: cols, Rows: rows}, nil
}

// enterRaw switches fd to raw mode and returns the idempotent restore
// function.
func enterRaw(fd int) (func(), error) {
	old, err := term.MakeRaw(fd)
	if err != nil {
		return nil, fmt.Errorf("console: cannot switch stdin to raw mode: %w", err)
	}
	return func() { _ = term.Restore(fd, old) }, nil
}

// watchResize reports SIGWINCH-driven window-size changes until stop is
// closed.
//
// The signal handler is installed for the duration of the session and removed
// on stop, so a second Open cannot leave a dangling handler.
func watchResize(stop <-chan struct{}, report func(Size)) {
	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	defer signal.Stop(winch)

	fd := int(os.Stdin.Fd())
	for {
		select {
		case <-stop:
			return
		case <-winch:
			size, err := sizeOf(fd)
			if err != nil {
				continue
			}
			report(size)
		}
	}
}

// newWriter wraps a local output file so a byte stream reaches the screen
// unchanged.
//
// os.File already writes bytes; the wrapper exists only to keep the shared
// code path identical on both platforms.
func newWriter(f *os.File) io.Writer {
	return f
}
