package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/MiFaZhan/jms-client/internal/api"
	"github.com/MiFaZhan/jms-client/internal/auth"
	"github.com/MiFaZhan/jms-client/internal/config"
	"github.com/MiFaZhan/jms-client/internal/endpoint"
)

// newConfigCommand builds the `jms config` command group.
//
// M1 registers: add, list, remove, set-default, test.
func newConfigCommand(deps *Deps) *cobra.Command {
	group := &cobra.Command{
		Use:   "config",
		Short: "Manage server configurations",
		Args:  cobra.NoArgs,
	}
	group.AddCommand(newConfigAddCommand(deps))
	group.AddCommand(newConfigListCommand(deps))
	group.AddCommand(newConfigRemoveCommand(deps))
	group.AddCommand(newConfigSetDefaultCommand(deps))
	group.AddCommand(newConfigTestCommand(deps))
	return group
}

func newConfigAddCommand(deps *Deps) *cobra.Command {
	var setDefault bool
	cmd := &cobra.Command{
		Use:   "add <alias>",
		Short: "Add or update a server configuration",
		Long: `Add or update a JumpServer configuration.

Prompts for the internal and external addresses (either may be empty, at
least one is required), the username, the password (hidden, confirmed)
and an optional TOTP secret. The credential is validated against a
reachable address before anything is written, so a rejected password
leaves no trace on disk. The first server added becomes the default.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runConfigAdd(cmd, *deps, args[0], setDefault)
		},
	}
	cmd.Flags().BoolVar(&setDefault, "set-default", false, "Set as the default server")
	return cmd
}

func newConfigListCommand(deps *Deps) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List configured servers",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runConfigList(cmd, *deps)
		},
	}
}

func newConfigRemoveCommand(deps *Deps) *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "remove <alias>",
		Short: "Remove a configured server",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runConfigRemove(cmd, *deps, args[0], yes)
		},
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "Do not ask for confirmation")
	return cmd
}

func newConfigSetDefaultCommand(deps *Deps) *cobra.Command {
	return &cobra.Command{
		Use:   "set-default <alias>",
		Short: "Set the default server",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runConfigSetDefault(cmd, *deps, args[0])
		},
	}
}

func newConfigTestCommand(deps *Deps) *cobra.Command {
	return &cobra.Command{
		Use:   "test [alias]",
		Short: "Probe both addresses and verify the login",
		Long: `Probe both configured addresses and verify the login.

Each address is probed for TCP reachability (1.5s timeout) and the first
reachable one gets a real login attempt, so a credential problem is
reported separately from an unreachable network.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			alias := ""
			if len(args) == 1 {
				alias = args[0]
			}
			return runConfigTest(cmd, *deps, alias)
		},
	}
}

// runConfigAdd interactively collects one server entry, validates the
// credential against a reachable address, and only then persists it.
//
// Persistence order is deliberate: the credential goes to the store first
// and the metadata file second, so a failure in between can be rolled
// back to a state the user can re-enter. The reverse order would leave a
// server that looks configured but can never log in.
func runConfigAdd(cmd *cobra.Command, deps Deps, alias string, setDefault bool) error {
	alias = strings.TrimSpace(alias)
	if alias == "" {
		return errors.New("server alias must not be empty")
	}
	p := newPrompter(deps)

	internal, err := p.line("Internal URL (http(s)://..., empty to skip): ")
	if err != nil {
		return err
	}
	external, err := p.line("External URL (http(s)://..., empty to skip): ")
	if err != nil {
		return err
	}
	username, err := p.line("Username: ")
	if err != nil {
		return err
	}
	password, err := p.secret("Password: ")
	if err != nil {
		return err
	}
	confirmation, err := p.secret("Confirm password: ")
	if err != nil {
		return err
	}
	if password != confirmation {
		return errors.New("passwords do not match")
	}
	secret, err := p.secret("TOTP secret (base32, empty to enter the code each time): ")
	if err != nil {
		return err
	}
	secret = strings.TrimSpace(secret)

	srv := &config.ServerConfig{
		Name:     alias,
		Internal: strings.TrimSpace(internal),
		External: strings.TrimSpace(external),
		Username: username,
	}
	// Everything that can be rejected locally is rejected here, before
	// the credential store or the network is touched.
	if err := config.Validate(srv); err != nil {
		return err
	}
	if strings.TrimSpace(password) == "" {
		return errors.New("password must not be empty")
	}

	ctx := commandContext(cmd)
	if _, _, err := selectEndpoint(ctx, deps, p, srv, "", password, secret); err != nil {
		return fmt.Errorf("cannot validate the credentials for %q: %w", alias, err)
	}

	// An unusable secret is only a warning: the user may be rotating it,
	// and the login above already proved the credential works.
	if secret != "" {
		if err := auth.ValidateSecret(secret); err != nil {
			fmt.Fprintf(deps.Err,
				"Warning: the TOTP secret for %q is not a usable base32 secret (%v); it was stored anyway\n",
				alias, err)
		}
	}

	cfg, err := loadOrNewConfig(cmd, deps)
	if err != nil {
		return err
	}
	if err := cfg.Put(srv); err != nil {
		return err
	}
	if setDefault {
		if err := cfg.SetDefault(alias); err != nil {
			return err
		}
	}

	previous := snapshotCredentials(deps, alias)
	if err := deps.Creds.Set(alias, config.CredPassword, password); err != nil {
		return fmt.Errorf("store the password for %q: %w", alias, err)
	}
	if secret != "" {
		if err := deps.Creds.Set(alias, config.CredOTP, secret); err != nil {
			rollbackCredentials(deps, alias, previous)
			return fmt.Errorf("store the TOTP secret for %q: %w", alias, err)
		}
	}

	path, err := saveConfigFor(cmd, deps, cfg)
	if err != nil {
		rollbackCredentials(deps, alias, previous)
		return fmt.Errorf("save the configuration for %q: %w", alias, err)
	}

	fmt.Fprintf(deps.Out, "Server %q saved to %s\n", alias, path)
	if setDefault {
		fmt.Fprintf(deps.Out, "Server %q is now the default.\n", alias)
	}
	return nil
}

// runConfigList renders one row per server, including where its password
// comes from.
//
// The credential source matters as much as the metadata: an alias whose
// password is missing fails at login time, and DESIGN.md §7.3 requires
// that to be a visible warning rather than a blank cell.
func runConfigList(cmd *cobra.Command, deps Deps) error {
	cfg, err := loadConfigFor(cmd, deps)
	if err != nil {
		return err
	}
	names := cfg.Names()
	if len(names) == 0 {
		return fmt.Errorf("%w: run `jms config add <alias>`", config.ErrNoServers)
	}

	table := newTable(deps.Out)
	table.setHeader("Alias", "Internal", "External", "Username", "Creds", "Default")
	for _, name := range names {
		srv := cfg.Servers[name]
		marker := ""
		if name == cfg.Default {
			marker = "*"
		}
		source := credentialSource(deps, name)
		if source == credsMissing {
			envName, envErr := config.EnvVarName(name, config.CredPassword)
			if envErr != nil {
				envName = "<unknown kind>"
			}
			fmt.Fprintf(deps.Err,
				"Warning: no stored password for server %q — run `jms config add %s` or set %s\n",
				name, name, envName)
		}
		table.addRow(name, orDash(srv.Internal), orDash(srv.External), srv.Username, source, marker)
	}
	table.setTrailer(fmt.Sprintf("\nTotal: %d server(s)", len(names)))
	return table.render()
}

// runConfigRemove deletes the metadata entry and then the stored
// credentials.
//
// The metadata file is written first: if the credential cleanup then
// fails the user still sees the removal succeed plus a precise warning,
// whereas the opposite order would silently strand a config entry whose
// password is already gone.
func runConfigRemove(cmd *cobra.Command, deps Deps, alias string, yes bool) error {
	cfg, err := loadConfigFor(cmd, deps)
	if err != nil {
		return err
	}
	if _, err := cfg.Get(alias); err != nil {
		return err
	}
	if !yes {
		confirmed, err := newPrompter(deps).confirm(fmt.Sprintf("Remove server %q?", alias))
		if err != nil {
			return err
		}
		if !confirmed {
			return errors.New("aborted by user")
		}
	}
	if err := cfg.Delete(alias); err != nil {
		return err
	}

	// Removing the last server leaves nothing to write: Save rejects a
	// serverless configuration because Load would refuse to read it back.
	// Deleting the file outright is the honest outcome - the user is back
	// to the pre-`config add` state and can add a server again.
	if len(cfg.Servers) == 0 {
		path, pathErr := configPathFor(cmd, deps)
		if pathErr != nil {
			return pathErr
		}
		if err := deps.Creds.Delete(alias); err != nil {
			return fmt.Errorf("server %q was removed, but its stored credentials could not be deleted: %w",
				alias, err)
		}
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove %s: %w", path, err)
		}
		fmt.Fprintf(deps.Out, "Server %q removed (%s) - no servers remain\n", alias, path)
		return nil
	}

	path, err := saveConfigFor(cmd, deps, cfg)
	if err != nil {
		return err
	}
	if err := deps.Creds.Delete(alias); err != nil {
		return fmt.Errorf("server %q was removed from %s, but its stored credentials could not be deleted: %w",
			alias, path, err)
	}
	fmt.Fprintf(deps.Out, "Server %q removed (%s)\n", alias, path)
	return nil
}

// runConfigSetDefault switches the default server.
func runConfigSetDefault(cmd *cobra.Command, deps Deps, alias string) error {
	cfg, err := loadConfigFor(cmd, deps)
	if err != nil {
		return err
	}
	if err := cfg.SetDefault(alias); err != nil {
		return err
	}
	path, err := saveConfigFor(cmd, deps, cfg)
	if err != nil {
		return err
	}
	fmt.Fprintf(deps.Out, "Server %q is now the default (%s)\n", alias, path)
	return nil
}

// endpointProbe is one row of the `jms config test` table.
type endpointProbe struct {
	kind      endpoint.Kind
	url       string
	reachable bool
	probeMS   int64
	login     string
}

// runConfigTest probes every configured address and logs in on a
// reachable one.
//
// This is the M1 acceptance command, so the two failure modes it must
// separate are a credential the server rejects (terminal, reported as
// such) and a network that never answers (retryable, reported per
// address).
func runConfigTest(cmd *cobra.Command, deps Deps, alias string) error {
	cfg, err := loadConfigFor(cmd, deps)
	if err != nil {
		return err
	}
	srv, err := resolveServer(cfg, alias)
	if err != nil {
		return err
	}
	ctx := commandContext(cmd)

	candidates := endpoint.Candidates(srv, "", stateFor(deps, srv))
	if len(candidates) == 0 {
		return fmt.Errorf("server %q has no address configured: run `jms config add %s`",
			srv.Name, srv.Name)
	}

	probes := make([]endpointProbe, 0, len(candidates))
	for _, cand := range candidates {
		started := deps.Now()
		reachable := deps.Probe(ctx, cand.URL, endpoint.ProbeTimeout)
		probes = append(probes, endpointProbe{
			kind:      cand.Kind,
			url:       cand.URL,
			reachable: reachable,
			probeMS:   deps.Now().Sub(started).Milliseconds(),
		})
	}

	target := -1
	for i := range probes {
		if probes[i].reachable {
			target = i
			break
		}
	}

	password, secret, credErr := loadCredentials(deps, srv.Name)
	if credErr != nil {
		for i := range probes {
			if probes[i].reachable {
				probes[i].login = "no credential"
			}
		}
		return fmt.Errorf("%w: %v", credErr, describeProbes(probes))
	}
	// Every reachable endpoint gets a login attempt, not just the first: the
	// command's whole purpose is telling the user which address actually
	// works, and a probe alone cannot — a VPN or proxy can accept the TCP
	// connection while nothing useful sits behind it (DESIGN.md §11.9).
	var loginErr error
	for i := range probes {
		if !probes[i].reachable {
			continue
		}
		_, err := loginSession(api.WithRetryBudget(ctx, 0), deps, newPrompter(deps), srv,
			probes[i].url, password, secret)
		probes[i].login = loginOutcome(err)
		if err != nil && loginErr == nil {
			loginErr = err
		}
	}

	printTestTable(deps.Out, srv, cfg.Default == srv.Name, probes)

	fmt.Fprintf(deps.Out, "\nCredentials: %s\n", credentialSource(deps, srv.Name))

	if target < 0 {
		return fmt.Errorf("%w: server %q is unreachable on every configured address (%s)",
			endpoint.ErrAllUnreachable, srv.Name, describeProbes(probes))
	}
	if credErr != nil {
		return fmt.Errorf("server %q is reachable at %s, but %w",
			srv.Name, probes[target].url, credErr)
	}
	if loginErr != nil {
		return describeLoginFailure(srv, probes[target].url, loginErr)
	}
	return nil
}

// credentialRejection statuses mirror the classification the endpoint
// policy uses: these mean the server saw the credential and refused it.
// Anything else - a transport failure, a 5xx - is an endpoint or network
// problem and must not be reported as a credential problem.
var credentialRejection = map[int]bool{
	http.StatusBadRequest:          true,
	http.StatusUnauthorized:        true,
	http.StatusForbidden:           true,
	http.StatusUnprocessableEntity: true,
}

// isCredentialRejection reports whether err means the stored credentials
// were seen and refused (as opposed to the endpoint being broken).
func isCredentialRejection(err error) bool {
	if errors.Is(err, auth.ErrMFANotProvided) || errors.Is(err, auth.ErrMissingSessionCookie) {
		return true
	}
	authErr, ok := api.AsAuthError(err)
	return ok && credentialRejection[authErr.StatusCode]
}

// describeLoginFailure words a failed login so that a rejected credential
// is never confused with an unreachable address.
func describeLoginFailure(srv *config.ServerConfig, baseURL string, err error) error {
	if isCredentialRejection(err) {
		return fmt.Errorf("the server %q rejected the stored credentials for %q: %w",
			srv.Name, baseURL, err)
	}
	return fmt.Errorf("login to %q failed for server %q (network or server error, not a credential problem): %w",
		baseURL, srv.Name, err)
}

// describeProbes summarises the probe results for an error message.
func describeProbes(probes []endpointProbe) string {
	parts := make([]string, 0, len(probes))
	for _, p := range probes {
		parts = append(parts, fmt.Sprintf("%s %s: unreachable", p.kind, p.url))
	}
	return strings.Join(parts, "; ")
}

// loginOutcome renders a login attempt result for the table.
func loginOutcome(err error) string {
	if err == nil {
		return "ok"
	}
	if isCredentialRejection(err) {
		return "auth failed"
	}
	if endpoint.IsNetworkError(err) {
		return "unreachable"
	}
	return "failed"
}

// printTestTable renders the per-address probe report.
func printTestTable(out io.Writer, srv *config.ServerConfig, isDefault bool, probes []endpointProbe) {
	marker := ""
	if isDefault {
		marker = " (default)"
	}
	fmt.Fprintf(out, "Server: %s%s\n\n", srv.Name, marker)

	table := newTable(out)
	table.setHeader("Endpoint", "URL", "Reachable", "Probe", "Login")
	for _, p := range probes {
		reachable := "no"
		if p.reachable {
			reachable = "yes"
		}
		login := p.login
		if login == "" {
			login = "skipped"
		}
		table.addRow(string(p.kind), p.url, reachable, fmt.Sprintf("%dms", p.probeMS), login)
	}
	_ = table.render()
}

// credentialSnapshot remembers what was stored for an alias before
// `config add` overwrote it, so a failed write can restore it.
type credentialSnapshot struct {
	password    string
	passwordSet bool
	otp         string
	otpSet      bool
}

// snapshotCredentials reads the current credential values for alias.
//
// Errors are ignored on purpose: the snapshot only serves a best-effort
// rollback, and a store that cannot be read cannot be restored either.
func snapshotCredentials(deps Deps, alias string) credentialSnapshot {
	var snap credentialSnapshot
	if deps.Creds == nil {
		return snap
	}
	if v, err := deps.Creds.Get(alias, config.CredPassword); err == nil {
		snap.password, snap.passwordSet = v, true
	}
	if v, err := deps.Creds.Get(alias, config.CredOTP); err == nil {
		snap.otp, snap.otpSet = v, true
	}
	return snap
}

// rollbackCredentials undoes a partially written credential set.
//
// It restores exactly the previous state: a fresh alias loses its
// entries, an existing alias keeps the credentials it had before the
// failed update. The user is told when even that fails, because a silent
// failure here would leave a half-configured alias behind.
func rollbackCredentials(deps Deps, alias string, previous credentialSnapshot) {
	if deps.Creds == nil {
		return
	}
	var problems []string
	if err := deps.Creds.Delete(alias); err != nil {
		problems = append(problems, fmt.Sprintf("delete: %v", err))
	}
	if previous.passwordSet {
		if err := deps.Creds.Set(alias, config.CredPassword, previous.password); err != nil {
			problems = append(problems, fmt.Sprintf("restore password: %v", err))
		}
	}
	if previous.otpSet {
		if err := deps.Creds.Set(alias, config.CredOTP, previous.otp); err != nil {
			problems = append(problems, fmt.Sprintf("restore TOTP secret: %v", err))
		}
	}
	if len(problems) > 0 {
		fmt.Fprintf(deps.Err,
			"Warning: could not roll back the stored credentials for %q (%s); re-run `jms config add %s`\n",
			alias, strings.Join(problems, "; "), alias)
		return
	}
	fmt.Fprintf(deps.Err, "The stored credentials for %q were rolled back.\n", alias)
}

// loadOrNewConfig loads the configuration, treating a missing file as an
// empty configuration.
//
// Any other failure propagates: a corrupt file must never be silently
// replaced, or an unrelated alias would lose its metadata.
func loadOrNewConfig(cmd *cobra.Command, deps Deps) (*config.AppConfig, error) {
	cfg, err := loadConfigFor(cmd, deps)
	if err == nil {
		return cfg, nil
	}
	if errors.Is(err, config.ErrConfigNotFound) {
		return &config.AppConfig{}, nil
	}
	return nil, err
}

// Credential source markers reported by `jms config list` and
// `jms config test`.
const (
	credsEnv     = "env"
	credsKeyring = "keyring"
	credsMissing = "missing"
)

// credentialSource reports where the password for alias would come from.
//
// The environment is checked first because it outranks the keyring in
// config.Chain; consulting the chain alone would report an environment
// variable as "keyring".
func credentialSource(deps Deps, alias string) string {
	envName, err := config.EnvVarName(alias, config.CredPassword)
	if err == nil {
		if v, ok := os.LookupEnv(envName); ok && strings.TrimSpace(v) != "" {
			return credsEnv
		}
	}
	if deps.Creds != nil {
		if _, err := deps.Creds.Get(alias, config.CredPassword); err == nil {
			return credsKeyring
		}
	}
	return credsMissing
}

// loadCredentials reads the password and optional TOTP secret for alias.
func loadCredentials(deps Deps, alias string) (password, secret string, err error) {
	if deps.Creds == nil {
		return "", "", fmt.Errorf("no credential store configured for server %q", alias)
	}
	password, err = deps.Creds.Get(alias, config.CredPassword)
	if err != nil {
		if errors.Is(err, config.ErrNotFound) {
			envName, envErr := config.EnvVarName(alias, config.CredPassword)
			if envErr != nil {
				envName = "<unknown kind>"
			}
			return "", "", fmt.Errorf(
				"no stored password for server %q: run `jms config add %s` or set %s",
				alias, alias, envName)
		}
		return "", "", fmt.Errorf("read the password for %q: %w", alias, err)
	}
	if value, otpErr := deps.Creds.Get(alias, config.CredOTP); otpErr == nil {
		secret = value
	}
	return password, secret, nil
}

// commandContext returns the command context, which is never nil for a
// command started through ExecuteArgs but may be for a directly invoked
// RunE in tests.
func commandContext(cmd *cobra.Command) context.Context {
	if cmd != nil {
		if ctx := cmd.Context(); ctx != nil {
			return ctx
		}
	}
	return context.Background()
}

// orDash renders an unset address as "-" so the table stays readable.
func orDash(value string) string {
	if strings.TrimSpace(value) == "" {
		return "-"
	}
	return value
}
