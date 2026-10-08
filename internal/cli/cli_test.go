package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MiFaZhan/jms-client/internal/api"
	"github.com/MiFaZhan/jms-client/internal/auth"
	"github.com/MiFaZhan/jms-client/internal/config"
	"github.com/MiFaZhan/jms-client/internal/endpoint"
)

// cliEnv is the hermetic harness every test in this package drives the
// command tree through.
//
// It supplies an in-memory credential store, an in-memory endpoint state
// store, a recording probe, a recording login factory and buffers for
// stdout/stderr: no network, no OS credential store, no terminal and no
// wall-clock sleep.
type cliEnv struct {
	t       *testing.T
	cfgPath string
	creds   config.CredentialStore
	state   endpoint.StateStore

	out    bytes.Buffer
	errOut bytes.Buffer

	probed   []string
	loggedIn []string

	// clientURL is what the fake session's API client points at; tests
	// set it to an httptest server so assets.List/Search run for real.
	clientURL string
	// loginErr is returned by the login seam when non-nil.
	loginErr error

	probeFn endpoint.ProbeFunc

	// promptLine and promptSecret answer the interactive prompts; nil
	// means the command falls back to Stdin, which the tests keep empty.
	promptLine   func(string) (string, error)
	promptSecret func(string) (string, error)
	// confirmAnswer is returned by the injected confirmation hook; it is
	// only installed when confirmSet is true.
	confirmAnswer bool
	confirmSet    bool

	// auditDirOverride points Runtime.AuditDir at a test location.
	auditDirOverride string
	// attachFn replaces the IPC dial seam for attach tests.
	attachFn AttachFunc
	// serveMCPFn replaces the MCP server seam.
	serveMCPFn ServeMCPFunc

	// rt is the Runtime the injected hooks live on.
	rt *Runtime

	// cancel is the command context's cancel func, installed by withCancel so
	// a blocking command can be released the way Ctrl-C releases it.
	cancel      context.CancelFunc
	ctxOverride context.Context
}

func newCLIEnv(t *testing.T) *cliEnv {
	t.Helper()
	env := &cliEnv{
		t:       t,
		cfgPath: filepath.Join(t.TempDir(), "config.toml"),
		creds:   config.NewMemoryStore(),
		state:   endpoint.NewMemoryStateStore(),
	}
	env.probeFn = func(_ context.Context, url string, _ time.Duration) bool {
		env.probed = append(env.probed, url)
		return true
	}
	return env
}

// deps returns a fully populated Deps for one command invocation.
func (e *cliEnv) deps() Deps {
	d := Deps{
		ConfigPath: e.cfgPath,
		Creds:      e.creds,
		Probe:      e.probeFn,
		State:      e.state,
		Login:      e.login,
		Stdin:      strings.NewReader(""),
		Out:        &e.out,
		Err:        &e.errOut,
		Now:        stepClock(25 * time.Millisecond),
	}
	if e.promptLine != nil {
		d.PromptLine = e.promptLine
	}
	if e.promptSecret != nil {
		d.PromptPassword = e.promptSecret
	}
	if e.confirmSet {
		answer := e.confirmAnswer
		d.Confirm = func(string) (bool, error) { return answer, nil }
	}
	// Runtime is built by WithDefaults, so the test hooks are installed on a
	// Runtime this environment owns rather than on a nil pointer.
	d.Runtime = e.runtime()
	if e.auditDirOverride != "" {
		d.Runtime.AuditDir = e.auditDirOverride
	}
	if e.attachFn != nil {
		d.Runtime.Attach = e.attachFn
	}
	if e.serveMCPFn != nil {
		d.Runtime.ServeMCP = e.serveMCPFn
	}
	return d
}

// withCancel gives the environment a cancellable command context.
func (e *cliEnv) withCancel() {
	e.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	e.cancel = cancel
	e.ctxOverride = ctx
	e.t.Cleanup(cancel)
}

// runtime returns this environment's Runtime, creating it once.
func (e *cliEnv) runtime() *Runtime {
	if e.rt == nil {
		e.rt = &Runtime{}
	}
	return e.rt
}

// login is the fake authentication seam.
//
// It records the address the CLI chose and answers with a session whose
// api.Client points at e.clientURL, so the assets code path is exercised
// against a local httptest server rather than the real network.
func (e *cliEnv) login(_ context.Context, srv *config.ServerConfig, baseURL string) (*auth.Session, error) {
	e.loggedIn = append(e.loggedIn, baseURL)
	if e.loginErr != nil {
		return nil, e.loginErr
	}
	return &auth.Session{Server: srv, Client: api.New(e.clientURL), BaseURL: e.clientURL}, nil
}

// run drives the command tree and returns the process exit code.
func (e *cliEnv) run(args ...string) int {
	e.t.Helper()
	e.out.Reset()
	e.errOut.Reset()
	e.probed = nil
	e.loggedIn = nil
	if e.ctxOverride != nil {
		return executeArgsContext(e.ctxOverride, args, e.deps())
	}
	return ExecuteArgs(args, e.deps())
}

// stdout and stderr expose the captured streams.
func (e *cliEnv) stdout() string { return e.out.String() }
func (e *cliEnv) stderr() string { return e.errOut.String() }

// seedServer writes one server entry into the configuration file.
func (e *cliEnv) seedServer(name, internal, external, username string) {
	e.t.Helper()
	cfg, err := config.Load(e.cfgPath)
	if err != nil {
		cfg = &config.AppConfig{}
	}
	if err := cfg.Put(&config.ServerConfig{
		Name: name, Internal: internal, External: external, Username: username,
	}); err != nil {
		e.t.Fatalf("seed %s: %v", name, err)
	}
	if err := config.Save(e.cfgPath, cfg); err != nil {
		e.t.Fatalf("seed %s: %v", name, err)
	}
}

// setDefaultServer rewrites the default alias in the config file.
func (e *cliEnv) setDefaultServer(name string) {
	e.t.Helper()
	cfg, err := config.Load(e.cfgPath)
	if err != nil {
		e.t.Fatalf("load for set-default: %v", err)
	}
	if err := cfg.SetDefault(name); err != nil {
		e.t.Fatalf("set default %s: %v", name, err)
	}
	if err := config.Save(e.cfgPath, cfg); err != nil {
		e.t.Fatalf("save default %s: %v", name, err)
	}
}

// requireNoFile asserts that the configuration file was never created.
func (e *cliEnv) requireNoFile() {
	e.t.Helper()
	if _, err := os.Stat(e.cfgPath); err == nil {
		e.t.Fatalf("config file %s exists although the command should have failed first", e.cfgPath)
	}
}

// requireNoCredential asserts that nothing is stored for an alias.
func (e *cliEnv) requireNoCredential(alias string) {
	e.t.Helper()
	for _, kind := range []string{config.CredPassword, config.CredOTP} {
		if value, err := e.creds.Get(alias, kind); err == nil {
			e.t.Fatalf("credential %s/%s was stored as %q although the command should have failed first",
				alias, kind, value)
		}
	}
}

// stepClock returns a deterministic clock that advances by step on every
// read, so probe durations are exact and no test sleeps.
func stepClock(step time.Duration) func() time.Time {
	cur := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	return func() time.Time {
		now := cur
		cur = cur.Add(step)
		return now
	}
}

// lineQueue returns a PromptLine hook that answers in order.
func lineQueue(values ...string) func(string) (string, error) {
	queue := append([]string(nil), values...)
	return func(prompt string) (string, error) {
		if len(queue) == 0 {
			return "", promptExhaustedError("prompt sequence exhausted at " + prompt)
		}
		value := queue[0]
		queue = queue[1:]
		return value, nil
	}
}

// promptExhaustedError reports that a test supplied too few answers.
type promptExhaustedError string

func (e promptExhaustedError) Error() string { return string(e) }

// assetsHandler answers the self-assets endpoint with a fixed page.
func assetsHandler(items []map[string]any, record func(*http.Request)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if record != nil {
			record(r)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"count":   len(items),
			"results": items,
		})
	}
}

// newAssetServer starts a local httptest server; the CLI never reaches
// the real network.
func newAssetServer(t *testing.T, items []map[string]any, record func(*http.Request)) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(assetsHandler(items, record))
	t.Cleanup(server.Close)
	return server
}

// assetRow builds one asset listing row.
func assetRow(id, name, address, platform string) map[string]any {
	return map[string]any{
		"id":       id,
		"name":     name,
		"address":  address,
		"platform": map[string]any{"name": platform},
	}
}

// assetRows builds count distinct asset rows.
func assetRows(count int) []map[string]any {
	rows := make([]map[string]any, 0, count)
	for i := 0; i < count; i++ {
		rows = append(rows, assetRow(
			"id-"+string(rune('a'+i)),
			"asset-"+string(rune('a'+i)),
			"10.0.0."+string(rune('1'+i)),
			"Linux",
		))
	}
	return rows
}

// --- root-level behaviour ---

func TestVersionPrintsVersionAndExitsZero(t *testing.T) {
	env := newCLIEnv(t)
	if code := env.run("version"); code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", code, env.stderr())
	}
	if !strings.Contains(env.stdout(), "jms "+Version) {
		t.Fatalf("stdout = %q, want it to contain %q", env.stdout(), "jms "+Version)
	}
	if env.stderr() != "" {
		t.Fatalf("stderr = %q, want empty", env.stderr())
	}
}

// TestHelpAdvertisesTheFullCommandSurface pins DESIGN.md「总体架构」: every command
// in the documented surface is registered.
func TestHelpAdvertisesTheFullCommandSurface(t *testing.T) {
	want := []string{
		"config", "ls", "version",
		"exec", "login", "sftp", "ssh-pipe",
		"mcp", "tail", "attach", "log",
	}

	env := newCLIEnv(t)
	root := NewRootCommand(env.deps())
	for _, name := range want {
		if cmd, _, err := root.Find([]string{name}); err != nil || cmd.Name() != name {
			t.Errorf("root command tree does not register %q (err = %v)", name, err)
		}
	}

	if code := env.run("--help"); code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", code, env.stderr())
	}
	help := env.stdout()
	for _, name := range want {
		if !strings.Contains(help, name) {
			t.Errorf("jms --help does not mention %q:\n%s", name, help)
		}
	}
}

// TestUnknownCommandFailsInsteadOfPrintingHelp pins the fix for a
// silent-success bug: a root command with no Run is "not runnable", so cobra
// treated an unknown subcommand as a help request and exited 0. A script
// using `jms bogus && next` would have continued as if it had succeeded.
func TestUnknownCommandFailsInsteadOfPrintingHelp(t *testing.T) {
	env := newCLIEnv(t)
	code := env.run("bogus-command", "some-target")
	if code == 0 {
		t.Fatalf("`jms bogus-command` exited 0, want non-zero (stdout: %s)", env.stdout())
	}
	if !strings.Contains(env.stderr(), "unknown command") {
		t.Fatalf("stderr = %q, want it to name the unknown command", env.stderr())
	}
}

// TestNoArgumentsPrintsHelpAndSucceeds keeps bare `jms` friendly.
func TestNoArgumentsPrintsHelpAndSucceeds(t *testing.T) {
	env := newCLIEnv(t)
	if code := env.run(); code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", code, env.stderr())
	}
	if !strings.Contains(env.stdout(), "Available Commands") {
		t.Fatalf("stdout = %q, want the help text", env.stdout())
	}
}

func TestLibraryErrorRendersOnStderrWithoutPanic(t *testing.T) {
	env := newCLIEnv(t)
	env.cfgPath = filepath.Join(t.TempDir(), "missing", "config.toml")

	code := env.run("config", "list")
	if code == 0 {
		t.Fatalf("exit code = 0, want non-zero (stdout: %s)", env.stdout())
	}
	if !strings.HasPrefix(env.stderr(), "Error: ") {
		t.Fatalf("stderr = %q, want it to start with %q", env.stderr(), "Error: ")
	}
	if !strings.Contains(env.stderr(), "config file not found") {
		t.Fatalf("stderr = %q, want it to explain the missing config", env.stderr())
	}
	if strings.Contains(env.stderr(), "Usage:") {
		t.Fatalf("stderr = %q, want no usage dump for a runtime error", env.stderr())
	}
}

func TestConfigPathPrecedence(t *testing.T) {
	dir := t.TempDir()
	depsPath := filepath.Join(dir, "deps.toml")
	envPath := filepath.Join(dir, "env.toml")
	flagPath := filepath.Join(dir, "flag.toml")

	for path, alias := range map[string]string{
		depsPath: "from-deps",
		envPath:  "from-env",
		flagPath: "from-flag",
	} {
		cfg := &config.AppConfig{Servers: map[string]*config.ServerConfig{
			alias: {Name: alias, Internal: "http://a.example:2280/", Username: "u"},
		}}
		if err := config.Save(path, cfg); err != nil {
			t.Fatalf("seed %s: %v", path, err)
		}
	}

	t.Run("deps path wins over env and flag", func(t *testing.T) {
		env := newCLIEnv(t)
		env.cfgPath = depsPath
		t.Setenv(config.EnvConfig, envPath)
		env.run("config", "list", "--config", flagPath)
		if !strings.Contains(env.stdout(), "from-deps") {
			t.Fatalf("stdout = %q, want the Deps.ConfigPath server", env.stdout())
		}
	})

	t.Run("flag wins over env", func(t *testing.T) {
		env := newCLIEnv(t)
		env.cfgPath = ""
		t.Setenv(config.EnvConfig, envPath)
		env.run("config", "list", "--config", flagPath)
		if !strings.Contains(env.stdout(), "from-flag") {
			t.Fatalf("stdout = %q, want the --config server", env.stdout())
		}
	})

	t.Run("env wins over the platform default", func(t *testing.T) {
		env := newCLIEnv(t)
		env.cfgPath = ""
		t.Setenv(config.EnvConfig, envPath)
		env.run("config", "list")
		if !strings.Contains(env.stdout(), "from-env") {
			t.Fatalf("stdout = %q, want the JMS_CONFIG server", env.stdout())
		}
	})
}
