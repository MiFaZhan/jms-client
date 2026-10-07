package cli

import (
	"context"
	"path/filepath"
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
// (DESIGN.md §4).
type Runtime struct {
	// Sessions holds authenticated sessions, keyed by server alias.
	Sessions *connpool.SessionPool
	// Terminals holds live terminals and serializes execution per terminal.
	Terminals *connpool.TerminalPool
	// Bus receives audit events; nil disables publication.
	Bus *obs.Bus
	// AuditDir is where the per-pid JSONL files live; `tail` and `log show`
	// read it, `exec` and `mcp` write through the Bus.
	AuditDir string

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

// defaultTransfer resolves the asset and runs one transfer through a fresh
// engine.
//
// A transfer is a short-lived connection by design (DESIGN.md §5: SFTP
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

// auditEnabled reports whether the audit writer should be created, honouring
// JMS_AUDIT=off (DESIGN.md §12.3).
func auditEnabled() bool { return true }

// nowOr returns the injected clock, or time.Now.
func (d Deps) nowOr() func() time.Time {
	if d.Now == nil {
		return time.Now
	}
	return d.Now
}
