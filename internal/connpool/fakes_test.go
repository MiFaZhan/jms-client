package connpool

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/MiFaZhan/jms-client/internal/assets"
	"github.com/MiFaZhan/jms-client/internal/auth"
	"github.com/MiFaZhan/jms-client/internal/config"
	"github.com/MiFaZhan/jms-client/internal/transport"
)

// fakeTerminal is a transport.Terminal whose behaviour each test scripts.
// It records how often it was closed and which commands it was handed, so a
// test can assert on what the pool did to the connection rather than on the
// pool's private state.
type fakeTerminal struct {
	mu       sync.Mutex
	commands []string
	closes   int

	// execute, when set, runs in place of the empty success.
	execute func(ctx context.Context, cmd string, timeout time.Duration) (transport.Result, error)
}

func (f *fakeTerminal) Execute(ctx context.Context, cmd string, timeout time.Duration) (transport.Result, error) {
	f.mu.Lock()
	f.commands = append(f.commands, cmd)
	f.mu.Unlock()

	if f.execute != nil {
		return f.execute(ctx, cmd, timeout)
	}
	return transport.Result{}, nil
}

func (f *fakeTerminal) Backend() transport.BackendType { return transport.BackendSSH }

func (f *fakeTerminal) Interactive(context.Context) error { return nil }

func (f *fakeTerminal) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closes++
	return nil
}

func (f *fakeTerminal) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.commands)
}

func (f *fakeTerminal) closeCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closes
}

// overlapCounter counts how many fake executions are in flight at once.
// Tests assert on this peak to prove serialization, which is observable
// behaviour rather than a pool internal.
type overlapCounter struct {
	mu     sync.Mutex
	active int
	max    int
}

func (o *overlapCounter) enter() {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.active++
	if o.active > o.max {
		o.max = o.active
	}
}

func (o *overlapCounter) exit() {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.active--
}

func (o *overlapCounter) peak() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.max
}

// fakeBackend supplies the pool's Resolve and Connect seams and records what
// they were called with, so a test can assert the resolve -> connect
// hand-off without a network.
type fakeBackend struct {
	mu        sync.Mutex
	resolves  int
	connects  int
	terms     []*fakeTerminal
	lastAsset assets.Info
	lastOpts  transport.ConnectOptions
	lastSess  *auth.Session

	// connectFn, when set, replaces the default "return a fresh
	// fakeTerminal" behaviour; attempt is the 0-based connect counter.
	connectFn func(assetName string, attempt int) (transport.Terminal, error)
	// resolveFn, when set, replaces the default resolve.
	resolveFn func(name, account, protocol string) (assets.Info, error)
}

// pool returns a TerminalPool wired to the fake.
func (b *fakeBackend) pool() *TerminalPool {
	p := NewTerminalPool()
	p.Resolve = func(_ context.Context, sess *auth.Session, name, account, protocol string) (assets.Info, error) {
		b.mu.Lock()
		b.resolves++
		b.mu.Unlock()
		if b.resolveFn != nil {
			return b.resolveFn(name, account, protocol)
		}
		resolvedProtocol := protocol
		if resolvedProtocol == "" {
			resolvedProtocol = "ssh"
		}
		return assets.Info{
			ID:       "uuid-" + name,
			Name:     name,
			Address:  "10.0.0.9",
			Account:  account,
			Protocol: resolvedProtocol,
		}, nil
	}
	p.Connect = func(_ context.Context, sess *auth.Session, asset assets.Info, opts transport.ConnectOptions) (transport.Terminal, error) {
		b.mu.Lock()
		attempt := b.connects
		b.connects++
		b.lastAsset = asset
		b.lastOpts = opts
		b.lastSess = sess
		b.mu.Unlock()

		if b.connectFn != nil {
			return b.connectFn(asset.Name, attempt)
		}
		return b.record(&fakeTerminal{}), nil
	}
	return p
}

// record registers a terminal so a test can inspect it after the pool is
// done with it.
func (b *fakeBackend) record(t *fakeTerminal) *fakeTerminal {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.terms = append(b.terms, t)
	return t
}

func (b *fakeBackend) connectCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.connects
}

func (b *fakeBackend) resolveCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.resolves
}

func (b *fakeBackend) terminal(i int) *fakeTerminal {
	b.mu.Lock()
	defer b.mu.Unlock()
	if i >= len(b.terms) {
		return nil
	}
	return b.terms[i]
}

// testServer is the config every pool test keys on.
func testServer() *config.ServerConfig {
	return &config.ServerConfig{Name: "bastion", Username: "testuser"}
}

// testSession stands in for a logged-in session; the fakes never touch it.
func testSession(srv *config.ServerConfig) *auth.Session {
	return &auth.Session{Server: srv, BaseURL: "https://jms.example.com"}
}

// stamp sets a pooled terminal's last-use time, which is how the reaper tests
// make a connection look idle without sleeping. Reap takes its own injected
// now, so no production test seam is needed for the clock.
//
// The pool lock is released before the terminal lock is taken, matching the
// lock order the pool itself uses.
func stamp(p *TerminalPool, key termKey, at time.Time) {
	t := p.current(key)
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.lastUse = at
}

// pooled reports whether key is currently in the pool.
func pooled(p *TerminalPool, key termKey) bool {
	return p.current(key) != nil
}

// waitForCommand blocks until a scripted execution starts, or fails the
// test.
func waitForCommand(t *testing.T, ch <-chan string) string {
	t.Helper()
	select {
	case cmd := <-ch:
		return cmd
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for a command to start")
		return ""
	}
}
