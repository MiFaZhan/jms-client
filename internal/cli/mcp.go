package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/MiFaZhan/jms-client/internal/connpool"
	"github.com/MiFaZhan/jms-client/internal/ipc"
	"github.com/MiFaZhan/jms-client/internal/mcpserver"
	"github.com/MiFaZhan/jms-client/internal/obs"
	"github.com/MiFaZhan/jms-client/internal/transport"
)

// newMCPCommand builds `jms mcp`, the stdio MCP server host.
func newMCPCommand(deps *Deps) *cobra.Command {
	var printConfig bool
	cmd := &cobra.Command{
		Use:   "mcp",
		Short: "Run the stdio MCP server",
		Long: `Run the MCP server over stdio, and host the local observation channel.

Every tool call goes through this process's connection pool, so a second call
for the same asset reuses the live session instead of logging in again. The
host also accepts ` + "`jms attach`" + ` clients, which then
watch the same event stream and can run commands on the same pooled
connections.

Diagnostics go to stderr: stdout is the MCP protocol channel.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if printConfig {
				_, err := fmt.Fprint(deps.Out, mcpserver.PrintConfig())
				return err
			}
			return runMCP(cmd, *deps)
		},
	}
	cmd.Flags().BoolVar(&printConfig, "print-config", false,
		"Print client configuration snippets (contains no credential)")
	return cmd
}

// hostHandle owns the embedded IPC host so the caller can shut it down.
type hostHandle struct {
	host *ipc.Host
}

// Close shuts the host down.
func (h *hostHandle) Close() error {
	if h == nil || h.host == nil {
		return nil
	}
	return h.host.Close()
}

// runMCP starts the IPC host and then serves the MCP protocol.
//
// The host is embedded in this process (DESIGN.md「IPC 宿主」, the v1 shape), so an
// attach client sees this server's pool and events. A host that cannot listen
// is a warning rather than a failure: observation is a convenience, and
// refusing to serve MCP because a pipe is already taken would break the
// primary job. The host is closed when the server returns, so a client
// disconnecting does not leak the endpoint.
func runMCP(cmd *cobra.Command, deps Deps) error {
	ctx := commandContext(cmd)

	host, err := startHost(ctx, deps)
	if err != nil {
		fmt.Fprintf(deps.Err, "Warning: observation channel unavailable: %v\n", err)
	} else if host != nil {
		defer host.Close()
	}

	return deps.Runtime.ServeMCP(ctx, mcpOptions(deps))
}

// startHost wires the IPC host to the event bus and the terminal pool.
//
// It returns (nil, nil) when there is no bus to observe: a host with nothing
// to publish would accept clients and then show them nothing.
func startHost(ctx context.Context, deps Deps) (*hostHandle, error) {
	if deps.Runtime == nil || deps.Runtime.Bus == nil {
		return nil, nil
	}
	dir, err := stateDir(deps)
	if err != nil {
		return nil, err
	}
	_ = dir

	bus := deps.Runtime.Bus
	host := &ipc.Host{
		Subscribe: func(sink func(event json.RawMessage)) {
			bus.Subscribe(obs.SubscriberFunc(func(e obs.Event) {
				if raw, err := json.Marshal(e); err == nil {
					sink(raw)
				}
			}))
		},
		Exec: func(callCtx context.Context, req ipc.ExecRequest) ipc.ExecResult {
			return runHostExec(callCtx, deps, req)
		},
	}

	if _, err := host.Listen(ctx); err != nil {
		return nil, err
	}
	return &hostHandle{host: host}, nil
}

// runHostExec runs one operator command through the shared pool.
//
// The pool serializes per terminal, so this and an AI tool call on the same
// asset queue rather than interleave (DESIGN.md「IPC 宿主」).
func runHostExec(ctx context.Context, deps Deps, req ipc.ExecRequest) ipc.ExecResult {
	if deps.Runtime == nil || deps.Runtime.Exec == nil {
		return ipc.ExecResult{Error: "no terminal pool is available in this process"}
	}
	t, err := parseCommandTarget(targetSpec(req.Asset, req.Server))
	if err != nil {
		return ipc.ExecResult{Error: err.Error()}
	}
	conn, err := resolveCommandTarget(nil, deps, t)
	if err != nil {
		return ipc.ExecResult{Error: err.Error()}
	}
	session, _, err := conn.connect(ctx, "")
	if err != nil {
		return ipc.ExecResult{Error: err.Error()}
	}
	// MCP automation defaults to the long-lived WebSocket transport, matching
	// jms exec and login: the WS endpoint rides the same HTTP entry as the Web
	// terminal, so it works wherever the bastion's web UI is reachable. The
	// pooled connection is kept alive with application-level PING frames and
	// reused across tool calls in this process.
	res, err := deps.Runtime.Exec(ctx, conn.srv, session, t.Asset, req.Command,
		connpool.ExecOptions{Account: req.Account, Backend: transport.BackendWS})
	if err != nil {
		return ipc.ExecResult{Error: err.Error()}
	}
	return ipc.ExecResult{Output: res.Output, ExitCode: res.ExitCode}
}

// targetSpec renders the <asset>[@<server>] form the parser expects.
func targetSpec(asset, server string) string {
	if server == "" {
		return asset
	}
	return asset + "@" + server
}

// mcpOptions builds the server options from the CLI dependencies.
//
// The pools and bus come from Runtime so the server shares this process's
// live sessions and publishes into the same audit stream `jms tail` follows.
func mcpOptions(deps Deps) mcpserver.Options {
	return mcpserver.Options{
		ConfigPath: deps.ConfigPath,
		Actor:      mcpActor(),
		Bus:        deps.Runtime.Bus,
		Sessions:   deps.Runtime.Sessions,
		Terminals:  deps.Runtime.Terminals,
		Creds:      deps.Creds,
		OTPPrompt:  deps.PromptOTP,
		Debug:      mcpserver.DebugEnabled(),
		In:         deps.Stdin,
		Out:        deps.Out,
	}
}

// mcpActor names this process in the audit stream, so a reader of the log can
// tell which client ran what (DESIGN.md「审计日志」).
func mcpActor() string {
	if v := os.Getenv("JMS_ACTOR"); v != "" {
		return v
	}
	return "mcp"
}
