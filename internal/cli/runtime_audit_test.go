package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MiFaZhan/jms-client/internal/assets"
	"github.com/MiFaZhan/jms-client/internal/auth"
	"github.com/MiFaZhan/jms-client/internal/config"
	"github.com/MiFaZhan/jms-client/internal/connpool"
	"github.com/MiFaZhan/jms-client/internal/obs"
	"github.com/MiFaZhan/jms-client/internal/transport"
)

// These tests exist because the observability chain was complete in every
// package and connected in none of them: the bus, the audit writer and the
// pool events each had their own tests, and no test asserted that a real
// command produced a real audit line. The result was a `jms tail` that could
// never show anything (DESIGN.md「可观测性」).

// auditEventsIn reads every JSONL line from the audit directory.
func auditEventsIn(t *testing.T, dir string) []obs.Event {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("read audit dir %s: %v", dir, err)
	}
	var events []obs.Event
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".jsonl") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", entry.Name(), err)
		}
		for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
			if strings.TrimSpace(line) == "" {
				continue
			}
			var e obs.Event
			if err := json.Unmarshal([]byte(line), &e); err != nil {
				t.Fatalf("audit line is not JSON (%s): %q", entry.Name(), line)
			}
			events = append(events, e)
		}
	}
	return events
}

// auditDirFor points one environment at its own audit directory.
func auditDirFor(t *testing.T, env *cliEnv) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "audit")
	env.auditDirOverride = dir
	return dir
}

// TestWithDefaultsWiresTheAuditBus is the regression for the missing wiring:
// the default Runtime must carry a bus and a sink, or every later command
// publishes into a nil bus and the audit log stays empty forever.
func TestWithDefaultsWiresTheAuditBus(t *testing.T) {
	env := newCLIEnv(t)
	auditDirFor(t, env)

	deps := env.deps().WithDefaults()

	if deps.Runtime.Bus == nil {
		t.Fatal("WithDefaults left Runtime.Bus nil: nothing can publish an audit event")
	}
	if deps.Runtime.Audit == nil {
		t.Fatal("WithDefaults left Runtime.Audit nil: nothing writes the audit file")
	}
	if err := deps.Runtime.AuditErr; err != nil {
		t.Fatalf("AuditErr = %v, want nil", err)
	}
}

// TestWithDefaultsWiresPoolNotifications is the other half of the same
// regression: a bus that no pool reports into still produces an empty tail.
func TestWithDefaultsWiresPoolNotifications(t *testing.T) {
	env := newCLIEnv(t)
	auditDirFor(t, env)

	deps := env.deps().WithDefaults()

	if deps.Runtime.Terminals.Notify == nil {
		t.Fatal("TerminalPool.Notify is nil: cold starts and hits would never be published")
	}
	if deps.Runtime.Sessions.Notify == nil {
		t.Fatal("SessionPool.Notify is nil: logins would never be published")
	}
}

// TestPoolEventsReachTheAuditFile proves the whole chain end to end: a pool
// transition published through the wired bus must appear as a JSONL line with
// the kind and pool marker `jms tail` renders.
func TestPoolEventsReachTheAuditFile(t *testing.T) {
	env := newCLIEnv(t)
	dir := auditDirFor(t, env)

	deps := env.deps().WithDefaults()
	deps.Runtime.Terminals.Notify(connpool.Event{
		Kind: connpool.KindCold, Server: "bastion", Asset: "web-01",
		Account: "root", Protocol: "ssh",
	})
	deps.Runtime.Terminals.Notify(connpool.Event{
		Kind: connpool.KindHit, Server: "bastion", Asset: "web-01",
	})
	deps.Runtime.Terminals.Notify(connpool.Event{
		Kind: connpool.KindReap, Server: "bastion", Asset: "web-01",
		Idle: 11 * time.Minute,
	})
	if err := deps.Runtime.Close(); err != nil {
		t.Fatalf("Runtime.Close: %v", err)
	}

	events := auditEventsIn(t, dir)
	if len(events) != 3 {
		t.Fatalf("audit events = %d, want 3: %+v", len(events), events)
	}

	want := []struct {
		kind obs.Kind
		pool string
	}{
		{obs.KindPoolCold, "cold"},
		{obs.KindPoolHit, "hit"},
		{obs.KindPoolReap, "reap"},
	}
	for i, w := range want {
		if events[i].Kind != w.kind {
			t.Errorf("event %d kind = %q, want %q", i, events[i].Kind, w.kind)
		}
		if events[i].Pool != w.pool {
			t.Errorf("event %d pool = %q, want %q", i, events[i].Pool, w.pool)
		}
		if events[i].Server != "bastion" || events[i].Asset != "web-01" {
			t.Errorf("event %d target = %s/%s, want bastion/web-01",
				i, events[i].Server, events[i].Asset)
		}
	}
	// The reaper's idle time is what tells a reader why the connection went
	// away, so it has to survive the trip.
	if got := events[2].IdleMS; got != (11 * time.Minute).Milliseconds() {
		t.Errorf("reap event idle_ms = %d, want %d", got, (11 * time.Minute).Milliseconds())
	}
}

// TestSessionEventsUseTheirOwnKinds keeps a first login distinguishable from
// a recovery: the retry-once policy re-logs in after Invalidate, and a reader
// chasing an expired token needs to see that happen.
func TestSessionEventsUseTheirOwnKinds(t *testing.T) {
	env := newCLIEnv(t)
	dir := auditDirFor(t, env)

	deps := env.deps().WithDefaults()
	deps.Runtime.Sessions.Notify(connpool.Event{Kind: connpool.KindLogin, Server: "bastion"})
	deps.Runtime.Sessions.Notify(connpool.Event{Kind: connpool.KindRelogin, Server: "bastion"})
	if err := deps.Runtime.Close(); err != nil {
		t.Fatalf("Runtime.Close: %v", err)
	}

	events := auditEventsIn(t, dir)
	if len(events) != 2 {
		t.Fatalf("audit events = %d, want 2", len(events))
	}
	if events[0].Kind != obs.KindSessionLogin {
		t.Errorf("first event kind = %q, want %q", events[0].Kind, obs.KindSessionLogin)
	}
	if events[1].Kind != obs.KindSessionRelogin {
		t.Errorf("second event kind = %q, want %q", events[1].Kind, obs.KindSessionRelogin)
	}
}

// TestOneShotCommandFlushesItsAuditEvents is the reason Runtime.Close drains
// the bus: delivery is asynchronous, so a command that publishes and returns
// would otherwise exit before its own record reached the file.
func TestOneShotCommandFlushesItsAuditEvents(t *testing.T) {
	env, fakes := newRuntimeEnv(t)
	dir := auditDirFor(t, env)

	// The seam publishes the way a real pool does: from inside the command,
	// just before it returns.
	fakes.execRes = transport.Result{Output: "ok\n"}
	fakes.afterExec = func(deps Deps) {
		deps.Runtime.Terminals.Notify(connpool.Event{
			Kind: connpool.KindHit, Server: "bastion", Asset: "web1",
		})
	}

	if code := fakes.runWithRuntime(env, "exec", "web1@bastion", "uptime"); code != 0 {
		t.Fatalf("exec exit code = %d, want 0 (stderr: %s)", code, env.stderr())
	}

	events := auditEventsIn(t, dir)
	if len(events) == 0 {
		t.Fatal("no audit event reached the file: the drain at process exit is missing")
	}
	if events[0].Kind != obs.KindPoolHit {
		t.Errorf("event kind = %q, want %q", events[0].Kind, obs.KindPoolHit)
	}
}

// TestReadOnlyCommandLeavesNoAuditFile keeps the fix from trading one problem
// for another: `jms version` runs through the same Runtime, and creating the
// directory (let alone the file) eagerly would litter the audit location for
// every command that never recorded anything.
func TestReadOnlyCommandLeavesNoAuditFile(t *testing.T) {
	env := newCLIEnv(t)
	dir := auditDirFor(t, env)

	if code := env.run("version"); code != 0 {
		t.Fatalf("version exit code = %d, want 0", code)
	}

	entries, err := os.ReadDir(dir)
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("read audit dir: %v", err)
	}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".jsonl") {
			t.Errorf("read-only command created %s: the audit file must be lazy", entry.Name())
		}
	}
}

// TestAuditDirectoryIsCreatedOnDemand pins the two-step contract the smoke
// test caught: the long-lived host prepares the directory at startup, and a
// one-shot command creates it only when there is an event to write.
func TestAuditDirectoryIsCreatedOnDemand(t *testing.T) {
	env := newCLIEnv(t)
	dir := auditDirFor(t, env)

	deps := env.deps().WithDefaults()
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("building the Runtime created %s: it must wait for Prepare or an event", dir)
	}
	if err := deps.Runtime.Audit.Prepare(); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("Prepare did not create %s: %v", dir, err)
	}
	if !info.IsDir() {
		t.Fatalf("%s is not a directory", dir)
	}
}

// TestAuditOffDisablesTheSink honours the documented switch: JMS_AUDIT=off
// must stop the file from being created at all, not merely be ignored.
func TestAuditOffDisablesTheSink(t *testing.T) {
	t.Setenv(obs.EnvAudit, "off")
	env := newCLIEnv(t)
	dir := auditDirFor(t, env)

	deps := env.deps().WithDefaults()
	if deps.Runtime.Audit != nil {
		t.Fatal("JMS_AUDIT=off still produced an audit sink")
	}
	// The bus stays usable, so IPC observation is unaffected by the switch.
	if deps.Runtime.Bus == nil {
		t.Fatal("JMS_AUDIT=off removed the bus as well")
	}
	deps.Runtime.Terminals.Notify(connpool.Event{Kind: connpool.KindHit, Server: "s", Asset: "a"})
	if err := deps.Runtime.Close(); err != nil {
		t.Fatalf("Runtime.Close with auditing off: %v", err)
	}
	if events := auditEventsIn(t, dir); len(events) != 0 {
		t.Fatalf("audit events = %d, want 0 with JMS_AUDIT=off", len(events))
	}
}

// auditTerminal is a transport.Terminal that records its close.
type auditTerminal struct {
	mu     sync.Mutex
	closes int
}

func (a *auditTerminal) Execute(context.Context, string, time.Duration) (transport.Result, error) {
	return transport.Result{}, nil
}
func (a *auditTerminal) Backend() transport.BackendType    { return transport.BackendSSH }
func (a *auditTerminal) Interactive(context.Context) error { return nil }
func (a *auditTerminal) Close() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.closes++
	return nil
}
func (a *auditTerminal) closeCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.closes
}

// TestReaperClosesIdleTerminals is the regression for the second missing
// link: Reap had no production caller, so an idle terminal lived until its
// process exited. The test drives the real pool, the real reaper and the real
// audit file, so it fails if any of the three is disconnected again.
func TestReaperClosesIdleTerminals(t *testing.T) {
	env := newCLIEnv(t)
	dir := auditDirFor(t, env)

	deps := env.deps().WithDefaults()
	// A short interval and an effectively-zero TTL make the test fast without
	// weakening what it proves.
	deps.Runtime.ReaperInterval = 5 * time.Millisecond
	deps.Runtime.IdleTTL = time.Nanosecond

	term := &auditTerminal{}
	deps.Runtime.Terminals.Resolve = func(context.Context, *auth.Session, string, string, string) (assets.Info, error) {
		return assets.Info{ID: "uuid-web-01", Name: "web-01", Address: "10.0.0.9", Protocol: "ssh"}, nil
	}
	deps.Runtime.Terminals.Connect = func(context.Context, *auth.Session, assets.Info, transport.ConnectOptions) (transport.Terminal, error) {
		return term, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	deps.Runtime.StartReaper(ctx)

	srv := &config.ServerConfig{Name: "bastion", Username: "ops"}
	if _, err := deps.Runtime.Terminals.Exec(ctx, srv, &auth.Session{}, "web-01", "uptime",
		connpool.ExecOptions{}); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if got := deps.Runtime.Terminals.Stats().Terminals; got != 1 {
		t.Fatalf("pooled terminals after Exec = %d, want 1", got)
	}

	waitFor(t, 2*time.Second, func() bool {
		return deps.Runtime.Terminals.Stats().Terminals == 0
	}, "the reaper never ran: idle terminals are not being closed")

	if got := term.closeCount(); got != 1 {
		t.Errorf("terminal closes = %d, want 1: the connection was evicted but not closed", got)
	}
	if err := deps.Runtime.Close(); err != nil {
		t.Fatalf("Runtime.Close: %v", err)
	}

	events := auditEventsIn(t, dir)
	var reaps int
	for _, e := range events {
		if e.Kind == obs.KindPoolReap {
			reaps++
		}
	}
	if reaps != 1 {
		t.Errorf("pool.reap events = %d, want 1 (events: %+v)", reaps, events)
	}
}

// waitFor polls until cond holds or the deadline passes.
func waitFor(t *testing.T, timeout time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal(msg)
}
