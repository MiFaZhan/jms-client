// Package obs is the local observability layer: a structured event bus that
// feeds an append-only JSONL audit log and a local IPC host.
//
// It exists because MCP stdio is a private channel between the AI client and
// jms: without it there is no independent, real-time, inspectable view of
// what the AI executed (DESIGN.md「可观测性」).
package obs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/MiFaZhan/jms-client/internal/config"
)

// Kind enumerates the audit event types (DESIGN.md「审计日志」).
type Kind string

// Event kinds.
const (
	KindExecStart      Kind = "exec.start"
	KindExecEnd        Kind = "exec.end"
	KindPoolCold       Kind = "pool.cold"
	KindPoolHit        Kind = "pool.hit"
	KindPoolEvict      Kind = "pool.evict"
	KindPoolReap       Kind = "pool.reap"
	KindSessionLogin   Kind = "session.login"
	KindSessionRelogin Kind = "session.relogin"
	KindEndpointSelect Kind = "endpoint.select"
	KindError          Kind = "error"
)

// Environment switches (DESIGN.md「审计日志」).
const (
	// EnvAudit disables auditing when set to "off".
	EnvAudit = "JMS_AUDIT"
	// EnvAuditDir relocates the audit directory.
	EnvAuditDir = "JMS_AUDIT_DIR"
	// EnvAuditFull stores the untruncated output preview when set to "1".
	EnvAuditFull = "JMS_AUDIT_FULL"
	// EnvAuditPreviewBytes tunes the preview truncation limit.
	EnvAuditPreviewBytes = "JMS_AUDIT_PREVIEW_BYTES"
)

// Defaults.
const (
	// DefaultRingSize bounds the in-memory replay buffer.
	DefaultRingSize = 512
	// DefaultPreviewBytes truncates a captured output preview.
	DefaultPreviewBytes = 4096
	// subscriberBuffer is how many events a subscriber may fall behind
	// before its deliveries are dropped (DESIGN.md「可观测性」).
	subscriberBuffer = 64
)

// Event is one audit record. The JSON shape is the on-disk contract.
type Event struct {
	TS          time.Time `json:"ts"`
	PID         int       `json:"pid"`
	Actor       string    `json:"actor,omitempty"`
	Kind        Kind      `json:"kind"`
	Server      string    `json:"server,omitempty"`
	Asset       string    `json:"asset,omitempty"`
	Backend     string    `json:"backend,omitempty"`
	Endpoint    string    `json:"endpoint,omitempty"`
	Command     string    `json:"command,omitempty"`
	ExitCode    *int      `json:"exit_code,omitempty"`
	DurationMS  int64     `json:"duration_ms,omitempty"`
	OutputBytes int       `json:"output_bytes,omitempty"`
	Preview     string    `json:"preview,omitempty"`
	Pool        string    `json:"pool,omitempty"`
	IdleMS      int64     `json:"idle_ms,omitempty"`
	Reason      string    `json:"reason,omitempty"`
	Error       string    `json:"error,omitempty"`
}

// Subscriber receives events. A slow subscriber must never block the
// publisher (DESIGN.md「可观测性」).
type Subscriber interface {
	// Publish delivers one event. It must not block indefinitely.
	Publish(Event)
}

// SubscriberFunc adapts a function to Subscriber.
type SubscriberFunc func(Event)

// Publish calls f.
func (f SubscriberFunc) Publish(e Event) { f(e) }

// subscriberSlot is one registered subscriber: a bounded queue plus the
// goroutine that drains it into the Subscriber.
//
// The queue is what keeps Bus.Publish non-blocking. Calling Subscriber.Publish
// straight from the publisher would hand a stuck observer the power to stall
// the execution path, and a goroutine per event would trade that for
// unbounded goroutine growth.
type subscriberSlot struct {
	sub Subscriber
	ch  chan Event
	// done is closed by drain once the queue is empty and the goroutine is
	// about to exit, so Drain can tell "nothing queued" from "nothing left to
	// deliver".
	done chan struct{}
	// pending counts events accepted but not yet handed to sub. Drain waits
	// for it to reach zero, which is the only correct completion signal: an
	// empty queue alone cannot distinguish an in-flight delivery.
	pending atomic.Int64
}

func newSubscriberSlot(s Subscriber) *subscriberSlot {
	slot := &subscriberSlot{sub: s, ch: make(chan Event, subscriberBuffer), done: make(chan struct{})}
	go slot.drain()
	return slot
}

// drain delivers queued events in order until the queue is closed.
//
// A Subscriber.Publish that never returns parks this goroutine; the slot is
// already unreachable from the bus at that point, so the only cost is the
// stuck goroutine the subscriber itself created.
func (s *subscriberSlot) drain() {
	defer close(s.done)
	for e := range s.ch {
		s.sub.Publish(e)
		s.pending.Add(-1)
	}
}

// Bus is a non-blocking event bus with a bounded replay ring.
//
// A nil *Bus is usable and discards everything: callers carry an optional bus
// and "nil disables publication" is part of their contract (mcpserver.Options,
// cli.Runtime).
type Bus struct {
	mu      sync.RWMutex
	slots   []*subscriberSlot
	ring    []Event
	next    int
	filled  bool
	dropped int64
}

// NewBus returns an empty bus with a ring of size ringSize (0 means
// DefaultRingSize).
func NewBus(ringSize int) *Bus {
	if ringSize <= 0 {
		ringSize = DefaultRingSize
	}
	return &Bus{ring: make([]Event, ringSize)}
}

// Subscribe registers a subscriber.
//
// Every call registers a delivery stream, so subscribing the same value twice
// delivers twice. Subscribers are identified for Unsubscribe by their
// interface value (see Unsubscribe).
//
// A delivery goroutine drains each registration until Unsubscribe removes it.
// A Bus is therefore process-scoped, like the pools it feeds: a caller that
// registers a short-lived subscriber must unsubscribe it to release that
// goroutine.
func (b *Bus) Subscribe(s Subscriber) {
	if b == nil || s == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.slots = append(b.slots, newSubscriberSlot(s))
}

// Unsubscribe removes every registration of s.
//
// Events already queued for s are still delivered: they were published before
// the unsubscribe. No event published after Unsubscribe returns reaches s.
//
// Subscribers are compared by their interface value: pointer-shaped
// subscribers are matched by identity. A SubscriberFunc is matched by its
// code pointer, because Go panics when an interface holding a function type
// is compared — two closures built from the same function literal are
// indistinguishable, so register a pointer-typed subscriber when that
// matters.
func (b *Bus) Unsubscribe(s Subscriber) {
	if b == nil || s == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	kept := b.slots[:0]
	for _, slot := range b.slots {
		if sameSubscriber(slot.sub, s) {
			// Closing the queue lets the drain goroutine finish what it holds
			// and then exit. Only Publish sends on the queue and it holds
			// b.mu, so no send races this close.
			close(slot.ch)
			continue
		}
		kept = append(kept, slot)
	}
	// Nil the vacated tail so the removed slots are not kept alive by the
	// shared backing array.
	for i := len(kept); i < len(b.slots); i++ {
		b.slots[i] = nil
	}
	b.slots = kept
}

// sameSubscriber reports whether two Subscriber values denote the same
// registration target.
func sameSubscriber(a, b Subscriber) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	ta, tb := reflect.TypeOf(a), reflect.TypeOf(b)
	if ta != tb {
		return false
	}
	if ta.Comparable() {
		return a == b
	}
	if ta.Kind() == reflect.Func {
		return reflect.ValueOf(a).Pointer() == reflect.ValueOf(b).Pointer()
	}
	return reflect.DeepEqual(a, b)
}

// Publish delivers e to every subscriber and records it in the ring.
//
// A subscriber that blocks is skipped and counted: the execution path must
// never stall on a slow observer. Delivery is asynchronous, so a subscriber
// may observe an event after Publish has returned.
func (b *Bus) Publish(e Event) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.record(e)
	for _, slot := range b.slots {
		// The count is taken before the send so Drain cannot observe the
		// gap between "queued" and "accounted for" and report completion
		// while an event is still on its way.
		slot.pending.Add(1)
		select {
		case slot.ch <- e:
		default:
			slot.pending.Add(-1)
			b.dropped++
		}
	}
}

// Drain waits until every queued event has been handed to its subscriber.
//
// It exists for the short-lived process: delivery is asynchronous by design,
// so a command that returns immediately after publishing would otherwise let
// os.Exit drop the very record of what it did. It reports false when the
// timeout expires first, which is the only outcome a stuck subscriber can
// produce; the caller decides whether that is worth reporting.
//
// A nil bus drains instantly: nothing was ever queued.
func (b *Bus) Drain(timeout time.Duration) bool {
	if b == nil {
		return true
	}
	deadline := time.Now().Add(timeout)
	for {
		if b.pendingDeliveries() == 0 {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		// The wait is a poll rather than a condition variable because the
		// counter is per-slot and Publish must stay lock-free on the hot
		// path. The interval only bounds how long a one-shot process waits at
		// exit, so it is deliberately short.
		time.Sleep(time.Millisecond)
	}
}

// pendingDeliveries sums the events accepted but not yet delivered.
func (b *Bus) pendingDeliveries() int64 {
	b.mu.RLock()
	defer b.mu.RUnlock()
	var total int64
	for _, slot := range b.slots {
		total += slot.pending.Load()
	}
	return total
}

// record appends e to the replay ring, overwriting the oldest entry once the
// ring is full. The caller holds b.mu.
//
// The zero Bus is usable, so a bus built without NewBus still replays the
// default number of events instead of panicking.
func (b *Bus) record(e Event) {
	if len(b.ring) == 0 {
		b.ring = make([]Event, DefaultRingSize)
	}
	b.ring[b.next] = e
	b.next++
	if b.next == len(b.ring) {
		b.next = 0
		b.filled = true
	}
}

// Last returns up to n most recent events, oldest first.
func (b *Bus) Last(n int) []Event {
	if b == nil || n <= 0 {
		return nil
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	count := b.next
	if b.filled {
		count = len(b.ring)
	}
	if n > count {
		n = count
	}
	if n == 0 {
		return nil
	}
	start := b.next - n
	if start < 0 {
		start += len(b.ring)
	}
	out := make([]Event, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, b.ring[(start+i)%len(b.ring)])
	}
	return out
}

// Dropped reports how many deliveries were skipped.
func (b *Bus) Dropped() int64 {
	if b == nil {
		return 0
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.dropped
}

// AuditWriter appends events as JSONL, one file per process.
//
// Per-pid files avoid the Windows lock contention of several MCP instances
// appending to one file (DESIGN.md「审计日志」).
type AuditWriter struct {
	mu           sync.Mutex
	dir          string
	path         string
	file         *os.File
	pid          int
	previewBytes int
	full         bool
	err          error
}

// AuditOptions tunes an AuditWriter.
type AuditOptions struct {
	// Dir is the audit directory; the empty value means <config_dir>/audit.
	Dir string
	// PreviewBytes truncates Preview; 0 means DefaultPreviewBytes.
	PreviewBytes int
	// Full disables truncation.
	Full bool
}

// NewAuditWriter opens the per-pid JSONL file.
//
// The directory and the file are created 0600/0700 on Unix.
func NewAuditWriter(opts AuditOptions) (*AuditWriter, error) {
	dir, err := auditDir(opts.Dir)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create audit directory %s: %w", dir, err)
	}
	// MkdirAll keeps the mode of a directory that already exists, so an
	// audit directory created by an earlier run (or by hand) would stay
	// world-readable and leak output previews (DESIGN.md「审计日志」).
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, fmt.Errorf("restrict audit directory %s: %w", dir, err)
	}
	pid := os.Getpid()
	path := filepath.Join(dir, fmt.Sprintf("audit-%s-%d.jsonl", time.Now().Format("20060102"), pid))
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open audit file %s: %w", path, err)
	}
	// O_CREATE does not tighten an existing file, and a file written by an
	// earlier run may be world-readable.
	if err := file.Chmod(0o600); err != nil {
		file.Close()
		return nil, fmt.Errorf("restrict audit file %s: %w", path, err)
	}
	preview := opts.PreviewBytes
	if preview <= 0 {
		preview = DefaultPreviewBytes
	}
	return &AuditWriter{
		dir:          dir,
		path:         path,
		file:         file,
		pid:          pid,
		previewBytes: preview,
		full:         opts.Full,
	}, nil
}

// auditDir resolves the directory that holds the per-pid files.
//
// An empty dir means <config_dir>/audit, so JMS_CONFIG (or an injected config
// path in the CLI) relocates the audit log together with the configuration.
func auditDir(dir string) (string, error) {
	if dir != "" {
		return dir, nil
	}
	base, err := config.ConfigDir()
	if err != nil {
		return "", fmt.Errorf("cannot locate the audit directory: %w", err)
	}
	return filepath.Join(base, "audit"), nil
}

// Publish implements Subscriber.
//
// The writer owns the per-pid file, so it stamps its own pid on every line: a
// record whose pid contradicts the file name would make `jms tail`'s
// per-file attribution wrong. A write failure cannot be reported to the
// publisher, so it is remembered and returned by Close.
func (w *AuditWriter) Publish(e Event) {
	if w == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file == nil || w.err != nil {
		return
	}
	e.PID = w.pid
	if !w.full {
		e.Preview = truncatePreview(e.Preview, w.previewBytes)
	}
	line, err := MarshalLine(e)
	if err != nil {
		w.err = fmt.Errorf("encode audit event %s: %w", e.Kind, err)
		return
	}
	line = append(line, '\n')
	if _, err := w.file.Write(line); err != nil {
		w.err = fmt.Errorf("write audit file %s: %w", w.path, err)
		return
	}
	// The write is unbuffered, so `jms tail` already sees the line. The sync
	// is for durability: an audit record for an executed command must not
	// vanish in a crash, which is the whole point of the log (DESIGN.md
	// 「审计日志」). Exec events are low-frequency, so the cost is not on a hot path.
	if err := w.file.Sync(); err != nil {
		w.err = fmt.Errorf("flush audit file %s: %w", w.path, err)
		return
	}
}

// Close flushes and closes the file.
//
// It is idempotent: every call reports the same outcome as the first one, and
// later calls never touch the closed file. A write failure recorded by
// Publish is returned here, because a Subscriber has nowhere else to report
// it. It is safe on a nil writer, which is what NewBusWithAudit returns when
// auditing is disabled.
func (w *AuditWriter) Close() error {
	if w == nil {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file != nil {
		// Writes are unbuffered and synced in Publish, so closing is the only
		// step left.
		if err := w.file.Close(); err != nil {
			w.err = errors.Join(w.err, fmt.Errorf("close audit file %s: %w", w.path, err))
		}
		w.file = nil
	}
	return w.err
}

// Path returns the file path.
//
// It returns "" for a nil writer (auditing disabled), so callers can print it
// without a nil check.
func (w *AuditWriter) Path() string {
	if w == nil {
		return ""
	}
	return w.path
}

// AuditSink is a Subscriber that opens its per-pid file on the first event.
//
// It exists for the CLI, where one Runtime is built for every invocation
// including read-only ones (`jms version`, `jms ls`): opening the file
// eagerly would leave an empty audit-*.jsonl behind for every command that
// never ran anything, and `jms tail` merges that whole directory. The
// directory is validated eagerly so a broken audit location is still
// reported before the command does its work, but no file is created until
// there is something to record.
type AuditSink struct {
	opts AuditOptions

	mu       sync.Mutex
	writer   *AuditWriter
	opened   bool
	closed   bool
	openErr  error
	closeErr error
}

// NewAuditSink validates the audit location and returns a sink that will
// open its file on first use.
//
// It honours the same environment switches as NewBusWithAudit: JMS_AUDIT=off
// disables auditing, JMS_AUDIT_DIR relocates the directory, JMS_AUDIT_FULL=1
// stores full output and JMS_AUDIT_PREVIEW_BYTES tunes the preview. It
// returns (nil, nil) when auditing is off.
func NewAuditSink(opts AuditOptions) (*AuditSink, error) {
	if strings.EqualFold(strings.TrimSpace(os.Getenv(EnvAudit)), "off") {
		return nil, nil
	}
	opts = resolveAuditOptions(opts)

	// Resolving the directory here catches a configuration that cannot be
	// located at all. Creating it is deferred to Prepare (or to the first
	// event): `jms version` builds the same Runtime as `jms exec`, and a
	// read-only command must not leave an audit directory behind.
	if _, err := auditDir(opts.Dir); err != nil {
		return nil, err
	}
	return &AuditSink{opts: opts}, nil
}

// Prepare creates the audit directory and verifies it is usable.
//
// The long-lived host calls it at startup so a misconfigured audit location
// is reported before any tool call runs, rather than being discovered when
// the first event tries to land. One-shot commands skip it: their failure is
// reported by Close, after the work they were asked to do.
func (s *AuditSink) Prepare() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.prepareLocked()
}

// resolveAuditOptions folds the environment switches into opts.
func resolveAuditOptions(opts AuditOptions) AuditOptions {
	if opts.Dir == "" {
		opts.Dir = strings.TrimSpace(os.Getenv(EnvAuditDir))
	}
	if !opts.Full && strings.TrimSpace(os.Getenv(EnvAuditFull)) == "1" {
		opts.Full = true
	}
	if opts.PreviewBytes <= 0 {
		// A malformed tunable falls back to the default: a typo must not
		// disable auditing or stop the process.
		if n, err := strconv.Atoi(strings.TrimSpace(os.Getenv(EnvAuditPreviewBytes))); err == nil && n > 0 {
			opts.PreviewBytes = n
		}
	}
	return opts
}

// Publish implements Subscriber, opening the file on the first event.
//
// A publish that arrives after Close is dropped: Close is terminal, and
// reopening here would leave a file no one closes.
func (s *AuditSink) Publish(e Event) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	if !s.opened {
		s.opened = true
		// The directory is created here as well as in Prepare, because a
		// one-shot command never calls Prepare: its first event is what
		// justifies the directory's existence.
		if err := s.prepareLocked(); err != nil {
			s.openErr = err
			return
		}
		writer, err := NewAuditWriter(s.opts)
		if err != nil {
			// A Subscriber cannot return the error, so it is remembered and
			// reported by Close. Losing the audit file must not stop the
			// command that is being audited.
			s.openErr = err
			return
		}
		s.writer = writer
	}
	if s.writer == nil {
		return
	}
	s.writer.Publish(e)
}

// prepareLocked creates the audit directory; the caller holds s.mu.
func (s *AuditSink) prepareLocked() error {
	dir, err := auditDir(s.opts.Dir)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create audit directory %s: %w", dir, err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("restrict audit directory %s: %w", dir, err)
	}
	return nil
}

// Close flushes and closes the file, reporting any failure.
//
// It is terminal and idempotent: every later call reports the first outcome,
// and a publish that arrives afterwards is dropped rather than reopening a
// file no one would close.
func (s *AuditSink) Close() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return s.closeErr
	}
	s.closed = true
	if s.openErr != nil {
		s.closeErr = s.openErr
		return s.closeErr
	}
	s.closeErr = s.writer.Close()
	return s.closeErr
}

// Path returns the file path, or "" when it was never opened.
func (s *AuditSink) Path() string {
	if s == nil {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.writer.Path()
}

// NewBusWithLazyAudit wires a bus to a sink that opens its file on first
// event, and returns the bus plus that sink.
//
// It is the CLI's variant of NewBusWithAudit: same environment switches and
// same non-blocking bus, but no empty audit file for a command that never
// recorded anything. The returned bus is never nil; the sink is nil when
// auditing is off, and the error reports an unusable audit location.
func NewBusWithLazyAudit(opts AuditOptions) (*Bus, *AuditSink, error) {
	bus := NewBus(0)
	sink, err := NewAuditSink(opts)
	if err != nil {
		return bus, nil, err
	}
	if sink != nil {
		bus.Subscribe(sink)
	}
	return bus, sink, nil
}

// NewBusWithAudit wires a bus to an audit writer when auditing is enabled.
//
// JMS_AUDIT=off disables it; JMS_AUDIT_DIR relocates the directory;
// JMS_AUDIT_FULL=1 stores full output; JMS_AUDIT_PREVIEW_BYTES tunes the
// preview. An explicit AuditOptions field wins over its environment switch,
// so an injected test or a caller that already resolved the config does not
// have to unset the environment.
//
// The returned bus is never nil, even when the writer is: a failed audit file
// still leaves IPC observation and `--last` replay working. On failure the
// error is returned together with the usable bus and a nil writer, leaving it
// to the caller to decide whether a missing audit log is fatal for the
// command it is about to run.
//
// ctx is accepted for symmetry with the rest of the runtime and is unused:
// opening a file is synchronous, and the writer's lifetime ends at Close
// rather than at cancellation.
func NewBusWithAudit(ctx context.Context, opts AuditOptions) (*Bus, *AuditWriter, error) {
	_ = ctx
	bus := NewBus(0)
	if strings.EqualFold(strings.TrimSpace(os.Getenv(EnvAudit)), "off") {
		return bus, nil, nil
	}
	opts = resolveAuditOptions(opts)
	writer, err := NewAuditWriter(opts)
	if err != nil {
		return bus, nil, err
	}
	bus.Subscribe(writer)
	return bus, writer, nil
}

// MarshalLine renders an event as one JSONL line, without a trailing
// newline.
//
// It is the single place the on-disk encoding is decided, so the writer and
// any reader agree by construction.
func MarshalLine(e Event) ([]byte, error) {
	return json.Marshal(e)
}

// truncatePreview cuts s to at most max bytes without splitting a UTF-8
// sequence.
//
// The preview is JSON on disk: a half rune would be encoded as U+FFFD, which
// silently corrupts the bytes a reader is trying to inspect (DESIGN.md「审计日志」).
func truncatePreview(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}
