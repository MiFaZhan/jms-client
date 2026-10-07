package cli

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/MiFaZhan/jms-client/internal/api"
	"github.com/MiFaZhan/jms-client/internal/auth"
	"github.com/MiFaZhan/jms-client/internal/config"
	"github.com/MiFaZhan/jms-client/internal/endpoint"
)

// processArgs is indirected so tests can supply an argument list.
var processArgs = func() []string { return os.Args[1:] }

// configPathFor resolves the configuration file path for one invocation.
//
// Precedence: Deps.ConfigPath, then --config, then JMS_CONFIG, then the
// platform default. An injected path stays authoritative, otherwise a
// test would silently pick up the developer's real JMS_CONFIG.
func configPathFor(cmd *cobra.Command, deps Deps) (string, error) {
	if p := strings.TrimSpace(deps.ConfigPath); p != "" {
		return p, nil
	}
	if cmd != nil {
		if flag := cmd.Flags().Lookup(flagConfig); flag != nil {
			if p := strings.TrimSpace(flag.Value.String()); p != "" {
				return p, nil
			}
		}
	}
	return config.DefaultPath()
}

// loadConfigFor reads the configuration file for one invocation.
func loadConfigFor(cmd *cobra.Command, deps Deps) (*config.AppConfig, error) {
	path, err := configPathFor(cmd, deps)
	if err != nil {
		return nil, err
	}
	return config.Load(path)
}

// saveConfigFor writes the configuration file for one invocation and
// returns the path it was written to.
func saveConfigFor(cmd *cobra.Command, deps Deps, cfg *config.AppConfig) (string, error) {
	path, err := configPathFor(cmd, deps)
	if err != nil {
		return "", err
	}
	if err := config.Save(path, cfg); err != nil {
		return "", err
	}
	return path, nil
}

// resolveServer returns the named server, or the default one when name is
// empty (the `jms ls [server]` positional argument).
func resolveServer(cfg *config.AppConfig, name string) (*config.ServerConfig, error) {
	if strings.TrimSpace(name) == "" {
		return cfg.DefaultServer()
	}
	return cfg.Get(name)
}

// parseEndpointKind validates the --endpoint flag value.
//
// The empty string means "no force": the endpoint package then applies
// the configured preference and the last-good memory.
func parseEndpointKind(raw string) (endpoint.Kind, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "":
		return "", nil
	case config.KindInternal:
		return endpoint.KindInternal, nil
	case config.KindExternal:
		return endpoint.KindExternal, nil
	default:
		return "", fmt.Errorf("invalid --endpoint %q: must be %q or %q",
			raw, config.KindInternal, config.KindExternal)
	}
}

// stateFor reads the persisted last-good endpoint for a server.
func stateFor(deps Deps, srv *config.ServerConfig) endpoint.State {
	if deps.State == nil || srv == nil {
		return endpoint.State{}
	}
	st, _ := deps.State.Get(srv.Name)
	return st
}

// loginSession authenticates against baseURL.
//
// This is the single seam through which the CLI reaches the
// authentication layer: Deps.Login replaces it in tests, otherwise the
// real auth.Login performs the dual login with the given credential.
func loginSession(ctx context.Context, deps Deps, p *prompter, srv *config.ServerConfig,
	baseURL, password, secret string) (*auth.Session, error) {

	if deps.Login != nil {
		return deps.Login(ctx, srv, baseURL)
	}
	return auth.Login(ctx, srv, baseURL,
		auth.Credentials{Password: password, OTPSecret: secret}, p.otp)
}

// selectEndpoint picks an address under the failover policy of
// DESIGN.md §4.7 and authenticates against it.
//
// The returned session is the one the login actually produced, so callers
// must use its Client instead of rebuilding a client from the selected
// URL — a test seam that answers with a client pointing elsewhere would
// otherwise be ignored.
func selectEndpoint(ctx context.Context, deps Deps, p *prompter, srv *config.ServerConfig,
	force endpoint.Kind, password, secret string) (*auth.Session, endpoint.Selection, error) {

	var session *auth.Session
	login := func(ctx context.Context, cand endpoint.Candidate) error {
		// Deciding whether an endpoint is usable is a probe, not work: cap the
		// retry budget so a server that answers 502 after a stall does not
		// turn one probe into the full retry schedule (measured at ~24s on a
		// Tailscale-intercepted address).
		s, err := loginSession(api.WithRetryBudget(ctx, 0), deps, p, srv, cand.URL, password, secret)
		if err != nil {
			return err
		}
		session = s
		return nil
	}

	sel, err := endpoint.SelectAndLogin(ctx, srv, force, deps.Probe, login, deps.State)
	if err != nil {
		return nil, endpoint.Selection{}, err
	}
	if session == nil || session.Client == nil {
		return nil, endpoint.Selection{}, fmt.Errorf("login for server %q returned no API client", srv.Name)
	}
	return session, sel, nil
}
