//go:build windows

package ipc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// endpointName renders the Windows named-pipe path.
//
// The hash keeps two config directories from sharing a pipe; the pipe name
// itself carries no user data because a pipe name is world-readable.
func endpointName(configDir string) string {
	return `\\.\pipe\` + endpointPrefix + endpointHash(configDir)
}

// pipeSDDL grants full control to the creating user and to the local
// administrators, and nothing to anyone else (DESIGN.md「IPC 宿主」).
//
// P protects the DACL so an inherited ACE from a parent object cannot widen
// it; the owner SID is filled in at listen time.
const pipeSDDL = "D:P(A;;GA;;;%s)(A;;GA;;;BA)"

// errListenerClosed marks an accept that the listener's Close interrupted.
var errListenerClosed = errors.New("ipc: listener closed while waiting")

// pipeSecurityDescriptor builds the owner-only security descriptor.
func pipeSecurityDescriptor() (*windows.SECURITY_DESCRIPTOR, error) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, fmt.Errorf("ipc: cannot read the current user: %w", err)
	}
	sid := user.User.Sid.String()
	if sid == "" {
		return nil, errors.New("ipc: cannot render the current user SID")
	}
	sd, err := windows.SecurityDescriptorFromString(fmt.Sprintf(pipeSDDL, sid))
	if err != nil {
		return nil, fmt.Errorf("ipc: cannot build the pipe ACL: %w", err)
	}
	return sd, nil
}

// listenEndpoint creates the first pipe instance and returns the listener.
//
// The first instance is created eagerly so the endpoint exists the moment
// Listen returns; otherwise a client connecting immediately would be told no
// host is listening.
func listenEndpoint(addr string) (endpointListener, error) {
	sd, err := pipeSecurityDescriptor()
	if err != nil {
		return nil, err
	}
	name, err := windows.UTF16PtrFromString(addr)
	if err != nil {
		return nil, fmt.Errorf("ipc: invalid pipe name %q: %w", addr, err)
	}
	l := &pipeListener{addr: addr, sd: sd, name: name, done: make(chan struct{})}
	handle, err := l.createInstance(true)
	if err != nil {
		if errors.Is(err, windows.ERROR_ACCESS_DENIED) {
			return nil, fmt.Errorf("ipc: %s is already in use", addr)
		}
		return nil, err
	}
	l.pending = handle
	return l, nil
}

// pipeListener creates one pipe instance per accepted connection.
//
// Windows named pipes are instance-based rather than backlog-based: a client
// is served by connecting to a waiting instance, so the listener keeps one
// unconnected instance ready at all times and replaces it before handing the
// connected one to the caller. That is what keeps the endpoint continuously
// present, so Dial succeeds immediately after Listen.
type pipeListener struct {
	addr string
	sd   *windows.SECURITY_DESCRIPTOR
	name *uint16
	done chan struct{}

	mu      sync.Mutex
	pending windows.Handle // created, not yet waiting for a client
	waiting windows.Handle // inside ConnectNamedPipe, abortable by Close
	closed  bool
	once    sync.Once
}

func (l *pipeListener) Closed() <-chan struct{} { return l.done }

// Close stops the listener and aborts the instance that is waiting for a
// client, so a blocked Accept returns immediately. It is idempotent.
//
// Handles are closed here rather than by the accept goroutine: this is the
// only place that owns both the pending and the waiting instance, which is
// what prevents a double close from reaping a recycled handle.
func (l *pipeListener) Close() error {
	l.once.Do(func() {
		l.mu.Lock()
		l.closed = true
		waiting := l.waiting
		l.waiting = 0
		pending := l.pending
		l.pending = 0
		l.mu.Unlock()
		close(l.done)
		if waiting != 0 {
			// CancelIoEx completes the pending overlapped connect so the
			// accept goroutine can exit instead of leaking.
			_ = windows.CancelIoEx(waiting, nil)
			_ = windows.CloseHandle(waiting)
		}
		if pending != 0 {
			_ = windows.CloseHandle(pending)
		}
	})
	return nil
}

// createInstance creates the next instance. FILE_FLAG_FIRST_PIPE_INSTANCE is
// applied only to the process's first instance, so a second host on the same
// name fails loudly instead of silently sharing the endpoint.
func (l *pipeListener) createInstance(first bool) (windows.Handle, error) {
	sa := &windows.SecurityAttributes{
		Length:             uint32(unsafe.Sizeof(windows.SecurityAttributes{})),
		SecurityDescriptor: l.sd,
		InheritHandle:      0,
	}
	flags := uint32(windows.PIPE_ACCESS_DUPLEX | windows.FILE_FLAG_OVERLAPPED)
	if first {
		flags |= windows.FILE_FLAG_FIRST_PIPE_INSTANCE
	}
	handle, err := windows.CreateNamedPipe(
		l.name,
		flags,
		windows.PIPE_TYPE_BYTE|windows.PIPE_READMODE_BYTE|windows.PIPE_WAIT|
			windows.PIPE_REJECT_REMOTE_CLIENTS,
		windows.PIPE_UNLIMITED_INSTANCES,
		64*1024, 64*1024,
		0,
		sa,
	)
	if err != nil {
		return 0, fmt.Errorf("ipc: cannot create the pipe %q: %w", l.addr, err)
	}
	return handle, nil
}

func (l *pipeListener) Accept() (io.ReadWriteCloser, error) {
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return nil, errListenerClosed
	}
	handle := l.pending
	l.pending = 0
	if handle == 0 {
		// Only reachable when the previous replacement instance could not be
		// created; create one outside the lock.
		l.mu.Unlock()
		next, err := l.createInstance(false)
		if err != nil {
			return nil, err
		}
		handle = next
		l.mu.Lock()
		if l.closed {
			l.mu.Unlock()
			_ = windows.CloseHandle(handle)
			return nil, errListenerClosed
		}
	}
	l.waiting = handle
	l.mu.Unlock()

	conn, err := l.acceptOne(handle)
	if err != nil {
		if errors.Is(err, errListenerClosed) {
			// Close owns the handle: it took it out of waiting before it
			// closed the listener.
			return nil, err
		}
		_ = windows.CloseHandle(handle)
		return nil, err
	}

	// Replace the instance before handing this one over, so the endpoint is
	// never absent and a client that connects next is not turned away.
	if next, cerr := l.createInstance(false); cerr == nil {
		l.mu.Lock()
		if l.closed {
			l.mu.Unlock()
			_ = windows.CloseHandle(next)
		} else {
			l.pending = next
			l.mu.Unlock()
		}
	}
	return conn, nil
}

// acceptOne waits for a client on an overlapped instance.
//
// The wait is overlapped so Close's CancelIoEx can complete it; the poll
// interval exists only so the done channel is observed even if the cancel
// cannot be issued.
func (l *pipeListener) acceptOne(handle windows.Handle) (*pipeConn, error) {
	event, err := windows.CreateEvent(nil, 0, 0, nil)
	if err != nil {
		return nil, fmt.Errorf("ipc: cannot create a pipe event: %w", err)
	}
	defer windows.CloseHandle(event)

	overlapped := &windows.Overlapped{HEvent: event}
	err = windows.ConnectNamedPipe(handle, overlapped)
	switch {
	case err == nil, errors.Is(err, windows.ERROR_PIPE_CONNECTED):
		// ERROR_PIPE_CONNECTED means a client raced in before the wait.
		return l.finishAccept(handle)
	case errors.Is(err, windows.ERROR_IO_PENDING):
	default:
		return nil, fmt.Errorf("ipc: connecting a client failed: %w", err)
	}

	for {
		select {
		case <-l.done:
			return nil, errListenerClosed
		default:
		}
		res, werr := windows.WaitForSingleObject(event, 50)
		if werr != nil {
			return nil, fmt.Errorf("ipc: waiting for a client failed: %w", werr)
		}
		if res == uint32(windows.WAIT_TIMEOUT) {
			continue
		}
		var done uint32
		if gerr := windows.GetOverlappedResult(handle, overlapped, &done, false); gerr != nil {
			return nil, fmt.Errorf("ipc: connecting a client failed: %w", gerr)
		}
		return l.finishAccept(handle)
	}
}

// finishAccept takes ownership of a connected handle.
//
// The hand-off happens under the listener's lock, so exactly one of Close and
// the accepting goroutine ever owns the handle: whoever clears `waiting`
// first, wins. That is what rules out both a double close and a leaked
// handle when a client arrives during shutdown.
func (l *pipeListener) finishAccept(handle windows.Handle) (*pipeConn, error) {
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return nil, errListenerClosed
	}
	l.waiting = 0
	l.mu.Unlock()

	conn, err := newPipeConn(handle)
	if err != nil {
		_ = windows.CloseHandle(handle)
		return nil, err
	}
	return conn, nil
}

// pipeConn is one connected named-pipe instance.
//
// The handle is opened for overlapped I/O on both ends, which is what makes
// Close able to abort an in-flight read: CancelIoEx completes the operation
// and the reader observes the abort instead of parking forever.
type pipeConn struct {
	handle windows.Handle
	// readEvent and writeEvent are separate because a read and a write may
	// be in flight at the same time, and one shared event would be reset out
	// from under the other.
	readEvent  windows.Handle
	writeEvent windows.Handle
	closed     chan struct{}
	once       sync.Once
	// readMu and writeMu make Close wait for in-flight operations before the
	// event handles are released.
	readMu  sync.Mutex
	writeMu sync.Mutex
}

func newPipeConn(handle windows.Handle) (*pipeConn, error) {
	readEvent, err := windows.CreateEvent(nil, 1, 0, nil)
	if err != nil {
		return nil, fmt.Errorf("ipc: cannot create a pipe event: %w", err)
	}
	writeEvent, err := windows.CreateEvent(nil, 1, 0, nil)
	if err != nil {
		_ = windows.CloseHandle(readEvent)
		return nil, fmt.Errorf("ipc: cannot create a pipe event: %w", err)
	}
	return &pipeConn{
		handle:     handle,
		readEvent:  readEvent,
		writeEvent: writeEvent,
		closed:     make(chan struct{}),
	}, nil
}

// Read reads from the pipe.
//
// A byte-mode pipe read returns as soon as one byte arrives, which is what
// the line reader wants; a broken pipe becomes io.EOF so the serving
// goroutine exits cleanly.
func (c *pipeConn) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	c.readMu.Lock()
	defer c.readMu.Unlock()

	select {
	case <-c.closed:
		return 0, io.EOF
	default:
	}

	var done uint32
	transferred, err := c.submit(c.readEvent, func(ov *windows.Overlapped) error {
		return windows.ReadFile(c.handle, p, &done, ov)
	})
	if err != nil {
		if isPipeGone(err) {
			return int(transferred), io.EOF
		}
		return int(transferred), err
	}
	if transferred == 0 {
		return 0, io.EOF
	}
	return int(transferred), nil
}

// Write writes a complete byte stream to the pipe.
func (c *pipeConn) Write(p []byte) (int, error) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()

	select {
	case <-c.closed:
		return 0, io.ErrClosedPipe
	default:
	}

	total := 0
	for total < len(p) {
		chunk := p[total:]
		var done uint32
		transferred, err := c.submit(c.writeEvent, func(ov *windows.Overlapped) error {
			return windows.WriteFile(c.handle, chunk, &done, ov)
		})
		total += int(transferred)
		if err != nil {
			if isPipeGone(err) {
				return total, io.ErrClosedPipe
			}
			return total, err
		}
		if transferred == 0 {
			return total, io.ErrShortWrite
		}
	}
	return total, nil
}

// submit issues one overlapped operation and waits for it, returning the
// number of bytes transferred.
//
// The event is manual-reset, so it is reset before each operation and the
// wait observes this operation's completion rather than the previous one's.
func (c *pipeConn) submit(event windows.Handle, issue func(*windows.Overlapped) error) (uint32, error) {
	if err := windows.ResetEvent(event); err != nil {
		return 0, fmt.Errorf("ipc: cannot reset the pipe event: %w", err)
	}
	overlapped := &windows.Overlapped{HEvent: event}
	err := issue(overlapped)
	if err != nil && !errors.Is(err, windows.ERROR_IO_PENDING) {
		return 0, err
	}
	for {
		select {
		case <-c.closed:
			return 0, io.ErrClosedPipe
		default:
		}
		res, werr := windows.WaitForSingleObject(event, 50)
		if werr != nil {
			return 0, fmt.Errorf("ipc: waiting for pipe I/O failed: %w", werr)
		}
		if res == uint32(windows.WAIT_TIMEOUT) {
			continue
		}
		var done uint32
		if gerr := windows.GetOverlappedResult(c.handle, overlapped, &done, false); gerr != nil {
			return done, gerr
		}
		return done, nil
	}
}

// Close closes the pipe instance. It is idempotent.
//
// Order matters: the cancel must complete in-flight operations, and the locks
// must then observe that they finished, before the event handle they wait on
// is released.
func (c *pipeConn) Close() error {
	var err error
	c.once.Do(func() {
		close(c.closed)
		_ = windows.CancelIoEx(c.handle, nil)
		c.readMu.Lock()
		c.writeMu.Lock()
		_ = windows.CloseHandle(c.readEvent)
		_ = windows.CloseHandle(c.writeEvent)
		err = windows.CloseHandle(c.handle)
		c.readMu.Unlock()
		c.writeMu.Unlock()
	})
	return err
}

// isPipeGone reports the errors that mean the peer went away.
func isPipeGone(err error) bool {
	return errors.Is(err, windows.ERROR_BROKEN_PIPE) ||
		errors.Is(err, windows.ERROR_PIPE_NOT_CONNECTED) ||
		errors.Is(err, windows.ERROR_OPERATION_ABORTED) ||
		errors.Is(err, windows.ERROR_NO_DATA) ||
		errors.Is(err, windows.ERROR_HANDLE_EOF) ||
		errors.Is(err, io.ErrClosedPipe)
}

// dialTimeout bounds the retry loop for a pipe whose instances are all busy.
const dialTimeout = 5 * time.Second

// dialEndpoint connects to the named pipe.
//
// ERROR_PIPE_BUSY means every instance is already serving another client;
// that is retried for dialTimeout rather than reported at once, because a host
// that is serving clients frees an instance as soon as one finishes.
func dialEndpoint(ctx context.Context, addr string) (io.ReadWriteCloser, error) {
	name, err := windows.UTF16PtrFromString(addr)
	if err != nil {
		return nil, fmt.Errorf("ipc: invalid pipe name %q: %w", addr, err)
	}
	deadline := time.Now().Add(dialTimeout)
	for {
		handle, err := windows.CreateFile(
			name,
			windows.GENERIC_READ|windows.GENERIC_WRITE,
			0, nil,
			windows.OPEN_EXISTING,
			windows.FILE_FLAG_OVERLAPPED, 0,
		)
		if err == nil {
			return newPipeConn(handle)
		}
		if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) {
			return nil, fmt.Errorf("ipc: no host is listening on %s", addr)
		}
		if !errors.Is(err, windows.ERROR_PIPE_BUSY) {
			return nil, fmt.Errorf("ipc: cannot connect to %s: %w", addr, err)
		}
		if cerr := ctx.Err(); cerr != nil {
			return nil, cerr
		}
		if !time.Now().Before(deadline) {
			return nil, fmt.Errorf("ipc: %s is busy", addr)
		}
		_ = waitNamedPipe(name, 100)
	}
}

// procWaitNamedPipe is declared here because x/sys/windows does not wrap it.
var procWaitNamedPipe = windows.NewLazySystemDLL("kernel32.dll").NewProc("WaitNamedPipeW")

// waitNamedPipe blocks until an instance of the pipe frees up or the timeout
// elapses. A failure is ignored: the caller's own deadline is authoritative.
func waitNamedPipe(name *uint16, milliseconds uint32) error {
	r1, _, err := procWaitNamedPipe.Call(uintptr(unsafe.Pointer(name)), uintptr(milliseconds))
	if r1 == 0 {
		return err
	}
	return nil
}
