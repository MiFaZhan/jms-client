package cli

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/MiFaZhan/jms-client/internal/assets"
	"github.com/MiFaZhan/jms-client/internal/auth"
	"github.com/MiFaZhan/jms-client/internal/config"
	"github.com/MiFaZhan/jms-client/internal/endpoint"
	"github.com/MiFaZhan/jms-client/internal/transport"
)

// commandTarget is a parsed <asset>[@<server>] argument.
//
// The last "@" separates the server alias; a leading or trailing "@" is
// part of the asset name (no alias given), matching the reference CLI's
// parse_target. The empty alias means the default server.
type commandTarget struct {
	Asset  string
	Server string
}

// parseCommandTarget splits <asset>[@<server>] into its parts.
func parseCommandTarget(spec string) (commandTarget, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return commandTarget{}, fmt.Errorf("empty target: expected <asset>[@<server>]")
	}
	at := strings.LastIndex(spec, "@")
	if at <= 0 || at == len(spec)-1 {
		return commandTarget{Asset: spec}, nil
	}
	return commandTarget{Asset: spec[:at], Server: spec[at+1:]}, nil
}

// serverConnection bundles what a pool-backed command needs after target
// resolution: the server entry, its credentials and the configuration
// that carries the proxy policy.
type serverConnection struct {
	deps     Deps
	cfg      *config.AppConfig
	srv      *config.ServerConfig
	password string
	secret   string
}

// resolveCommandTarget resolves the server and the credentials behind one
// <asset>[@<server>] target, following the same shape `jms ls` uses.
//
// cmd is forwarded to the config loader so --config keeps its precedence.
func resolveCommandTarget(cmd *cobra.Command, deps Deps, t commandTarget) (*serverConnection, error) {
	cfg, err := loadConfigFor(cmd, deps)
	if err != nil {
		return nil, err
	}
	srv, err := resolveServer(cfg, t.Server)
	if err != nil {
		return nil, err
	}
	password, secret, err := loadCredentials(deps, srv.Name)
	if err != nil {
		return nil, err
	}
	return &serverConnection{deps: deps, cfg: cfg, srv: srv, password: password, secret: secret}, nil
}

// connect authenticates and returns the session bound to the chosen
// endpoint, reporting the selection on stderr the way `jms ls` does.
//
// The endpoint is forced when force is non-empty (DESIGN.md §4.7 point 7).
func (c *serverConnection) connect(ctx context.Context, force endpoint.Kind) (*auth.Session, endpoint.Kind, error) {
	session, sel, err := selectEndpoint(ctx, c.deps, newPrompter(c.deps), c.cfg, c.srv, force, c.password, c.secret)
	if err != nil {
		return nil, "", describeEndpointFailure(c.srv, force, err)
	}
	fmt.Fprintf(c.deps.Err, "Using %s endpoint %s\n", sel.Candidate.Kind, sel.Candidate.URL)
	return session, sel.Candidate.Kind, nil
}

// connectAndResolve is connect plus the asset lookup, for commands that
// open a terminal directly (jms login) rather than through the pool. It also
// returns the selected endpoint kind so the caller can reuse the backend
// that previously worked for that endpoint.
func (c *serverConnection) connectAndResolve(ctx context.Context, force endpoint.Kind,
	target commandTarget, account, protocol string) (*auth.Session, assets.Info, endpoint.Kind, error) {

	session, kind, err := c.connect(ctx, force)
	if err != nil {
		return nil, assets.Info{}, "", err
	}
	info, err := assets.Resolve(ctx, session.Client, target.Asset, account, protocol)
	if err != nil {
		return nil, assets.Info{}, "", err
	}
	return session, info, kind, nil
}

// parseBackend validates the --backend flag value. The empty string means
// auto, which is the transport default.
func parseBackend(raw string) (transport.BackendType, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "":
		return transport.BackendAuto, nil
	case "ssh":
		return transport.BackendSSH, nil
	case "ws":
		return transport.BackendWS, nil
	case "auto":
		return transport.BackendAuto, nil
	default:
		return "", fmt.Errorf("invalid --backend %q: must be ssh, ws or auto", raw)
	}
}

// execTimeout converts the -t flag into a duration; a non-positive value
// means no local ceiling beyond the context.
func execTimeout(seconds int) time.Duration {
	if seconds <= 0 {
		return 0
	}
	return time.Duration(seconds) * time.Second
}

// preferredBackend returns the backend remembered for the endpoint the
// session is bound to, if any. It lets a cold start skip the auto sequence
// instead of paying a handshake timeout on a blocked port first
// (DESIGN.md §4.7 point 4).
func (c *serverConnection) preferredBackend(kind endpoint.Kind) transport.BackendType {
	if c.deps.Runtime == nil || c.deps.State == nil {
		return ""
	}
	st, ok := c.deps.State.Get(c.srv.Name)
	if !ok {
		return ""
	}
	if b := st.BackendFor(endpoint.Kind(kind)); b != "" {
		return transport.BackendType(b)
	}
	return ""
}

// rememberBackend records the backend that worked for an address kind, so the
// next cold start can go straight to it. A failure to persist is not an
// error: it only costs the next call the same handshake it would have paid
// anyway.
func (c *serverConnection) rememberBackend(kind endpoint.Kind, backend transport.BackendType) {
	if backend == "" || c.deps.Runtime == nil || c.deps.State == nil {
		return
	}
	st, _ := c.deps.State.Get(c.srv.Name)
	if st.Backend == nil {
		st.Backend = map[endpoint.Kind]string{}
	}
	st.Backend[endpoint.Kind(kind)] = string(backend)
	_ = c.deps.State.Set(c.srv.Name, st)
}
