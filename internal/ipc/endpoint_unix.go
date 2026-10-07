//go:build !windows

package ipc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"syscall"
)

// endpointName renders the Unix domain-socket path.
//
// The socket lives under the configuration directory so its 0600 mode and its
// parent's 0700 mode together make it owner-only (DESIGN.md §11.6).
func endpointName(configDir string) string {
	return filepath.Join(endpointDir(configDir), endpointPrefix+endpointHash(configDir)+".sock")
}

// unixListener adapts net.UnixListener to endpointListener.
type unixListener struct {
	inner  *net.UnixListener
	path   string
	closed chan struct{}
	once   sync.Once
}

// Closed reports when the listener has been closed.
func (l *unixListener) Closed() <-chan struct{} { return l.closed }

// Accept accepts one connection, translating the post-Close error.
func (l *unixListener) Accept() (io.ReadWriteCloser, error) {
	conn, err := l.inner.Accept()
	if err != nil {
		select {
		case <-l.closed:
			return nil, errors.New("ipc: listener is closed")
		default:
		}
		return nil, err
	}
	return conn, nil
}

// Close stops the listener and removes the socket file.
//
// The file is removed here rather than at Listen time so a second host on the
// same endpoint fails with "address already in use" instead of silently
// stealing the first host's clients.
func (l *unixListener) Close() error {
	var err error
	l.once.Do(func() {
		close(l.closed)
		err = l.inner.Close()
		if rmErr := os.Remove(l.path); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) && err == nil {
			err = rmErr
		}
	})
	return err
}

// listenEndpoint binds the owner-only Unix socket.
//
// A socket file left behind by a crashed host would block the bind forever,
// so a stale socket is removed first. Staleness is proved by a refused
// connect, not by the file merely existing: an unreadable socket belongs to
// another user, and removing it would be an escalation, not a cleanup.
func listenEndpoint(addr string) (endpointListener, error) {
	if err := os.MkdirAll(filepath.Dir(addr), 0o700); err != nil {
		return nil, fmt.Errorf("ipc: cannot create the socket directory: %w", err)
	}
	if info, err := os.Stat(addr); err == nil && !info.IsDir() {
		conn, derr := net.Dial("unix", addr)
		switch {
		case derr == nil:
			_ = conn.Close()
			return nil, fmt.Errorf("ipc: %s is already in use", addr)
		case errors.Is(derr, syscall.ECONNREFUSED):
			// Nothing is listening: the owner is gone and the file is stale.
			if err := os.Remove(addr); err != nil {
				return nil, fmt.Errorf("ipc: cannot remove the stale socket %s: %w", addr, err)
			}
		default:
			return nil, fmt.Errorf("ipc: cannot use %s: %w", addr, derr)
		}
	}

	inner, err := net.ListenUnix("unix", &net.UnixAddr{Name: addr, Net: "unix"})
	if err != nil {
		return nil, fmt.Errorf("ipc: cannot listen on %s: %w", addr, err)
	}
	if err := os.Chmod(addr, socketFileMode); err != nil {
		_ = inner.Close()
		_ = os.Remove(addr)
		return nil, fmt.Errorf("ipc: cannot restrict %s to the owner: %w", addr, err)
	}
	return &unixListener{inner: inner, path: addr, closed: make(chan struct{})}, nil
}

// dialEndpoint connects to the Unix socket.
func dialEndpoint(ctx context.Context, addr string) (io.ReadWriteCloser, error) {
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "unix", addr)
	if err != nil {
		return nil, fmt.Errorf("ipc: cannot connect to %s: %w", addr, err)
	}
	return conn, nil
}
