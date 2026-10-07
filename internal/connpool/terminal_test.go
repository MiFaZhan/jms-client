package connpool

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MiFaZhan/jms-client/internal/assets"
	"github.com/MiFaZhan/jms-client/internal/config"
	"github.com/MiFaZhan/jms-client/internal/transport"
)

// TestTerminalPoolExecReusesTerminal is the pool's headline saving: the
// second command on one asset must run on the connection the first one
// opened, not on a new one.
func TestTerminalPoolExecReusesTerminal(t *testing.T) {
	b := &fakeBackend{}
	p := b.pool()
	srv, sess := testServer(), testSession(testServer())

	for _, cmd := range []string{"uname -r", "uptime"} {
		if _, err := p.Exec(context.Background(), srv, sess, "web-01", cmd, ExecOptions{}); err != nil {
			t.Fatalf("Exec(%q) = %v, want nil", cmd, err)
		}
	}

	if got := b.connectCount(); got != 1 {
		t.Fatalf("connects = %d, want 1", got)
	}
	if got := b.resolveCount(); got != 1 {
		t.Fatalf("resolves = %d, want 1 (a pooled terminal is not re-resolved)", got)
	}
	term := b.terminal(0)
	if got := term.callCount(); got != 2 {
		t.Fatalf("commands on the pooled terminal = %d, want 2", got)
	}
	stats := p.Stats()
	if stats.Terminals != 1 || stats.Cold != 1 || stats.Hits != 1 {
		t.Fatalf("Stats() = %+v, want {Terminals:1 Hits:1 Cold:1}", stats)
	}
}

// TestTerminalPoolDifferentAssetsUseDifferentTerminals pins the key: two
// assets never share a PTY.
func TestTerminalPoolDifferentAssetsUseDifferentTerminals(t *testing.T) {
	b := &fakeBackend{}
	p := b.pool()
	srv, sess := testServer(), testSession(testServer())

	for _, asset := range []string{"web-01", "db-01"} {
		if _, err := p.Exec(context.Background(), srv, sess, asset, "hostname", ExecOptions{}); err != nil {
			t.Fatalf("Exec(%q) = %v, want nil", asset, err)
		}
	}

	if got := b.connectCount(); got != 2 {
		t.Fatalf("connects = %d, want 2", got)
	}
	if b.terminal(0) == b.terminal(1) {
		t.Fatal("both assets were served by the same terminal")
	}
	if stats := p.Stats(); stats.Terminals != 2 || stats.Cold != 2 || stats.Hits != 0 {
		t.Fatalf("Stats() = %+v, want {Terminals:2 Hits:0 Cold:2}", stats)
	}
}

// TestTerminalPoolKeyIncludesAccountAndProtocol guards the rest of the key:
// the same asset reached as a different account or protocol is a different
// connection, because JumpServer binds both into the connection token.
func TestTerminalPoolKeyIncludesAccountAndProtocol(t *testing.T) {
	b := &fakeBackend{}
	p := b.pool()
	srv, sess := testServer(), testSession(testServer())

	opts := []ExecOptions{{Account: "@USER"}, {Account: "root"}, {Account: "@USER", Protocol: "sftp"}}
	for _, o := range opts {
		if _, err := p.Exec(context.Background(), srv, sess, "web-01", "id", o); err != nil {
			t.Fatalf("Exec(%+v) = %v, want nil", o, err)
		}
	}

	if got := b.connectCount(); got != 3 {
		t.Fatalf("connects = %d, want 3 (one per account/protocol key)", got)
	}
}

// TestTerminalPoolExecSerializesSameAsset proves the per-terminal lock with
// observable overlap: the second command cannot enter Execute while the first
// is inside it.
//
// Each command signals its own entry channel, so the assertion does not depend
// on goroutine scheduling: the second call is launched only after the first is
// provably inside the fake.
func TestTerminalPoolExecSerializesSameAsset(t *testing.T) {
	b := &fakeBackend{}
	var overlap overlapCounter
	entered := map[string]chan struct{}{"first": make(chan struct{}), "second": make(chan struct{})}
	release := make(chan struct{})

	term := &fakeTerminal{}
	term.execute = func(_ context.Context, cmd string, _ time.Duration) (transport.Result, error) {
		overlap.enter()
		defer overlap.exit()
		close(entered[cmd])
		<-release
		return transport.Result{Output: cmd}, nil
	}
	b.connectFn = func(string, int) (transport.Terminal, error) { return b.record(term), nil }

	p := b.pool()
	srv, sess := testServer(), testSession(testServer())

	var wg sync.WaitGroup
	firstErr := make(chan error, 1)
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, err := p.Exec(context.Background(), srv, sess, "web-01", "first", ExecOptions{})
		firstErr <- err
	}()

	select {
	case <-entered["first"]:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the first command to enter the fake")
	}

	secondErr := make(chan error, 1)
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, err := p.Exec(context.Background(), srv, sess, "web-01", "second", ExecOptions{})
		secondErr <- err
	}()

	// The first command is still inside the fake, so the second must be
	// blocked on the terminal lock and never reach Execute.
	select {
	case <-entered["second"]:
		t.Fatal("the second command entered Execute while the first was still running")
	case <-time.After(100 * time.Millisecond):
	}

	close(release)
	wg.Wait()

	if err := <-firstErr; err != nil {
		t.Fatalf("first Exec() = %v, want nil", err)
	}
	if err := <-secondErr; err != nil {
		t.Fatalf("second Exec() = %v, want nil", err)
	}
	if got := overlap.peak(); got != 1 {
		t.Fatalf("peak concurrent executions on one terminal = %d, want 1", got)
	}
	if got := term.callCount(); got != 2 {
		t.Fatalf("commands = %d, want 2", got)
	}
}

// TestTerminalPoolExecDoesNotSerializeAcrossAssets is the other half of the
// lock's scope: different assets must really run in parallel, otherwise the
// pool would serialize unrelated work.
func TestTerminalPoolExecDoesNotSerializeAcrossAssets(t *testing.T) {
	b := &fakeBackend{}
	var overlap overlapCounter
	entered := make(chan string, 2)
	release := make(chan struct{})

	b.connectFn = func(name string, _ int) (transport.Terminal, error) {
		term := &fakeTerminal{}
		term.execute = func(_ context.Context, cmd string, _ time.Duration) (transport.Result, error) {
			overlap.enter()
			defer overlap.exit()
			entered <- name
			<-release
			return transport.Result{Output: cmd}, nil
		}
		return b.record(term), nil
	}

	p := b.pool()
	srv, sess := testServer(), testSession(testServer())

	var wg sync.WaitGroup
	for _, asset := range []string{"web-01", "db-01"} {
		wg.Add(1)
		go func(asset string) {
			defer wg.Done()
			_, _ = p.Exec(context.Background(), srv, sess, asset, "sleep 1", ExecOptions{})
		}(asset)
	}

	// Both executions must be inside the fake at the same time. A pool that
	// serialized across assets would never deliver the second one and this
	// wait would fail.
	seen := map[string]bool{}
	for i := 0; i < 2; i++ {
		seen[waitForCommand(t, entered)] = true
	}
	if !seen["web-01"] || !seen["db-01"] {
		t.Fatalf("assets that ran = %v, want both", seen)
	}
	if got := overlap.peak(); got != 2 {
		t.Fatalf("peak concurrent executions = %d, want 2", got)
	}

	close(release)
	wg.Wait()
}

// TestTerminalPoolConcurrentColdStartConnectsOnce covers the terminal-side
// single flight: two calls that miss the same key together must not open two
// connections.
func TestTerminalPoolConcurrentColdStartConnectsOnce(t *testing.T) {
	b := &fakeBackend{}
	entered := make(chan struct{})
	release := make(chan struct{})
	b.connectFn = func(name string, _ int) (transport.Terminal, error) {
		entered <- struct{}{}
		<-release
		return b.record(&fakeTerminal{}), nil
	}

	p := b.pool()
	srv, sess := testServer(), testSession(testServer())

	const n = 4
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = p.Exec(context.Background(), srv, sess, "web-01", "hostname", ExecOptions{})
		}(i)
	}

	<-entered
	close(release)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("Exec() #%d = %v, want nil", i, err)
		}
	}
	if got := b.connectCount(); got != 1 {
		t.Fatalf("connects = %d, want 1", got)
	}
	if got := b.terminal(0).callCount(); got != n {
		t.Fatalf("commands on the shared terminal = %d, want %d", got, n)
	}
	if stats := p.Stats(); stats.Cold != 1 || stats.Hits != n-1 {
		t.Fatalf("Stats() = %+v, want Cold:1 Hits:%d", stats, n-1)
	}
}

// TestTerminalPoolRebuildsAfterSilentFailure is the transparent-rebuild
// case: a connection that broke before the command produced anything is
// replaced, and the caller sees a success.
func TestTerminalPoolRebuildsAfterSilentFailure(t *testing.T) {
	b := &fakeBackend{}
	b.connectFn = func(name string, attempt int) (transport.Terminal, error) {
		term := &fakeTerminal{}
		if attempt == 0 {
			term.execute = func(context.Context, string, time.Duration) (transport.Result, error) {
				return transport.Result{}, errors.New("ssh: connection lost before the command started")
			}
		} else {
			term.execute = func(context.Context, string, time.Duration) (transport.Result, error) {
				return transport.Result{Output: "rebuilt\n"}, nil
			}
		}
		return b.record(term), nil
	}

	p := b.pool()
	srv, sess := testServer(), testSession(testServer())

	res, err := p.Exec(context.Background(), srv, sess, "web-01", "uptime", ExecOptions{})
	if err != nil {
		t.Fatalf("Exec() = %v, want nil after the transparent rebuild", err)
	}
	if res.Output != "rebuilt\n" {
		t.Fatalf("Output = %q, want the rebuilt terminal's output", res.Output)
	}
	if got := b.connectCount(); got != 2 {
		t.Fatalf("connects = %d, want 2 (one broken, one rebuilt)", got)
	}
	broken := b.terminal(0)
	if got := broken.callCount(); got != 1 {
		t.Fatalf("commands on the broken terminal = %d, want 1 (no replay on it)", got)
	}
	if got := broken.closeCount(); got != 1 {
		t.Fatalf("close calls on the broken terminal = %d, want 1 (it must be evicted)", got)
	}
	if got := b.terminal(1).callCount(); got != 1 {
		t.Fatalf("commands on the rebuilt terminal = %d, want 1", got)
	}
	stats := p.Stats()
	if stats.Terminals != 1 || stats.Cold != 2 {
		t.Fatalf("Stats() = %+v, want {Terminals:1 Cold:2}", stats)
	}
}

// TestTerminalPoolRebuildsAfterConnectFailure covers the same policy for a
// failure that never produced a terminal at all.
func TestTerminalPoolRebuildsAfterConnectFailure(t *testing.T) {
	b := &fakeBackend{}
	b.connectFn = func(name string, attempt int) (transport.Terminal, error) {
		if attempt == 0 {
			return nil, errors.New("ssh: dial tcp 10.0.0.9:2222: connection refused")
		}
		return b.record(&fakeTerminal{}), nil
	}

	p := b.pool()
	srv, sess := testServer(), testSession(testServer())

	if _, err := p.Exec(context.Background(), srv, sess, "web-01", "uptime", ExecOptions{}); err != nil {
		t.Fatalf("Exec() = %v, want nil after the rebuild", err)
	}
	if got := b.connectCount(); got != 2 {
		t.Fatalf("connects = %d, want 2", got)
	}
}

// TestTerminalPoolNeverReplaysACommandThatProducedOutput is the non-idempotency
// safety line: once output has been observed the remote host has run the
// command, so the pool must not send it again — `rm`/`reboot` are not
// idempotent (DESIGN.md §4.3 point 5, §4.7 point 3).
func TestTerminalPoolNeverReplaysACommandThatProducedOutput(t *testing.T) {
	b := &fakeBackend{}
	term := &fakeTerminal{}
	term.execute = func(context.Context, string, time.Duration) (transport.Result, error) {
		return transport.Result{Output: "rm: removing /srv\n"}, errors.New("ssh: connection lost mid-command")
	}
	b.connectFn = func(string, int) (transport.Terminal, error) { return b.record(term), nil }

	p := b.pool()
	srv, sess := testServer(), testSession(testServer())

	res, err := p.Exec(context.Background(), srv, sess, "web-01", "rm -rf /srv", ExecOptions{})
	if err == nil {
		t.Fatal("Exec() = nil error, want the mid-command failure to surface")
	}
	if got := term.callCount(); got != 1 {
		t.Fatalf("commands executed = %d, want exactly 1 (no replay after output)", got)
	}
	if got := b.connectCount(); got != 1 {
		t.Fatalf("connects = %d, want 1 (no rebuild for a command that already ran)", got)
	}
	if res.Output != "rm: removing /srv\n" {
		t.Fatalf("Output = %q, want the partial output that was already received", res.Output)
	}
	if got := term.closeCount(); got != 1 {
		t.Fatalf("close calls = %d, want 1 (the desynchronised terminal must be evicted)", got)
	}
}

// TestTerminalPoolDoesNotReplayAfterTimeout keeps the same line for the
// timeout class: a command that ran out of time may still be running on the
// host, so it is never sent twice.
func TestTerminalPoolDoesNotReplayAfterTimeout(t *testing.T) {
	b := &fakeBackend{}
	term := &fakeTerminal{}
	term.execute = func(context.Context, string, time.Duration) (transport.Result, error) {
		return transport.Result{}, fmt.Errorf("command timed out: %w", context.DeadlineExceeded)
	}
	b.connectFn = func(string, int) (transport.Terminal, error) { return b.record(term), nil }

	p := b.pool()
	srv, sess := testServer(), testSession(testServer())

	if _, err := p.Exec(context.Background(), srv, sess, "web-01", "reboot", ExecOptions{}); err == nil {
		t.Fatal("Exec() = nil error, want the timeout to surface")
	}
	if got := term.callCount(); got != 1 {
		t.Fatalf("commands executed = %d, want exactly 1", got)
	}
	if got := b.connectCount(); got != 1 {
		t.Fatalf("connects = %d, want 1", got)
	}
}

// TestTerminalPoolRebuildFailureSurfacesBothErrors keeps a failed rebuild
// from hiding the original cause.
func TestTerminalPoolRebuildFailureSurfacesBothErrors(t *testing.T) {
	b := &fakeBackend{}
	b.connectFn = func(string, int) (transport.Terminal, error) {
		return nil, errors.New("ssh: dial tcp: connection refused")
	}

	p := b.pool()
	srv, sess := testServer(), testSession(testServer())

	_, err := p.Exec(context.Background(), srv, sess, "web-01", "uptime", ExecOptions{})
	if err == nil {
		t.Fatal("Exec() = nil error, want a failure")
	}
	if got := b.connectCount(); got != 2 {
		t.Fatalf("connects = %d, want 2 (original + one rebuild)", got)
	}
	if !strings.Contains(err.Error(), "connection refused") {
		t.Fatalf("error %q does not mention the underlying cause", err)
	}
}

// TestTerminalPoolResolveFailureIsNotRetried keeps a resolve error from
// being masked as a rebuildable connection error.
func TestTerminalPoolResolveFailureIsNotRetried(t *testing.T) {
	b := &fakeBackend{}
	b.resolveFn = func(name, _, _ string) (assets.Info, error) {
		return assets.Info{}, errors.New("asset not found: " + name)
	}

	p := b.pool()
	srv, sess := testServer(), testSession(testServer())

	if _, err := p.Exec(context.Background(), srv, sess, "nope", "uptime", ExecOptions{}); err == nil {
		t.Fatal("Exec() = nil error, want the resolve failure")
	}
	if got := b.connectCount(); got != 0 {
		t.Fatalf("connects = %d, want 0", got)
	}
}

// TestTerminalPoolReapClosesIdleTerminalAndKeepsFreshOne drives the idle
// clock by stamping last-use directly and passing an explicit now, so nothing
// sleeps and the boundary is exactly the TTL.
func TestTerminalPoolReapClosesIdleTerminalAndKeepsFreshOne(t *testing.T) {
	b := &fakeBackend{}
	p := b.pool()
	srv, sess := testServer(), testSession(testServer())

	for _, asset := range []string{"idle-01", "fresh-01"} {
		if _, err := p.Exec(context.Background(), srv, sess, asset, "hostname", ExecOptions{}); err != nil {
			t.Fatalf("Exec(%q) = %v, want nil", asset, err)
		}
	}

	now := time.Now()
	idleKey := termKey{Server: srv.Name, Asset: "idle-01"}
	freshKey := termKey{Server: srv.Name, Asset: "fresh-01"}
	stamp(p, idleKey, now.Add(-2*IdleTTL))
	stamp(p, freshKey, now.Add(-time.Minute))

	if got := p.Reap(now, IdleTTL); got != 1 {
		t.Fatalf("Reap() = %d, want 1", got)
	}
	if got := b.terminal(0).closeCount(); got != 1 {
		t.Fatalf("idle terminal close calls = %d, want 1", got)
	}
	if got := b.terminal(1).closeCount(); got != 0 {
		t.Fatalf("fresh terminal close calls = %d, want 0", got)
	}
	if pooled(p, idleKey) {
		t.Fatal("the idle key is still in the pool")
	}
	if !pooled(p, freshKey) {
		t.Fatal("the fresh key was evicted")
	}
	if stats := p.Stats(); stats.Terminals != 1 {
		t.Fatalf("Stats().Terminals = %d, want 1", stats.Terminals)
	}

	// The reaped key must be gone, so the next command rebuilds rather than
	// finding a closed terminal.
	if _, err := p.Exec(context.Background(), srv, sess, "idle-01", "hostname", ExecOptions{}); err != nil {
		t.Fatalf("Exec() after reap = %v, want nil", err)
	}
	if got := b.connectCount(); got != 3 {
		t.Fatalf("connects = %d, want 3 (the reaped terminal was rebuilt)", got)
	}
}

// TestTerminalPoolReapBoundaryIsExclusive pins the ttl comparison: a terminal
// exactly at the ttl is still fresh, one tick past it is not.
func TestTerminalPoolReapBoundaryIsExclusive(t *testing.T) {
	b := &fakeBackend{}
	p := b.pool()
	srv, sess := testServer(), testSession(testServer())

	if _, err := p.Exec(context.Background(), srv, sess, "web-01", "hostname", ExecOptions{}); err != nil {
		t.Fatalf("Exec() = %v, want nil", err)
	}
	base := time.Now()
	stamp(p, termKey{Server: srv.Name, Asset: "web-01"}, base)

	if got := p.Reap(base.Add(IdleTTL), IdleTTL); got != 0 {
		t.Fatalf("Reap() at exactly ttl = %d, want 0", got)
	}
	if got := p.Reap(base.Add(IdleTTL+time.Nanosecond), IdleTTL); got != 1 {
		t.Fatalf("Reap() one tick past ttl = %d, want 1", got)
	}
}

// TestTerminalPoolReapCountsEveryClosedTerminal covers the return value with
// more than one victim.
func TestTerminalPoolReapCountsEveryClosedTerminal(t *testing.T) {
	b := &fakeBackend{}
	p := b.pool()
	srv, sess := testServer(), testSession(testServer())

	assets := []string{"a-01", "b-01", "c-01"}
	for _, asset := range assets {
		if _, err := p.Exec(context.Background(), srv, sess, asset, "hostname", ExecOptions{}); err != nil {
			t.Fatalf("Exec(%q) = %v, want nil", asset, err)
		}
	}

	base := time.Now().Add(-IdleTTL - time.Minute)
	for _, asset := range assets {
		stamp(p, termKey{Server: srv.Name, Asset: asset}, base)
	}

	if got := p.Reap(time.Now(), IdleTTL); got != 3 {
		t.Fatalf("Reap() = %d, want 3", got)
	}
	if got := p.Reap(time.Now(), IdleTTL); got != 0 {
		t.Fatalf("second Reap() = %d, want 0 (nothing left)", got)
	}
	if stats := p.Stats(); stats.Terminals != 0 {
		t.Fatalf("Stats().Terminals = %d, want 0", stats.Terminals)
	}
	for i := 0; i < 3; i++ {
		if got := b.terminal(i).closeCount(); got != 1 {
			t.Fatalf("terminal %d close calls = %d, want 1", i, got)
		}
	}
}

// TestTerminalPoolReapDoesNotCloseBusyTerminal proves the reaper cannot pull a
// connection out from under a running command, and that the command's own
// completion refreshes the idle clock.
func TestTerminalPoolReapDoesNotCloseBusyTerminal(t *testing.T) {
	b := &fakeBackend{}
	started := make(chan struct{})
	release := make(chan struct{})
	term := &fakeTerminal{}
	term.execute = func(context.Context, string, time.Duration) (transport.Result, error) {
		close(started)
		<-release
		return transport.Result{Output: "done"}, nil
	}
	b.connectFn = func(string, int) (transport.Terminal, error) { return b.record(term), nil }

	p := b.pool()
	srv, sess := testServer(), testSession(testServer())

	done := make(chan error, 1)
	go func() {
		_, err := p.Exec(context.Background(), srv, sess, "web-01", "sleep 600", ExecOptions{})
		done <- err
	}()
	<-started

	// The terminal is busy, so a reaper running with any clock must skip it
	// rather than block or close it.
	if got := p.Reap(time.Now().Add(2*IdleTTL), IdleTTL); got != 0 {
		t.Fatalf("Reap() during a running command = %d, want 0", got)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("Exec() = %v, want nil", err)
	}
	if got := term.closeCount(); got != 0 {
		t.Fatalf("close calls = %d, want 0", got)
	}
	// The command refreshed lastUse, so the terminal is fresh again.
	if got := p.Reap(time.Now(), IdleTTL); got != 0 {
		t.Fatalf("Reap() right after the command = %d, want 0", got)
	}
}

// TestTerminalPoolEvictClosesAndIsIdempotent covers both the happy path and
// the "already gone" path the retry-once policy produces.
func TestTerminalPoolEvictClosesAndIsIdempotent(t *testing.T) {
	b := &fakeBackend{}
	p := b.pool()
	srv, sess := testServer(), testSession(testServer())

	if _, err := p.Exec(context.Background(), srv, sess, "web-01", "hostname", ExecOptions{}); err != nil {
		t.Fatalf("Exec() = %v, want nil", err)
	}
	term := b.terminal(0)

	p.Evict(srv.Name, "web-01", "", "")
	if got := term.closeCount(); got != 1 {
		t.Fatalf("close calls after Evict = %d, want 1", got)
	}
	if stats := p.Stats(); stats.Terminals != 0 {
		t.Fatalf("Stats().Terminals = %d, want 0", stats.Terminals)
	}

	// Evicting again, and evicting a key that was never pooled, must both be
	// no-ops.
	p.Evict(srv.Name, "web-01", "", "")
	p.Evict(srv.Name, "never-seen", "", "")
	if got := term.closeCount(); got != 1 {
		t.Fatalf("close calls after redundant Evicts = %d, want 1", got)
	}
}

// TestTerminalPoolCloseIsIdempotent closes the pool twice, which is what a
// deferred Close plus an explicit one looks like.
func TestTerminalPoolCloseIsIdempotent(t *testing.T) {
	b := &fakeBackend{}
	p := b.pool()
	srv, sess := testServer(), testSession(testServer())

	if _, err := p.Exec(context.Background(), srv, sess, "web-01", "hostname", ExecOptions{}); err != nil {
		t.Fatalf("Exec() = %v, want nil", err)
	}
	term := b.terminal(0)

	if err := p.Close(); err != nil {
		t.Fatalf("first Close() = %v, want nil", err)
	}
	if err := p.Close(); err != nil {
		t.Fatalf("second Close() = %v, want nil", err)
	}
	if got := term.closeCount(); got != 1 {
		t.Fatalf("close calls = %d, want 1", got)
	}
	if stats := p.Stats(); stats.Terminals != 0 {
		t.Fatalf("Stats().Terminals = %d, want 0", stats.Terminals)
	}
}

// TestTerminalPoolExecAfterCloseReturnsErrClosed keeps a closed pool from
// opening new connections.
func TestTerminalPoolExecAfterCloseReturnsErrClosed(t *testing.T) {
	b := &fakeBackend{}
	p := b.pool()
	srv, sess := testServer(), testSession(testServer())

	if err := p.Close(); err != nil {
		t.Fatalf("Close() = %v, want nil", err)
	}
	if _, err := p.Exec(context.Background(), srv, sess, "web-01", "hostname", ExecOptions{}); !errors.Is(err, ErrClosed) {
		t.Fatalf("Exec() after Close = %v, want ErrClosed", err)
	}
	if got := b.connectCount(); got != 0 {
		t.Fatalf("connects = %d, want 0", got)
	}
}

// TestTerminalPoolNilReceiverIsSafe covers the nil-receiver behaviour the
// frozen signatures allow.
func TestTerminalPoolNilReceiverIsSafe(t *testing.T) {
	var p *TerminalPool
	srv, sess := testServer(), testSession(testServer())

	if _, err := p.Exec(context.Background(), srv, sess, "web-01", "hostname", ExecOptions{}); !errors.Is(err, ErrClosed) {
		t.Fatalf("nil pool Exec() = %v, want ErrClosed", err)
	}
	p.Evict("bastion", "web-01", "", "")
	if got := p.Reap(time.Now(), IdleTTL); got != 0 {
		t.Fatalf("nil pool Reap() = %d, want 0", got)
	}
	if err := p.Close(); err != nil {
		t.Fatalf("nil pool Close() = %v, want nil", err)
	}
	if got := p.Stats(); got != (Stats{}) {
		t.Fatalf("nil pool Stats() = %+v, want the zero value", got)
	}
}

// TestTerminalPoolPassesResolvedAssetToConnect asserts the resolve -> connect
// hand-off: the connection is opened for the resolved asset, with the
// account and protocol the caller asked for, and the KoKo port comes from
// the server entry.
func TestTerminalPoolPassesResolvedAssetToConnect(t *testing.T) {
	b := &fakeBackend{}
	p := b.pool()
	srv := &config.ServerConfig{Name: "bastion", Username: "testuser", SSHPort: 2222}
	sess := testSession(srv)

	if _, err := p.Exec(context.Background(), srv, sess, "web-01", "hostname", ExecOptions{
		Account: "@USER",
		Backend: transport.BackendWS,
	}); err != nil {
		t.Fatalf("Exec() = %v, want nil", err)
	}

	if b.lastAsset.ID != "uuid-web-01" || b.lastAsset.Account != "@USER" {
		t.Fatalf("connect asset = %+v, want the resolved asset with account @USER", b.lastAsset)
	}
	if b.lastOpts.Backend != transport.BackendWS {
		t.Fatalf("connect backend = %q, want %q", b.lastOpts.Backend, transport.BackendWS)
	}
	if b.lastOpts.SSHPort != 2222 {
		t.Fatalf("connect SSHPort = %d, want 2222", b.lastOpts.SSHPort)
	}
	if b.lastOpts.Protocol != "ssh" {
		t.Fatalf("connect protocol = %q, want the resolved %q", b.lastOpts.Protocol, "ssh")
	}
	if b.lastSess != sess {
		t.Fatal("connect received a different session")
	}
}

// TestTerminalPoolDefaultResolveNeedsSession guards the nil-session path of
// the built-in resolver instead of dereferencing it.
func TestTerminalPoolDefaultResolveNeedsSession(t *testing.T) {
	p := NewTerminalPool()
	if _, err := p.Exec(context.Background(), testServer(), nil, "web-01", "hostname", ExecOptions{}); err == nil {
		t.Fatal("Exec() with a nil session = nil error, want an error")
	}
}
