package console

import (
	"bytes"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeOps builds a consoleOps whose primitives are recorded, so the shared
// Open logic can be exercised without a real console.
type fakeOps struct {
	mu sync.Mutex

	terminal bool
	size     Size
	sizeErr  error

	rawCalls    int
	restoreCall int
	rawErr      error

	report func(Size)
}

func (f *fakeOps) ops() *consoleOps {
	return &consoleOps{
		isTerminal: func(int) bool {
			f.mu.Lock()
			defer f.mu.Unlock()
			return f.terminal
		},
		size: func(int) (Size, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			return f.size, f.sizeErr
		},
		enterRaw: func(int) (func(), error) {
			f.mu.Lock()
			f.rawCalls++
			err := f.rawErr
			f.mu.Unlock()
			if err != nil {
				return nil, err
			}
			return func() {
				f.mu.Lock()
				f.restoreCall++
				f.mu.Unlock()
			}, nil
		},
		watchResize: func(stop <-chan struct{}, report func(Size)) {
			f.mu.Lock()
			f.report = report
			f.mu.Unlock()
			<-stop
		},
	}
}

func (f *fakeOps) counts() (raw, restore int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.rawCalls, f.restoreCall
}

// TestOpenOnAPipeFailsClearly covers the redirected-stdin case: raw mode is
// impossible, so Open must refuse instead of corrupting output.
func TestOpenOnAPipeFailsClearly(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	defer reader.Close()
	defer writer.Close()

	session, restore, err := Open(Options{In: reader, Out: &bytes.Buffer{}})
	if err == nil {
		if restore != nil {
			restore()
		}
		t.Fatalf("Open on a pipe returned session %v, wanted an error", session)
	}
	if !strings.Contains(err.Error(), "terminal") {
		t.Fatalf("error %q does not explain that a terminal is required", err)
	}
	if restore != nil {
		t.Fatal("a failed Open must not hand back a restore function")
	}
}

// TestOpenRejectsANonFileInput covers a bytes.Buffer, which has no descriptor
// to put into raw mode.
func TestOpenRejectsANonFileInput(t *testing.T) {
	_, _, err := Open(Options{In: &bytes.Buffer{}})
	if err == nil {
		t.Fatal("Open with a non-file stdin must fail")
	}
	if !strings.Contains(err.Error(), "terminal") {
		t.Fatalf("error %q is not actionable", err)
	}
}

// TestRestoreIsIdempotent is the deferred-cleanup contract: a caller that
// defers restore and also calls it on an error path must not double-restore.
func TestRestoreIsIdempotent(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	defer reader.Close()
	defer writer.Close()

	fake := &fakeOps{terminal: true, size: Size{Cols: 80, Rows: 24}}
	session, restore, err := Open(Options{
		In:      reader,
		Out:     &bytes.Buffer{},
		ops:     fake.ops(),
		Initial: Size{Cols: 120, Rows: 40},
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if session == nil {
		t.Fatal("Open returned a nil session")
	}

	restore()
	restore()
	restore()

	if raw, restored := fake.counts(); raw != 1 || restored != 1 {
		t.Fatalf("raw=%d restore=%d, wanted exactly one of each", raw, restored)
	}
}

// TestOpenReportsTheInitialSizeFromTheSizeSource checks that the size source
// wins over the caller's guess.
func TestOpenReportsTheInitialSizeFromTheSizeSource(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	defer reader.Close()
	defer writer.Close()

	fake := &fakeOps{terminal: true, size: Size{Cols: 100, Rows: 30}}
	session, restore, err := Open(Options{
		In:      reader,
		Out:     &bytes.Buffer{},
		ops:     fake.ops(),
		Initial: Size{Cols: 10, Rows: 5},
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer restore()

	if got := session.Size(); got != (Size{Cols: 100, Rows: 30}) {
		t.Fatalf("Size = %+v, wanted the injected 100x30", got)
	}
}

// TestOpenFallsBackToTheDefaultSize keeps a console whose size cannot be read
// usable rather than zero-sized.
func TestOpenFallsBackToTheDefaultSize(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	defer reader.Close()
	defer writer.Close()

	fake := &fakeOps{terminal: true, sizeErr: errors.New("no window size")}
	session, restore, err := Open(Options{In: reader, Out: &bytes.Buffer{}, ops: fake.ops()})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer restore()

	if got := session.Size(); got != DefaultSize {
		t.Fatalf("Size = %+v, wanted the default %+v", got, DefaultSize)
	}
}

// TestResizeNotificationReportsTheNewSize is the resize contract: the
// callback fires with the size the source reports.
func TestResizeNotificationReportsTheNewSize(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	defer reader.Close()
	defer writer.Close()

	fake := &fakeOps{terminal: true, size: Size{Cols: 80, Rows: 24}}
	reported := make(chan Size, 4)
	session, restore, err := Open(Options{
		In:       reader,
		Out:      &bytes.Buffer{},
		ops:      fake.ops(),
		OnResize: func(size Size) { reported <- size },
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer restore()
	if session == nil {
		t.Fatal("Open returned a nil session")
	}

	// The fake watcher waits for the stop channel, so the reporter is
	// registered by the time Open returns; retry briefly in case the
	// goroutine has not stored it yet.
	var report func(Size)
	deadline := time.Now().Add(5 * time.Second)
	for report == nil && time.Now().Before(deadline) {
		fake.mu.Lock()
		report = fake.report
		fake.mu.Unlock()
		if report == nil {
			time.Sleep(time.Millisecond)
		}
	}
	if report == nil {
		t.Fatal("Open never started the resize watcher")
	}

	report(Size{Cols: 132, Rows: 43})
	select {
	case got := <-reported:
		if got != (Size{Cols: 132, Rows: 43}) {
			t.Fatalf("resize reported %+v, wanted 132x43", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the resize callback never fired")
	}
}

// TestRestoreStopsTheResizeWatcher checks that restore releases the watcher
// goroutine, so a session cannot leak it.
func TestRestoreStopsTheResizeWatcher(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	defer reader.Close()
	defer writer.Close()

	fake := &fakeOps{terminal: true, size: Size{Cols: 80, Rows: 24}}
	stopped := make(chan struct{})
	fakeOps := fake.ops()
	inner := fakeOps.watchResize
	fakeOps.watchResize = func(stop <-chan struct{}, report func(Size)) {
		inner(stop, report)
		close(stopped)
	}

	_, restore, err := Open(Options{
		In:       reader,
		Out:      &bytes.Buffer{},
		ops:      fakeOps,
		OnResize: func(Size) {},
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	restore()

	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("restore did not stop the resize watcher")
	}
}

// TestOpenRunsRestoreWhenRawModeFails keeps a failed Open from leaving the
// console half-configured.
func TestOpenRunsRestoreWhenRawModeFails(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	defer reader.Close()
	defer writer.Close()

	fake := &fakeOps{terminal: true, rawErr: errors.New("console mode refused")}
	session, restore, err := Open(Options{In: reader, Out: &bytes.Buffer{}, ops: fake.ops()})
	if err == nil {
		t.Fatal("Open must fail when raw mode cannot be entered")
	}
	if session != nil || restore != nil {
		t.Fatal("a failed Open must return neither a session nor a restore function")
	}
	if raw, restored := fake.counts(); raw != 1 || restored != 0 {
		t.Fatalf("raw=%d restore=%d, wanted one attempt and no restore", raw, restored)
	}
}

// TestSessionWriteReachesTheOutput proves the session is a byte-stream sink
// rather than a formatter.
func TestSessionWriteReachesTheOutput(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	defer reader.Close()
	defer writer.Close()

	var out bytes.Buffer
	fake := &fakeOps{terminal: true, size: Size{Cols: 80, Rows: 24}}
	session, restore, err := Open(Options{In: reader, Out: &out, ops: fake.ops()})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer restore()

	payload := []byte("\x1b[31m中文\x1b[0m\n")
	if _, err := session.Write(payload); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if !bytes.Equal(out.Bytes(), payload) {
		t.Fatalf("output = %q, wanted the exact byte stream %q", out.Bytes(), payload)
	}
}

// TestSessionReadForwardsInput keeps Read wired to the local input.
func TestSessionReadForwardsInput(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	defer reader.Close()
	defer writer.Close()

	fake := &fakeOps{terminal: true, size: Size{Cols: 80, Rows: 24}}
	session, restore, err := Open(Options{In: reader, Out: &bytes.Buffer{}, ops: fake.ops()})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer restore()

	if _, err := writer.Write([]byte("ls\n")); err != nil {
		t.Fatalf("write to the pipe: %v", err)
	}
	buf := make([]byte, 16)
	n, err := session.Read(buf)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got := string(buf[:n]); got != "ls\n" {
		t.Fatalf("Read returned %q, wanted %q", got, "ls\n")
	}
}

// TestIsTerminalOnAPipeIsFalse is the redirected-output regression: a pipe is
// not a terminal on any platform.
func TestIsTerminalOnAPipeIsFalse(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	defer reader.Close()
	defer writer.Close()

	if IsTerminal(int(reader.Fd())) {
		t.Fatal("IsTerminal reported true for a pipe")
	}
	if IsTerminal(-1) {
		t.Fatal("IsTerminal reported true for an invalid descriptor")
	}
}

// TestIsTerminalIsStdinOnlyOnWindows pins the platform split: on Windows only
// stdin may report true, because a redirected stdout must not disable raw
// mode (DESIGN.md「交互式终端」).
func TestIsTerminalIsStdinOnlyOnWindows(t *testing.T) {
	if !stdinOnlyTerminal {
		t.Skip("this platform treats each descriptor independently")
	}
	if IsTerminal(int(os.Stdout.Fd())) {
		t.Fatal("Windows IsTerminal must be stdin-only")
	}
}

// TestIsTerminalPositiveCaseRunsWhenAttached exercises the positive path only
// when the test runner actually has a console.
func TestIsTerminalPositiveCaseRunsWhenAttached(t *testing.T) {
	if !platform.isTerminal(int(os.Stdin.Fd())) {
		t.Skip("the test runner has no terminal on stdin")
	}
	if !IsTerminal(int(os.Stdin.Fd())) {
		t.Fatal("IsTerminal disagreed with the platform primitive for stdin")
	}
}
