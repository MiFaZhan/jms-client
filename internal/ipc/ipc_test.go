package ipc

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// testHost builds a host bound to a throwaway endpoint and returns the sink
// the test uses to publish events.
func testHost(t *testing.T, exec func(context.Context, ExecRequest) ExecResult) (*Host, string, func(json.RawMessage)) {
	t.Helper()
	dir := t.TempDir()
	var (
		mu   sync.Mutex
		sink func(json.RawMessage)
	)
	host := &Host{
		Subscribe: func(s func(json.RawMessage)) {
			mu.Lock()
			sink = s
			mu.Unlock()
		},
		Exec:     exec,
		Endpoint: EndpointName(dir),
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	addr, err := host.Listen(ctx)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	t.Cleanup(func() { _ = host.Close() })

	publish := func(event json.RawMessage) {
		mu.Lock()
		s := sink
		mu.Unlock()
		if s == nil {
			t.Fatal("host never registered its event sink")
		}
		s(event)
	}
	return host, addr, publish
}

// waitAttached blocks until the host has at least n attached clients.
func waitAttached(t *testing.T, h *Host, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		h.mu.Lock()
		count := 0
		for c := range h.clients {
			c.mu.Lock()
			attached := c.attached
			c.mu.Unlock()
			if attached {
				count++
			}
		}
		h.mu.Unlock()
		if count >= n {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("host never saw %d attached clients", n)
}

// eventCollector records the events a client receives.
type eventCollector struct {
	mu     sync.Mutex
	events []json.RawMessage
}

func (c *eventCollector) onEvent(event json.RawMessage) {
	c.mu.Lock()
	c.events = append(c.events, event)
	c.mu.Unlock()
}

func (c *eventCollector) waitFor(t *testing.T, n int) []json.RawMessage {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		c.mu.Lock()
		if len(c.events) >= n {
			out := append([]json.RawMessage(nil), c.events...)
			c.mu.Unlock()
			return out
		}
		c.mu.Unlock()
		time.Sleep(time.Millisecond)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	t.Fatalf("client received %d events, wanted %d", len(c.events), n)
	return nil
}

func mustEvent(t *testing.T, asset, command string) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(map[string]string{"asset": asset, "command": command})
	if err != nil {
		t.Fatalf("marshal event: %v", err)
	}
	return raw
}

// mustRaw marshals v into an envelope payload.
func mustRaw(v any) json.RawMessage {
	raw, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return raw
}

// TestEndpointNameIsDeterministicAndPerDirectory pins the endpoint contract:
// the same config dir always yields the same address, and two config dirs
// never share one.
func TestEndpointNameIsDeterministicAndPerDirectory(t *testing.T) {
	first := EndpointName(filepath.Join("root", "a"))
	if again := EndpointName(filepath.Join("root", "a")); again != first {
		t.Fatalf("EndpointName is not deterministic: %q then %q", first, again)
	}
	second := EndpointName(filepath.Join("root", "b"))
	if second == first {
		t.Fatalf("two config dirs share the endpoint %q", first)
	}
	if !strings.Contains(first, endpointPrefix) {
		t.Fatalf("endpoint %q does not carry the %q prefix", first, endpointPrefix)
	}
	if runtime.GOOS == "windows" {
		if !strings.HasPrefix(first, `\\.\pipe\`) {
			t.Fatalf("windows endpoint %q is not a named pipe", first)
		}
	} else if !strings.HasSuffix(first, ".sock") {
		t.Fatalf("unix endpoint %q is not a socket path", first)
	}
}

// TestEndpointNameTreatsAnEmptyConfigDirAsTheTempDir keeps the error path
// coherent: a host that could not resolve its config directory must still
// land on the same endpoint the client computes for the same input.
func TestEndpointNameTreatsAnEmptyConfigDirAsTheTempDir(t *testing.T) {
	if EndpointName("") != EndpointName(os.TempDir()) {
		t.Fatalf("EndpointName(%q) = %q, wanted EndpointName(os.TempDir()) = %q",
			"", EndpointName(""), EndpointName(os.TempDir()))
	}
	if EndpointName("") != EndpointName("") {
		t.Fatal("EndpointName is not deterministic for an empty config dir")
	}
}

// TestHostClientRoundTrip drives the real endpoint: hello is delivered and a
// pushed event reaches the client.
func TestHostClientRoundTrip(t *testing.T) {
	host, addr, publish := testHost(t, nil)

	collector := &eventCollector{}
	conn, err := Dial(context.Background(), addr, &Client{Role: RoleObserver, OnEvent: collector.onEvent})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close()
	waitAttached(t, host, 1)

	publish(mustEvent(t, "web-01", "uptime"))
	events := collector.waitFor(t, 1)
	if !strings.Contains(string(events[0]), "uptime") {
		t.Fatalf("client received %s, wanted the pushed event", events[0])
	}
}

// TestObserverMayNotExec is the authorization half of arbitration: an
// observer attaches to watch, never to drive the pool.
func TestObserverMayNotExec(t *testing.T) {
	called := make(chan struct{}, 1)
	host, addr, _ := testHost(t, func(context.Context, ExecRequest) ExecResult {
		called <- struct{}{}
		return ExecResult{Output: "should not run"}
	})

	conn, err := Dial(context.Background(), addr, &Client{Role: RoleObserver})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close()
	waitAttached(t, host, 1)

	res, err := conn.Exec(context.Background(), ExecRequest{Asset: "web-01", Command: "id"})
	if err == nil {
		t.Fatal("an observer exec must fail")
	}
	if !strings.Contains(err.Error(), "observer") {
		t.Fatalf("error %q does not name the observer role", err)
	}
	if res.Output != "" {
		t.Fatalf("observer exec produced output %q", res.Output)
	}
	select {
	case <-called:
		t.Fatal("the host's Exec seam ran for an observer")
	default:
	}
}

// TestOperatorExecReachesSeamAndReturnsResult checks both directions of the
// operator path: the request arrives intact and the result comes back with
// output and exit code.
func TestOperatorExecReachesSeamAndReturnsResult(t *testing.T) {
	requests := make(chan ExecRequest, 1)
	host, addr, _ := testHost(t, func(_ context.Context, req ExecRequest) ExecResult {
		requests <- req
		return ExecResult{Output: "uid=0(root)\n", ExitCode: 7}
	})

	conn, err := Dial(context.Background(), addr, &Client{Role: RoleOperator})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close()
	waitAttached(t, host, 1)

	res, err := conn.Exec(context.Background(), ExecRequest{
		Server:  "prod",
		Asset:   "web-01",
		Command: "id",
		Account: "@root",
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.Output != "uid=0(root)\n" || res.ExitCode != 7 {
		t.Fatalf("result = %+v, wanted the seam's output and exit code", res)
	}

	select {
	case req := <-requests:
		want := ExecRequest{Server: "prod", Asset: "web-01", Command: "id", Account: "@root"}
		if req != want {
			t.Fatalf("seam saw %+v, wanted %+v", req, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the host's Exec seam was never called")
	}
}

// TestExecReportsBusy covers the arbitration rule's refusal path: the host
// refuses instead of queueing, and the client can see why.
func TestExecReportsBusy(t *testing.T) {
	host, addr, _ := testHost(t, func(context.Context, ExecRequest) ExecResult {
		return ExecResult{Busy: true}
	})

	conn, err := Dial(context.Background(), addr, &Client{Role: RoleOperator})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close()
	waitAttached(t, host, 1)

	res, err := conn.Exec(context.Background(), ExecRequest{Asset: "web-01", Command: "id"})
	if err == nil {
		t.Fatal("a busy host must report an error to the operator")
	}
	if !res.Busy {
		t.Fatalf("result %+v does not carry busy", res)
	}
	if !strings.Contains(err.Error(), "busy") {
		t.Fatalf("error %q does not explain the refusal", err)
	}
}

// TestAssetFilterLimitsEvents is `--asset`: only matching events reach the
// client.
//
// A sentinel event after the filtered ones proves the negative: had a
// db-01 event been delivered it would have arrived before the sentinel, so
// the assertion does not need a settling delay.
func TestAssetFilterLimitsEvents(t *testing.T) {
	host, addr, publish := testHost(t, nil)

	collector := &eventCollector{}
	conn, err := Dial(context.Background(), addr, &Client{
		Role:        RoleObserver,
		AssetFilter: "web-01",
		OnEvent:     collector.onEvent,
	})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close()
	waitAttached(t, host, 1)

	publish(mustEvent(t, "db-01", "select 1"))
	publish(mustEvent(t, "web-01", "uptime"))
	publish(mustEvent(t, "db-01", "select 2"))
	collector.waitFor(t, 1)

	publish(mustEvent(t, "web-01", "sentinel"))
	events := collector.waitFor(t, 2)
	if len(events) != 2 {
		t.Fatalf("received %d events, wanted only the two matching ones: %s", len(events), events)
	}
	for i, want := range []string{"uptime", "sentinel"} {
		if !strings.Contains(string(events[i]), want) {
			t.Fatalf("event[%d] = %s, wanted %s", i, events[i], want)
		}
	}
}

// TestLastReplaysBufferedEventsBeforeLiveOnes is `--last N`: the ring is
// replayed in order, then live events follow.
func TestLastReplaysBufferedEventsBeforeLiveOnes(t *testing.T) {
	host, addr, publish := testHost(t, nil)

	// Publish with nobody attached; the host ring buffers them.
	for i := 1; i <= 5; i++ {
		publish(mustEvent(t, "web-01", fmt.Sprintf("cmd-%d", i)))
	}

	collector := &eventCollector{}
	conn, err := Dial(context.Background(), addr, &Client{
		Role:    RoleObserver,
		Last:    3,
		OnEvent: collector.onEvent,
	})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close()
	waitAttached(t, host, 1)

	replayed := collector.waitFor(t, 3)
	for i, want := range []string{"cmd-3", "cmd-4", "cmd-5"} {
		if !strings.Contains(string(replayed[i]), want) {
			t.Fatalf("replay[%d] = %s, wanted %s", i, replayed[i], want)
		}
	}

	publish(mustEvent(t, "web-01", "live"))
	events := collector.waitFor(t, 4)
	if !strings.Contains(string(events[3]), "live") {
		t.Fatalf("live event %s did not follow the replay", events[3])
	}
}

// TestDroppedClientDoesNotStopTheHost is the reconnect guarantee: a client
// that goes away leaves the host serving the next one.
func TestDroppedClientDoesNotStopTheHost(t *testing.T) {
	host, addr, publish := testHost(t, nil)

	first, err := Dial(context.Background(), addr, &Client{Role: RoleObserver})
	if err != nil {
		t.Fatalf("first Dial: %v", err)
	}
	waitAttached(t, host, 1)
	if err := first.Close(); err != nil {
		t.Fatalf("close first client: %v", err)
	}

	// Wait for the host to notice the disconnect before the second client
	// attaches, so the test really exercises a host that lost a client.
	deadline := time.Now().Add(5 * time.Second)
	for {
		host.mu.Lock()
		count := len(host.clients)
		host.mu.Unlock()
		if count == 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(time.Millisecond)
	}

	collector := &eventCollector{}
	second, err := Dial(context.Background(), addr, &Client{Role: RoleObserver, OnEvent: collector.onEvent})
	if err != nil {
		t.Fatalf("second Dial after a client disconnect: %v", err)
	}
	defer second.Close()
	waitAttached(t, host, 1)

	publish(mustEvent(t, "web-01", "after-reconnect"))
	events := collector.waitFor(t, 1)
	if !strings.Contains(string(events[0]), "after-reconnect") {
		t.Fatalf("second client received %s", events[0])
	}
}

// TestMalformedJSONDoesNotCrashTheHost sends garbage and then proves the
// session still works.
func TestMalformedJSONDoesNotCrashTheHost(t *testing.T) {
	host, addr, publish := testHost(t, nil)

	collector := &eventCollector{}
	conn, err := Dial(context.Background(), addr, &Client{Role: RoleObserver, OnEvent: collector.onEvent})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close()
	waitAttached(t, host, 1)

	if _, err := conn.conn.Write([]byte("{not json at all\n")); err != nil {
		t.Fatalf("write malformed line: %v", err)
	}
	if _, err := conn.conn.Write([]byte("\n")); err != nil {
		t.Fatalf("write blank line: %v", err)
	}

	publish(mustEvent(t, "web-01", "still-alive"))
	events := collector.waitFor(t, 1)
	if !strings.Contains(string(events[0]), "still-alive") {
		t.Fatalf("client received %s after a malformed line", events[0])
	}
	if host.isClosed() {
		t.Fatal("a malformed line closed the host")
	}
}

// TestHostCloseIsIdempotentAndReleasesTheEndpoint keeps Close honest: a second
// Close must not panic, and a re-listen on the same address must succeed.
func TestHostCloseIsIdempotentAndReleasesTheEndpoint(t *testing.T) {
	dir := t.TempDir()
	addr := EndpointName(dir)

	newHost := func() *Host {
		return &Host{Endpoint: addr}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	host := newHost()
	if _, err := host.Listen(ctx); err != nil {
		t.Fatalf("first Listen: %v", err)
	}
	if err := host.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := host.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}

	second := newHost()
	if _, err := second.Listen(ctx); err != nil {
		t.Fatalf("re-listen after Close: %v", err)
	}
	if err := second.Close(); err != nil {
		t.Fatalf("second host Close: %v", err)
	}
}

// TestListenReturnsWhenContextIsCancelled is the lifecycle contract the CLI
// depends on: cancelling the context tears the endpoint down.
func TestListenReturnsWhenContextIsCancelled(t *testing.T) {
	dir := t.TempDir()
	addr := EndpointName(dir)
	host := &Host{Endpoint: addr}

	ctx, cancel := context.WithCancel(context.Background())
	if _, err := host.Listen(ctx); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	cancel()

	deadline := time.Now().Add(5 * time.Second)
	for !host.isClosed() {
		if time.Now().After(deadline) {
			t.Fatal("cancelling the context did not close the host")
		}
		time.Sleep(time.Millisecond)
	}
}

// TestDialRejectsAMissingHost reports an actionable error rather than hanging.
func TestDialRejectsAMissingHost(t *testing.T) {
	addr := EndpointName(t.TempDir())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if _, err := Dial(ctx, addr, &Client{Role: RoleObserver}); err == nil {
		t.Fatal("Dial to a host that is not listening must fail")
	}
}

// TestDialRejectsAnEmptyAddressAndNilClient covers the argument checks.
func TestDialRejectsAnEmptyAddressAndNilClient(t *testing.T) {
	if _, err := Dial(context.Background(), "", &Client{Role: RoleObserver}); err == nil {
		t.Fatal("Dial with an empty address must fail")
	}
	if _, err := Dial(context.Background(), "x", nil); err == nil {
		t.Fatal("Dial with a nil client must fail")
	}
	var nilConn *Conn
	if err := nilConn.Close(); err != nil {
		t.Fatalf("Close on a nil Conn: %v", err)
	}
	if _, err := nilConn.Exec(context.Background(), ExecRequest{Command: "id"}); err == nil {
		t.Fatal("Exec on a nil Conn must fail")
	}
	var nilHost *Host
	if err := nilHost.Close(); err != nil {
		t.Fatalf("Close on a nil Host: %v", err)
	}
	if _, err := nilHost.Listen(context.Background()); err == nil {
		t.Fatal("Listen on a nil Host must fail")
	}
}

// TestUnixSocketIsOwnerOnly is the Unix half of the security boundary: the
// endpoint must not be readable by another user (DESIGN.md §11.6).
func TestUnixSocketIsOwnerOnly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the Windows endpoint is a named pipe with an owner-only ACL")
	}
	dir := t.TempDir()
	addr := EndpointName(dir)
	host := &Host{Endpoint: addr}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := host.Listen(ctx); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer host.Close()

	info, err := os.Stat(addr)
	if err != nil {
		t.Fatalf("stat the socket: %v", err)
	}
	if perm := info.Mode().Perm(); perm != socketFileMode {
		t.Fatalf("socket mode = %04o, wanted %04o", perm, socketFileMode)
	}
	if info.Mode()&os.ModeSocket == 0 {
		t.Fatalf("endpoint %s is not a socket (mode %v)", addr, info.Mode())
	}
}

// TestSecondHostOnTheSameEndpointFails keeps two hosts from silently sharing
// one endpoint: the second must fail loudly instead of stealing clients.
func TestSecondHostOnTheSameEndpointFails(t *testing.T) {
	addr := EndpointName(t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	first := &Host{Endpoint: addr}
	if _, err := first.Listen(ctx); err != nil {
		t.Fatalf("first Listen: %v", err)
	}
	defer first.Close()

	second := &Host{Endpoint: addr}
	if _, err := second.Listen(ctx); err == nil {
		_ = second.Close()
		t.Fatal("a second host on the same endpoint must fail")
	}
}

// TestRepeatedHelloIsAccepted keeps a client that repeats its handshake from
// panicking the host on a double close.
func TestRepeatedHelloIsAccepted(t *testing.T) {
	host, addr, publish := testHost(t, nil)

	collector := &eventCollector{}
	conn, err := Dial(context.Background(), addr, &Client{Role: RoleObserver, OnEvent: collector.onEvent})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close()
	waitAttached(t, host, 1)

	if err := conn.send(MsgHello, Hello{Type: MsgHello, Role: RoleOperator, Last: 1}); err != nil {
		t.Fatalf("repeat hello: %v", err)
	}

	publish(mustEvent(t, "web-01", "after-second-hello"))
	events := collector.waitFor(t, 1)
	if !strings.Contains(string(events[0]), "after-second-hello") {
		t.Fatalf("client received %s after repeating hello", events[0])
	}
}

// TestEveryAttachedClientReceivesTheEvent is the fan-out contract: one
// broadcast reaches each attached client exactly once.
func TestEveryAttachedClientReceivesTheEvent(t *testing.T) {
	host, addr, publish := testHost(t, nil)

	first := &eventCollector{}
	one, err := Dial(context.Background(), addr, &Client{Role: RoleObserver, OnEvent: first.onEvent})
	if err != nil {
		t.Fatalf("first Dial: %v", err)
	}
	defer one.Close()

	second := &eventCollector{}
	two, err := Dial(context.Background(), addr, &Client{Role: RoleOperator, OnEvent: second.onEvent})
	if err != nil {
		t.Fatalf("second Dial: %v", err)
	}
	defer two.Close()
	waitAttached(t, host, 2)

	publish(mustEvent(t, "web-01", "fan-out"))
	for i, collector := range []*eventCollector{first, second} {
		events := collector.waitFor(t, 1)
		if !strings.Contains(string(events[0]), "fan-out") {
			t.Fatalf("client %d received %s", i, events[0])
		}
	}
}

// TestLastWithAFilterReplaysOnlyMatchingEvents checks that `--asset` also
// narrows the replay, not just live delivery.
func TestLastWithAFilterReplaysOnlyMatchingEvents(t *testing.T) {
	host, addr, publish := testHost(t, nil)

	publish(mustEvent(t, "db-01", "select 1"))
	publish(mustEvent(t, "web-01", "uptime"))
	publish(mustEvent(t, "db-01", "select 2"))
	publish(mustEvent(t, "web-01", "df -h"))

	collector := &eventCollector{}
	conn, err := Dial(context.Background(), addr, &Client{
		Role:        RoleObserver,
		AssetFilter: "web-01",
		Last:        10,
		OnEvent:     collector.onEvent,
	})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close()
	waitAttached(t, host, 1)

	collector.waitFor(t, 2)
	publish(mustEvent(t, "web-01", "sentinel"))
	events := collector.waitFor(t, 3)
	if len(events) != 3 {
		t.Fatalf("received %d events, wanted the two matching replays plus the live one", len(events))
	}
	for i, want := range []string{"uptime", "df -h", "sentinel"} {
		if !strings.Contains(string(events[i]), want) {
			t.Fatalf("event[%d] = %s, wanted %s", i, events[i], want)
		}
	}
}

// TestHostCloseDropsAttachedClients keeps Close from leaking a client's
// goroutines: the client's read loop must observe the disconnect.
func TestHostCloseDropsAttachedClients(t *testing.T) {
	addr := EndpointName(t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	host := &Host{Endpoint: addr}
	if _, err := host.Listen(ctx); err != nil {
		t.Fatalf("Listen: %v", err)
	}

	conn, err := Dial(context.Background(), addr, &Client{Role: RoleObserver})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close()
	waitAttached(t, host, 1)

	if err := host.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	select {
	case <-conn.done:
	case <-time.After(5 * time.Second):
		t.Fatal("the client never observed the host closing the connection")
	}
}

// TestKeepalivePingIsAnswered checks the keepalive half of the protocol over
// a raw connection, so the pong is observed on the wire rather than inferred.
func TestKeepalivePingIsAnswered(t *testing.T) {
	_, addr, _ := testHost(t, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	raw, err := dialEndpoint(ctx, addr)
	if err != nil {
		t.Fatalf("dialEndpoint: %v", err)
	}
	defer raw.Close()

	write := func(v any) {
		t.Helper()
		line, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if _, err := raw.Write(append(line, '\n')); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	read := func() Envelope {
		t.Helper()
		var env Envelope
		if err := json.NewDecoder(raw).Decode(&env); err != nil {
			t.Fatalf("read: %v", err)
		}
		return env
	}

	write(Envelope{Type: MsgHello, Data: mustRaw(Hello{Type: MsgHello, Role: RoleObserver})})
	write(Envelope{Type: MsgPing})
	if env := read(); env.Type != MsgPong {
		t.Fatalf("host answered %q, wanted %q", env.Type, MsgPong)
	}
}

// TestFlatHelloIsAccepted covers the shape DESIGN.md §12.4 documents, where
// hello is the whole line rather than an envelope payload.
func TestFlatHelloIsAccepted(t *testing.T) {
	host, addr, publish := testHost(t, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	raw, err := dialEndpoint(ctx, addr)
	if err != nil {
		t.Fatalf("dialEndpoint: %v", err)
	}
	defer raw.Close()

	line, err := json.Marshal(Hello{Type: MsgHello, Role: RoleObserver})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if _, err := raw.Write(append(line, '\n')); err != nil {
		t.Fatalf("write: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		host.mu.Lock()
		attached := false
		for c := range host.clients {
			c.mu.Lock()
			attached = c.attached
			c.mu.Unlock()
		}
		host.mu.Unlock()
		if attached {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("a flat hello never attached the client")
		}
		time.Sleep(time.Millisecond)
	}

	publish(mustEvent(t, "web-01", "flat-hello"))
	var env Envelope
	if err := json.NewDecoder(raw).Decode(&env); err != nil {
		t.Fatalf("read: %v", err)
	}
	if env.Type != MsgEvent || !strings.Contains(string(env.Data), "flat-hello") {
		t.Fatalf("received %s %s, wanted the pushed event", env.Type, env.Data)
	}
}

// TestUnknownMessageTypeIsReportedNotFatal keeps a future protocol addition
// from killing the host when it talks to an older host.
func TestUnknownMessageTypeIsReportedNotFatal(t *testing.T) {
	_, addr, _ := testHost(t, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	raw, err := dialEndpoint(ctx, addr)
	if err != nil {
		t.Fatalf("dialEndpoint: %v", err)
	}
	defer raw.Close()

	if _, err := raw.Write([]byte(`{"type":"future.thing"}` + "\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	var env Envelope
	if err := json.NewDecoder(raw).Decode(&env); err != nil {
		t.Fatalf("read: %v", err)
	}
	if env.Type != MsgExecResult {
		t.Fatalf("host answered %q, wanted an error result", env.Type)
	}
	var res ExecResult
	if err := json.Unmarshal(env.Data, &res); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if res.Error == "" {
		t.Fatal("an unknown message type must be answered with an error")
	}
}

// TestHelloWithAnUnknownRoleIsRejected keeps a client from attaching with no
// privileges and then being treated as an operator by accident.
func TestHelloWithAnUnknownRoleIsRejected(t *testing.T) {
	_, addr, _ := testHost(t, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	raw, err := dialEndpoint(ctx, addr)
	if err != nil {
		t.Fatalf("dialEndpoint: %v", err)
	}
	defer raw.Close()

	line, err := json.Marshal(Hello{Type: MsgHello, Role: Role("admin")})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if _, err := raw.Write(append(line, '\n')); err != nil {
		t.Fatalf("write: %v", err)
	}
	var env Envelope
	if err := json.NewDecoder(raw).Decode(&env); err != nil {
		t.Fatalf("read: %v", err)
	}
	var res ExecResult
	if err := json.Unmarshal(env.Data, &res); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if res.Error == "" {
		t.Fatal("an unknown role must be rejected")
	}
}

// blockingConn is a connection whose writes never complete, used to model a
// client that cannot keep up.
type blockingConn struct {
	release chan struct{}
	once    sync.Once
}

func (c *blockingConn) Read([]byte) (int, error) { return 0, io.EOF }

func (c *blockingConn) Write(p []byte) (int, error) {
	<-c.release
	return len(p), nil
}

func (c *blockingConn) Close() error {
	c.once.Do(func() { close(c.release) })
	return nil
}

// TestCongestedClientIsDroppedNotWaitedFor is the backpressure rule
// (DESIGN.md §11.8): a client that cannot keep up must never stall the
// publishing path, and its dropped events are counted.
func TestCongestedClientIsDroppedNotWaitedFor(t *testing.T) {
	host := &Host{Endpoint: EndpointName(t.TempDir())}
	conn := &blockingConn{release: make(chan struct{})}
	defer conn.Close()

	slow := newClient(host, conn)
	slow.mu.Lock()
	slow.attached = true
	slow.mu.Unlock()
	go slow.writeLoop()
	defer slow.shutdown()

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < clientQueue*3; i++ {
			slow.offer(mustEvent(t, "web-01", "spam"))
		}
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("offer blocked on a congested client; the publishing path stalled")
	}

	slow.mu.Lock()
	dropped := slow.dropped
	slow.mu.Unlock()
	if dropped == 0 {
		t.Fatalf("no events were dropped for a client that cannot write (queue %d)", clientQueue)
	}
	if dropped < int64(clientQueue) {
		t.Fatalf("dropped %d events; wanted at least the queue size %d", dropped, clientQueue)
	}
}
