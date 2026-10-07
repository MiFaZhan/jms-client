// Package connpool keeps authenticated sessions and live terminals alive
// across calls, which is the rewrite's central difference from the Python
// implementation.
//
// The Python MCP server performed a full login -> resolve -> connect ->
// teardown cycle for every tool call. Here a session is logged in once and
// reused, and a terminal is opened once and kept until it goes idle or
// breaks (DESIGN.md §4).
package connpool

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/MiFaZhan/jms-client/internal/assets"
	"github.com/MiFaZhan/jms-client/internal/auth"
	"github.com/MiFaZhan/jms-client/internal/config"
	"github.com/MiFaZhan/jms-client/internal/transport"
)

// Defaults from DESIGN.md §4.5.
const (
	// IdleTTL is how long a terminal may sit unused before the reaper
	// closes it.
	IdleTTL = 10 * time.Minute
	// ResolveTTL caches an asset resolution.
	ResolveTTL = 5 * time.Minute
	// ReapInterval is the reaper's scan period.
	ReapInterval = 60 * time.Second
)

// ErrClosed reports use of a pool after Close.
var ErrClosed = errors.New("pool is closed")

// sessionCall is one login in flight, shared by every caller that asks for
// the same alias while it runs.
type sessionCall struct {
	done chan struct{}
	sess *auth.Session
	err  error
}

// SessionPool caches authenticated sessions, keyed by server alias.
//
// Health is verified lazily: there is no periodic probe. A call that hits an
// api.AuthError re-logs in once and retries the operation, which avoids idle
// probe traffic while still recovering from an expired token (DESIGN.md
// §4.2).
//
// The pool's half of that retry-once policy is Invalidate: the caller that
// sees an api.AuthError drops the alias and re-issues the operation once, and
// the drop makes the retry perform a real login instead of handing back the
// session that just failed.
type SessionPool struct {
	mu       sync.Mutex
	sessions map[string]*auth.Session
	// creating holds the logins in flight, so two callers asking for the
	// same alias share one login instead of racing. MCP clients do issue
	// concurrent calls, so this is load-bearing, not an optimisation.
	creating map[string]*sessionCall
	closed   bool
}

// NewSessionPool returns an empty pool.
func NewSessionPool() *SessionPool {
	return &SessionPool{sessions: map[string]*auth.Session{}}
}

// Get returns a session for srv, logging in when necessary.
//
// creds supplies the password and TOTP secret; otpPrompt is consulted when
// the server demands MFA and no secret is stored.
//
// The key is the server alias: one alias means one endpoint, and a session is
// bound to the address it logged in against (DESIGN.md §4.7 point 1). The
// alias is deliberately part of the key even though baseURL identifies the
// endpoint, because the alias is what Invalidate is given and what the CLI
// and MCP surfaces name.
func (p *SessionPool) Get(ctx context.Context, srv *config.ServerConfig, baseURL string,
	creds auth.Credentials, otpPrompt func() (string, error)) (*auth.Session, error) {
	if p == nil {
		return nil, ErrClosed
	}
	if srv == nil {
		return nil, errors.New("connpool: Get needs a server config")
	}
	alias := srv.Name

	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil, ErrClosed
	}
	if sess, ok := p.sessions[alias]; ok {
		p.mu.Unlock()
		return sess, nil
	}
	if call, ok := p.creating[alias]; ok {
		p.mu.Unlock()
		select {
		case <-call.done:
			return call.sess, call.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	call := &sessionCall{done: make(chan struct{})}
	if p.creating == nil {
		p.creating = map[string]*sessionCall{}
	}
	p.creating[alias] = call
	p.mu.Unlock()

	sess, err := auth.Login(ctx, srv, baseURL, creds, otpPrompt)

	p.mu.Lock()
	delete(p.creating, alias)
	if err == nil && p.closed {
		// A Close that raced this login wins: the pool no longer hands out
		// sessions, so the fresh one is dropped rather than cached.
		err, sess = ErrClosed, nil
	}
	if err == nil {
		if p.sessions == nil {
			p.sessions = map[string]*auth.Session{}
		}
		p.sessions[alias] = sess
	}
	p.mu.Unlock()

	call.sess, call.err = sess, err
	close(call.done)
	return sess, err
}

// Invalidate drops the cached session for an alias so the next Get logs in
// again. It is called when a call hits an authentication error.
//
// A login already in flight is not cancelled: it finishes and caches its
// result, which is harmless because that login is newer than the session
// being dropped.
func (p *SessionPool) Invalidate(alias string) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.sessions, alias)
}

// Close drops every cached session.
//
// A session owns no closable handle — its HTTP keep-alive connections are
// reaped by the transport — so dropping the references is the whole teardown.
// Close is idempotent, and a nil pool behaves as a closed one.
func (p *SessionPool) Close() error {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed = true
	p.sessions = nil
	return nil
}

// TerminalPool keeps live terminals keyed by server, asset, account and
// protocol, and serializes execution per terminal.
type TerminalPool struct {
	mu        sync.Mutex
	terminals map[termKey]*pooledTerm
	// creating holds the terminals being built, so concurrent calls for one
	// key share a single connect instead of opening two connections.
	creating map[termKey]*createCall
	closed   bool

	// hits and cold are cumulative, so the numbers survive a reaped or
	// rebuilt terminal.
	hits int
	cold int

	// Connect and Resolve are seams so tests can drive the pool without a
	// network.
	Connect func(ctx context.Context, sess *auth.Session, asset assets.Info, opts transport.ConnectOptions) (transport.Terminal, error)
	Resolve func(ctx context.Context, sess *auth.Session, name, account, protocol string) (assets.Info, error)
}

// NewTerminalPool returns an empty terminal pool.
//
// The Connect and Resolve seams are left nil so the caller wires them; the
// CLI sets them to the real transport functions, and tests set fakes.
func NewTerminalPool() *TerminalPool {
	return &TerminalPool{terminals: map[termKey]*pooledTerm{}}
}

// createCall is one terminal being built, shared by every caller that asks
// for the same key while it is in flight.
type createCall struct {
	done chan struct{}
	term *pooledTerm
	err  error
}

// termKey identifies one pooled terminal.
//
// Account and protocol are part of the key because JumpServer binds both into
// the connection token: the same asset as @USER and as a named account, or
// over ssh and sftp, are different connections.
type termKey struct {
	Server   string
	Asset    string
	Account  string
	Protocol string
}

// pooledTerm is one terminal plus its serialization lock and last-use time.
type pooledTerm struct {
	term transport.Terminal
	// mu serializes execution on this terminal: a shared PTY or
	// exec-channel cannot carry two commands at once, and ordering keeps
	// output attribution unambiguous (DESIGN.md §4.3 point 1). It also
	// guards lastUse, so the reaper never closes a terminal mid-command.
	mu      sync.Mutex
	lastUse time.Time
}

// ExecOptions tunes one pooled execution.
type ExecOptions struct {
	Account  string
	Protocol string
	// Backend selects the transport when the terminal is built. A terminal
	// already in the pool keeps the backend it was built with, because the
	// key does not include it.
	Backend transport.BackendType
	Timeout time.Duration
	// OnBackend, when set, is called with the backend the terminal actually
	// uses, once the terminal is in hand. It lets a caller record backend
	// memory (DESIGN.md §4.7 point 4) without probing the terminal afterwards.
	OnBackend func(transport.BackendType)
}

// Exec runs one command on a pooled terminal, opening it when necessary.
//
// The same asset always uses the same terminal, and execution is serialized
// on that terminal: a shared PTY cannot interleave two commands, and ordering
// keeps output attribution unambiguous (DESIGN.md §4.3 point 1). Different
// assets use different terminals and run in parallel.
//
// Draining the previous command's residual output is the transport's job —
// transport.Terminal.Execute drains before it sends, because only the backend
// knows where its stream ended (DESIGN.md §4.3 point 3); the lock this pool
// holds is what makes that drain meaningful.
//
// A failure that happened before the command produced any output evicts the
// terminal and replays the command once on a connection built from scratch. A
// command that already emitted output is never replayed: the remote host has
// seen it, and rm/reboot are not idempotent (DESIGN.md §4.3 point 5 and §4.7
// point 3). Output that arrived before the failure is returned with the error.
func (p *TerminalPool) Exec(ctx context.Context, srv *config.ServerConfig, sess *auth.Session,
	assetName string, cmd string, opts ExecOptions) (transport.Result, error) {
	if p == nil {
		return transport.Result{}, ErrClosed
	}
	key := termKey{
		Server:   serverAlias(srv),
		Asset:    assetName,
		Account:  opts.Account,
		Protocol: opts.Protocol,
	}
	res, err := p.runOnce(ctx, key, srv, sess, assetName, cmd, opts)
	if err == nil || !replayable(ctx, res, err) {
		return res, err
	}

	// The failed attempt evicted its own terminal, so this second call misses
	// the pool and dials a fresh connection: the rebuild is the miss, not a
	// separate code path.
	//
	// Both failures are wrapped, so errors.As still reaches the underlying
	// *api.AuthError or net error whichever attempt produced it.
	retried, retryErr := p.runOnce(ctx, key, srv, sess, assetName, cmd, opts)
	if retryErr != nil {
		return transport.Result{}, fmt.Errorf("rebuilt terminal failed after %w: %w", err, retryErr)
	}
	return retried, nil
}

// acquireAttempts bounds the retry that a terminal reaped between the map
// lookup and the lock costs. A terminal that keeps being reaped means the
// configured TTL is shorter than a command, and looping forever would hide
// that.
const acquireAttempts = 3

// runOnce acquires the terminal for key and runs cmd on it exactly once.
//
// A failed execution evicts and closes the terminal before returning: a
// backend that errored leaves its stream in an unknown state, so the next
// call must get a fresh connection rather than inherit the desynchronised
// one. The eviction happens while the terminal lock is still held, so no
// waiting caller can slip in between the failure and the eviction and execute
// on a terminal that is on its way out.
func (p *TerminalPool) runOnce(ctx context.Context, key termKey, srv *config.ServerConfig, sess *auth.Session,
	assetName, cmd string, opts ExecOptions) (transport.Result, error) {
	for attempt := 0; attempt < acquireAttempts; attempt++ {
		t, reused, err := p.acquire(ctx, key, srv, sess, assetName, opts)
		if err != nil {
			return transport.Result{}, err
		}

		t.mu.Lock()
		if !p.isCurrent(key, t) {
			// The terminal was reaped or evicted while this call waited for
			// its lock. It is closed (or about to be), so the command must
			// not run on it; take the pool's current terminal instead.
			t.mu.Unlock()
			continue
		}
		if reused {
			// Counted here rather than in acquire so a terminal that was
			// reaped while this call waited for its lock is not reported as a
			// saving that never happened.
			p.mu.Lock()
			p.hits++
			p.mu.Unlock()
		}
		if opts.OnBackend != nil {
			opts.OnBackend(t.term.Backend())
		}
		res, err := t.term.Execute(ctx, cmd, opts.Timeout)
		t.lastUse = time.Now()
		evicted := err != nil && p.detach(key, t)
		t.mu.Unlock()

		if evicted {
			// Closing outside the lock is safe: t is out of the pool, and
			// every other caller re-checks the map under t.mu before
			// executing, so nothing can reach it again.
			_ = t.term.Close()
		}
		return res, err
	}
	return transport.Result{}, errors.New("connpool: terminal was reaped before the command could start")
}

// acquireStage names the step of building a terminal that failed, because the
// retry policy differs between them: a connection that refused is worth
// rebuilding, while an asset that does not resolve is not.
type acquireStage int

const (
	stageResolve acquireStage = iota
	stageConnect
)

// stageError tags a failure with the step that produced it. It stays
// unexported and unwraps to the original error, so callers still see
// *api.AuthError and friends through errors.As.
type stageError struct {
	stage acquireStage
	err   error
}

func (e *stageError) Error() string { return e.err.Error() }
func (e *stageError) Unwrap() error { return e.err }

// replayable reports whether a failed execution may be sent again on a
// freshly built terminal.
//
// This is a safety rule, not an optimisation: only a failure that happened
// before any output arrived is retried, and a cancelled or timed-out call is
// never retried because the command may well have started on the remote host.
// The signal available to the pool is the absence of output — a backend that
// fails after the command ran but before it produced any is indistinguishable
// here, which is why the transport must report an error only when it is sure.
//
// A failure to resolve the asset is not replayable either: the same lookup
// would fail the same way, and a second attempt would only bury the real error
// under a rebuild message.
func replayable(ctx context.Context, res transport.Result, err error) bool {
	if err == nil || res.Output != "" || ctx.Err() != nil || errors.Is(err, ErrClosed) {
		return false
	}
	var staged *stageError
	if errors.As(err, &staged) {
		return staged.stage == stageConnect
	}
	return !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded)
}

// acquire returns the pooled terminal for key, building it when absent, and
// reports whether the caller is reusing a terminal that already existed.
func (p *TerminalPool) acquire(ctx context.Context, key termKey, srv *config.ServerConfig, sess *auth.Session,
	assetName string, opts ExecOptions) (*pooledTerm, bool, error) {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil, false, ErrClosed
	}
	if t, ok := p.terminals[key]; ok {
		p.mu.Unlock()
		return t, true, nil
	}
	if call, ok := p.creating[key]; ok {
		p.mu.Unlock()
		select {
		case <-call.done:
		case <-ctx.Done():
			return nil, false, ctx.Err()
		}
		if call.err != nil {
			return nil, false, call.err
		}
		return call.term, true, nil
	}
	if p.creating == nil {
		p.creating = map[termKey]*createCall{}
	}
	call := &createCall{done: make(chan struct{})}
	p.creating[key] = call
	p.mu.Unlock()

	t, err := p.dial(ctx, srv, sess, assetName, opts)

	p.mu.Lock()
	delete(p.creating, key)
	if err == nil && p.closed {
		err = ErrClosed
	}
	if err == nil {
		if p.terminals == nil {
			p.terminals = map[termKey]*pooledTerm{}
		}
		p.terminals[key] = t
		p.cold++
	}
	p.mu.Unlock()

	if err != nil {
		if t != nil {
			_ = t.term.Close()
		}
		call.err = err
		close(call.done)
		return nil, false, err
	}
	call.term = t
	close(call.done)
	return t, false, nil
}

// dial resolves the asset and opens a terminal for it.
//
// The connection token is created inside Connect, not here: the token is
// backend-specific (SFTP needs protocol=sftp with connect_method=web_sftp) and
// the auto backend needs a fresh token per attempt, so the transport owns it
// (DESIGN.md §3.1, §3.3).
func (p *TerminalPool) dial(ctx context.Context, srv *config.ServerConfig, sess *auth.Session,
	assetName string, opts ExecOptions) (*pooledTerm, error) {
	resolve := p.Resolve
	if resolve == nil {
		resolve = resolveViaAPI
	}
	connect := p.Connect
	if connect == nil {
		connect = transport.Connect
	}

	info, err := resolve(ctx, sess, assetName, opts.Account, opts.Protocol)
	if err != nil {
		return nil, &stageError{stage: stageResolve, err: err}
	}
	protocol := opts.Protocol
	if protocol == "" {
		protocol = info.Protocol
	}
	term, err := connect(ctx, sess, info, transport.ConnectOptions{
		Backend:  opts.Backend,
		SSHPort:  serverPort(srv),
		Protocol: protocol,
	})
	if err != nil {
		return nil, &stageError{stage: stageConnect, err: err}
	}
	return &pooledTerm{term: term, lastUse: time.Now()}, nil
}

// resolveViaAPI is the default Resolve: the asset API of the session's
// endpoint.
func resolveViaAPI(ctx context.Context, sess *auth.Session, name, account, protocol string) (assets.Info, error) {
	if sess == nil || sess.Client == nil {
		return assets.Info{}, errors.New("connpool: cannot resolve without a session")
	}
	return assets.Resolve(ctx, sess.Client, name, account, protocol)
}

// current returns the pool's terminal for key, or nil.
func (p *TerminalPool) current(key termKey) *pooledTerm {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.terminals[key]
}

// isCurrent reports whether t is still the pooled terminal for key.
//
// Callers hold t.mu, so the pool lock is always taken second. That order is
// the only one used in this file — acquire takes the pool lock alone — and
// reversing it here would deadlock against the reaper.
func (p *TerminalPool) isCurrent(key termKey, t *pooledTerm) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.terminals[key] == t
}

// detach removes t from the pool when it is still the terminal for key,
// reporting whether it removed anything. The caller holds t.mu, so no command
// can be running on t.
func (p *TerminalPool) detach(key termKey, t *pooledTerm) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.terminals[key] != t {
		return false
	}
	delete(p.terminals, key)
	return true
}

// evictTerm removes t from the pool and closes it, but only while t is still
// the terminal for key: another goroutine may have rebuilt it in the meantime,
// and closing that one would break a healthy terminal.
//
// The close waits for the terminal lock, so a command in flight always
// finishes first and a backend never sees Close race with Execute. The wait is
// bounded by that command, and no new command can start on t once it is
// detached.
func (p *TerminalPool) evictTerm(key termKey, t *pooledTerm) {
	if p == nil || t == nil {
		return
	}
	if !p.detach(key, t) {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	_ = t.term.Close()
}

// Evict closes and removes one terminal.
//
// It is idempotent and safe for an unknown key; both are what the retry-once
// policy needs, since the caller may evict a terminal another call already
// replaced.
func (p *TerminalPool) Evict(srv, assetName, account, protocol string) {
	if p == nil {
		return
	}
	key := termKey{Server: srv, Asset: assetName, Account: account, Protocol: protocol}
	p.evictTerm(key, p.current(key))
}

// Reap closes terminals idle for longer than ttl and reports how many.
//
// now is injected so the caller (a ticker, or a test) owns the clock. A
// terminal that is running a command is skipped rather than waited for: the
// reaper must not stall behind a long command, and the command's own
// completion refreshes lastUse before it releases the lock, so a busy terminal
// is re-examined on the next pass.
func (p *TerminalPool) Reap(now time.Time, ttl time.Duration) int {
	if p == nil {
		return 0
	}

	p.mu.Lock()
	keys := make([]termKey, 0, len(p.terminals))
	for key := range p.terminals {
		keys = append(keys, key)
	}
	p.mu.Unlock()

	closed := 0
	for _, key := range keys {
		t := p.current(key)
		if t == nil || !t.mu.TryLock() {
			continue
		}
		// The idle test is repeated under the terminal lock: a command that
		// finished while this loop walked the keys has refreshed lastUse.
		if now.Sub(t.lastUse) > ttl && p.detach(key, t) {
			closed++
			t.mu.Unlock()
			_ = t.term.Close()
			continue
		}
		t.mu.Unlock()
	}
	return closed
}

// Close closes every terminal.
//
// It is idempotent and safe on a nil pool. Each terminal is closed under its
// own lock, so a command in flight finishes before its connection goes away.
func (p *TerminalPool) Close() error {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	terms := make([]*pooledTerm, 0, len(p.terminals))
	for _, t := range p.terminals {
		terms = append(terms, t)
	}
	p.terminals = nil
	p.mu.Unlock()

	for _, t := range terms {
		t.mu.Lock()
		_ = t.term.Close()
		t.mu.Unlock()
	}
	return nil
}

// Stats reports the pool's current shape, for `jms_pool_status`.
type Stats struct {
	Terminals int
	Hits      int
	Cold      int
}

// Stats returns the current counters.
//
// Hits and Cold are cumulative for the pool's lifetime: a reaped terminal must
// not make the savings disappear from the report.
func (p *TerminalPool) Stats() Stats {
	if p == nil {
		return Stats{}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return Stats{Terminals: len(p.terminals), Hits: p.hits, Cold: p.cold}
}

// serverAlias is the pool key for a server config; nil keys as "".
func serverAlias(srv *config.ServerConfig) string {
	if srv == nil {
		return ""
	}
	return srv.Name
}

// serverPort is the KoKo SSH port for a server, defaulted when unset.
func serverPort(srv *config.ServerConfig) int {
	if srv == nil {
		return transport.DefaultSSHPort
	}
	return srv.Port()
}

// AssetCache caches asset resolutions for ResolveTTL.
type AssetCache struct {
	mu      sync.Mutex
	entries map[string]assetEntry
}

type assetEntry struct {
	info assets.Info
	at   time.Time
}

// NewAssetCache returns an empty cache.
func NewAssetCache() *AssetCache { return &AssetCache{entries: map[string]assetEntry{}} }

// Get returns a cached resolution when it is still fresh.
//
// Freshness is measured against the injected now, and an entry expires at
// exactly ResolveTTL: answering a lookup the caller asked to re-check would
// hide a renamed asset (DESIGN.md §4.4).
func (c *AssetCache) Get(key string, now time.Time) (assets.Info, bool) {
	if c == nil {
		return assets.Info{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[key]
	if !ok || now.Sub(entry.at) >= ResolveTTL {
		return assets.Info{}, false
	}
	return entry.info, true
}

// Put stores a resolution.
func (c *AssetCache) Put(key string, info assets.Info, now time.Time) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = map[string]assetEntry{}
	}
	c.entries[key] = assetEntry{info: info, at: now}
}

// Invalidate drops one entry.
func (c *AssetCache) Invalidate(key string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, key)
}
