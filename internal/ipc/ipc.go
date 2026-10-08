// Package ipc is the local observation and control channel used by
// `jms attach`.
//
// Transport is a Windows named pipe or a Unix domain socket, owner-only, and
// it never listens on a network port (DESIGN.md「IPC 宿主」).
//
// The wire protocol is JSON-Lines: the client opens with `hello{role,
// asset_filter?, last?}`, the host pushes `event`, an `operator` may send
// `exec{...}` and receives `exec.result`, and `ping`/`pong` keep the channel
// alive. Arbitration between a human command and an AI command happens at the
// Exec seam, which shares the terminal pool's per-terminal mutex, so the two
// callers serialize on one connection instead of racing for it (「IPC 宿主」).
package ipc

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/MiFaZhan/jms-client/internal/config"
)

// Role is what a client asks to be.
type Role string

// Client roles.
const (
	RoleObserver Role = "observer"
	RoleOperator Role = "operator"
)

// Message types on the wire (JSON-Lines).
const (
	MsgHello      = "hello"
	MsgEvent      = "event"
	MsgExec       = "exec"
	MsgExecResult = "exec.result"
	MsgPing       = "ping"
	MsgPong       = "pong"
)

// Defaults.
const (
	// DefaultPingInterval keeps an idle connection alive.
	DefaultPingInterval = 30 * time.Second
	// execTimeout bounds an operator command whose request carries no
	// deadline, so a wedged host cannot strand the attached client.
	execTimeout = 5 * time.Minute
	// socketFileMode is the Unix socket's permission: owner-only (「IPC 宿主」).
	socketFileMode = 0o600
	// hostRingSize bounds the host's replay buffer behind `--last`.
	hostRingSize = 512
	// clientQueue is how many events one client may have in flight. A client
	// that falls further behind is dropped rather than allowed to stall the
	// host (「可观测性」).
	clientQueue = 256
	// maxLineBytes bounds one wire message. A longer line ends that client's
	// session instead of growing the host's memory.
	maxLineBytes = 1 << 20
	// lineBuffer is the initial read buffer.
	lineBuffer = 64 * 1024
	// endpointPrefix is the shared endpoint name prefix on both platforms.
	endpointPrefix = "jms-"
)

// Hello is the client's opening message.
type Hello struct {
	Type        string `json:"type"`
	Role        Role   `json:"role"`
	AssetFilter string `json:"asset_filter,omitempty"`
	Last        int    `json:"last,omitempty"`
}

// Envelope is one wire message.
type Envelope struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data,omitempty"`
}

// ExecRequest is an operator's request to run a command.
type ExecRequest struct {
	Server  string `json:"server,omitempty"`
	Asset   string `json:"asset"`
	Command string `json:"command"`
	Account string `json:"account,omitempty"`
}

// ExecResult is the answer to an ExecRequest.
//
// Busy reports that another command (an AI tool call or a second operator)
// currently holds the terminal: the host refuses rather than queueing
// unboundedly (「IPC 宿主」).
type ExecResult struct {
	Output   string `json:"output"`
	ExitCode int    `json:"exit_code"`
	Error    string `json:"error,omitempty"`
	Busy     bool   `json:"busy,omitempty"`
}

// execWire is the on-the-wire exec pair. The correlation key is protocol
// plumbing, so it is kept out of the frozen ExecRequest/ExecResult surface.
type execWire struct {
	Key string `json:"key,omitempty"`
	ExecRequest
	Result ExecResult `json:"result,omitempty"`
}

// Host is the server side of the channel, embedded in the jms mcp process.
type Host struct {
	// Subscribe receives bus events to broadcast. It must not block the
	// caller: a congested client is dropped, never waited for (「可观测性」).
	Subscribe func(sink func(event json.RawMessage))
	// Exec runs an operator command through the shared pool.
	Exec func(ctx context.Context, req ExecRequest) ExecResult
	// Endpoint pins the endpoint address. The empty value means the per-user
	// endpoint derived from the configuration directory.
	//
	// It exists because the CLI's configuration directory is not always
	// config.ConfigDir(): an explicit --config must put the host on the same
	// endpoint that `jms attach` computes, or the two would never meet.
	Endpoint string

	mu        sync.Mutex
	addr      string
	clients   map[*client]struct{}
	listener  endpointListener
	unsub     func()
	ring      []json.RawMessage
	next      int
	filled    bool
	closed    bool
	closeOnce sync.Once
}

// Listen starts the host on the platform's owner-only endpoint and returns
// its address.
//
// Listen returns once the endpoint is accepting; the endpoint stays up until
// ctx is cancelled or Close is called.
func (h *Host) Listen(ctx context.Context) (string, error) {
	if h == nil {
		return "", errors.New("ipc: nil host")
	}
	addr := h.endpoint()
	ln, err := listenEndpoint(addr)
	if err != nil {
		return "", err
	}

	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		_ = ln.Close()
		return "", errors.New("ipc: host is closed")
	}
	h.addr = addr
	h.clients = make(map[*client]struct{})
	h.ring = make([]json.RawMessage, hostRingSize)
	h.listener = ln
	h.mu.Unlock()

	if h.Subscribe != nil {
		// The subscription is registered before the cancellation watcher
		// starts, so a cancelled context cannot tear the host down between
		// the two and leave the bus subscribed forever.
		unsub := h.subscribeBus()
		h.mu.Lock()
		if h.closed {
			h.mu.Unlock()
			unsub()
		} else {
			h.unsub = unsub
			h.mu.Unlock()
		}
	}
	go func() {
		<-ctx.Done()
		h.close()
	}()

	go h.acceptLoop(ln)
	return addr, nil
}

// Close tears the endpoint down.
//
// It unblocks Listen, drops every attached client and removes the Unix socket
// file. It is idempotent.
func (h *Host) Close() error {
	if h == nil {
		return nil
	}
	h.close()
	return nil
}

func (h *Host) close() {
	h.closeOnce.Do(func() {
		h.mu.Lock()
		h.closed = true
		ln := h.listener
		h.listener = nil
		clients := make([]*client, 0, len(h.clients))
		for c := range h.clients {
			clients = append(clients, c)
		}
		h.clients = nil
		unsub := h.unsub
		h.unsub = nil
		h.mu.Unlock()

		if unsub != nil {
			unsub()
		}
		if ln != nil {
			_ = ln.Close()
		}
		for _, c := range clients {
			c.shutdown()
		}
	})
}

// subscribeBus turns the bus subscription callback into a fan-out over the
// attached clients.
//
// Every event is recorded in the host ring first, so a client that attaches
// later can still replay it with `--last` (DESIGN.md「IPC 宿主」).
func (h *Host) subscribeBus() func() {
	var mu sync.Mutex
	subscribed := true
	h.Subscribe(func(event json.RawMessage) {
		mu.Lock()
		alive := subscribed
		mu.Unlock()
		if !alive {
			return
		}
		h.broadcast(append(json.RawMessage(nil), event...))
	})
	return func() {
		mu.Lock()
		subscribed = false
		mu.Unlock()
	}
}

// broadcast records an event and offers it to every attached client.
func (h *Host) broadcast(event json.RawMessage) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.recordLocked(event)
	for c := range h.clients {
		c.offer(event)
	}
}

// recordLocked appends an event to the replay ring.
func (h *Host) recordLocked(event json.RawMessage) {
	if len(h.ring) == 0 {
		return
	}
	h.ring[h.next] = event
	h.next = (h.next + 1) % len(h.ring)
	if h.next == 0 {
		h.filled = true
	}
}

// snapshotLocked returns up to n buffered events, oldest first.
func (h *Host) snapshotLocked(n int) []json.RawMessage {
	size := len(h.ring)
	count := h.next
	if h.filled {
		count = size
	}
	if n > 0 && n < count {
		count = n
	}
	out := make([]json.RawMessage, 0, count)
	for i := 0; i < count; i++ {
		idx := h.next - count + i
		if idx < 0 {
			idx += size
		}
		out = append(out, h.ring[idx])
	}
	return out
}

// addClient registers c, reporting false when the host has already closed.
func (h *Host) addClient(c *client) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.clients == nil {
		return false
	}
	h.clients[c] = struct{}{}
	return true
}

func (h *Host) removeClient(c *client) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.clients, c)
}

func (h *Host) isClosed() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.closed
}

// endpoint renders the host's endpoint address from the configuration
// directory, which is what the CLI hashes for `jms attach` too.
func (h *Host) endpoint() string {
	if h.Endpoint != "" {
		return h.Endpoint
	}
	dir, err := config.ConfigDir()
	if err != nil {
		dir = ""
	}
	return EndpointName(dir)
}

// endpointListener accepts connections on the platform endpoint.
//
// It exists so the accept loop is written once against an interface rather
// than twice against a named pipe and a Unix socket, whose accept semantics
// differ enough that sharing the loop matters.
type endpointListener interface {
	Accept() (io.ReadWriteCloser, error)
	Close() error
	Closed() <-chan struct{}
}

// acceptLoop serves every client on its own goroutine.
//
// A failed accept ends the loop only when the listener is closed: one client
// that cannot complete the handshake must never take the host down.
func (h *Host) acceptLoop(ln endpointListener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			if h.isClosed() {
				return
			}
			select {
			case <-ln.Closed():
				return
			default:
			}
			continue
		}
		h.serve(conn)
	}
}

// serve registers one connection and starts its serving goroutine.
//
// A connection that arrives after Close is dropped rather than served, so a
// shutdown never leaves a client goroutine parked on an unregistered socket.
func (h *Host) serve(conn io.ReadWriteCloser) *client {
	c := newClient(h, conn)
	if !h.addClient(c) {
		_ = conn.Close()
		return c
	}
	go c.serve()
	return c
}

// client is one attached connection.
type client struct {
	host *Host
	conn io.ReadWriteCloser

	writeMu sync.Mutex
	// queue carries pushed events to the writer goroutine. A full queue means
	// the client is not keeping up, and the event is dropped (「可观测性」).
	queue chan json.RawMessage
	done  chan struct{}
	// hello is closed once the handshake has been read, which is what tells
	// the writer goroutine it may start. It is a Once because a client may
	// legally repeat its hello.
	hello     chan struct{}
	helloOnce sync.Once

	mu     sync.Mutex
	role   Role
	filter string
	// replay holds the `--last` snapshot. The writer goroutine sends it before
	// it drains the queue, which is what guarantees replay-before-live
	// ordering without holding the host lock across the writes.
	replay   []json.RawMessage
	attached bool
	closed   bool
	dropped  int64
}

func newClient(h *Host, conn io.ReadWriteCloser) *client {
	return &client{
		host:  h,
		conn:  conn,
		queue: make(chan json.RawMessage, clientQueue),
		done:  make(chan struct{}),
		hello: make(chan struct{}),
	}
}

func (c *client) serve() {
	defer func() {
		c.shutdown()
		c.host.removeClient(c)
	}()
	go c.writeLoop()

	scanner := newLineScanner(c.conn)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(strings.TrimSpace(string(line))) == 0 {
			continue
		}
		c.handle(line)
	}
}

// shutdown stops the writer goroutine and closes the connection. It is
// idempotent and is what makes a dropped client harmless to the host.
func (c *client) shutdown() {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	c.mu.Unlock()
	close(c.done)
	_ = c.conn.Close()
}

// writeLoop sends the replay snapshot, then drains the outbound queue.
//
// Doing both on one goroutine is what orders `--last` before live events:
// the snapshot is captured and the client is marked attached under the host
// lock, and every live event published after that goes through the queue,
// which is only drained once the snapshot has been written.
func (c *client) writeLoop() {
	select {
	case <-c.hello:
	case <-c.done:
		return
	}

	c.mu.Lock()
	replay := c.replay
	c.replay = nil
	c.mu.Unlock()
	for _, event := range replay {
		if err := c.send(MsgEvent, event); err != nil {
			c.shutdown()
			return
		}
	}

	for {
		select {
		case <-c.done:
			return
		case event := <-c.queue:
			if err := c.send(MsgEvent, event); err != nil {
				c.shutdown()
				return
			}
		}
	}
}

// handle processes one wire line. A malformed line is answered with an error
// and never reaches the host's seams.
//
// A message may be sent either enveloped (`{"type":"hello","data":{...}}`)
// or flat (`{"type":"hello","role":"observer"}`), because the documented
// shapes in DESIGN.md「IPC 宿主」 are flat and a human poking at the pipe will
// write them that way.
func (c *client) handle(line []byte) {
	var env Envelope
	if err := json.Unmarshal(line, &env); err != nil {
		_ = c.send(MsgExecResult, ExecResult{Error: "malformed message"})
		return
	}
	switch env.Type {
	case MsgHello:
		c.handleHello(payloadOrLine(env.Data, line))
	case MsgExec:
		c.handleExec(payloadOrLine(env.Data, line))
	case MsgPing:
		_ = c.send(MsgPong, nil)
	case MsgPong:
		// Keepalive acknowledgement; nothing to do.
	default:
		_ = c.send(MsgExecResult, ExecResult{Error: "unknown message type"})
	}
}

// payloadOrLine prefers the envelope's data and falls back to the whole line,
// so a flat message is read exactly like an enveloped one.
func payloadOrLine(data, line []byte) json.RawMessage {
	if len(data) > 0 {
		return data
	}
	return line
}

// handleHello registers the role and captures the `--last` snapshot.
//
// The snapshot and the switch to live delivery happen under the host lock, so
// an event published in between is either in the snapshot or queued live,
// never neither and never twice.
func (c *client) handleHello(data json.RawMessage) {
	var hello Hello
	if err := json.Unmarshal(data, &hello); err != nil {
		_ = c.send(MsgExecResult, ExecResult{Error: "malformed hello"})
		return
	}
	switch hello.Role {
	case RoleObserver, RoleOperator:
	default:
		_ = c.send(MsgExecResult, ExecResult{Error: "unknown role"})
		return
	}

	c.host.mu.Lock()
	c.mu.Lock()
	c.role = hello.Role
	c.filter = hello.AssetFilter
	if hello.Last > 0 {
		for _, event := range c.host.snapshotLocked(hello.Last) {
			if matchesFilter(c.filter, event) {
				c.replay = append(c.replay, event)
			}
		}
	}
	c.attached = true
	c.mu.Unlock()
	c.host.mu.Unlock()
	c.helloOnce.Do(func() { close(c.hello) })
}

func (c *client) handleExec(data json.RawMessage) {
	var req execWire
	if err := json.Unmarshal(data, &req); err != nil {
		_ = c.send(MsgExecResult, ExecResult{Error: "malformed exec request"})
		return
	}
	if !c.isOperator() {
		// Arbitration is also an authorization rule: an observer may not
		// drive the terminal pool (「IPC 宿主」).
		_ = c.send(MsgExecResult, execWire{Key: req.Key, Result: ExecResult{
			Error: "role observer may not exec",
		}})
		return
	}
	if strings.TrimSpace(req.Command) == "" {
		_ = c.send(MsgExecResult, execWire{Key: req.Key, Result: ExecResult{
			Error: "exec request needs a command",
		}})
		return
	}
	if c.host.Exec == nil {
		_ = c.send(MsgExecResult, execWire{Key: req.Key, Result: ExecResult{
			Error: "host has no exec seam",
		}})
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), execTimeout)
	defer cancel()
	res := c.host.Exec(ctx, req.ExecRequest)
	if res.Busy && res.Error == "" {
		// A busy result must still carry a reason, or the operator sees a
		// refusal with no explanation.
		res.Error = "busy: another command holds the terminal"
	}
	_ = c.send(MsgExecResult, execWire{Key: req.Key, Result: res})
}

func (c *client) isOperator() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.role == RoleOperator
}

// offer queues an event for an attached client, dropping it when the client
// has fallen behind. It never blocks, so the publishing path never stalls
// (「可观测性」).
//
// An event that arrives before `hello` is not queued: the client's filter is
// unknown until then, and `--last` is the explicit way to ask for history.
func (c *client) offer(event json.RawMessage) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.attached || c.closed || !matchesFilter(c.filter, event) {
		return
	}
	select {
	case c.queue <- event:
	default:
		c.dropped++
	}
}

// send writes one envelope. A write failure shuts the client down, and
// nothing else: the host and every other client carry on.
func (c *client) send(msgType string, data any) error {
	raw, err := encodeData(data)
	if err != nil {
		return err
	}
	line, err := json.Marshal(Envelope{Type: msgType, Data: raw})
	if err != nil {
		return err
	}
	line = append(line, '\n')

	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return errors.New("ipc: connection is closed")
	}
	c.mu.Unlock()

	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_, err = c.conn.Write(line)
	return err
}

// matchesFilter applies `--asset`. An empty filter matches everything; an
// event without an asset field cannot match a named filter.
func matchesFilter(filter string, event json.RawMessage) bool {
	if filter == "" {
		return true
	}
	var probe struct {
		Asset string `json:"asset"`
	}
	if err := json.Unmarshal(event, &probe); err != nil {
		return false
	}
	return probe.Asset == filter
}

// Client is the `jms attach` side.
type Client struct {
	Role        Role
	AssetFilter string
	Last        int
	// OnEvent receives broadcast events.
	OnEvent func(event json.RawMessage)
}

// Dial connects to a host endpoint.
//
// The returned Conn runs a reader goroutine that feeds OnEvent, answers
// keepalive pings and dispatches exec results.
func Dial(ctx context.Context, addr string, c *Client) (*Conn, error) {
	if c == nil {
		return nil, errors.New("ipc: nil client")
	}
	if addr == "" {
		return nil, errors.New("ipc: empty endpoint address")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	conn, err := dialEndpoint(ctx, addr)
	if err != nil {
		return nil, err
	}
	return newConn(conn, c)
}

// newConn performs the hello handshake over an established connection.
func newConn(conn io.ReadWriteCloser, c *Client) (*Conn, error) {
	out := &Conn{
		conn:         conn,
		PingInterval: DefaultPingInterval,
		events:       c.OnEvent,
		done:         make(chan struct{}),
		pending:      make(map[string]chan ExecResult),
	}
	if err := out.send(MsgHello, Hello{
		Type:        MsgHello,
		Role:        c.Role,
		AssetFilter: c.AssetFilter,
		Last:        c.Last,
	}); err != nil {
		_ = conn.Close()
		return nil, err
	}
	go out.readLoop()
	go out.pingLoop()
	return out, nil
}

// Conn is one attached connection.
type Conn struct {
	// PingInterval keeps the channel alive.
	PingInterval time.Duration

	conn   io.ReadWriteCloser
	events func(json.RawMessage)

	writeMu sync.Mutex
	mu      sync.Mutex
	closed  bool
	seq     int
	pending map[string]chan ExecResult
	done    chan struct{}
}

// Exec asks the host to run a command.
//
// A host-reported failure comes back as both a non-nil error and the result
// it arrived in, so a caller can still inspect Busy and ExitCode (「IPC 宿主」).
func (c *Conn) Exec(ctx context.Context, req ExecRequest) (ExecResult, error) {
	if c == nil {
		return ExecResult{}, errors.New("ipc: nil connection")
	}
	if err := ctx.Err(); err != nil {
		return ExecResult{}, err
	}
	key := c.nextKey()
	ch := make(chan ExecResult, 1)

	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return ExecResult{}, errors.New("ipc: connection is closed")
	}
	c.pending[key] = ch
	c.mu.Unlock()

	if err := c.send(MsgExec, execWire{Key: key, ExecRequest: req}); err != nil {
		c.forget(key)
		return ExecResult{}, err
	}

	select {
	case res := <-ch:
		if res.Error != "" {
			return res, errors.New(res.Error)
		}
		return res, nil
	case <-ctx.Done():
		c.forget(key)
		return ExecResult{}, ctx.Err()
	case <-c.done:
		c.forget(key)
		return ExecResult{}, errors.New("ipc: connection is closed")
	}
}

func (c *Conn) nextKey() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.seq++
	return fmt.Sprintf("e%d", c.seq)
}

func (c *Conn) forget(key string) {
	c.mu.Lock()
	delete(c.pending, key)
	c.mu.Unlock()
}

func (c *Conn) send(msgType string, data any) error {
	raw, err := encodeData(data)
	if err != nil {
		return err
	}
	line, err := json.Marshal(Envelope{Type: msgType, Data: raw})
	if err != nil {
		return err
	}
	line = append(line, '\n')

	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_, err = c.conn.Write(line)
	return err
}

func (c *Conn) readLoop() {
	defer close(c.done)
	scanner := newLineScanner(c.conn)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(strings.TrimSpace(string(line))) == 0 {
			continue
		}
		var env Envelope
		if err := json.Unmarshal(line, &env); err != nil {
			continue
		}
		switch env.Type {
		case MsgEvent:
			if c.events != nil {
				c.events(append(json.RawMessage(nil), env.Data...))
			}
		case MsgPing:
			_ = c.send(MsgPong, nil)
		case MsgPong:
			// Keepalive acknowledgement.
		case MsgExecResult:
			c.dispatch(env.Data)
		}
	}
	c.failPending()
}

func (c *Conn) dispatch(data json.RawMessage) {
	var keyed execWire
	if err := json.Unmarshal(data, &keyed); err != nil {
		return
	}
	c.mu.Lock()
	ch := c.pending[keyed.Key]
	delete(c.pending, keyed.Key)
	c.mu.Unlock()
	if ch == nil {
		return
	}
	ch <- keyed.Result
}

func (c *Conn) failPending() {
	c.mu.Lock()
	c.closed = true
	pending := c.pending
	c.pending = make(map[string]chan ExecResult)
	c.mu.Unlock()
	for _, ch := range pending {
		ch <- ExecResult{Error: "ipc: host closed the connection"}
	}
}

func (c *Conn) pingLoop() {
	interval := c.PingInterval
	if interval <= 0 {
		interval = DefaultPingInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-c.done:
			return
		case <-ticker.C:
			if err := c.send(MsgPing, nil); err != nil {
				return
			}
		}
	}
}

// Close closes the connection.
//
// It tolerates a nil receiver and a zero-value Conn: Close is deferred by
// every caller, so panicking here would mask whatever the command was
// actually reporting. A zero-value Conn has no transport to close, which is
// a no-op rather than an error.
func (c *Conn) Close() error {
	if c == nil || c.conn == nil {
		return nil
	}
	return c.conn.Close()
}

// EndpointName renders the per-user endpoint address.
//
// Windows: \\.\pipe\jms-<hash>; Unix: a 0600 socket under the config dir.
//
// The hash is derived from the config directory so that two installations
// with separate config directories never collide, and the name stays stable
// across restarts: the host is addressed, not discovered.
func EndpointName(configDir string) string {
	return endpointName(configDir)
}

// endpointDir canonicalizes the configuration directory that names the
// endpoint.
//
// An empty directory means the OS temp directory, so the host and the client
// agree even on the error path where the config directory could not be
// resolved. The spelling is lower-cased on Windows because the same directory
// can be written with either case there; on Unix the spelling is significant.
func endpointDir(configDir string) string {
	dir := strings.TrimSpace(configDir)
	if dir == "" {
		dir = os.TempDir()
	}
	dir = filepath.Clean(dir)
	if runtime.GOOS == "windows" {
		dir = strings.ToLower(dir)
	}
	return dir
}

// endpointHash is a short, filesystem-safe digest of the endpoint directory.
func endpointHash(configDir string) string {
	sum := sha256.Sum256([]byte(endpointDir(configDir)))
	return fmt.Sprintf("%08x", binary.BigEndian.Uint32(sum[:4]))
}

// newLineScanner reads newline-terminated messages, tolerating CRLF and
// bounding a single message's size.
func newLineScanner(r io.Reader) *bufio.Scanner {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, lineBuffer), maxLineBytes)
	return scanner
}

// encodeData renders one envelope payload.
func encodeData(data any) (json.RawMessage, error) {
	if data == nil {
		return nil, nil
	}
	if raw, ok := data.(json.RawMessage); ok {
		return raw, nil
	}
	return json.Marshal(data)
}
