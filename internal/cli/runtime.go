package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/MiFaZhan/jms-client/internal/auth"
	"github.com/MiFaZhan/jms-client/internal/config"
	"github.com/MiFaZhan/jms-client/internal/connpool"
	"github.com/MiFaZhan/jms-client/internal/ipc"
	"github.com/MiFaZhan/jms-client/internal/mcpserver"
	"github.com/MiFaZhan/jms-client/internal/obs"
	"github.com/MiFaZhan/jms-client/internal/transport"
	"github.com/MiFaZhan/jms-client/internal/xfer"
)

// Runtime carries the long-lived collaborators that the pool-backed
// commands share, and the seams that make those commands testable without a
// live JumpServer.
//
// Every field is a seam rather than a concrete pool call, because the pool
// methods live on concrete structs and a test cannot replace a method. A nil
// seam means "use the real implementation", matching the rest of Deps.
//
// One Runtime is built per process, so the pools inside it survive across
// commands. That is the whole point of the rewrite: a second `jms exec` on
// the same asset reuses the live connection instead of logging in again
// (DESIGN.md「连接池」).
type Runtime struct {
	// Sessions holds authenticated sessions, keyed by server alias.
	Sessions *connpool.SessionPool
	// Terminals holds live terminals and serializes execution per terminal.
	Terminals *connpool.TerminalPool
	// Bus receives audit events; nil disables publication.
	Bus *obs.Bus
	// Audit is the sink behind Bus; nil means auditing is off or failed.
	Audit *obs.AuditSink
	// AuditErr records why the audit sink could not be prepared, when it
	// could not. The bus stays usable so IPC observation still works.
	AuditErr error
	// AuditDir is where the per-pid JSONL files live; `tail` and `log show`
	// read it, `exec` and `mcp` write through the Bus.
	AuditDir string

	// Actor names this process in the audit stream ("cli", "mcp", or the
	// JMS_ACTOR override), so a reader can tell which surface ran what.
	Actor string

	// ReaperInterval and IdleTTL drive the background idle reaper. Zero
	// means the defaults from DESIGN.md「数值基线」.
	ReaperInterval time.Duration
	IdleTTL        time.Duration

	// closeOnce makes Close idempotent: the command path and the long-lived
	// host both close the runtime, and neither knows about the other.
	closeOnce sync.Once
	closeErr  error

	// Exec runs one command through the shared terminal pool. Nil means
	// Runtime.Terminals.Exec.
	Exec ExecFunc
	// Transfer performs one file transfer. Nil means a fresh xfer.Engine
	// bound to the resolved asset.
	Transfer TransferFunc
	// Attach connects to a running host for `jms attach`. Nil means
	// ipc.Dial.
	Attach AttachFunc
	// ServeMCP runs the stdio MCP server. Nil means mcpserver.New(opts).Serve.
	ServeMCP ServeMCPFunc
	// Bridge runs the stdio relay behind `rsync -e`. Nil means
	// xfer.RunBridge.
	Bridge BridgeFunc
}

// BridgeFunc runs the ssh-pipe relay and returns the remote exit code.
//
// It matches xfer.RunBridge so the production default is that function.
// stdout carries the caller's protocol bytes, so nothing may write
// diagnostics there.
type BridgeFunc func(ctx context.Context, args []string, b xfer.Bridge) (int, error)

// ExecFunc runs one command on an asset through the shared pool.
//
// It matches connpool.TerminalPool.Exec so the production default is that
// method itself.
type ExecFunc func(ctx context.Context, srv *config.ServerConfig, sess *auth.Session,
	assetName, command string, opts connpool.ExecOptions) (transport.Result, error)

// TransferRequest is one file transfer, already resolved to a session.
type TransferRequest struct {
	Session *auth.Session
	Asset   string
	Account string
	// Task carries the direction, the local and remote paths and the
	// verification settings.
	Task xfer.Task
	// Endpoint is the address kind the session is bound to, for the audit
	// record.
	Endpoint string
}

// TransferFunc performs one transfer.
type TransferFunc func(ctx context.Context, req TransferRequest) (xfer.Result, error)

// AttachFunc connects to a running host.
type AttachFunc func(ctx context.Context, addr string, client *ipc.Client) (*ipc.Conn, error)

// ServeMCPFunc runs the stdio MCP server until the client disconnects.
type ServeMCPFunc func(ctx context.Context, opts mcpserver.Options) error

// withRuntimeDefaults fills in the Runtime itself and every nil seam inside
// it.
//
// The pools are created here rather than at package scope so that an
// injected ConfigPath (or JMS_CONFIG) also relocates the audit directory,
// instead of state leaking into the platform default.
//
// This is also where the observability chain is completed: the event bus and
// its audit writer are built here and the pool transitions are published into
// them, so `jms tail` and `jms attach` see real traffic instead of an empty
// stream (DESIGN.md「可观测性」). A failure to open the audit file is not fatal:
// the bus still works for IPC observation, and the error is remembered for
// the caller to surface.
func (d Deps) withRuntimeDefaults() Deps {
	if d.Runtime == nil {
		d.Runtime = &Runtime{}
	}
	rt := d.Runtime

	if rt.Sessions == nil {
		rt.Sessions = connpool.NewSessionPool()
	}
	if rt.Terminals == nil {
		rt.Terminals = connpool.NewTerminalPool()
	}
	if rt.Exec == nil {
		rt.Exec = rt.Terminals.Exec
	}
	if rt.AuditDir == "" {
		rt.AuditDir = defaultAuditDir(d)
	}
	if rt.Actor == "" {
		rt.Actor = defaultActor()
	}
	if rt.ReaperInterval == 0 {
		rt.ReaperInterval = connpool.ReapInterval
	}
	if rt.IdleTTL == 0 {
		rt.IdleTTL = connpool.IdleTTL
	}
	if rt.Bus == nil {
		// The bus is always built: even with auditing off it carries events to
		// `jms attach`. Whether a file is written is the sink's decision, and
		// it reports JMS_AUDIT=off by returning a nil sink.
		bus, sink, err := obs.NewBusWithLazyAudit(obs.AuditOptions{Dir: rt.AuditDir})
		rt.Bus = bus
		rt.Audit = sink
		rt.AuditErr = err
	}
	if rt.Bus != nil {
		// The pool's own events are the ones `tail` shows as 冷连/命中/回收.
		// Wiring them here keeps the pools free of any dependency on the
		// observability layer.
		rt.Terminals.Notify = func(ev connpool.Event) { rt.Bus.Publish(rt.poolEvent(ev)) }
		rt.Sessions.Notify = func(ev connpool.Event) { rt.Bus.Publish(rt.sessionEvent(ev)) }
	}
	if rt.Transfer == nil {
		rt.Transfer = defaultTransfer
	}
	if rt.Attach == nil {
		rt.Attach = func(ctx context.Context, addr string, client *ipc.Client) (*ipc.Conn, error) {
			return ipc.Dial(ctx, addr, client)
		}
	}
	if rt.ServeMCP == nil {
		rt.ServeMCP = func(ctx context.Context, opts mcpserver.Options) error {
			return mcpserver.New(opts).Serve(ctx)
		}
	}
	if rt.Bridge == nil {
		rt.Bridge = xfer.RunBridge
	}
	return d
}

// defaultAuditDir places the audit directory beside the configuration file.
func defaultAuditDir(d Deps) string {
	dir, err := stateDir(d)
	if err != nil {
		return "audit"
	}
	return filepath.Join(dir, "audit")
}

// defaultActor names the CLI in the audit stream.
//
// The surface is part of the name because the audit log is read to tell which
// surface ran what: `jms mcp` resolves its own actor before it publishes
// anything, so an event never inherits the CLI's name just because the
// Runtime was built first (DESIGN.md「审计日志」).
func defaultActor() string { return actorFor("cli") }

// actorFor resolves the audit actor for one surface.
//
// JMS_ACTOR wins, which is how several clients on one machine stay
// distinguishable in a shared audit file; otherwise the surface names itself.
func actorFor(surface string) string {
	if v := strings.TrimSpace(os.Getenv(actorEnv)); v != "" {
		return v
	}
	return surface
}

// actorEnv is the environment variable that overrides the audit actor.
const actorEnv = "JMS_ACTOR"

// poolEvent renders one terminal-pool transition as an audit event.
func (rt *Runtime) poolEvent(ev connpool.Event) obs.Event {
	e := obs.Event{
		TS:     time.Now(),
		Actor:  rt.Actor,
		Server: ev.Server,
		Asset:  ev.Asset,
		Pool:   string(ev.Kind),
		IdleMS: ev.Idle.Milliseconds(),
		Reason: ev.Reason,
	}
	switch ev.Kind {
	case connpool.KindCold:
		e.Kind = obs.KindPoolCold
	case connpool.KindHit:
		e.Kind = obs.KindPoolHit
	case connpool.KindEvict:
		e.Kind = obs.KindPoolEvict
	case connpool.KindReap:
		e.Kind = obs.KindPoolReap
	default:
		e.Kind = obs.KindPoolHit
	}
	return e
}

// sessionEvent renders one session-pool transition as an audit event.
func (rt *Runtime) sessionEvent(ev connpool.Event) obs.Event {
	kind := obs.KindSessionLogin
	if ev.Kind == connpool.KindRelogin {
		kind = obs.KindSessionRelogin
	}
	return obs.Event{
		TS:     time.Now(),
		Actor:  rt.Actor,
		Kind:   kind,
		Server: ev.Server,
	}
}

// StartReaper runs the idle-terminal reaper in the background until ctx is
// cancelled.
//
// It is called by the long-lived commands (`mcp`), because that is where
// pooled terminals actually accumulate: a one-shot command's process exit is
// its own cleanup. Calling it from a short-lived command would be harmless
// but pointless.
func (rt *Runtime) StartReaper(ctx context.Context) {
	if rt == nil || rt.Terminals == nil {
		return
	}
	go rt.Terminals.RunReaper(ctx, rt.ReaperInterval, rt.IdleTTL)
}

// Close flushes the audit log and reports any failure.
//
// It waits for queued events to be delivered first: delivery is asynchronous
// by design, so a short-lived command would otherwise let its own process
// exit drop the record of what it did. A stuck subscriber is bounded by
// drainTimeout rather than hanging the process.
//
// It is idempotent, and safe on a nil Runtime: the command path and the
// long-lived host both call it, and one of them may run twice.
func (rt *Runtime) Close() error {
	if rt == nil {
		return nil
	}
	rt.closeOnce.Do(func() {
		if rt.Bus != nil && !rt.Bus.Drain(drainTimeout) {
			// Losing an audit line is worth saying out loud, but not worth
			// failing a command that already succeeded.
			rt.closeErr = fmt.Errorf("audit events were still queued after %s", drainTimeout)
		}
		if err := rt.Audit.Close(); err != nil && rt.closeErr == nil {
			rt.closeErr = err
		}
	})
	return rt.closeErr
}

// drainTimeout bounds the wait for queued audit events at exit.
const drainTimeout = 2 * time.Second

// defaultTransfer resolves the asset and runs one transfer through a fresh
// engine.
//
// A transfer is a short-lived connection by design (DESIGN.md「总体架构」: SFTP
// connections are not pooled), so it is built per call rather than cached.
// req.Asset carries the asset NAME, not a resolved Info: the engine resolves
// it on demand through Engine.AssetName (Engine.Asset is assets.Info and
// would need a full Resolve here). Dropping the name — as an earlier version
// of this function did — made every SFTP transfer fail with "no asset
// resolved".
func defaultTransfer(ctx context.Context, req TransferRequest) (xfer.Result, error) {
	engine := &xfer.Engine{Session: req.Session, AssetName: req.Asset, Account: req.Account}
	return engine.Run(ctx, req.Task)
}

// nowOr returns the injected clock, or time.Now.
func (d Deps) nowOr() func() time.Time {
	if d.Now == nil {
		return time.Now
	}
	return d.Now
}
