// Package mcpserver exposes the pool to MCP clients over stdio.
//
// It is the rewrite's core change: every tool call goes through the shared
// connection pool instead of performing a full login/resolve/connect/
// teardown cycle (DESIGN.md §5).
package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/MiFaZhan/jms-client/internal/assets"
	"github.com/MiFaZhan/jms-client/internal/auth"
	"github.com/MiFaZhan/jms-client/internal/config"
	"github.com/MiFaZhan/jms-client/internal/connpool"
	"github.com/MiFaZhan/jms-client/internal/endpoint"
	"github.com/MiFaZhan/jms-client/internal/obs"
	"github.com/MiFaZhan/jms-client/internal/transport"
	"github.com/MiFaZhan/jms-client/internal/xfer"
)

// Tool names, kept identical to the Python implementation so existing
// prompts and skills keep working (DESIGN.md §5).
const (
	ToolLS           = "jms_ls"
	ToolResolveAsset = "jms_resolve_asset"
	ToolExec         = "jms_exec"
	ToolSFTPUpload   = "jms_sftp_upload"
	ToolSFTPDownload = "jms_sftp_download"
	ToolSFTPRelay    = "jms_sftp_relay"
	ToolConfigList   = "jms_config_list"
	ToolPoolStatus   = "jms_pool_status"
)

// EnvDebug enables the debug-only jms_pool_status tool.
const EnvDebug = "JMS_MCP_DEBUG"

// DefaultExecTimeout bounds one jms_exec call when the client omits
// timeout. It matches the Python tool's default (30s).
const DefaultExecTimeout = 30 * time.Second

// ServerName is the MCP implementation name.
const ServerName = "jms"

// SessionFunc returns an authenticated session for one server alias,
// together with the server entry it belongs to.
//
// It is the seam that keeps a tool call off the network in tests: the real
// implementation goes through the SessionPool, a test answers with a
// pre-built session.
type SessionFunc func(ctx context.Context, serverName string) (*auth.Session, *config.ServerConfig, error)

// ResolveFunc resolves an asset to its connection info.
//
// It matches connpool.TerminalPool.Resolve so the production default is the
// same call the pool makes.
type ResolveFunc func(ctx context.Context, sess *auth.Session, name, account, protocol string) (assets.Info, error)

// ExecFunc runs one command on an asset through the shared terminal pool.
//
// It matches connpool.TerminalPool.Exec so the production default is that
// method itself.
type ExecFunc func(ctx context.Context, srv *config.ServerConfig, sess *auth.Session,
	assetName, command string, opts connpool.ExecOptions) (transport.Result, error)

// TransferFunc performs one file transfer, one-sided or relayed.
type TransferFunc func(ctx context.Context, req TransferRequest) (xfer.Result, error)

// PublishFunc records one audit event.
type PublishFunc func(obs.Event)

// TransferRequest is one file transfer as the MCP tools describe it.
//
// A relay has both Remote sides set and no Local; a one-sided transfer sets
// Remote and Local and leaves Relay nil.
type TransferRequest struct {
	// Session and Asset describe the remote side of a one-sided transfer.
	Session *auth.Session
	Asset   string
	Account string
	// Local and Remote are the paths of a one-sided transfer.
	Local  string
	Remote string
	// Direction is upload or download for a one-sided transfer.
	Direction xfer.Direction
	// Relay describes a remote-to-remote transfer; nil for one-sided.
	Relay *RelayRequest
	// Verify asks for an md5 comparison after the transfer.
	Verify bool
	// Chunk and Parallel override the xfer defaults when non-zero.
	Chunk    int
	Parallel int
}

// RelayRequest is one remote-to-remote transfer.
type RelayRequest struct {
	SrcSession *auth.Session
	SrcAsset   string
	SrcAccount string
	SrcPath    string
	DstSession *auth.Session
	DstAsset   string
	DstAccount string
	DstPath    string
}

// Options configures the server.
type Options struct {
	// ConfigPath overrides the configuration file path.
	ConfigPath string
	// Actor names this client in the audit stream (e.g. "mcp:pi").
	Actor string
	// Bus receives audit events; nil disables publication.
	Bus *obs.Bus
	// Sessions and Terminals are the pools to serve from.
	Sessions  *connpool.SessionPool
	Terminals *connpool.TerminalPool
	// Debug registers jms_pool_status.
	Debug bool
	// In and Out are the stdio transport; nil means os.Stdin/os.Stdout.
	In  interface{ Read([]byte) (int, error) }
	Out interface{ Write([]byte) (int, error) }

	// Creds reads the stored password and TOTP secret. Nil means the
	// environment-then-OS-store chain.
	Creds config.CredentialStore
	// OTPPrompt asks for a one-time code when the server demands MFA and no
	// secret is stored. It is nil by default because an MCP client has no
	// terminal; the session then fails with an actionable message.
	OTPPrompt func() (string, error)

	// Session overrides how a server alias becomes a session. Nil means the
	// Sessions pool with credentials from Creds.
	Session SessionFunc
	// Probe checks address reachability during endpoint selection. Nil means
	// endpoint.TCPProbe; a test can inject a fake to avoid real dials.
	Probe endpoint.ProbeFunc
	// state reads the persisted last-good endpoint for a server alias.
	// Nil means a file store next to the configuration file.
	state endpoint.StateStore
	// Resolve overrides asset resolution. Nil means assets.Resolve on the
	// session's API client.
	Resolve ResolveFunc
	// Exec overrides command execution. Nil means Terminals.Exec.
	Exec ExecFunc
	// Transfer overrides file transfer. Nil means a fresh xfer engine.
	Transfer TransferFunc
	// Publish overrides audit publication. Nil means Bus.Publish.
	Publish PublishFunc

	// Stderr receives diagnostics. Nil means os.Stderr.
	//
	// It exists because stdio is the protocol channel: a log line on stdout
	// would corrupt the JSON-RPC stream (DESIGN.md §8).
	Stderr io.Writer
}

// Server is a stdio MCP server.
type Server struct {
	opts Options

	// stateOnce/stateStore lazily build the default endpoint state store.
	stateOnce  sync.Once
	stateStore endpoint.StateStore
}

// New returns a server for opts.
func New(opts Options) *Server { return &Server{opts: opts} }

// DebugEnabled reports whether JMS_MCP_DEBUG asks for the debug tool.
//
// Only an explicit "1"/"true" enables it: a stray value must not widen the
// tool surface the AI can see.
func DebugEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(EnvDebug))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// Serve runs the stdio protocol until the client disconnects or ctx is
// cancelled.
func (s *Server) Serve(ctx context.Context) error {
	return s.serve(ctx, s.transport())
}

// serve runs the protocol over an explicit transport.
//
// It is separate from Serve so tests can drive the real server over
// mcp.NewInMemoryTransports instead of a process's stdio.
func (s *Server) serve(ctx context.Context, t mcp.Transport) error {
	return s.build().Run(ctx, t)
}

// transport returns the stdio transport Serve uses.
func (s *Server) transport() mcp.Transport {
	if s.opts.In == nil && s.opts.Out == nil {
		return &mcp.StdioTransport{}
	}
	var in io.Reader = os.Stdin
	if s.opts.In != nil {
		in = s.opts.In
	}
	var out io.Writer = os.Stdout
	if s.opts.Out != nil {
		out = s.opts.Out
	}
	return &mcp.IOTransport{Reader: io.NopCloser(in), Writer: nopWriteCloser{out}}
}

// nopWriteCloser adapts an io.Writer to the io.WriteCloser IOTransport needs.
type nopWriteCloser struct{ io.Writer }

// Close is a no-op: the caller owns the underlying stream.
func (nopWriteCloser) Close() error { return nil }

// logger returns the SDK logger.
//
// Diagnostics always go to stderr: stdout carries the protocol (DESIGN §8).
func (s *Server) logger() *slog.Logger {
	w := s.opts.Stderr
	if w == nil {
		w = os.Stderr
	}
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: slog.LevelWarn}))
}

// build creates the SDK server with every tool registered.
func (s *Server) build() *mcp.Server {
	srv := mcp.NewServer(
		&mcp.Implementation{Name: ServerName, Version: "1.0.0"},
		&mcp.ServerOptions{Logger: s.logger()},
	)

	register(srv, s.lsTool)
	register(srv, s.resolveTool)
	register(srv, s.execTool)
	register(srv, s.uploadTool)
	register(srv, s.downloadTool)
	register(srv, s.relayTool)
	register(srv, s.configListTool)
	if s.opts.Debug {
		register(srv, s.poolStatusTool)
	}
	return srv
}

// addTool registers one (tool, handler) pair.
// register adds one tool produced by a factory.
//
// The factories return (tool, handler) together, and Go cannot expand a
// multi-value call into a call that already has a leading argument, so the
// pair is bound first.
func register(srv *mcp.Server, factory func() (*mcp.Tool, mcp.ToolHandler)) {
	t, h := factory()
	if t == nil || h == nil {
		return
	}
	srv.AddTool(t, h)
}

// --- result helpers ---

// textResult renders a successful tool call.
func textResult(text string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}
}

// errorResult renders a failed tool call.
//
// The "ERROR: " prefix matches the Python implementation, so existing
// prompts that look for it keep working. The result is marked as an error so
// a client can tell a failure from a successful run.
func errorResult(err error) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: "ERROR: " + err.Error()}},
		IsError: true,
	}
}

// --- audit ---

// publish records one audit event.
//
// A zero timestamp or pid is filled in so every line is attributable; the
// actor is the configured one. Nothing here interpolates a credential.
func (s *Server) publish(e obs.Event) {
	if e.TS.IsZero() {
		e.TS = time.Now()
	}
	if e.PID == 0 {
		e.PID = os.Getpid()
	}
	if e.Actor == "" {
		e.Actor = s.opts.Actor
	}
	switch {
	case s.opts.Publish != nil:
		s.opts.Publish(e)
	case s.opts.Bus != nil:
		s.opts.Bus.Publish(e)
	}
}

// startOp publishes the exec.start half of one tool call.
func (s *Server) startOp(server, asset, backend, command string) time.Time {
	started := time.Now()
	s.publish(obs.Event{
		TS:      started,
		Kind:    obs.KindExecStart,
		Server:  server,
		Asset:   asset,
		Backend: backend,
		Command: command,
	})
	return started
}

// endOp publishes the exec.end half of one tool call.
//
// A failed call is recorded with KindError so the audit stream distinguishes
// "the command ran and exited non-zero" from "the call never got that far".
func (s *Server) endOp(started time.Time, server, asset, backend, command string, res transferOutcome) {
	e := obs.Event{
		TS:          time.Now(),
		Kind:        obs.KindExecEnd,
		Server:      server,
		Asset:       asset,
		Backend:     backend,
		Command:     command,
		DurationMS:  time.Since(started).Milliseconds(),
		OutputBytes: res.outputBytes,
		Preview:     truncatePreview(res.output),
	}
	if res.exitCode != nil {
		e.ExitCode = res.exitCode
	}
	if res.err != nil {
		e.Kind = obs.KindError
		e.Error = res.err.Error()
	}
	s.publish(e)
}

// transferOutcome is what one tool call produced, for the audit record.
type transferOutcome struct {
	output      string
	outputBytes int
	exitCode    *int
	err         error
}

// truncatePreview bounds a preview to the audit default (DESIGN.md §12.3).
func truncatePreview(s string) string {
	if len(s) <= obs.DefaultPreviewBytes {
		return s
	}
	return s[:obs.DefaultPreviewBytes]
}

// --- session and resolution ---

// configPath returns the effective configuration path.
func (s *Server) configPath(override string) string {
	if p := strings.TrimSpace(override); p != "" {
		return p
	}
	return strings.TrimSpace(s.opts.ConfigPath)
}

// sessionFor returns the session and server entry for one tool call.
func (s *Server) sessionFor(ctx context.Context, serverName, configPath string) (*auth.Session, *config.ServerConfig, error) {
	if s.opts.Session != nil {
		return s.opts.Session(ctx, serverName)
	}

	cfg, err := config.Load(s.configPath(configPath))
	if err != nil {
		return nil, nil, err
	}
	srv, err := serverByName(cfg, serverName)
	if err != nil {
		return nil, nil, err
	}
	if s.opts.Sessions == nil {
		return nil, nil, errors.New("no session pool is configured for the MCP server")
	}

	// Endpoint selection follows the same failover policy as the CLI
	// (DESIGN.md §4.7): try the last-good address first, then the configured
	// preference, probing each with a short TCP dial. MCP lives in one
	// process for many calls, so the recorded last-good address would drift
	// from the CLI's without this - an internal address that answers 502
	// would otherwise be retried forever just because config.toml prefers it.
	st, _ := s.state(srv.Name).Get(srv.Name)
	candidates := endpoint.Candidates(srv, "", st)
	if len(candidates) == 0 {
		return nil, nil, fmt.Errorf("server %q has no address configured: run `jms config add %s`",
			srv.Name, srv.Name)
	}

	creds, err := s.credentials(srv.Name)
	if err != nil {
		return nil, nil, err
	}

	var sess *auth.Session
	var loginErr error
	for _, cand := range candidates {
		if ctx.Err() != nil {
			return nil, nil, ctx.Err()
		}
		if s.opts.Probe != nil && !s.opts.Probe(ctx, cand.URL, endpoint.ProbeTimeout) {
			// The probe is an accelerator, not a verdict: remember that this
			// candidate was skipped so the last one still gets a full login
			// attempt (§4.7, §11.9).
			loginErr = fmt.Errorf("probe failed for %s address %s", cand.Kind, cand.URL)
			continue
		}
		sess, loginErr = s.opts.Sessions.Get(ctx, srv, cand.URL, creds, s.opts.OTPPrompt)
		if loginErr == nil {
			return sess, srv, nil
		}
		if !endpoint.IsNetworkError(loginErr) {
			// A credential rejection is not fixed by another address.
			return nil, nil, loginErr
		}
	}
	return nil, nil, loginErr
}

// state returns the endpoint state store, creating the default file store
// on first use. A store that cannot be located degrades to in-memory: the
// cost is one extra probe per cold start, never a failed call.
func (s *Server) state(server string) endpoint.StateStore {
	if s.opts.state != nil {
		return s.opts.state
	}
	s.stateOnce.Do(func() {
		if dir, err := config.ConfigDir(); err == nil {
			s.stateStore = endpoint.NewFileStateStore(filepath.Join(dir, endpoint.StateFileName))
		} else {
			s.stateStore = endpoint.NewMemoryStateStore()
		}
	})
	return s.stateStore
}

// credentials reads the stored credential for one server alias.
//
// The returned error names the alias and the environment variable to set,
// but never a credential value: a password must not reach an error string,
// a log line or the audit stream (shared rules, DESIGN.md §7.1).
func (s *Server) credentials(alias string) (auth.Credentials, error) {
	store := s.opts.Creds
	if store == nil {
		store = config.Chain(config.NewEnvStore(), config.NewKeyringStore())
	}
	password, err := store.Get(alias, config.CredPassword)
	if err != nil {
		if errors.Is(err, config.ErrNotFound) {
			envName, envErr := config.EnvVarName(alias, config.CredPassword)
			if envErr != nil {
				envName = "<unknown>"
			}
			return auth.Credentials{}, fmt.Errorf(
				"no stored password for server %q: run `jms config add %s` or set %s",
				alias, alias, envName)
		}
		return auth.Credentials{}, fmt.Errorf("read the stored password for server %q: %w", alias, err)
	}
	var secret string
	if value, otpErr := store.Get(alias, config.CredOTP); otpErr == nil {
		secret = value
	}
	return auth.Credentials{Password: password, OTPSecret: secret}, nil
}

// resolveFor resolves an asset on a session.
func (s *Server) resolveFor(ctx context.Context, sess *auth.Session, name, account, protocol string) (assets.Info, error) {
	if s.opts.Resolve != nil {
		return s.opts.Resolve(ctx, sess, name, account, protocol)
	}
	if sess == nil || sess.Client == nil {
		return assets.Info{}, errors.New("session has no API client")
	}
	return assets.Resolve(ctx, sess.Client, name, account, protocol)
}

// serverByName returns a named server, or the default when name is empty.
func serverByName(cfg *config.AppConfig, name string) (*config.ServerConfig, error) {
	if strings.TrimSpace(name) == "" {
		return cfg.DefaultServer()
	}
	return cfg.Get(name)
}

// splitTarget parses the <asset>[@<server>] syntax.
//
// The last "@" separates the two; a leading or trailing "@" is part of the
// asset name, matching the Python parse_target.
func splitTarget(spec string) (asset, server string) {
	at := strings.LastIndex(spec, "@")
	if at <= 0 || at == len(spec)-1 {
		return spec, ""
	}
	return spec[:at], spec[at+1:]
}

// targetFor combines an explicit server argument with the target syntax.
//
// An explicit argument wins, so a prompt that passes both is unambiguous.
func targetFor(assetSpec, serverArg string) (asset, server string) {
	asset, embedded := splitTarget(strings.TrimSpace(assetSpec))
	if s := strings.TrimSpace(serverArg); s != "" {
		return asset, s
	}
	return asset, embedded
}

// --- tool schemas ---

// objectSchema renders a JSON Schema object for a tool's arguments.
func objectSchema(required []string, props map[string]map[string]any) json.RawMessage {
	schema := map[string]any{"type": "object"}
	if len(props) > 0 {
		schema["properties"] = props
	}
	if len(required) > 0 {
		schema["required"] = required
	}
	encoded, err := json.Marshal(schema)
	if err != nil {
		// The schema is built from string and bool literals only; a failure
		// here would be a programming error, and returning an empty object
		// schema keeps the server usable.
		return json.RawMessage(`{"type":"object"}`)
	}
	return encoded
}

// strProp is a string property.
func strProp(description string) map[string]any {
	return map[string]any{"type": "string", "description": description}
}

// intProp is an integer property.
func intProp(description string) map[string]any {
	return map[string]any{"type": "integer", "description": description}
}

// decodeArgs unmarshals a tool call's arguments.
func decodeArgs(req *mcp.CallToolRequest, out any) error {
	if req == nil || req.Params == nil {
		return nil
	}
	raw := req.Params.Arguments
	if len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("invalid arguments: %w", err)
	}
	return nil
}

// --- jms_ls ---

// lsArgs are the jms_ls arguments (identical to the Python tool).
type lsArgs struct {
	Server     string `json:"server,omitempty"`
	Keyword    string `json:"keyword,omitempty"`
	ConfigPath string `json:"config_path,omitempty"`
}

// lsTool lists or searches the authorized assets.
func (s *Server) lsTool() (*mcp.Tool, mcp.ToolHandler) {
	tool := &mcp.Tool{
		Name:        ToolLS,
		Description: "List or search authorized assets on a JumpServer.",
		InputSchema: objectSchema(nil, map[string]map[string]any{
			"server":      strProp("Server alias; the default server when omitted."),
			"keyword":     strProp("Search keyword; lists everything when omitted."),
			"config_path": strProp("Configuration file path override."),
		}),
	}
	return tool, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var args lsArgs
		if err := decodeArgs(req, &args); err != nil {
			return errorResult(err), nil
		}

		sess, srv, err := s.sessionFor(ctx, args.Server, args.ConfigPath)
		if err != nil {
			return errorResult(err), nil
		}
		if sess == nil || sess.Client == nil {
			return errorResult(errors.New("session has no API client")), nil
		}

		started := s.startOp(srv.Name, "", string(transport.BackendAuto), "ls "+args.Keyword)
		var listed []assets.Asset
		if strings.TrimSpace(args.Keyword) != "" {
			listed, err = assets.Search(ctx, sess.Client, args.Keyword)
		} else {
			listed, err = assets.List(ctx, sess.Client, 0)
		}
		if err != nil {
			s.endOp(started, srv.Name, "", string(transport.BackendAuto), "ls "+args.Keyword,
				transferOutcome{err: err})
			return errorResult(err), nil
		}

		text := renderAssetList(listed)
		s.endOp(started, srv.Name, "", string(transport.BackendAuto), "ls "+args.Keyword,
			transferOutcome{output: text, outputBytes: len(text)})
		return textResult(text), nil
	}
}

// renderAssetList renders one asset per line, matching the Python output.
func renderAssetList(listed []assets.Asset) string {
	if len(listed) == 0 {
		return "No assets found."
	}
	lines := make([]string, 0, len(listed))
	for _, a := range listed {
		lines = append(lines, fmt.Sprintf("%s  %s  %s", a.Name, a.Address, a.Platform))
	}
	return strings.Join(lines, "\n")
}

// --- jms_resolve_asset ---

// resolveArgs are the jms_resolve_asset arguments.
type resolveArgs struct {
	Asset      string `json:"asset"`
	Server     string `json:"server,omitempty"`
	ConfigPath string `json:"config_path,omitempty"`
}

// resolveTool resolves an asset to its connection parameters.
func (s *Server) resolveTool() (*mcp.Tool, mcp.ToolHandler) {
	tool := &mcp.Tool{
		Name:        ToolResolveAsset,
		Description: "Resolve an asset to its connection info (address, account, protocol).",
		InputSchema: objectSchema([]string{"asset"}, map[string]map[string]any{
			"asset":       strProp("Asset name or address."),
			"server":      strProp("Server alias; the default server when omitted."),
			"config_path": strProp("Configuration file path override."),
		}),
	}
	return tool, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var args resolveArgs
		if err := decodeArgs(req, &args); err != nil {
			return errorResult(err), nil
		}
		assetName, serverName := targetFor(args.Asset, args.Server)
		if strings.TrimSpace(assetName) == "" {
			return errorResult(errors.New("asset is required")), nil
		}

		sess, srv, err := s.sessionFor(ctx, serverName, args.ConfigPath)
		if err != nil {
			return errorResult(err), nil
		}
		started := s.startOp(srv.Name, assetName, string(transport.BackendAuto), "resolve")

		info, err := s.resolveFor(ctx, sess, assetName, "", "")
		if err != nil {
			s.endOp(started, srv.Name, assetName, string(transport.BackendAuto), "resolve",
				transferOutcome{err: err})
			return errorResult(err), nil
		}

		text := fmt.Sprintf("name: %s\naddress: %s\naccount: %s\nprotocol: %s",
			info.Name, info.Address, info.Account, info.Protocol)
		s.endOp(started, srv.Name, assetName, string(transport.BackendAuto), "resolve",
			transferOutcome{output: text, outputBytes: len(text)})
		return textResult(text), nil
	}
}

// --- jms_exec ---

// execArgs are the jms_exec arguments.
type execArgs struct {
	Asset      string `json:"asset"`
	Command    string `json:"command"`
	Server     string `json:"server,omitempty"`
	ConfigPath string `json:"config_path,omitempty"`
	Timeout    int    `json:"timeout,omitempty"`
	Account    string `json:"account,omitempty"`
	Protocol   string `json:"protocol,omitempty"`
}

// execTool runs one command through the terminal pool.
func (s *Server) execTool() (*mcp.Tool, mcp.ToolHandler) {
	tool := &mcp.Tool{
		Name:        ToolExec,
		Description: "Execute a command on an asset and return its output.",
		InputSchema: objectSchema([]string{"asset", "command"}, map[string]map[string]any{
			"asset":       strProp("Asset name or address, optionally <asset>@<server>."),
			"command":     strProp("Shell command to run."),
			"server":      strProp("Server alias; the default server when omitted."),
			"config_path": strProp("Configuration file path override."),
			"timeout":     intProp("Timeout in seconds; 30 when omitted."),
			"account":     strProp("Account override."),
			"protocol":    strProp("Protocol override."),
		}),
	}
	return tool, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var args execArgs
		if err := decodeArgs(req, &args); err != nil {
			return errorResult(err), nil
		}
		assetName, serverName := targetFor(args.Asset, args.Server)
		if strings.TrimSpace(assetName) == "" {
			return errorResult(errors.New("asset is required")), nil
		}
		if strings.TrimSpace(args.Command) == "" {
			return errorResult(errors.New("command is required")), nil
		}

		timeout := time.Duration(args.Timeout) * time.Second
		if args.Timeout <= 0 {
			timeout = DefaultExecTimeout
		}

		sess, srv, err := s.sessionFor(ctx, serverName, args.ConfigPath)
		if err != nil {
			return errorResult(err), nil
		}

		started := s.startOp(srv.Name, assetName, string(transport.BackendWS), args.Command)
		result, err := s.exec(ctx, srv, sess, assetName, args.Command, connpool.ExecOptions{
			Account:  args.Account,
			Protocol: args.Protocol,
			Backend:  transport.BackendWS,
			Timeout:  timeout,
		})
		if err != nil {
			s.endOp(started, srv.Name, assetName, string(transport.BackendWS), args.Command,
				transferOutcome{err: err})
			return errorResult(err), nil
		}

		s.endOp(started, srv.Name, assetName, string(transport.BackendWS), args.Command,
			transferOutcome{
				output:      result.Output,
				outputBytes: len(result.Output),
				exitCode:    &result.ExitCode,
			})
		return textResult(result.Output), nil
	}
}

// exec runs one command through the injected seam or the real pool.
func (s *Server) exec(ctx context.Context, srv *config.ServerConfig, sess *auth.Session,
	assetName, command string, opts connpool.ExecOptions) (transport.Result, error) {
	if s.opts.Exec != nil {
		return s.opts.Exec(ctx, srv, sess, assetName, command, opts)
	}
	if s.opts.Terminals == nil {
		return transport.Result{}, errors.New("no terminal pool is configured for the MCP server")
	}
	return s.opts.Terminals.Exec(ctx, srv, sess, assetName, command, opts)
}

// --- jms_sftp_upload / download / relay ---

// uploadArgs are the jms_sftp_upload arguments.
type uploadArgs struct {
	Src        string `json:"src"`
	Asset      string `json:"asset"`
	Dst        string `json:"dst"`
	Server     string `json:"server,omitempty"`
	ConfigPath string `json:"config_path,omitempty"`
}

// downloadArgs are the jms_sftp_download arguments.
type downloadArgs struct {
	Asset      string `json:"asset"`
	Src        string `json:"src"`
	Dst        string `json:"dst"`
	Server     string `json:"server,omitempty"`
	ConfigPath string `json:"config_path,omitempty"`
}

// relayArgs are the jms_sftp_relay arguments.
type relayArgs struct {
	SrcSpec    string `json:"src_spec"`
	DstSpec    string `json:"dst_spec"`
	ConfigPath string `json:"config_path,omitempty"`
}

// uploadTool uploads a local file to an asset.
func (s *Server) uploadTool() (*mcp.Tool, mcp.ToolHandler) {
	tool := &mcp.Tool{
		Name:        ToolSFTPUpload,
		Description: "Upload a local file to an asset via SFTP.",
		InputSchema: objectSchema([]string{"src", "asset", "dst"}, map[string]map[string]any{
			"src":         strProp("Local source path."),
			"asset":       strProp("Target asset, optionally <asset>@<server>."),
			"dst":         strProp("Destination path inside the asset."),
			"server":      strProp("Server alias; the default server when omitted."),
			"config_path": strProp("Configuration file path override."),
		}),
	}
	return tool, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var args uploadArgs
		if err := decodeArgs(req, &args); err != nil {
			return errorResult(err), nil
		}
		assetName, serverName := targetFor(args.Asset, args.Server)
		if strings.TrimSpace(assetName) == "" {
			return errorResult(errors.New("asset is required")), nil
		}
		if strings.TrimSpace(args.Src) == "" || strings.TrimSpace(args.Dst) == "" {
			return errorResult(errors.New("src and dst are required")), nil
		}

		sess, srv, err := s.sessionFor(ctx, serverName, args.ConfigPath)
		if err != nil {
			return errorResult(err), nil
		}
		started := s.startOp(srv.Name, assetName, "sftp", "upload "+args.Src)

		result, err := s.transfer(ctx, TransferRequest{
			Session:   sess,
			Asset:     assetName,
			Local:     args.Src,
			Remote:    args.Dst,
			Direction: xfer.Upload,
			Verify:    true,
		})
		if err != nil {
			s.endOp(started, srv.Name, assetName, "sftp", "upload "+args.Src,
				transferOutcome{err: err})
			return errorResult(err), nil
		}

		text := fmt.Sprintf("OK: uploaded %s -> %s:%s (%d bytes)",
			args.Src, assetName, args.Dst, result.Bytes)
		s.endOp(started, srv.Name, assetName, "sftp", "upload "+args.Src,
			transferOutcome{output: text, outputBytes: int(result.Bytes)})
		return textResult(text), nil
	}
}

// downloadTool downloads a file from an asset.
func (s *Server) downloadTool() (*mcp.Tool, mcp.ToolHandler) {
	tool := &mcp.Tool{
		Name:        ToolSFTPDownload,
		Description: "Download a file from an asset to the local machine via SFTP.",
		InputSchema: objectSchema([]string{"asset", "src", "dst"}, map[string]map[string]any{
			"asset":       strProp("Source asset, optionally <asset>@<server>."),
			"src":         strProp("Source path inside the asset."),
			"dst":         strProp("Local destination path."),
			"server":      strProp("Server alias; the default server when omitted."),
			"config_path": strProp("Configuration file path override."),
		}),
	}
	return tool, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var args downloadArgs
		if err := decodeArgs(req, &args); err != nil {
			return errorResult(err), nil
		}
		assetName, serverName := targetFor(args.Asset, args.Server)
		if strings.TrimSpace(assetName) == "" {
			return errorResult(errors.New("asset is required")), nil
		}
		if strings.TrimSpace(args.Src) == "" || strings.TrimSpace(args.Dst) == "" {
			return errorResult(errors.New("src and dst are required")), nil
		}

		sess, srv, err := s.sessionFor(ctx, serverName, args.ConfigPath)
		if err != nil {
			return errorResult(err), nil
		}
		started := s.startOp(srv.Name, assetName, "sftp", "download "+args.Src)

		result, err := s.transfer(ctx, TransferRequest{
			Session:   sess,
			Asset:     assetName,
			Local:     args.Dst,
			Remote:    args.Src,
			Direction: xfer.Download,
			Verify:    true,
		})
		if err != nil {
			s.endOp(started, srv.Name, assetName, "sftp", "download "+args.Src,
				transferOutcome{err: err})
			return errorResult(err), nil
		}

		text := fmt.Sprintf("OK: downloaded %s:%s -> %s (%d bytes)",
			assetName, args.Src, args.Dst, result.Bytes)
		s.endOp(started, srv.Name, assetName, "sftp", "download "+args.Src,
			transferOutcome{output: text, outputBytes: int(result.Bytes)})
		return textResult(text), nil
	}
}

// relayTool streams a file between two assets.
func (s *Server) relayTool() (*mcp.Tool, mcp.ToolHandler) {
	tool := &mcp.Tool{
		Name:        ToolSFTPRelay,
		Description: "Relay a file between two assets (streamed, no local disk).",
		InputSchema: objectSchema([]string{"src_spec", "dst_spec"}, map[string]map[string]any{
			"src_spec":    strProp("Source spec <asset>[@<server>]:<path>."),
			"dst_spec":    strProp("Destination spec <asset>[@<server>]:<path>."),
			"config_path": strProp("Configuration file path override."),
		}),
	}
	return tool, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var args relayArgs
		if err := decodeArgs(req, &args); err != nil {
			return errorResult(err), nil
		}

		src, err := parseRemoteSpec(args.SrcSpec)
		if err != nil {
			return errorResult(err), nil
		}
		dst, err := parseRemoteSpec(args.DstSpec)
		if err != nil {
			return errorResult(err), nil
		}

		srcSess, srcSrv, err := s.sessionFor(ctx, src.server, args.ConfigPath)
		if err != nil {
			return errorResult(err), nil
		}
		dstSess, dstSrv, err := s.sessionFor(ctx, dst.server, args.ConfigPath)
		if err != nil {
			return errorResult(err), nil
		}
		_ = dstSrv
		started := s.startOp(srcSrv.Name, src.asset, "sftp",
			fmt.Sprintf("relay %s -> %s", args.SrcSpec, args.DstSpec))

		result, err := s.transfer(ctx, TransferRequest{
			Verify: true,
			Relay: &RelayRequest{
				SrcSession: srcSess, SrcAsset: src.asset, SrcPath: src.path,
				DstSession: dstSess, DstAsset: dst.asset, DstPath: dst.path,
			},
		})
		if err != nil {
			s.endOp(started, srcSrv.Name, src.asset, "sftp",
				fmt.Sprintf("relay %s -> %s", args.SrcSpec, args.DstSpec),
				transferOutcome{err: err})
			return errorResult(err), nil
		}

		text := fmt.Sprintf("OK: relayed %s -> %s (%d bytes)", args.SrcSpec, args.DstSpec, result.Bytes)
		s.endOp(started, srcSrv.Name, src.asset, "sftp",
			fmt.Sprintf("relay %s -> %s", args.SrcSpec, args.DstSpec),
			transferOutcome{output: text, outputBytes: int(result.Bytes)})
		return textResult(text), nil
	}
}

// transfer runs one transfer through the injected seam or the real engine.
func (s *Server) transfer(ctx context.Context, req TransferRequest) (xfer.Result, error) {
	if s.opts.Transfer != nil {
		return s.opts.Transfer(ctx, req)
	}
	return defaultTransfer(ctx, req)
}

// defaultTransfer builds an xfer engine per call.
//
// A transfer is short-lived by design: SFTP connections are not pooled, so
// the connection lives only for the transfer (DESIGN.md §5).
//
// Asset names (req.Asset / req.Relay.SrcAsset / DstAsset) reach the engine
// as AssetName so each side resolves on demand; the relay needs this per
// side because src and dst may live on different servers.
func defaultTransfer(ctx context.Context, req TransferRequest) (xfer.Result, error) {
	if req.Relay != nil {
		src := &xfer.Engine{Session: req.Relay.SrcSession, AssetName: req.Relay.SrcAsset, Account: req.Relay.SrcAccount}
		dst := &xfer.Engine{Session: req.Relay.DstSession, AssetName: req.Relay.DstAsset, Account: req.Relay.DstAccount}
		task := xfer.Task{Verify: req.Verify, Chunk: req.Chunk, Parallel: req.Parallel}
		return xfer.Relay(ctx, src, dst, req.Relay.SrcPath, req.Relay.DstPath, task)
	}
	engine := &xfer.Engine{Session: req.Session, AssetName: req.Asset, Account: req.Account}
	return engine.Run(ctx, xfer.Task{
		Direction: req.Direction,
		Local:     req.Local,
		Remote:    req.Remote,
		Verify:    req.Verify,
		Chunk:     req.Chunk,
		Parallel:  req.Parallel,
	})
}

// remoteSpec is one parsed <asset>[@<server>]:<path> argument.
type remoteSpec struct {
	asset  string
	server string
	path   string
}

// parseRemoteSpec parses <asset>[@<server>]:<path>.
//
// The first colon splits host from path so that a colon inside the path
// survives, and the last "@" within the host splits the server alias —
// matching the Python _parse_remote_spec.
func parseRemoteSpec(spec string) (remoteSpec, error) {
	raw := strings.TrimSpace(spec)
	colon := strings.Index(raw, ":")
	if colon < 0 {
		return remoteSpec{}, fmt.Errorf("invalid remote spec %q: expected <asset>[@<server>]:<path>", spec)
	}
	host, path := raw[:colon], raw[colon+1:]
	asset, server := splitTarget(host)
	if strings.TrimSpace(asset) == "" {
		return remoteSpec{}, fmt.Errorf("invalid remote spec %q: asset name is empty", spec)
	}
	if strings.TrimSpace(path) == "" {
		return remoteSpec{}, fmt.Errorf("invalid remote spec %q: remote path is empty", spec)
	}
	return remoteSpec{asset: asset, server: server, path: path}, nil
}

// --- jms_config_list ---

// configListArgs are the jms_config_list arguments.
type configListArgs struct {
	ConfigPath string `json:"config_path,omitempty"`
}

// configListTool lists the configured servers.
//
// It reads the file directly and never touches the pool: introspection must
// work before any server is reachable (DESIGN.md §5).
func (s *Server) configListTool() (*mcp.Tool, mcp.ToolHandler) {
	tool := &mcp.Tool{
		Name:        ToolConfigList,
		Description: "List configured JumpServer servers, marking the default with '*'.",
		InputSchema: objectSchema(nil, map[string]map[string]any{
			"config_path": strProp("Configuration file path override."),
		}),
	}
	return tool, func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var args configListArgs
		if err := decodeArgs(req, &args); err != nil {
			return errorResult(err), nil
		}
		cfg, err := config.Load(s.configPath(args.ConfigPath))
		if err != nil {
			return errorResult(err), nil
		}
		return textResult(renderConfigList(cfg)), nil
	}
}

// renderConfigList renders one server per line, default marked with '*'.
//
// It renders addresses and usernames only. A credential never appears here,
// which is the whole point of DESIGN.md §7.1.
func renderConfigList(cfg *config.AppConfig) string {
	if cfg == nil || len(cfg.Servers) == 0 {
		return "No servers configured."
	}
	lines := make([]string, 0, len(cfg.Servers))
	for _, name := range cfg.Names() {
		srv := cfg.Servers[name]
		marker := ""
		if name == cfg.Default {
			marker = "*"
		}
		address := srv.Internal
		if strings.TrimSpace(address) == "" {
			address = srv.External
		}
		lines = append(lines, fmt.Sprintf("%-16s %-32s %-16s %s", name, address, srv.Username, marker))
	}
	return strings.Join(lines, "\n")
}

// --- jms_pool_status (debug only) ---

// poolStatusTool reports the pool's current shape.
//
// It is registered only under JMS_MCP_DEBUG=1 (DESIGN.md §5): the tool
// surface the AI sees stays the production tools by default.
func (s *Server) poolStatusTool() (*mcp.Tool, mcp.ToolHandler) {
	tool := &mcp.Tool{
		Name:        ToolPoolStatus,
		Description: "Debug: report the connection pool's terminals, hits and cold starts.",
		InputSchema: objectSchema(nil, nil),
	}
	return tool, func(_ context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		stats := connpool.Stats{}
		if s.opts.Terminals != nil {
			stats = s.opts.Terminals.Stats()
		}
		return textResult(fmt.Sprintf("terminals: %d\nhits: %d\ncold: %d",
			stats.Terminals, stats.Hits, stats.Cold)), nil
	}
}

// PrintConfig renders the client configuration snippet for
// `jms mcp --print-config`.
//
// The snippet carries no credential: the MCP entry is just the command, and
// credentials live in the OS credential store (DESIGN.md §7.5). That is
// what makes the entry safe to sync through a client manager.
func PrintConfig() string {
	return `{
  "mcpServers": {
    "jms": {
      "command": "jms",
      "args": ["mcp"]
    }
  }
}
`
}

var errNotImplemented = errors.New("not implemented")
