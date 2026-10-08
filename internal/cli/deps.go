package cli

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/MiFaZhan/jms-client/internal/auth"
	"github.com/MiFaZhan/jms-client/internal/config"
	"github.com/MiFaZhan/jms-client/internal/endpoint"
)

// Deps carries every dependency the command surface needs.
//
// A zero Deps is not usable; call Deps.WithDefaults first, which is what
// ExecuteArgs and NewRootCommand do.
type Deps struct {
	// ConfigPath overrides the configuration file path. Empty means the
	// platform default (or JMS_CONFIG). When set it wins over JMS_CONFIG.
	ConfigPath string

	// Creds is the credential store chain. Nil means
	// config.Chain(config.NewEnvStore(), config.NewKeyringStore()).
	Creds config.CredentialStore

	// Probe checks address reachability. Nil means endpoint.ProbeWith with
	// the server's configured proxy policy (DESIGN.md「代理策略」), which falls back
	// to endpoint.TCPProbe when no proxy is configured.
	Probe endpoint.ProbeFunc

	// State persists the last-good address. Nil means a file store in
	// the configuration directory.
	State endpoint.StateStore

	// Login authenticates against one candidate address and returns the
	// session, whose api.Client the command then uses. Nil means the real
	// auth.Login.
	Login LoginFactory

	// PromptPassword reads a password without echoing it. Nil means the
	// x/term implementation with a stdin fallback.
	PromptPassword func(prompt string) (string, error)

	// PromptLine reads one line, used for the username, addresses and
	// the optional TOTP secret. Nil means reading from Stdin.
	PromptLine func(prompt string) (string, error)

	// PromptOTP requests a one-time code when MFA triggers and no secret
	// is stored. Nil means reading from Stdin.
	PromptOTP func() (string, error)

	// Confirm asks a yes/no question. Nil means reading from Stdin.
	Confirm func(prompt string) (bool, error)

	// Stdin is the input stream for the default prompts.
	Stdin io.Reader

	// Out receives normal command output.
	Out io.Writer

	// Err receives diagnostics and error messages.
	Err io.Writer

	// Now returns the current time; overridable in tests so probe
	// durations are deterministic.
	Now func() time.Time

	// Runtime carries the shared pools, the event bus and the seams the
	// pool-backed commands (exec, login, sftp, ssh-pipe, tail, attach,
	// log, mcp) use. Nil means WithDefaults builds one.
	Runtime *Runtime
}

// LoginFactory authenticates against one server address.
//
// It exists so the CLI can build a session without hard-wiring
// auth.Login, and so tests can inject a fake session. The returned
// session's Client is what list/search calls use.
type LoginFactory func(ctx context.Context, srv *config.ServerConfig, baseURL string) (*auth.Session, error)

// WithDefaults returns a copy of d with every nil field filled in.
func (d Deps) WithDefaults() Deps {
	if d.Creds == nil {
		d.Creds = config.Chain(config.NewEnvStore(), config.NewKeyringStore())
	}
	// Probe is deliberately left nil when unset. The default depends on the
	// server's proxy policy (DESIGN.md「代理策略」), which is only known after the
	// configuration is loaded, so each call site resolves it via
	// endpoint.ProbeWith. Filling in TCPProbe here would silently pin every
	// command to a direct dial and reintroduce the probe/login split.
	if d.Out == nil {
		d.Out = os.Stdout
	}
	if d.Err == nil {
		d.Err = os.Stderr
	}
	if d.Stdin == nil {
		d.Stdin = os.Stdin
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.State == nil {
		d.State = defaultStateStore(d)
	}
	d = d.withRuntimeDefaults()
	return d
}

// defaultStateStore places the cross-process endpoint memory next to the
// configuration file, so that an injected ConfigPath also relocates it
// instead of scattering state into the platform default directory.
func defaultStateStore(d Deps) endpoint.StateStore {
	dir, err := stateDir(d)
	if err != nil {
		return endpoint.NewMemoryStateStore()
	}
	return endpoint.NewFileStateStore(filepath.Join(dir, endpoint.StateFileName))
}

// stateDir returns the directory that holds config.toml and state.json.
func stateDir(d Deps) (string, error) {
	if p := d.ConfigPath; p != "" {
		return filepath.Dir(p), nil
	}
	return config.ConfigDir()
}
